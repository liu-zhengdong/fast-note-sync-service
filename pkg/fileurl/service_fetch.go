package fileurl

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// UpgradeBudget is how long an in-app upgrade may spend downloading.
	// It is independent of the API request timeout.
	UpgradeBudget = 10 * time.Minute

	defaultMinBytesPerSec = 64 << 10 // 64 KiB/s — above the ~40 KiB/s dead accelerators
	defaultGrace          = 8 * time.Second
	defaultStall          = 8 * time.Second
	defaultAttemptTimeout = 3 * time.Minute
	defaultMetaTimeout    = 6 * time.Second
	defaultDirectMeta     = 3 * time.Second
	defaultMaxAssetBytes  = 512 << 20
	checksumBodyLimit     = 1 << 20
)

// DefaultGitHubProxies are accelerators that actually delivered a release
// archive quickly from a mainland China host. Each value is a prefix; the
// request URL is prefix + the full GitHub URL.
var DefaultGitHubProxies = []string{
	"https://gh-proxy.com/",
	"https://ghfast.top/",
	"https://gh-proxy.org/",
}

var (
	ErrStalled          = errors.New("download stalled")
	ErrSlow             = errors.New("download below minimum throughput")
	ErrChecksumMismatch = errors.New("sha256 mismatch")
	ErrChecksumDisagree = errors.New("checksum sources disagree")
	ErrChecksumMissing  = errors.New("release SHA256SUMS unavailable")
	ErrNoUpgradeSource  = errors.New("all upgrade sources failed")
)

// FetchHooks receives progress from a verified download. Nil functions are skipped.
type FetchHooks struct {
	Warn func(msg, rawURL string, err error)
	Info func(msg, rawURL string)
}

func (h FetchHooks) warn(msg, rawURL string, err error) {
	if h.Warn != nil {
		h.Warn(msg, rawURL, err)
	}
}

func (h FetchHooks) info(msg, rawURL string) {
	if h.Info != nil {
		h.Info(msg, rawURL)
	}
}

// ServiceFetchConfig controls service-release downloads and version-list fallback.
// A nil Proxies pointer means the built-in accelerator list. A non-nil empty
// slice disables accelerators. Mode "github" always uses the direct URL only.
type ServiceFetchConfig struct {
	Mode              string
	Proxies           *[]string
	Client            *http.Client
	MinBytesPerSec    int64
	Grace             time.Duration
	Stall             time.Duration
	AttemptTimeout    time.Duration
	MetaTimeout       time.Duration
	DirectMetaTimeout time.Duration
	MaxAssetBytes     int64
	Hooks             FetchHooks
}

func (c ServiceFetchConfig) normalized() ServiceFetchConfig {
	if c.MinBytesPerSec <= 0 {
		c.MinBytesPerSec = defaultMinBytesPerSec
	}
	if c.Grace <= 0 {
		c.Grace = defaultGrace
	}
	if c.Stall <= 0 {
		c.Stall = defaultStall
	}
	if c.AttemptTimeout <= 0 {
		c.AttemptTimeout = defaultAttemptTimeout
	}
	if c.MetaTimeout <= 0 {
		c.MetaTimeout = defaultMetaTimeout
	}
	if c.DirectMetaTimeout <= 0 {
		c.DirectMetaTimeout = defaultDirectMeta
	}
	if c.MaxAssetBytes <= 0 {
		c.MaxAssetBytes = defaultMaxAssetBytes
	}
	if c.Client == nil {
		c.Client = &http.Client{Transport: http.DefaultTransport}
	}
	return c
}

// DirectOnly reports whether service releases must use GitHub with no accelerator.
func DirectOnly(mode string) bool {
	return strings.EqualFold(strings.TrimSpace(mode), SourceGitHub)
}

type candidate struct {
	raw    string
	direct bool
}

// PrefixGitHub joins an accelerator prefix with a full GitHub URL
// (<prefix>/<https://github.com/...>).
func PrefixGitHub(prefix, rawURL string) (string, error) {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return "", fmt.Errorf("empty github proxy")
	}
	u, err := url.Parse(prefix)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return "", fmt.Errorf("invalid github proxy %q", prefix)
	}
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return prefix + rawURL, nil
}

func serviceCandidates(direct, mode string, proxies *[]string) ([]candidate, []string) {
	directC := candidate{raw: direct, direct: true}
	if DirectOnly(mode) {
		return []candidate{directC}, nil
	}
	list := DefaultGitHubProxies
	if proxies != nil {
		list = *proxies
	}
	out := make([]candidate, 0, len(list)+1)
	seen := map[string]struct{}{direct: {}}
	var skipped []string
	for _, p := range list {
		u, err := PrefixGitHub(p, direct)
		if err != nil {
			skipped = append(skipped, strings.TrimSpace(p))
			continue
		}
		if _, ok := seen[u]; ok {
			continue
		}
		seen[u] = struct{}{}
		out = append(out, candidate{raw: u})
	}
	out = append(out, directC)
	return out, skipped
}

// ServiceReleaseListURLs is the version-check order: GitHub direct, then accelerators.
// pull-source=github returns only the direct API URL.
func ServiceReleaseListURLs(mode string, proxies *[]string) []string {
	cands, _ := serviceCandidates(ServiceReleaseAPI, mode, proxies)
	urls := make([]string, 0, len(cands))
	var rest []string
	for _, c := range cands {
		if c.direct {
			urls = append(urls, c.raw)
			continue
		}
		rest = append(rest, c.raw)
	}
	return append(urls, rest...)
}

// VerifiedDownload is a release archive that matched the chosen SHA256SUMS entry.
type VerifiedDownload struct {
	URL            string
	SHA256         string
	ChecksumURL    string
	ChecksumDirect bool
}

type sumResult struct {
	url     string
	direct  bool
	sum     string
	latency time.Duration
	err     error
}

// DownloadVerified fetches SHA256SUMS, then the asset, trying accelerators when
// mode is not github. A stalled or slow body is abandoned. The file is written
// to dest only after the hash matches. On failure dest is removed.
func DownloadVerified(ctx context.Context, assetURL, fileName, sumsURL string, cfg ServiceFetchConfig, dest string) (VerifiedDownload, error) {
	cfg = cfg.normalized()
	if err := ctx.Err(); err != nil {
		return VerifiedDownload{}, err
	}
	assets, skipped := serviceCandidates(assetURL, cfg.Mode, cfg.Proxies)
	sums, _ := serviceCandidates(sumsURL, cfg.Mode, cfg.Proxies)
	for _, p := range skipped {
		cfg.Hooks.warn("upgrade: ignoring invalid github proxy", p, fmt.Errorf("invalid github proxy"))
	}
	results := fetchSums(ctx, cfg, sums, fileName)
	for _, r := range results {
		if r.err != nil {
			cfg.Hooks.warn("upgrade: checksum source failed", r.url, r.err)
		}
	}
	sum, sumURL, direct, err := chooseSum(results)
	if err != nil {
		return VerifiedDownload{}, err
	}
	cfg.Hooks.info("upgrade: checksum selected", sumURL)

	order := orderByChecksum(assets, results)
	var errs []error
	for _, src := range order {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		if err := downloadToFile(ctx, cfg, src.raw, dest); err != nil {
			os.Remove(dest)
			errs = append(errs, fmt.Errorf("%s: %w", src.raw, err))
			cfg.Hooks.warn("upgrade: asset source failed", src.raw, err)
			continue
		}
		if err := verifyFileSHA256(dest, sum); err != nil {
			os.Remove(dest)
			errs = append(errs, fmt.Errorf("%s: %w", src.raw, err))
			cfg.Hooks.warn("upgrade: asset rejected", src.raw, err)
			continue
		}
		cfg.Hooks.info("upgrade: asset verified", src.raw)
		return VerifiedDownload{URL: src.raw, SHA256: sum, ChecksumURL: sumURL, ChecksumDirect: direct}, nil
	}
	os.Remove(dest)
	if len(errs) == 0 {
		errs = append(errs, ctx.Err())
	}
	return VerifiedDownload{}, fmt.Errorf("%w: %w", ErrNoUpgradeSource, errors.Join(errs...))
}

func fetchSums(ctx context.Context, cfg ServiceFetchConfig, sums []candidate, fileName string) []sumResult {
	results := make([]sumResult, len(sums))
	var wg sync.WaitGroup
	for i, c := range sums {
		wg.Add(1)
		go func(i int, c candidate) {
			defer wg.Done()
			timeout := cfg.MetaTimeout
			if c.direct {
				timeout = cfg.DirectMetaTimeout
			}
			reqCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			start := time.Now()
			body, err := getLimited(reqCtx, cfg.Client, c.raw, checksumBodyLimit)
			results[i] = sumResult{url: c.raw, direct: c.direct, latency: time.Since(start), err: err}
			if err != nil {
				return
			}
			sum, perr := ParseSHA256SUMS(body, fileName)
			results[i].sum = sum
			results[i].err = perr
		}(i, c)
	}
	wg.Wait()
	return results
}

func chooseSum(results []sumResult) (sum, from string, direct bool, err error) {
	var directSum, directURL string
	counts := map[string]int{}
	firstURL := map[string]string{}
	for _, r := range results {
		if r.err != nil || r.sum == "" {
			continue
		}
		if r.direct && directSum == "" {
			directSum = r.sum
			directURL = r.url
		}
		counts[r.sum]++
		if _, ok := firstURL[r.sum]; !ok {
			firstURL[r.sum] = r.url
		}
	}
	if directSum != "" {
		return directSum, directURL, true, nil
	}
	if len(counts) == 0 {
		return "", "", false, ErrChecksumMissing
	}
	best, bestN := "", 0
	tie := false
	for digest, n := range counts {
		switch {
		case n > bestN:
			best, bestN, tie = digest, n, false
		case n == bestN:
			tie = true
		}
	}
	if tie {
		return "", "", false, ErrChecksumDisagree
	}
	return best, firstURL[best], false, nil
}

type ranked struct {
	c       candidate
	latency time.Duration
	ok      bool
}

func orderByChecksum(assets []candidate, sums []sumResult) []candidate {
	byURL := map[string]sumResult{}
	for _, r := range sums {
		byURL[r.url] = r
	}
	rows := make([]ranked, len(assets))
	for i, a := range assets {
		// Match a binary candidate to the checksum candidate with the same role.
		// Proxied asset URLs and proxied checksum URLs share the prefix, not the path.
		var match sumResult
		var found bool
		for _, r := range sums {
			if r.direct == a.direct && sameProxy(r.url, a.raw) {
				match = r
				found = true
				break
			}
		}
		if !found {
			match = byURL[a.raw]
		}
		rows[i] = ranked{c: a, latency: match.latency, ok: match.err == nil && match.sum != ""}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].ok != rows[j].ok {
			return rows[i].ok
		}
		if !rows[i].ok {
			return false
		}
		return rows[i].latency < rows[j].latency
	})
	out := make([]candidate, len(rows))
	for i, row := range rows {
		out[i] = row.c
	}
	return out
}

// sameProxy reports whether two absolute URLs were produced from the same
// accelerator (or are both direct). Direct URLs share the GitHub host.
func sameProxy(a, b string) bool {
	au, aerr := url.Parse(a)
	bu, berr := url.Parse(b)
	if aerr != nil || berr != nil || au.Host == "" || bu.Host == "" {
		return false
	}
	return au.Scheme == bu.Scheme && strings.EqualFold(au.Host, bu.Host)
}

// ParseSHA256SUMS finds fileName in a GNU sha256sum listing.
func ParseSHA256SUMS(data []byte, fileName string) (string, error) {
	var found string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(strings.TrimRight(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		sum := strings.ToLower(fields[0])
		name := strings.TrimPrefix(fields[len(fields)-1], "*")
		if len(sum) != sha256.Size*2 || !isHex(sum) {
			continue
		}
		if name == fileName {
			found = sum
		}
	}
	if found == "" {
		return "", fmt.Errorf("%w: no entry for %s", ErrChecksumMissing, fileName)
	}
	return found, nil
}

func isHex(s string) bool {
	_, err := hex.DecodeString(s)
	return err == nil
}

func verifyFileSHA256(path, want string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != strings.ToLower(want) {
		return fmt.Errorf("%w: got %s want %s", ErrChecksumMismatch, got, strings.ToLower(want))
	}
	return nil
}

func getLimited(ctx context.Context, client *http.Client, rawURL string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", UserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %s", resp.Status)
	}
	buf, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(buf)) > limit {
		return nil, fmt.Errorf("response exceeds %d bytes", limit)
	}
	return buf, nil
}

func downloadToFile(ctx context.Context, cfg ServiceFetchConfig, rawURL, dest string) error {
	ctx, cancel := context.WithTimeout(ctx, cfg.AttemptTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", UserAgent)
	resp, err := cfg.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %s", resp.Status)
	}

	tmp := dest + ".part"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	renamed := false
	defer func() {
		f.Close()
		if !renamed {
			os.Remove(tmp)
		}
	}()

	errCh := make(chan error, 1)
	done := make(chan struct{})
	var written int64
	var writeMu sync.Mutex
	progress := func() int64 {
		writeMu.Lock()
		defer writeMu.Unlock()
		return written
	}
	go func() {
		defer close(done)
		buf := make([]byte, 32<<10)
		var total int64
		for {
			n, rerr := resp.Body.Read(buf)
			if n > 0 {
				if _, werr := f.Write(buf[:n]); werr != nil {
					errCh <- werr
					return
				}
				total += int64(n)
				writeMu.Lock()
				written = total
				writeMu.Unlock()
				if total > cfg.MaxAssetBytes {
					errCh <- fmt.Errorf("asset exceeds %d bytes", cfg.MaxAssetBytes)
					return
				}
			}
			if rerr != nil {
				if rerr == io.EOF {
					errCh <- nil
					return
				}
				errCh <- rerr
				return
			}
		}
	}()

	tick := 200 * time.Millisecond
	if cfg.Stall/4 < tick && cfg.Stall > 0 {
		tick = cfg.Stall / 4
	}
	if tick < 10*time.Millisecond {
		tick = 10 * time.Millisecond
	}
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	start := time.Now()
	lastN := int64(0)
	lastChange := start
	var copyErr error
	wait := true
	for wait {
		select {
		case err := <-errCh:
			copyErr = err
			wait = false
		case <-ctx.Done():
			copyErr = ctx.Err()
			wait = false
		case now := <-ticker.C:
			n := progress()
			if n != lastN {
				lastN = n
				lastChange = now
			}
			if now.Sub(lastChange) >= cfg.Stall {
				cancel()
				copyErr = fmt.Errorf("%w: no progress for %s", ErrStalled, cfg.Stall)
				wait = false
				break
			}
			elapsed := now.Sub(start)
			if n > 0 && elapsed >= cfg.Grace && n*1000 < cfg.MinBytesPerSec*elapsed.Milliseconds() {
				cancel()
				copyErr = fmt.Errorf("%w: %d bytes in %s", ErrSlow, n, elapsed.Round(time.Millisecond))
				wait = false
			}
		}
	}
	// Unblock the copy goroutine and wait until it has stopped writing.
	cancel()
	resp.Body.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
	if copyErr != nil {
		return copyErr
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, dest); err != nil {
		return err
	}
	renamed = true
	return nil
}
