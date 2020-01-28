// Copyright 2019 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	version "github.com/hashicorp/go-version"
	homedir "github.com/mitchellh/go-homedir"
)

const (
	bazelReal      = "BAZEL_REAL"
	skipWrapperEnv = "BAZELISK_SKIP_WRAPPER"
	bazelURLEnv    = "BAZELISK_BASE_URL"
	wrapperPath    = "./tools/bazel"
	bazelUpstream  = "bazelbuild"
)

var (
	BazeliskVersion = "development"
)

func findWorkspaceRoot(root string) string {
	if _, err := os.Stat(filepath.Join(root, "WORKSPACE")); err == nil {
		return root
	}

	if _, err := os.Stat(filepath.Join(root, "WORKSPACE.bazel")); err == nil {
		return root
	}

	parentDirectory := filepath.Dir(root)
	if parentDirectory == root {
		return ""
	}

	return findWorkspaceRoot(parentDirectory)
}

func getBazelVersion() (string, error) {
	// Check in this order:
	// - env var "USE_BAZEL_VERSION" is set to a specific version.
	// - env var "USE_NIGHTLY_BAZEL" or "USE_BAZEL_NIGHTLY" is set -> latest
	//   nightly. (TODO)
	// - env var "USE_CANARY_BAZEL" or "USE_BAZEL_CANARY" is set -> latest
	//   rc. (TODO)
	// - the file workspace_root/tools/bazel exists -> that version. (TODO)
	// - workspace_root/.bazelversion exists -> read contents, that version.
	// - workspace_root/WORKSPACE contains a version -> that version. (TODO)
	// - fallback: latest release
	bazelVersion := os.Getenv("USE_BAZEL_VERSION")
	if len(bazelVersion) != 0 {
		return bazelVersion, nil
	}

	workingDirectory, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("could not get working directory: %v", err)
	}

	workspaceRoot := findWorkspaceRoot(workingDirectory)
	if len(workspaceRoot) != 0 {
		bazelVersionPath := filepath.Join(workspaceRoot, ".bazelversion")
		if _, err := os.Stat(bazelVersionPath); err == nil {
			f, err := os.Open(bazelVersionPath)
			if err != nil {
				return "", fmt.Errorf("could not read %s: %v", bazelVersionPath, err)
			}
			defer f.Close()

			scanner := bufio.NewScanner(f)
			scanner.Scan()
			bazelVersion := scanner.Text()
			if err := scanner.Err(); err != nil {
				return "", fmt.Errorf("could not read version from file %s: %v", bazelVersion, err)
			}

			if len(bazelVersion) != 0 {
				return bazelVersion, nil
			}
		}
	}

	return "latest", nil
}

type versionDetails struct {
	IsSourceReference bool
	Fork              string
	VersionOrCommit   string
}

func parseBazelForkAndVersion(bazelForkAndVersion string) (*versionDetails, error) {
	var bazelFork, bazelVersionOrCommit string

	versionInfo := strings.Split(bazelForkAndVersion, "/")

	var isSourceBuild = false

	if len(versionInfo) == 1 {
		bazelFork, bazelVersionOrCommit = bazelUpstream, versionInfo[0]
	} else if len(versionInfo) == 2 {
		bazelFork, bazelVersionOrCommit = versionInfo[0], versionInfo[1]
	} else if len(versionInfo) == 3 {
		if versionInfo[1] != "commit" {
			return nil, fmt.Errorf("invalid syntax \"%s\" for source version string, valid format is <FORK>/commit/<SHA>", bazelForkAndVersion)
		}
		bazelFork, bazelVersionOrCommit, isSourceBuild = versionInfo[0], versionInfo[2], true
	} else {
		return nil, fmt.Errorf("invalid version \"%s\", could not parse version with more than 3 components", bazelForkAndVersion)
	}

	result := &versionDetails{
		IsSourceReference: isSourceBuild,
		Fork:              bazelFork,
		VersionOrCommit:   bazelVersionOrCommit,
	}

	return result, nil
}

type release struct {
	TagName    string `json:"tag_name"`
	Prerelease bool   `json:"prerelease"`
}

func readRemoteFile(url string, token string) ([]byte, error) {
	client := &http.Client{}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("could not create request: %v", err)
	}

	if token != "" {
		req.Header.Set("Authorization", "token "+token)
	}

	res, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not fetch %s: %v", url, err)
	}
	defer res.Body.Close()

	if res.StatusCode != 200 {
		return nil, fmt.Errorf("unexpected status code while reading %s: %v", url, res.StatusCode)
	}

	body, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read content at %s: %v", url, err)
	}
	return body, nil
}

// maybeDownload will download a file from the given url and cache the result under bazeliskHome.
// It skips the download if the file already exists and is not outdated.
// description is used only to provide better error messages.
func maybeDownload(bazeliskHome, url, filename, description string) ([]byte, error) {
	cachePath := filepath.Join(bazeliskHome, filename)

	if cacheStat, err := os.Stat(cachePath); err == nil {
		if time.Since(cacheStat.ModTime()).Hours() < 1 {
			res, err := ioutil.ReadFile(cachePath)
			if err != nil {
				return nil, fmt.Errorf("could not read %s: %v", cachePath, err)
			}
			return res, nil
		}
	}

	// We could also use go-github here, but I can't get it to build with Bazel's rules_go and it pulls in a lot of dependencies.
	body, err := readRemoteFile(url, os.Getenv("BAZELISK_GITHUB_TOKEN"))
	if err != nil {
		return nil, fmt.Errorf("could not download %s: %v", description, err)
	}

	err = ioutil.WriteFile(cachePath, body, 0666)
	if err != nil {
		return nil, fmt.Errorf("could not create %s: %v", cachePath, err)
	}

	return body, nil
}

func resolveLatestVersion(bazeliskHome, bazelFork string, offset int) (string, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/bazel/releases", bazelFork)
	releasesJSON, err := maybeDownload(bazeliskHome, url, bazelFork+"-releases.json", "list of Bazel releases from github.com/"+bazelFork)
	if err != nil {
		return "", fmt.Errorf("could not get releases from github.com/%s/bazel: %v", bazelFork, err)
	}

	var releases []release
	if err := json.Unmarshal(releasesJSON, &releases); err != nil {
		return "", fmt.Errorf("could not parse JSON into list of releases: %v", err)
	}

	var tags []string
	for _, release := range releases {
		if release.Prerelease {
			continue
		}
		tags = append(tags, release.TagName)
	}
	return getNthMostRecentVersion(tags, offset)
}

func getNthMostRecentVersion(versions []string, offset int) (string, error) {
	if offset >= len(versions) {
		return "", fmt.Errorf("cannot resolve version \"latest-%d\": There are only %d Bazel versions", offset, len(versions))
	}

	wrappers := make([]*version.Version, len(versions))
	for i, v := range versions {
		wrapper, err := version.NewVersion(v)
		if err != nil {
			log.Printf("WARN: Could not parse version: %s", v)
		}
		wrappers[i] = wrapper
	}
	sort.Sort(version.Collection(wrappers))
	return wrappers[len(wrappers)-1-offset].Original(), nil
}

type gcsListResponse struct {
	Prefixes []string `json:"prefixes"`
}

func resolveLatestRcVersion() (string, error) {
	versions, err := listDirectoriesInReleaseBucket("")
	if err != nil {
		return "", fmt.Errorf("could not list Bazel versions in GCS bucket: %v", err)
	}

	latestVersion, err := getHighestBazelVersion(versions)
	if err != nil {
		return "", fmt.Errorf("got invalid version number: %v", err)
	}

	// Append slash to match directories
	rcVersions, err := listDirectoriesInReleaseBucket(latestVersion + "/")
	if err != nil {
		return "", fmt.Errorf("could not list release candidates for latest release: %v", err)
	}
	return getHighestRcVersion(rcVersions)
}

func listDirectoriesInReleaseBucket(prefix string) ([]string, error) {
	url := "https://www.googleapis.com/storage/v1/b/bazel/o?delimiter=/"
	if prefix != "" {
		url = fmt.Sprintf("%s&prefix=%s", url, prefix)
	}
	content, err := readRemoteFile(url, "")
	if err != nil {
		return nil, fmt.Errorf("could not list GCS objects at %s: %v", url, err)
	}

	var response gcsListResponse
	if err := json.Unmarshal(content, &response); err != nil {
		return nil, fmt.Errorf("could not parse GCS index JSON: %v", err)
	}
	return response.Prefixes, nil
}

func getHighestBazelVersion(versions []string) (string, error) {
	for i, v := range versions {
		versions[i] = strings.TrimSuffix(v, "/")
	}
	return getNthMostRecentVersion(versions, 0)
}

func getHighestRcVersion(versions []string) (string, error) {
	var version string
	var lastRc int
	re := regexp.MustCompile(`(\d+.\d+.\d+)/rc(\d+)/`)
	for _, v := range versions {
		// Fallback: use latest release if there is no active RC.
		if strings.Index(v, "release") > -1 {
			return strings.Split(v, "/")[0], nil
		}

		m := re.FindStringSubmatch(v)
		version = m[1]
		rc, err := strconv.Atoi(m[2])
		if err != nil {
			return "", fmt.Errorf("Invalid version number %s: %v", strings.TrimSuffix(v, "/"), err)
		}
		if rc > lastRc {
			lastRc = rc
		}
	}
	return fmt.Sprintf("%src%d", version, lastRc), nil
}

func resolveVersionLabel(bazeliskHome, bazelFork, bazelVersion string) (string, bool, error) {
	if bazelFork == bazelUpstream {
		// Returns three values:
		// 1. The label of a Blaze release (if the label resolves to a release) or a commit (for unreleased binaries),
		// 2. Whether the first value refers to a commit,
		// 3. An error.
		lastGreenCommitPathSuffixes := map[string]string{
			"last_green":            "github.com/bazelbuild/bazel.git/bazel-bazel",
			"last_downstream_green": "downstream_pipeline",
		}
		if pathSuffix, ok := lastGreenCommitPathSuffixes[bazelVersion]; ok {
			commit, err := getLastGreenCommit(pathSuffix)
			if err != nil {
				return "", false, fmt.Errorf("cannot resolve last green commit: %v", err)
			}

			return commit, true, nil
		}

		if bazelVersion == "last_rc" {
			version, err := resolveLatestRcVersion()
			return version, false, err
		}
	}

	r := regexp.MustCompile(`^latest(?:-(?P<offset>\d+))?$`)

	match := r.FindStringSubmatch(bazelVersion)
	if match != nil {
		offset := 0
		if match[1] != "" {
			var err error
			offset, err = strconv.Atoi(match[1])
			if err != nil {
				return "", false, fmt.Errorf("invalid version \"%s\", could not parse offset: %v", bazelVersion, err)
			}
		}
		version, err := resolveLatestVersion(bazeliskHome, bazelFork, offset)
		return version, false, err
	}

	return bazelVersion, false, nil
}

const lastGreenBasePath = "https://storage.googleapis.com/bazel-untrusted-builds/last_green_commit/"

func getLastGreenCommit(pathSuffix string) (string, error) {
	content, err := readRemoteFile(lastGreenBasePath+pathSuffix, "")
	if err != nil {
		return "", fmt.Errorf("could not determine last green commit: %v", err)
	}
	return strings.TrimSpace(string(content)), nil
}

func determineBazelFilename(version string) (string, error) {
	var machineName string
	switch runtime.GOARCH {
	case "amd64":
		machineName = "x86_64"
	default:
		return "", fmt.Errorf("unsupported machine architecture \"%s\", must be x86_64", runtime.GOARCH)
	}

	var osName string
	switch runtime.GOOS {
	case "darwin", "linux", "windows":
		osName = runtime.GOOS
	default:
		return "", fmt.Errorf("unsupported operating system \"%s\", must be Linux, macOS or Windows", runtime.GOOS)
	}

	filenameSuffix := ""
	if runtime.GOOS == "windows" {
		filenameSuffix = ".exe"
	}

	return fmt.Sprintf("bazel-%s-%s-%s%s", version, osName, machineName, filenameSuffix), nil
}

func determineDistributionURL(fork string, version string, isCommit bool, filename string) string {
	baseURL := os.Getenv(bazelURLEnv)

	if isCommit {
		if len(baseURL) == 0 {
			baseURL = "https://storage.googleapis.com/bazel-builds/artifacts"
		}
		var platforms = map[string]string{"darwin": "macos", "linux": "ubuntu1404", "windows": "windows"}
		// No need to check the OS thanks to determineBazelFilename().
		log.Printf("Using unreleased version at commit %s", version)
		return fmt.Sprintf("%s/%s/%s/bazel", baseURL, platforms[runtime.GOOS], version)
	}

	kind := "release"
	if strings.Contains(version, "rc") {
		versionComponents := strings.Split(version, "rc")
		// Replace version with the part before rc
		version = versionComponents[0]
		kind = "rc" + versionComponents[1]
	}

	if len(baseURL) != 0 {
		return fmt.Sprintf("%s/%s/%s", baseURL, version, filename)
	}

	if fork == bazelUpstream {
		return fmt.Sprintf("https://releases.bazel.build/%s/%s/%s", version, kind, filename)
	}

	return fmt.Sprintf("https://github.com/%s/bazel/releases/download/%s/%s", fork, version, filename)
}

func determineSourceURL(fork string) string {
	baseUrl := os.Getenv(bazelURLEnv)

	if len(baseUrl) == 0 {
		baseUrl = "ssh://git@github.com"
	}

	return fmt.Sprintf("%s/%s/bazel.git", baseUrl, fork)
}

func downloadBazel(fork string, version string, isCommit bool, directory string) (string, error) {
	filename, err := determineBazelFilename(version)
	if err != nil {
		return "", fmt.Errorf("could not determine filename to use for Bazel binary: %v", err)
	}

	url := determineDistributionURL(fork, version, isCommit, filename)
	destinationPath := filepath.Join(directory, filename)

	if _, err := os.Stat(destinationPath); err != nil {
		tmpfile, err := ioutil.TempFile(directory, "download")
		if err != nil {
			return "", fmt.Errorf("could not create temporary file: %v", err)
		}
		defer func() {
			err := tmpfile.Close()
			if err == nil {
				os.Remove(tmpfile.Name())
			}
		}()

		log.Printf("Downloading %s...", url)
		resp, err := http.Get(url)
		if err != nil {
			return "", fmt.Errorf("HTTP GET %s failed: %v", url, err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != 200 {
			return "", fmt.Errorf("HTTP GET %s failed with error %v", url, resp.StatusCode)
		}

		_, err = io.Copy(tmpfile, resp.Body)
		if err != nil {
			return "", fmt.Errorf("could not copy from %s to %s: %v", url, tmpfile.Name(), err)
		}

		err = os.Chmod(tmpfile.Name(), 0755)
		if err != nil {
			return "", fmt.Errorf("could not chmod file %s: %v", tmpfile.Name(), err)
		}

		tmpfile.Close()
		err = os.Rename(tmpfile.Name(), destinationPath)
		if err != nil {
			return "", fmt.Errorf("could not move %s to %s: %v", tmpfile.Name(), destinationPath, err)
		}
	}

	return destinationPath, nil
}

func bazelFromSource(bazeliskHome string, fork string, commitHash string) (string, error) {
	binaryDirectory := filepath.Join(bazeliskHome, "bin", fork)

	if err := os.MkdirAll(binaryDirectory, 0755); err != nil {
		return "", fmt.Errorf("failed to create binary directory %s: %v", binaryDirectory, err)
	}

	identifier := fmt.Sprintf("bazel-%s", commitHash)
	bazelBinary := filepath.Join(binaryDirectory, identifier)

	// we keep a single folder per source checkout. This optimizes for the use-case that someone might be getting
	// their bazel version updated semi-regularly and we want to leverage the local build artifacts they already have
	// to speed up the rebuild process.
	checkoutDirectory := filepath.Join(bazeliskHome, "checkouts", fork)

	if _, err := os.Stat(bazelBinary); err != nil {
		if os.IsNotExist(err) {
			err := checkoutSource(checkoutDirectory, fork, commitHash)

			if err != nil {
				return "", fmt.Errorf("failed to checkout sources for %s/commit/%s: %v", fork, commitHash, err)
			}

			bootstrapBazel, _ := bazelFromPrebuilt(bazeliskHome, "bazelbuild", "latest")
			err = buildBazel(bootstrapBazel, checkoutDirectory, bazelBinary)

			if err != nil {
				return "", fmt.Errorf("failed to build bazel sources for %s/commit/%s: %v", fork, commitHash, err)
			}
		} else {
			return "", fmt.Errorf("unknown IO error at cached bazel binary %s: %v", bazelBinary, err)
		}
	}

	return bazelBinary, nil
}

func buildBazel(bootstrapBazel string, checkoutDirectory string, outputFile string) error {
	log.Printf("Building bazel")
	runOrFatal(checkoutDirectory, "failed to build bazel", bootstrapBazel, "build", "//src:bazel", "--compilation_mode=opt")

	in, err := os.Open(filepath.Join(checkoutDirectory, "bazel-bin", "src", "bazel"))

	if err != nil {
		return fmt.Errorf("failed to copy build output to bazelisk bin folder: %v", err)
	}
	defer in.Close()

	out, err := os.Create(outputFile)

	if err != nil {
		return fmt.Errorf("failed to copy build output to bazelisk bin folder: %v", err)
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return fmt.Errorf("failed to copy build output to bazelisk bin folder: %v", err)
	}

	if err := out.Chmod(0755); err != nil {
		return fmt.Errorf("failed to copy build output to bazelisk bin folder: %v", err)
	}

	out.Sync()

	return nil
}

func runOrFatal(cwd string, errorMessage string, command string, arg ...string) string {
	cmd := exec.Command(command, arg...)
	cmd.Dir = cwd

	bytes, err := cmd.CombinedOutput()

	if err != nil {
		log.Fatalf(errorMessage)
	}

	return string(bytes)
}

func checkoutSource(checkoutDirectory string, fork string, commitHash string) error {
	if _, err := os.Stat(filepath.Join(checkoutDirectory, ".git")); err != nil {
		if os.IsNotExist(err) {
			tmpCheckout, err := ioutil.TempDir("", filepath.Base(checkoutDirectory))

			if err != nil {
				return fmt.Errorf("failed to create temporary checkout directory for clone: %v", err)
			}


			cloneAddress := determineSourceURL(fork)

			log.Printf("Bazelisk is downloading bazel's source from %s, this will take a few minutes", cloneAddress)

			// do a minimal fetch here instead of a full clone
			runOrFatal(tmpCheckout, "failed to initialize repository", "git", "init")
			runOrFatal(tmpCheckout, "failed to set up remote", "git", "remote", "add", "origin", cloneAddress)
			runOrFatal(tmpCheckout, "failed to fetch initial revision", "git", "fetch", "origin", commitHash)
			runOrFatal(tmpCheckout, "failed to checkout initial revision", "git", "checkout", commitHash)

			if err := os.Rename(tmpCheckout, checkoutDirectory); err != nil {
				return fmt.Errorf("failed to move temporary source checkout %s to final location: %v", tmpCheckout, err)
			}
			log.Printf("Checkout of %s/bazel/commit/%s done", fork, commitHash)
		} else {
			return fmt.Errorf("unknown IO error while checking out git sources: %v", err)
		}
	}

	currentHash := strings.TrimSpace(runOrFatal(checkoutDirectory, "failed to get current revision", "git", "rev-list", "HEAD", "-n", "1"))

	if currentHash != commitHash {
		log.Printf("Updating sources from %s to %s", currentHash, commitHash)
		runOrFatal(checkoutDirectory, "failed to fetch new revision", "git", "fetch", "origin", commitHash)
		runOrFatal(checkoutDirectory, "failed to checkout new revision", "git", "reset", "--hard", commitHash)
	}

	return nil
}

func maybeDelegateToWrapper(bazel string) string {
	if os.Getenv(skipWrapperEnv) != "" {
		return bazel
	}

	wd, err := os.Getwd()
	if err != nil {
		return bazel
	}

	root := findWorkspaceRoot(wd)
	wrapper := filepath.Join(root, wrapperPath)
	if stat, err := os.Stat(wrapper); err != nil || stat.Mode().Perm()&0001 == 0 {
		return bazel
	}

	return wrapper
}

func runBazel(bazel string, args []string) (int, error) {
	execPath := maybeDelegateToWrapper(bazel)
	if execPath != bazel {
		os.Setenv(bazelReal, bazel)
	}

	cmd := exec.Command(execPath, args...)
	cmd.Env = append(os.Environ(), skipWrapperEnv+"=true")
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Start()
	if err != nil {
		return 1, fmt.Errorf("could not start Bazel: %v", err)
	}

	c := make(chan os.Signal)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		s := <-c
		if runtime.GOOS != "windows" {
			cmd.Process.Signal(s)
		} else {
			cmd.Process.Kill()
		}
	}()

	err = cmd.Wait()
	if err != nil {
		if exitError, ok := err.(*exec.ExitError); ok {
			waitStatus := exitError.Sys().(syscall.WaitStatus)
			return waitStatus.ExitStatus(), nil
		}
		return 1, fmt.Errorf("could not launch Bazel: %v", err)
	}
	return 0, nil
}

type label struct {
	Name string `json:"name"`
}

type issue struct {
	Title  string  `json:"title"`
	URL    string  `json:"html_url"`
	Labels []label `json:"labels"`
}

type issueList struct {
	Items []issue `json:"items"`
}

type flagDetails struct {
	Name          string
	ReleaseToFlip string
	IssueURL      string
}

func (f *flagDetails) String() string {
	return fmt.Sprintf("%s (Bazel %s: %s)", f.Name, f.ReleaseToFlip, f.IssueURL)
}

func getIncompatibleFlags(bazeliskHome, resolvedBazelVersion string) (map[string]*flagDetails, error) {
	result := make(map[string]*flagDetails)
	// GitHub labels use only major and minor version, we ignore the patch number (and any other suffix).
	re := regexp.MustCompile(`^\d+\.\d+`)
	version := re.FindString(resolvedBazelVersion)
	if len(version) == 0 {
		return nil, fmt.Errorf("invalid version %v", resolvedBazelVersion)
	}
	url := "https://api.github.com/search/issues?per_page=100&q=repo:bazelbuild/bazel+label:migration-" + version
	issuesJSON, err := maybeDownload(bazeliskHome, url, "flags-"+version, "list of flags from GitHub")
	if err != nil {
		return nil, fmt.Errorf("could not get issues from GitHub: %v", err)
	}

	var issueList issueList
	if err := json.Unmarshal(issuesJSON, &issueList); err != nil {
		return nil, fmt.Errorf("could not parse JSON into list of issues: %v", err)
	}

	re = regexp.MustCompile(`^incompatible_\w+`)
	for _, issue := range issueList.Items {
		flag := re.FindString(issue.Title)
		if len(flag) > 0 {
			name := "--" + flag
			result[name] = &flagDetails{
				Name:          name,
				ReleaseToFlip: getBreakingRelease(issue.Labels),
				IssueURL:      issue.URL,
			}
		}
	}

	return result, nil
}

func getBreakingRelease(labels []label) string {
	for _, l := range labels {
		if release := strings.TrimPrefix(l.Name, "breaking-change-"); release != l.Name {
			return release
		}
	}
	return "TBD"
}

// insertArgs will insert newArgs in baseArgs. If baseArgs contains the
// "--" argument, newArgs will be inserted before that. Otherwise, newArgs
// is appended.
func insertArgs(baseArgs []string, newArgs []string) []string {
	var result []string
	inserted := false
	for _, arg := range baseArgs {
		if !inserted && arg == "--" {
			result = append(result, newArgs...)
			inserted = true
		}
		result = append(result, arg)
	}

	if !inserted {
		result = append(result, newArgs...)
	}
	return result
}

func shutdownIfNeeded(bazelPath string) {
	bazeliskClean := os.Getenv("BAZELISK_SHUTDOWN")
	if len(bazeliskClean) == 0 {
		return
	}

	fmt.Printf("bazel shutdown\n")
	exitCode, err := runBazel(bazelPath, []string{"shutdown"})
	fmt.Printf("\n")
	if err != nil {
		log.Fatalf("failed to run bazel shutdown: %v", err)
	}
	if exitCode != 0 {
		fmt.Printf("Failure: shutdown command failed.\n")
		os.Exit(exitCode)
	}
}

func cleanIfNeeded(bazelPath string) {
	bazeliskClean := os.Getenv("BAZELISK_CLEAN")
	if len(bazeliskClean) == 0 {
		return
	}

	fmt.Printf("bazel clean --expunge\n")
	exitCode, err := runBazel(bazelPath, []string{"clean", "--expunge"})
	fmt.Printf("\n")
	if err != nil {
		log.Fatalf("failed to run clean: %v", err)
	}
	if exitCode != 0 {
		fmt.Printf("Failure: clean command failed.\n")
		os.Exit(exitCode)
	}
}

// migrate will run Bazel with each newArgs separately and report which ones are failing.
func migrate(bazelPath string, baseArgs []string, flags map[string]*flagDetails) {
	newArgs := getSortedKeys(flags)
	// 1. Try with all the flags.
	args := insertArgs(baseArgs, newArgs)
	fmt.Printf("\n\n--- Running Bazel with all incompatible flags\n\n")
	shutdownIfNeeded(bazelPath)
	cleanIfNeeded(bazelPath)
	fmt.Printf("bazel %s\n", strings.Join(args, " "))
	exitCode, err := runBazel(bazelPath, args)
	if err != nil {
		log.Fatalf("could not run Bazel: %v", err)
	}
	if exitCode == 0 {
		fmt.Printf("Success: No migration needed.\n")
		os.Exit(0)
	}

	// 2. Try with no flags, as a sanity check.
	args = baseArgs
	fmt.Printf("\n\n--- Running Bazel with no incompatible flags\n\n")
	shutdownIfNeeded(bazelPath)
	cleanIfNeeded(bazelPath)
	fmt.Printf("bazel %s\n", strings.Join(args, " "))
	exitCode, err = runBazel(bazelPath, args)
	if err != nil {
		log.Fatalf("could not run Bazel: %v", err)
	}
	if exitCode != 0 {
		fmt.Printf("Failure: Command failed, even without incompatible flags.\n")
		os.Exit(exitCode)
	}

	// 3. Try with each flag separately.
	var passList []string
	var failList []string
	for _, arg := range newArgs {
		args = insertArgs(baseArgs, []string{arg})
		fmt.Printf("\n\n--- Running Bazel with %s\n\n", arg)
		shutdownIfNeeded(bazelPath)
		cleanIfNeeded(bazelPath)
		fmt.Printf("bazel %s\n", strings.Join(args, " "))
		exitCode, err = runBazel(bazelPath, args)
		if err != nil {
			log.Fatalf("could not run Bazel: %v", err)
		}
		if exitCode == 0 {
			passList = append(passList, arg)
		} else {
			failList = append(failList, arg)
		}
	}

	print := func(l []string) {
		for _, arg := range l {
			fmt.Printf("  %s\n", flags[arg])
		}
	}

	// 4. Print report
	fmt.Printf("\n\n+++ Result\n\n")
	fmt.Printf("Command was successful with the following flags:\n")
	print(passList)
	fmt.Printf("\n")
	fmt.Printf("Migration is needed for the following flags:\n")
	print(failList)

	os.Exit(1)
}

func main() {
	bazeliskHome := os.Getenv("BAZELISK_HOME")
	if len(bazeliskHome) == 0 {
		userCacheDir, err := os.UserCacheDir()
		if err != nil {
			log.Fatalf("could not get the user's cache directory: %v", err)
		}

		bazeliskHome = filepath.Join(userCacheDir, "bazelisk")
	}

	err := os.MkdirAll(bazeliskHome, 0755)
	if err != nil {
		log.Fatalf("could not create directory %s: %v", bazeliskHome, err)
	}

	bazelVersionString, err := getBazelVersion()
	if err != nil {
		log.Fatalf("could not get Bazel version: %v", err)
	}

	bazelPath, err := homedir.Expand(bazelVersionString)
	if err != nil {
		log.Fatalf("could not expand home directory in path: %v", err)
	}

	// If the Bazel version is an absolute path to a Bazel binary in the filesystem, we can
	// use it directly. In that case, we don't know which exact version it is, though.
	resolvedBazelVersion := "unknown"

	// If we aren't using a local Bazel binary, we'll have to parse the version string and
	// download the version that the user wants.
	if !filepath.IsAbs(bazelPath) {
		versionInfo, err := parseBazelForkAndVersion(bazelVersionString)

		if err != nil {
			log.Fatalf("could not parse Bazel fork and version: %v", err)
		}

		if versionInfo.IsSourceReference {
			bazelPath, err = bazelFromSource(bazeliskHome, versionInfo.Fork, versionInfo.VersionOrCommit)

			if err != nil {
				log.Fatalf("could not build Bazel from source: %v", err)
			}
		} else {
			bazelPath, resolvedBazelVersion = bazelFromPrebuilt(bazeliskHome, versionInfo.Fork, versionInfo.VersionOrCommit)
		}
	}

	args := os.Args[1:]

	// --strict and --migrate must be the first argument.
	if len(args) > 0 && (args[0] == "--strict" || args[0] == "--migrate") {
		cmd := args[0]
		newFlags, err := getIncompatibleFlags(bazeliskHome, resolvedBazelVersion)
		if err != nil {
			log.Fatalf("could not get the list of incompatible flags: %v", err)
		}

		if cmd == "--migrate" {
			migrate(bazelPath, args[1:], newFlags)
		} else {
			// When --strict is present, it expands to the list of --incompatible_ flags
			// that should be enabled for the given Bazel version.
			args = insertArgs(args[1:], getSortedKeys(newFlags))
		}
	}

	// print bazelisk version information if "version" is the first argument
	// bazel version is executed after this command
	if len(args) > 0 && args[0] == "version" {
		// Check if the --gnu_format flag is set, if that is the case,
		// the version is printed differently
		var gnuFormat bool
		for _, arg := range args {
			if arg == "--gnu_format" {
				gnuFormat = true
				break
			}
		}

		if gnuFormat {
			fmt.Printf("Bazelisk %s\n", BazeliskVersion)
		} else {
			fmt.Printf("Bazelisk version: %s\n", BazeliskVersion)
		}
	}

	exitCode, err := runBazel(bazelPath, args)
	if err != nil {
		log.Fatalf("could not run Bazel: %v", err)
	}
	os.Exit(exitCode)
}

func bazelFromPrebuilt(bazeliskHome string, bazelFork string, bazelVersion string) (string, string) {
	resolvedBazelVersion, isCommit, err := resolveVersionLabel(bazeliskHome, bazelFork, bazelVersion)
	if err != nil {
		log.Fatalf("could not resolve the bazelVersion '%s' to an actual bazelVersion number: %v", bazelVersion, err)
	}

	bazelDirectory := filepath.Join(bazeliskHome, "bin", bazelFork)
	err = os.MkdirAll(bazelDirectory, 0755)
	if err != nil {
		log.Fatalf("could not create directory %s: %v", bazelDirectory, err)
	}

	bazelPath, err := downloadBazel(bazelFork, resolvedBazelVersion, isCommit, bazelDirectory)

	if err != nil {
		log.Fatalf("could not download Bazel: %v", err)
	}
	return bazelPath, resolvedBazelVersion
}

func getSortedKeys(data map[string]*flagDetails) []string {
	result := make([]string, 0)
	for key, _ := range data {
		result = append(result, key)
	}
	sort.Strings(result)
	return result
}
