package fileurl

import (
	"fmt"
	"net/url"
	"strings"

	"golang.org/x/mod/semver"
)

// Service releases are published only by this fork. Accelerators may front these
// GitHub URLs; they never select another repository. pull-source=github stays direct.
const (
	ServiceRepoPath     = "liu-zhengdong/fast-note-sync-service"
	ServiceRepoURL      = "https://github.com/" + ServiceRepoPath
	ServiceReleaseAPI   = "https://api.github.com/repos/" + ServiceRepoPath + "/releases"
	ServiceChecksumFile = "SHA256SUMS"
	UserAgent           = "fast-note-sync-service"
)

// ServiceVersion validates a release tag before it is used in a URL or filename.
func ServiceVersion(tag string) (string, error) {
	version := strings.TrimPrefix(tag, "v")
	if version == "" || strings.ContainsAny(tag, "/\\?#% \t\r\n") || !semver.IsValid("v"+version) || semver.Canonical("v"+version) != "v"+strings.Split(version, "+")[0] {
		return "", fmt.Errorf("invalid version format: %q", tag)
	}
	return version, nil
}

// ServiceDownload uses the exact tag and the archive naming of the release pipeline.
func ServiceDownload(tag, goos, goarch string) (downloadURL, fileName string, err error) {
	_, err = ServiceVersion(tag)
	if err != nil {
		return "", "", err
	}
	switch goos + "/" + goarch {
	case "darwin/amd64", "darwin/arm64", "linux/amd64", "linux/arm64", "windows/amd64":
	case "linux/arm":
		goarch = "armv7l"
	default:
		return "", "", fmt.Errorf("unsupported service upgrade platform: %s/%s", goos, goarch)
	}
	fileName = fmt.Sprintf("fast-note-sync-service-%s-%s-%s.tar.gz", tag, goos, goarch)
	return ServiceRepoURL + "/releases/download/" + url.PathEscape(tag) + "/" + url.PathEscape(fileName), fileName, nil
}

// ServiceChecksumURL is the direct GitHub URL of the release SHA256SUMS file.
func ServiceChecksumURL(tag string) (string, error) {
	if _, err := ServiceVersion(tag); err != nil {
		return "", err
	}
	return ServiceRepoURL + "/releases/download/" + url.PathEscape(tag) + "/" + ServiceChecksumFile, nil
}

// IsServiceReleaseList reports whether raw is this fork's GitHub releases API,
// including the same URL behind an accelerator prefix.
func IsServiceReleaseList(raw string) bool {
	return strings.Contains(raw, "/repos/"+ServiceRepoPath+"/releases")
}
