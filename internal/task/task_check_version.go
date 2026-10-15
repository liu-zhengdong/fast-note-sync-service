package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/haierkeys/fast-note-sync-service/internal/app"
	pkgapp "github.com/haierkeys/fast-note-sync-service/pkg/app"
	"github.com/haierkeys/fast-note-sync-service/pkg/fileurl"
	"go.uber.org/zap"
	"golang.org/x/mod/semver"
)

const (
	GitHubServiceReleaseURL = fileurl.ServiceReleaseAPI
	GitHubPluginReleaseURL  = "https://api.github.com/repos/haierkeys/obsidian-fast-note-sync/releases"
	ServiceRepoPath         = fileurl.ServiceRepoPath
	ServiceRepoURL          = fileurl.ServiceRepoURL
	PluginRepoPath          = "haierkeys/obsidian-fast-note-sync"
	PluginRepoURL           = "https://github.com/" + PluginRepoPath

	CNBPluginReleaseURL = "https://api.cnb.cool/" + PluginRepoPath + "/-/releases"
	CNBPluginURL        = "https://cnb.cool/" + PluginRepoPath
	CNBPluginToken      = "9pFNKcjlej36e0w6MHKT6YMn53G"

	// fetchHTTPTimeout bounds each release-list HTTP request so a hung source
	// can't stall the whole task; the task-layer fallback then retries the other source.
	// fetchHTTPTimeout 限制单次 release 列表请求的耗时，防止某源卡死拖住整个任务；
	// task 层 fallback 会回退到另一源重试。
	fetchHTTPTimeout = 8 * time.Second
)

type GitHubAsset struct {
	Name  string `json:"name"`  // Asset name // 资源包名称
	State string `json:"state"` // Upload state // 上传状态
}

type CNBRelease struct {
	TagName    string        `json:"tag_name"`
	Prerelease bool          `json:"prerelease"`
	Body       string        `json:"body"`   // Release description (changelog) // 版本发布说明（更新日志）
	Assets     []GitHubAsset `json:"assets"` // Release assets // 资源列表
}

type GitHubRelease struct {
	Draft      bool          `json:"draft"`
	TagName    string        `json:"tag_name"`
	Prerelease bool          `json:"prerelease"`
	Body       string        `json:"body"`   // Release description (changelog) // 版本发布说明（更新日志）
	Assets     []GitHubAsset `json:"assets"` // Release assets // 资源列表
}

type GitHubTag struct {
	Name string `json:"name"`
}

// releaseSource identifies which source a release list came from.
// releaseSource 标识 release 列表来自哪个源。
type releaseSource string

const (
	sourceGitHub releaseSource = "github"
	sourceCNB    releaseSource = "cnb"
)

type CheckVersionTask struct {
	app *app.App
}

func init() {
	RegisterWithApp(func(appContainer *app.App) (Task, error) {
		return &CheckVersionTask{
			app: appContainer,
		}, nil
	})
}

func (t *CheckVersionTask) Name() string {
	return "check_version"
}

func (t *CheckVersionTask) Run(ctx context.Context) error {
	// Service releases stay on this fork. pull-source=github is direct only;
	// auto (and cnb, which has no service mirror) retry the same API via accelerators.
	serviceReleases, err := t.fetchServiceReleases(ctx)
	if err != nil {
		return err
	}
	serviceLink, serviceChangelog, serviceChangelogContent := buildLinks(serviceReleases, true, true)

	// Plugin checks remain upstream because the plugin fork has no releases.
	pluginReleases, pluginLink, pluginChangelog, pluginChangelogContent, perr :=
		t.fetchReleasesWithFallbackLinksOnly(ctx, GitHubPluginReleaseURL, CNBPluginReleaseURL, CNBPluginToken, false, true)
	if perr != nil {
		// Plugin version is best-effort; don't fail the whole task if only plugin fetch errors.
		// 插件版本尽力而为：仅插件抓取失败不应导致整个任务失败。
		t.app.Logger().Warn("check_version: plugin releases fetch failed, keeping service result only", zap.Error(perr))
		pluginReleases = nil
	}

	var serviceLatest, pluginLatest string
	if len(serviceReleases) > 0 {
		serviceLatest = serviceReleases[0].Version
	}
	if len(pluginReleases) > 0 {
		pluginLatest = pluginReleases[0].Version
	}

	currentServiceVersion := t.app.Version().Version
	if !strings.HasPrefix(currentServiceVersion, "v") {
		currentServiceVersion = "v" + currentServiceVersion
	}

	if serviceLatest != "" && !strings.HasPrefix(serviceLatest, "v") {
		serviceLatest = "v" + serviceLatest
	}

	if pluginLatest != "" && !strings.HasPrefix(pluginLatest, "v") {
		pluginLatest = "v" + pluginLatest
	}

	info := pkgapp.CheckVersionInfo{
		GithubAvailable:                  true,
		VersionNewName:                   serviceLatest,
		VersionNewTag:                    latestTag(serviceReleases),
		VersionIsNew:                     serviceLatest != "" && semver.Compare(serviceLatest, currentServiceVersion) > 0,
		VersionNewLink:                   serviceLink,
		VersionNewChangelog:              serviceChangelog,
		VersionNewChangelogContent:       serviceChangelogContent,
		PluginVersionNewName:             pluginLatest,
		PluginVersionNewLink:             pluginLink,
		PluginVersionNewChangelog:        pluginChangelog,
		PluginVersionNewChangelogContent: pluginChangelogContent,
	}

	// 更新 App 中的版本信息和发布列表
	t.app.SetCheckVersionInfo(info)
	t.app.SetCheckVersionReleases(serviceReleases, pluginReleases)

	// 推送版本信息给所有已连接客户端
	t.app.BroadcastClientInfo()

	return nil
}

// fetchReleasesWithFallback tries the primary source first; on failure/empty it
// falls back to the other source once. Returns the source actually used and the
// assembled release list + link/changelog for the latest release.
// fetchReleasesWithFallback 先试主源；失败或为空时回退另一源重试一次。
// 返回实际命中的源、已过滤的 release 列表，以及最新版的 link/changelog。
func (t *CheckVersionTask) fetchReleasesWithFallback(
	ctx context.Context, ghURL, cnbURL, cnbToken string, isService, githubFirst bool,
) (usedGitHub bool, releases []pkgapp.HistoricalVersion, link, changelogLink, changelogContent string, err error) {
	if isService {
		releases, err = t.fetchServiceReleases(ctx)
		link, changelogLink, changelogContent = buildLinks(releases, true, true)
		return true, releases, link, changelogLink, changelogContent, err
	}
	releases, link, changelogLink, changelogContent, usedGitHub, err = t.tryFetch(ctx, ghURL, cnbURL, cnbToken, isService, githubFirst)
	if err == nil && len(releases) > 0 {
		return usedGitHub, releases, link, changelogLink, changelogContent, nil
	}
	if err != nil {
		t.app.Logger().Warn("check_version: primary source failed, falling back",
			zap.Bool("githubFirst", githubFirst),
			zap.Bool("isService", isService),
			zap.Error(err))
	} else {
		t.app.Logger().Warn("check_version: primary source returned no releases, falling back",
			zap.Bool("githubFirst", githubFirst),
			zap.Bool("isService", isService))
	}

	// fallback to the other source
	// 回退另一源
	releases, link, changelogLink, changelogContent, usedGitHub, ferr := t.tryFetch(ctx, ghURL, cnbURL, cnbToken, isService, !githubFirst)
	if ferr != nil {
		if err == nil {
			err = ferr
		}
		return githubFirst, nil, "", "", "", fmt.Errorf("both sources failed (primary err fallback): %w", ferr)
	}
	return usedGitHub, releases, link, changelogLink, changelogContent, nil
}

// fetchReleasesWithFallbackOnly is the variant returning links-only without the
// usedGitHub being needed by callers that ignore it. Kept the signature explicit
// for clarity in Run(); see fetchReleasesWithFallback for behavior.
// fetchReleasesWithFallbackLinksOnly 与 fetchReleasesWithFallback 行为相同，
// 供不需要 usedGitHub 的调用点使用（语义清晰）。
func (t *CheckVersionTask) fetchReleasesWithFallbackLinksOnly(
	ctx context.Context, ghURL, cnbURL, cnbToken string, isService, githubFirst bool,
) ([]pkgapp.HistoricalVersion, string, string, string, error) {
	_, releases, link, cl, clc, err := t.fetchReleasesWithFallback(ctx, ghURL, cnbURL, cnbToken, isService, githubFirst)
	return releases, link, cl, clc, err
}

// tryFetch fetches from exactly one source (github or cnb) and assembles links.
// tryFetch 只从一个源抓取（github 或 cnb）并拼装链接。
func (t *CheckVersionTask) tryFetch(
	ctx context.Context, ghURL, cnbURL, cnbToken string, isService, useGitHub bool,
) (releases []pkgapp.HistoricalVersion, link, changelogLink, changelogContent string, usedGitHub bool, err error) {
	if useGitHub {
		releases, err = t.fetchGitHubReleasesCtx(ctx, ghURL)
	} else {
		releases, err = t.fetchCNBVersionCtx(ctx, cnbURL, cnbToken)
	}
	if err != nil {
		return nil, "", "", "", useGitHub, err
	}
	link, changelogLink, changelogContent = buildLinks(releases, isService, useGitHub)
	return releases, link, changelogLink, changelogContent, useGitHub, nil
}

// buildLinks assembles the release page / changelog URLs for the latest release,
// depending on which source it came from.
// buildLinks 根据来源源，为最新版拼装 release 页面与 changelog 的 URL。
func buildLinks(releases []pkgapp.HistoricalVersion, isService, fromGitHub bool) (link, changelogLink, changelogContent string) {
	if len(releases) == 0 {
		return "", "", ""
	}
	latest := releases[0].Version
	changelogContent = releases[0].ChangelogContent
	latestClean := strings.TrimPrefix(latest, "v")
	if isService {
		tag := latestTag(releases)
		if _, err := fileurl.ServiceVersion(tag); err != nil {
			return "", "", ""
		}
		return ServiceRepoURL + "/releases/tag/" + tag, ServiceRepoURL + "/releases/download/" + tag + "/changelog.txt", changelogContent
	}
	if fromGitHub {
		base := PluginRepoURL
		link = base + "/releases/tag/" + latestClean
		changelogLink = base + "/releases/download/" + latestClean + "/changelog.txt"
	} else {
		base := CNBPluginURL
		link = base + "/-/releases/tag/" + latestClean
		changelogLink = base + "/-/releases/download/" + latestClean + "/changelog.txt"
	}
	return link, changelogLink, changelogContent
}

// hasValidAssets checks if there is at least one uploaded zip or tar.gz file
// hasValidAssets 检查是否包含至少一个已成功上传的 zip 或 tar.gz 资源文件
func hasValidAssets(assets []GitHubAsset) bool {
	for _, asset := range assets {
		name := strings.ToLower(asset.Name)
		if strings.HasSuffix(name, ".zip") || strings.HasSuffix(name, ".tar.gz") {
			if asset.State == "" || asset.State == "uploaded" {
				return true
			}
		}
	}
	return false
}

func (t *CheckVersionTask) fetchGitHubReleasesCtx(ctx context.Context, url string) ([]pkgapp.HistoricalVersion, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", fileurl.UserAgent)
	cli := &http.Client{Timeout: fetchHTTPTimeout}
	resp, err := cli.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API returned status: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var releases []GitHubRelease
	if err := json.Unmarshal(body, &releases); err != nil {
		return nil, err
	}

	releaseChannel := t.app.Config().App.PullReleaseChannel
	var result []pkgapp.HistoricalVersion
	for _, release := range releases {
		if release.Draft || (releaseChannel == "stable" && release.Prerelease) {
			continue
		}
		if !hasValidAssets(release.Assets) {
			continue
		}
		if fileurl.IsServiceReleaseList(url) {
			_, fileName, err := fileurl.ServiceDownload(release.TagName, runtime.GOOS, runtime.GOARCH)
			if err != nil {
				continue
			}
			found := false
			for _, asset := range release.Assets {
				if asset.Name == fileName && (asset.State == "" || asset.State == "uploaded") {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}
		tagName := release.TagName
		if _, err := fileurl.ServiceVersion(tagName); err != nil {
			continue
		}
		if releaseChannel == "stable" && semver.Prerelease("v"+strings.TrimPrefix(tagName, "v")) != "" {
			continue
		}
		if !strings.HasPrefix(tagName, "v") {
			tagName = "v" + tagName
		}
		result = append(result, pkgapp.HistoricalVersion{
			Version:          tagName,
			Tag:              release.TagName,
			ChangelogContent: release.Body,
		})
	}

	return result, nil
}

// fetchServiceReleases reads this fork's GitHub release list. In every mode except
// pull-source=github, a failed direct call is retried through github-proxies.
// Accelerators only front the same API URL; CNB and the upstream repository are never used.
func (t *CheckVersionTask) fetchServiceReleases(ctx context.Context) ([]pkgapp.HistoricalVersion, error) {
	mode := ""
	var proxies *[]string
	if cfg := t.app.Config(); cfg != nil {
		mode = cfg.App.PullSource
		proxies = cfg.App.GitHubProxies
	}
	var errs []error
	for _, u := range fileurl.ServiceReleaseListURLs(mode, proxies) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		releases, err := t.fetchGitHubReleasesCtx(ctx, u)
		if err == nil {
			if u != fileurl.ServiceReleaseAPI {
				t.app.Logger().Info("check_version: service releases fetched via accelerator", zap.String("url", u))
			}
			return releases, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", u, err))
		t.app.Logger().Warn("check_version: service release source failed", zap.String("url", u), zap.Error(err))
	}
	return nil, fmt.Errorf("fetch service releases: %w", errors.Join(errs...))
}

func (t *CheckVersionTask) fetchCNBVersionCtx(ctx context.Context, url string, token string) ([]pkgapp.HistoricalVersion, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Accept", "application/vnd.cnb.api+json")
	req.Header.Set("Authorization", token)

	cli := &http.Client{Timeout: fetchHTTPTimeout}
	resp, err := cli.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var releases []CNBRelease
	if err := json.Unmarshal(body, &releases); err != nil {
		return nil, err
	}

	releaseChannel := t.app.Config().App.PullReleaseChannel
	var result []pkgapp.HistoricalVersion
	for _, release := range releases {
		// CNB API usually follows Gitea/GitHub pattern
		// Also fallback check for common prerelease suffixes if field is not enough
		isPrerelease := release.Prerelease
		if !isPrerelease {
			tagName := strings.ToLower(release.TagName)
			if strings.Contains(tagName, "-beta") || strings.Contains(tagName, "-rc") || strings.Contains(tagName, "-alpha") {
				isPrerelease = true
			}
		}

		if releaseChannel == "stable" && isPrerelease {
			continue
		}
		if !hasValidAssets(release.Assets) {
			continue
		}
		tagName := release.TagName
		if _, err := fileurl.ServiceVersion(tagName); err != nil {
			continue
		}
		if releaseChannel == "stable" && semver.Prerelease("v"+strings.TrimPrefix(tagName, "v")) != "" {
			continue
		}
		if !strings.HasPrefix(tagName, "v") {
			tagName = "v" + tagName
		}
		result = append(result, pkgapp.HistoricalVersion{
			Version:          tagName,
			Tag:              release.TagName,
			ChangelogContent: release.Body,
		})
	}

	return result, nil
}

func (t *CheckVersionTask) LoopInterval() time.Duration {
	return 10 * time.Minute
}

func (t *CheckVersionTask) IsStartupRun() bool {
	return true
}

func latestTag(releases []pkgapp.HistoricalVersion) string {
	if len(releases) == 0 {
		return ""
	}
	if releases[0].Tag != "" {
		return releases[0].Tag
	}
	return strings.TrimPrefix(releases[0].Version, "v")
}
