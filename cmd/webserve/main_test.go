package main

import (
	"bufio"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func setup(t *testing.T) (*httptest.Server, *httptest.Server) {
	t.Helper()
	dist := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dist, "assets"), 0o750); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"index.html": "<html>console</html>", "assets/app-123.js": "js", "assets/app-123.js.map": "{}", "favicon.svg": "<svg/>"} {
		if err := os.WriteFile(filepath.Join(dist, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/slow" {
			select { // answers only after the proxy has given up
			case <-time.After(2 * time.Second):
			case <-r.Context().Done():
			}
			return
		}
		if r.Method == http.MethodPost {
			n, _ := io.Copy(io.Discard, r.Body)
			_, _ = io.WriteString(w, "read "+strconv.FormatInt(n, 10))
			return
		}
		if r.URL.Path == "/v1/metrics/live" {
			fl := w.(http.Flusher)
			_, _ = w.Write([]byte("{\"result\":1}\n"))
			fl.Flush()
			<-r.Context().Done() // a stream that never ends
			return
		}
		_, _ = io.WriteString(w, r.Method+" "+r.URL.RequestURI()+" auth="+r.Header.Get("Authorization"))
	}))
	t.Cleanup(upstream.Close)
	h, err := newHandler(config{Dist: dist, API: upstream.URL, Issuer: upstream.URL, Summarizer: "http://127.0.0.1:1", UpstreamTimeout: 200 * time.Millisecond}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, upstream
}

func get(t *testing.T, url string, hdr ...string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestProxiesStripPrefixAndForwardAuth(t *testing.T) {
	srv, _ := setup(t)
	resp, body := get(t, srv.URL+"/api/v1/cases?page_size=5", "Authorization", "Bearer tok")
	if resp.StatusCode != 200 || body != "GET /v1/cases?page_size=5 auth=Bearer tok" {
		t.Fatalf("api proxy: %d %q", resp.StatusCode, body)
	}
	if _, body := get(t, srv.URL+"/auth/personas"); body != "GET /personas auth=" {
		t.Fatalf("auth proxy: %q", body)
	}
	resp, body = get(t, srv.URL+"/summarizer/v1/summaries/x")
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(body, "upstream unavailable") {
		t.Fatalf("down upstream: %d %q", resp.StatusCode, body)
	}
}

func TestSPAFallbackCachingAndHeaders(t *testing.T) {
	srv, _ := setup(t)
	resp, body := get(t, srv.URL+"/cases/abc")
	if resp.StatusCode != 200 || body != "<html>console</html>" || resp.Header.Get("Cache-Control") != "no-cache" {
		t.Fatalf("client route: %d %q %q", resp.StatusCode, body, resp.Header.Get("Cache-Control"))
	}
	if !strings.Contains(resp.Header.Get("Content-Security-Policy"), "frame-ancestors 'none'") || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("security headers %v", resp.Header)
	}
	resp, body = get(t, srv.URL+"/assets/app-123.js")
	if body != "js" || !strings.Contains(resp.Header.Get("Cache-Control"), "immutable") {
		t.Fatalf("asset: %q %q", body, resp.Header.Get("Cache-Control"))
	}
	if _, body := get(t, srv.URL+"/favicon.svg"); body != "<svg/>" {
		t.Fatalf("static file: %q", body)
	}
	if _, body := get(t, srv.URL+"/../../etc/passwd"); body != "<html>console</html>" {
		t.Fatalf("path traversal must not escape dist: %q", body)
	}
	if resp, _ := get(t, srv.URL+"/healthz"); resp.StatusCode != 200 {
		t.Fatal("healthz")
	}
}

func TestLiveMetricsStreamIsNotBuffered(t *testing.T) {
	srv, _ := setup(t)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(srv.URL + "/api/v1/metrics/live")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || line != "{\"result\":1}\n" {
		t.Fatalf("first streamed line %q %v", line, err)
	}
}

func TestProxyLimitsBodiesAndGivesUpOnSlowUpstreams(t *testing.T) {
	srv, _ := setup(t)
	post := func(n int) (int, string) {
		resp, err := http.Post(srv.URL+"/api/v1/rules:test", "application/json", strings.NewReader(strings.Repeat("x", n)))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := post(1000); code != 200 || body != "read 1000" {
		t.Fatalf("small body: %d %q", code, body)
	}
	if code, _ := post(maxBody + 1); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body: %d, want 413", code)
	}
	start := time.Now()
	resp, body := get(t, srv.URL+"/api/v1/slow")
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(body, "upstream unavailable") {
		t.Fatalf("slow upstream: %d %q", resp.StatusCode, body)
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("slow upstream answered after %v; the proxy must give up after its timeout", took)
	}
}

func TestSourceMapsAreNotServed(t *testing.T) {
	srv, _ := setup(t)
	if resp, body := get(t, srv.URL+"/assets/app-123.js.map"); resp.StatusCode != http.StatusNotFound || body == "{}" {
		t.Fatalf("source map: %d %q", resp.StatusCode, body)
	}
}
