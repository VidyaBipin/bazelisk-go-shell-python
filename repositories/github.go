package repositories

import (
	"encoding/json"
	"fmt"

	"github.com/bazelbuild/bazelisk/httputil"
	"github.com/bazelbuild/bazelisk/platforms"
)

const (
	urlPattern = "https://github.com/%s/bazel/releases/download/%s/%s"
)

// GitHubRepo represents a fork of Bazel hosted on GitHub, and provides a list of all available Bazel binaries in that repo, as well as the ability to download them.
type GitHubRepo struct {
	token string
}

// CreateGitHubRepo instantiates a new GitHubRepo.
func CreateGitHubRepo(token string) *GitHubRepo {
	return &GitHubRepo{token}
}

// ForkRepo

// GetVersions returns the versions of all available Bazel binaries in the given fork.
func (gh *GitHubRepo) GetVersions(bazeliskHome, bazelFork string) ([]string, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/bazel/releases", bazelFork)
	releasesJSON, err := httputil.MaybeDownload(bazeliskHome, url, bazelFork+"-releases.json", "list of Bazel releases from github.com/"+bazelFork, gh.token)
	if err != nil {
		return []string{}, fmt.Errorf("unable to dermine '%s' releases: %v", bazelFork, err)
	}

	var releases []gitHubRelease
	if err := json.Unmarshal(releasesJSON, &releases); err != nil {
		return []string{}, fmt.Errorf("could not parse JSON into list of releases: %v", err)
	}

	var tags []string
	for _, release := range releases {
		if release.Prerelease {
			continue
		}
		tags = append(tags, release.TagName)
	}
	return tags, nil
}

type gitHubRelease struct {
	TagName    string `json:"tag_name"`
	Prerelease bool   `json:"prerelease"`
}

// DownloadVersion downloads a Bazel binary for the given version and fork to the specified location and returns the absolute path.
func (gh *GitHubRepo) DownloadVersion(fork, version, destDir, destFile string) (string, error) {
	filename, err := platforms.DetermineBazelFilename(version, true)
	if err != nil {
		return "", err
	}
	url := fmt.Sprintf(urlPattern, fork, version, filename)
	return httputil.DownloadBinary(url, "", "", destDir, destFile)
}
