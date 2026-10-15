package api_router

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/haierkeys/fast-note-sync-service/pkg/code"
	"github.com/haierkeys/fast-note-sync-service/pkg/fileurl"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExtendWriteDeadlineSurvivesServerWriteTimeout(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/slow", func(c *gin.Context) {
		extendWriteDeadline(c.Writer, 3*time.Second)
		time.Sleep(700 * time.Millisecond)
		c.String(http.StatusOK, "ok")
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := &http.Server{Handler: r, WriteTimeout: 200 * time.Millisecond, ReadHeaderTimeout: time.Second}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })

	resp, err := http.Get("http://" + ln.Addr().String() + "/slow")
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, "ok", string(body))
}

func TestUpgradeDownloadIgnoresShortRequestDeadline(t *testing.T) {
	gin.SetMode(gin.TestMode)
	_, fileName, err := fileurl.ServiceDownload("v9.9.9", runtime.GOOS, runtime.GOARCH)
	require.NoError(t, err)
	payload := tarGzWithFile(t, "fast-note-sync-service")
	if runtime.GOOS == "windows" {
		payload = tarGzWithFile(t, "fast-note-sync-service.exe")
	}
	sum := sha256.Sum256(payload)
	sums := hex.EncodeToString(sum[:]) + "  " + fileName + "\n"

	old := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = old })
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		timer := time.NewTimer(400 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-r.Context().Done():
			return nil, r.Context().Err()
		case <-timer.C:
		}
		body := payload
		if strings.Contains(r.URL.String(), fileurl.ServiceChecksumFile) {
			body = []byte(sums)
		}
		return &http.Response{
			StatusCode:    http.StatusOK,
			Status:        "200 OK",
			Header:        make(http.Header),
			Body:          io.NopCloser(bytes.NewReader(body)),
			ContentLength: int64(len(body)),
		}, nil
	})

	handler, appContainer, _ := newTestAdminHandler()
	appContainer.Config().App.TempPath = t.TempDir()
	appContainer.Config().App.PullSource = "auto"
	appContainer.UpgradeSignal = make(chan string, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	c, w := newAdminTestContext("GET", "/api/admin/upgrade?version=9.9.9", "", 1)
	c.Request = c.Request.WithContext(ctx)

	handler.Upgrade(c)

	assertResponseCode(t, w, code.Success.Code())
	select {
	case path := <-appContainer.UpgradeSignal:
		assert.NotEmpty(t, path)
	default:
		t.Fatal("upgrade was not triggered")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func tarGzWithFile(t *testing.T, name string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := []byte("#!/bin/sh\necho upgraded\n")
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body))}))
	_, err := tw.Write(body)
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	return buf.Bytes()
}
