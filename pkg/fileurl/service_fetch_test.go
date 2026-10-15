package fileurl

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPrefixGitHub(t *testing.T) {
	raw := "https://github.com/liu-zhengdong/fast-note-sync-service/releases/download/v3.6.5/file.tar.gz"
	got, err := PrefixGitHub("https://gh-proxy.com/", raw)
	if err != nil {
		t.Fatal(err)
	}
	want := "https://gh-proxy.com/" + raw
	if got != want {
		t.Fatalf("got %s", got)
	}
	u, err := url.Parse(got)
	if err != nil || u.Host != "gh-proxy.com" || !strings.Contains(u.Path, raw) && !strings.Contains(u.String(), raw) {
		t.Fatalf("parsed %#v %v", u, err)
	}
	if _, err := PrefixGitHub("https://gh-proxy.com", raw); err != nil || !strings.HasPrefix(mustPrefix(t, "https://gh-proxy.com", raw), "https://gh-proxy.com/") {
		t.Fatal("missing slash was not normalized")
	}
	for _, bad := range []string{"", "ftp://example.com/", "github.com", "https://user:pass@example.com/", "/tmp/x"} {
		if _, err := PrefixGitHub(bad, raw); err == nil {
			t.Errorf("accepted bad proxy %q", bad)
		}
	}
}

func mustPrefix(t *testing.T, prefix, raw string) string {
	t.Helper()
	u, err := PrefixGitHub(prefix, raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestServiceReleaseListURLs(t *testing.T) {
	direct := []string{ServiceReleaseAPI}
	if got := ServiceReleaseListURLs(SourceGitHub, nil); strings.Join(got, ",") != strings.Join(direct, ",") {
		t.Fatalf("github mode: %v", got)
	}
	custom := []string{"https://example.test/"}
	got := ServiceReleaseListURLs(SourceAuto, &custom)
	if len(got) != 2 || got[0] != ServiceReleaseAPI || got[1] != "https://example.test/"+ServiceReleaseAPI {
		t.Fatalf("auto custom: %v", got)
	}
	empty := []string{}
	if got := ServiceReleaseListURLs(SourceAuto, &empty); len(got) != 1 || got[0] != ServiceReleaseAPI {
		t.Fatalf("empty proxies: %v", got)
	}
	got = ServiceReleaseListURLs(SourceCNB, nil)
	if len(got) != 1+len(DefaultGitHubProxies) || got[0] != ServiceReleaseAPI {
		t.Fatalf("cnb uses accelerators, not CNB: %v", got)
	}
	for _, u := range got {
		if strings.Contains(u, "cnb.cool") || strings.Contains(u, "haierkeys") {
			t.Fatalf("upstream URL in service list: %s", u)
		}
	}
	if !IsServiceReleaseList(got[1]) {
		t.Fatalf("proxied API not recognized: %s", got[1])
	}
}

func TestParseSHA256SUMS(t *testing.T) {
	const name = "fast-note-sync-service-v3.6.5-linux-amd64.tar.gz"
	body := "51d337b6e9543de2ed6ccb40426fc5d6fceafa97fab46205b989730724d5c086  " + name + "\n" +
		"abcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcd *other.tar.gz\n"
	got, err := ParseSHA256SUMS([]byte(body), name)
	if err != nil || got != "51d337b6e9543de2ed6ccb40426fc5d6fceafa97fab46205b989730724d5c086" {
		t.Fatalf("got %s %v", got, err)
	}
	if _, err := ParseSHA256SUMS([]byte(body), "missing.tar.gz"); !errors.Is(err, ErrChecksumMissing) {
		t.Fatalf("missing entry: %v", err)
	}
}

func TestServiceChecksumURL(t *testing.T) {
	u, err := ServiceChecksumURL("v3.6.5")
	if err != nil || u != ServiceRepoURL+"/releases/download/v3.6.5/SHA256SUMS" {
		t.Fatalf("%s %v", u, err)
	}
	if _, err := ServiceChecksumURL("../v3"); err == nil {
		t.Fatal("accepted unsafe tag")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type scripted struct {
	mu    sync.Mutex
	calls []string
	// handler returns the response for a request.
	fn func(r *http.Request) (*http.Response, error)
}

func (s *scripted) RoundTrip(r *http.Request) (*http.Response, error) {
	s.mu.Lock()
	s.calls = append(s.calls, r.URL.String())
	s.mu.Unlock()
	if r.Header.Get("User-Agent") != UserAgent {
		return nil, errors.New("missing user agent")
	}
	return s.fn(r)
}

func (s *scripted) URLs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]string(nil), s.calls...)
	return out
}

func httpResp(status int, body []byte) *http.Response {
	return &http.Response{
		StatusCode:    status,
		Status:        http.StatusText(status),
		Header:        make(http.Header),
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
	}
}

func sumFile(payload []byte, name string) []byte {
	sum := sha256.Sum256(payload)
	return []byte(hex.EncodeToString(sum[:]) + "  " + name + "\n")
}

func isSums(r *http.Request) bool {
	return strings.Contains(r.URL.String(), ServiceChecksumFile)
}

type stallBody struct{ ctx context.Context }

func (s stallBody) Read([]byte) (int, error) {
	<-s.ctx.Done()
	return 0, s.ctx.Err()
}
func (s stallBody) Close() error { return nil }

type dripBody struct {
	ctx   context.Context
	left  int
	every time.Duration
}

func (d *dripBody) Read(p []byte) (int, error) {
	if d.left <= 0 {
		return 0, io.EOF
	}
	timer := time.NewTimer(d.every)
	defer timer.Stop()
	select {
	case <-d.ctx.Done():
		return 0, d.ctx.Err()
	case <-timer.C:
	}
	if len(p) == 0 {
		return 0, nil
	}
	p[0] = 'x'
	d.left--
	return 1, nil
}
func (d *dripBody) Close() error { return nil }

func testFetchCfg(mode string, proxies []string, rt http.RoundTripper) ServiceFetchConfig {
	var p *[]string
	if proxies != nil {
		p = &proxies
	}
	return ServiceFetchConfig{
		Mode:              mode,
		Proxies:           p,
		Client:            &http.Client{Transport: rt},
		MinBytesPerSec:    64 << 10,
		Grace:             200 * time.Millisecond,
		Stall:             200 * time.Millisecond,
		AttemptTimeout:    2 * time.Second,
		MetaTimeout:       time.Second,
		DirectMetaTimeout: time.Second,
		MaxAssetBytes:     1 << 20,
	}
}

func TestDownloadVerifiedDirectAndProxyFallback(t *testing.T) {
	const name = "pkg.tar.gz"
	payload := bytes.Repeat([]byte("good-archive"), 100)
	asset := "https://github.com/liu-zhengdong/fast-note-sync-service/releases/download/v9.9.9/" + name
	sums := "https://github.com/liu-zhengdong/fast-note-sync-service/releases/download/v9.9.9/" + ServiceChecksumFile
	proxy := "https://gh-proxy.com/"
	s := &scripted{fn: func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Host, "github.com") {
			return httpResp(500, []byte("nope")), nil
		}
		if isSums(r) {
			return httpResp(200, sumFile(payload, name)), nil
		}
		return httpResp(200, payload), nil
	}}
	dest := filepath.Join(t.TempDir(), name)
	proxies := []string{proxy}
	got, err := DownloadVerified(context.Background(), asset, name, sums, testFetchCfg(SourceAuto, proxies, s), dest)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got.URL, proxy) || got.ChecksumDirect {
		t.Fatalf("used %+v", got)
	}
	body, err := os.ReadFile(dest)
	if err != nil || !bytes.Equal(body, payload) {
		t.Fatalf("dest %v %d", err, len(body))
	}
	for _, u := range s.URLs() {
		if strings.Contains(u, "cnb.cool") {
			t.Fatal(u)
		}
	}
}

func TestDownloadVerifiedGitHubModeIgnoresProxy(t *testing.T) {
	const name = "pkg.tar.gz"
	payload := []byte("direct-bytes")
	asset := "https://github.com/example/releases/download/v1.2.3/" + name
	sums := "https://github.com/example/releases/download/v1.2.3/" + ServiceChecksumFile
	s := &scripted{fn: func(r *http.Request) (*http.Response, error) {
		if !strings.Contains(r.URL.Host, "github.com") {
			t.Errorf("proxy contacted in github mode: %s", r.URL)
			return httpResp(500, nil), nil
		}
		if isSums(r) {
			return httpResp(200, sumFile(payload, name)), nil
		}
		return httpResp(200, payload), nil
	}}
	proxies := []string{"https://gh-proxy.com/"}
	dest := filepath.Join(t.TempDir(), name)
	if _, err := DownloadVerified(context.Background(), asset, name, sums, testFetchCfg(SourceGitHub, proxies, s), dest); err != nil {
		t.Fatal(err)
	}
}

func TestDownloadVerifiedMismatchFallsForward(t *testing.T) {
	const name = "pkg.tar.gz"
	good := []byte("correct-archive")
	asset := "https://github.com/example/a/" + name
	sums := "https://github.com/example/a/" + ServiceChecksumFile
	var assetCalls []string
	s := &scripted{fn: func(r *http.Request) (*http.Response, error) {
		if isSums(r) {
			// Delay the proxy checksum so GitHub is tried first and its tampered body is rejected.
			if !strings.Contains(r.URL.Host, "github.com") {
				time.Sleep(40 * time.Millisecond)
			}
			return httpResp(200, sumFile(good, name)), nil
		}
		assetCalls = append(assetCalls, r.URL.Host)
		if strings.Contains(r.URL.Host, "github.com") {
			return httpResp(200, []byte("tampered")), nil
		}
		return httpResp(200, good), nil
	}}
	proxies := []string{"https://mirror.example/"}
	dest := filepath.Join(t.TempDir(), name)
	got, err := DownloadVerified(context.Background(), asset, name, sums, testFetchCfg(SourceAuto, proxies, s), dest)
	if err != nil {
		t.Fatal(err)
	}
	if !got.ChecksumDirect || !strings.HasPrefix(got.URL, "https://mirror.example/") {
		t.Fatalf("want proxy bytes checked against GitHub sums, got %+v", got)
	}
	body, _ := os.ReadFile(dest)
	if !bytes.Equal(body, good) {
		t.Fatalf("installed %q", body)
	}
	if len(assetCalls) < 2 || !strings.Contains(assetCalls[0], "github.com") {
		t.Fatalf("expected GitHub to be rejected before the proxy, calls %v", assetCalls)
	}
}

func TestDownloadVerifiedRejectsTotalMismatch(t *testing.T) {
	const name = "pkg.tar.gz"
	asset := "https://github.com/example/a/" + name
	sums := "https://github.com/example/a/" + ServiceChecksumFile
	s := &scripted{fn: func(r *http.Request) (*http.Response, error) {
		if isSums(r) {
			return httpResp(200, sumFile([]byte("expected"), name)), nil
		}
		return httpResp(200, []byte("injected")), nil
	}}
	dest := filepath.Join(t.TempDir(), name)
	_, err := DownloadVerified(context.Background(), asset, name, sums, testFetchCfg(SourceGitHub, nil, s), dest)
	if !errors.Is(err, ErrChecksumMismatch) && !errors.Is(err, ErrNoUpgradeSource) {
		t.Fatalf("err %v", err)
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Fatal("bad archive left on disk")
	}
}

func TestDownloadVerifiedChecksumDisagree(t *testing.T) {
	const name = "pkg.tar.gz"
	asset := "https://github.com/example/a/" + name
	sums := "https://github.com/example/a/" + ServiceChecksumFile
	s := &scripted{fn: func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Host, "github.com") {
			return httpResp(500, nil), nil
		}
		if !isSums(r) {
			t.Errorf("asset fetched without an agreed checksum: %s", r.URL)
			return httpResp(500, nil), nil
		}
		payload := []byte(r.URL.Host)
		return httpResp(200, sumFile(payload, name)), nil
	}}
	proxies := []string{"https://a.example/", "https://b.example/"}
	_, err := DownloadVerified(context.Background(), asset, name, sums, testFetchCfg(SourceAuto, proxies, s), filepath.Join(t.TempDir(), name))
	if !errors.Is(err, ErrChecksumDisagree) {
		t.Fatalf("err %v", err)
	}
}

func TestDownloadVerifiedStallThenFallback(t *testing.T) {
	const name = "pkg.tar.gz"
	payload := bytes.Repeat([]byte("fast"), 200)
	asset := "https://github.com/example/a/" + name
	sums := "https://github.com/example/a/" + ServiceChecksumFile
	s := &scripted{fn: func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Host, "github.com") && !isSums(r) {
			return &http.Response{
				StatusCode: 200,
				Status:     "200 OK",
				Header:     make(http.Header),
				Body:       stallBody{ctx: r.Context()},
			}, nil
		}
		if !strings.Contains(r.URL.Host, "github.com") && isSums(r) {
			time.Sleep(30 * time.Millisecond) // lose the latency sort, so GitHub is tried first
		}
		if isSums(r) {
			return httpResp(200, sumFile(payload, name)), nil
		}
		return httpResp(200, payload), nil
	}}
	proxies := []string{"https://fast.example/"}
	cfg := testFetchCfg(SourceAuto, proxies, s)
	cfg.Stall = 150 * time.Millisecond
	cfg.Grace = time.Second
	dest := filepath.Join(t.TempDir(), name)
	start := time.Now()
	got, err := DownloadVerified(context.Background(), asset, name, sums, cfg, dest)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("stall was not abandoned quickly: %s", time.Since(start))
	}
	if !strings.Contains(got.URL, "fast.example") {
		t.Fatalf("used %s", got.URL)
	}
}

func TestDownloadVerifiedSlowSourceAbandoned(t *testing.T) {
	const name = "pkg.tar.gz"
	asset := "https://github.com/example/a/" + name
	sums := "https://github.com/example/a/" + ServiceChecksumFile
	s := &scripted{fn: func(r *http.Request) (*http.Response, error) {
		if isSums(r) {
			return httpResp(200, sumFile(bytes.Repeat([]byte("z"), 1000), name)), nil
		}
		return &http.Response{
			StatusCode: 200,
			Status:     "200 OK",
			Header:     make(http.Header),
			Body:       &dripBody{ctx: r.Context(), left: 100000, every: 20 * time.Millisecond},
		}, nil
	}}
	cfg := testFetchCfg(SourceGitHub, nil, s)
	cfg.MinBytesPerSec = 50_000
	cfg.Grace = 120 * time.Millisecond
	cfg.Stall = time.Second
	start := time.Now()
	_, err := DownloadVerified(context.Background(), asset, name, sums, cfg, filepath.Join(t.TempDir(), name))
	if !errors.Is(err, ErrSlow) && !errors.Is(err, ErrNoUpgradeSource) {
		t.Fatalf("err %v", err)
	}
	if !errors.Is(err, ErrSlow) {
		t.Fatalf("slow source was not classified: %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("slow source ran for %s", time.Since(start))
	}
}

func TestDownloadVerifiedNoChecksum(t *testing.T) {
	const name = "pkg.tar.gz"
	s := &scripted{fn: func(r *http.Request) (*http.Response, error) {
		if !isSums(r) {
			t.Errorf("downloaded asset with no checksum: %s", r.URL)
		}
		return httpResp(404, []byte("missing")), nil
	}}
	_, err := DownloadVerified(context.Background(), "https://github.com/example/a/"+name, name, "https://github.com/example/a/"+ServiceChecksumFile, testFetchCfg(SourceAuto, []string{}, s), filepath.Join(t.TempDir(), name))
	if !errors.Is(err, ErrChecksumMissing) {
		t.Fatalf("err %v", err)
	}
}
