// Package platforms determins file names and extensions based on the current operating system.
package platforms

import (
	"fmt"
	sem_ver "github.com/hashicorp/go-version"
	"runtime"
)

var platforms = map[string]string{"darwin": "macos", "linux": "ubuntu1404", "windows": "windows"}

// GetPlatform returns a Bazel CI-compatible platform identifier for the current operating system.
// TODO(fweikert): raise an error for unsupported platforms
func GetPlatform() string {
	return platforms[runtime.GOOS]
}

// DetermineExecutableFilenameSuffix returns the extension for binaries on the current operating system.
func DetermineExecutableFilenameSuffix() string {
	filenameSuffix := ""
	if runtime.GOOS == "windows" {
		filenameSuffix = ".exe"
	}
	return filenameSuffix
}

// DetermineBazelFilename returns the correct file name of a local Bazel binary.
func DetermineBazelFilename(version string, includeSuffix bool) (string, error) {
	var machineName string
	switch runtime.GOARCH {
	case "amd64":
		machineName = "x86_64"
	case "arm64":
		machineName = "arm64"
	default:
		return "", fmt.Errorf("unsupported machine architecture \"%s\", must be arm64 or x86_64", runtime.GOARCH)
	}

	var osName string
	switch runtime.GOOS {
	case "darwin", "linux", "windows":
		osName = runtime.GOOS
	default:
		return "", fmt.Errorf("unsupported operating system \"%s\", must be Linux, macOS or Windows", runtime.GOOS)
	}

	if osName == "darwin" {
		var err error
		if machineName, err = DarwinFallback(machineName, version); err != nil {
			return "", err
		}
	}

	var filenameSuffix string
	if includeSuffix {
		filenameSuffix = DetermineExecutableFilenameSuffix()
	}

	return fmt.Sprintf("bazel-%s-%s-%s%s", version, osName, machineName, filenameSuffix), nil
}

// DarwinFallback Darwin arm64 was supported since 4.1.0, before 4.1.0, fall back to x86_64
func DarwinFallback(machineName string, version string) (alterMachineName string, err error) {
	v, err := sem_ver.NewVersion(version)
	if err != nil {
		return "", err
	}

	armSupportVer, err := sem_ver.NewVersion("4.1.0")
	if err != nil {
		return "", err
	}

	if machineName == "arm64" && v.LessThan(armSupportVer) {
		return "x86_64", nil
	}
	return machineName, nil
}
