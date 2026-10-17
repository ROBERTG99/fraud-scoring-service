// Command webserve serves the analyst console (web/dist) on :8500 and proxies its API calls:
// /api/* → scorer HTTP gateway, /auth/* → dev-issuer, /summarizer/* → case-summarizer (spec §4.1).
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ROBERTG99/fraud-scoring-service/internal/telemetry"
)

// csp allows only same-origin scripts, styles, fonts and API calls (the build has no inline scripts).
const csp = "default-src 'self'; script-src 'self'; style-src 'self'; font-src 'self'; img-src 'self' data:; " +
	"connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'"

// maxBody bounds proxied request bodies; the largest console request (a label note of 2,000 characters)
// is a few kilobytes.
const maxBody = 64 << 10

type config struct {
	Dist       string
	API        string
	Issuer     string
	Summarizer string
	// UpstreamTimeout bounds the wait for an upstream's response headers (a stream may then run on).
	UpstreamTimeout time.Duration
}

func proxy(target, prefix string, timeout time.Duration, log *slog.Logger) (http.Handler, error) {
	u, err := url.Parse(target)
	if err != nil {
		return nil, err
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = timeout
	rp := &httputil.ReverseProxy{
		Transport: tr,
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(u)
			r.Out.URL.Path = strings.TrimPrefix(r.In.URL.Path, prefix)
			r.Out.URL.RawPath = ""
			r.SetXForwarded()
		},
		FlushInterval: -1, // stream /v1/metrics/live without buffering
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if tooLarge := new(http.MaxBytesError); errors.As(err, &tooLarge) {
				tooLargeResponse(w)
				return
			}
			log.Warn("upstream unavailable", "prefix", prefix, "path", r.URL.Path, "err", err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"code":14,"message":"upstream unavailable"}`))
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > maxBody {
			tooLargeResponse(w)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		rp.ServeHTTP(w, r)
	}), nil
}

// tooLargeResponse answers 413 in the gateway's error shape (code 8: RESOURCE_EXHAUSTED).
func tooLargeResponse(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusRequestEntityTooLarge)
	_, _ = w.Write([]byte(`{"code":8,"message":"request body too large"}`))
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

// spa serves files from dist; unknown paths get index.html so client-side routes work on reload.
func spa(dist string) http.Handler {
	files := http.FileServer(http.Dir(dist))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clean := path.Clean("/" + r.URL.Path)
		if strings.HasSuffix(clean, ".map") { // the build keeps source maps for debugging; never ship them
			http.NotFound(w, r)
			return
		}
		if strings.HasPrefix(clean, "/assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable") // content-hashed names
			files.ServeHTTP(w, r)
			return
		}
		if clean != "/" {
			if fi, err := os.Stat(filepath.Join(dist, filepath.FromSlash(clean))); err == nil && !fi.IsDir() {
				files.ServeHTTP(w, r)
				return
			}
		}
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFile(w, r, filepath.Join(dist, "index.html"))
	})
}

func newHandler(cfg config, log *slog.Logger) (http.Handler, error) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	for prefix, target := range map[string]string{"/api": cfg.API, "/auth": cfg.Issuer, "/summarizer": cfg.Summarizer} {
		h, err := proxy(target, prefix, cfg.UpstreamTimeout, log)
		if err != nil {
			return nil, err
		}
		mux.Handle(prefix+"/", h)
	}
	mux.Handle("/", spa(cfg.Dist))
	return securityHeaders(mux), nil
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(telemetry.Healthcheck(envOr("FRAUD_HEALTHCHECK_URL", "http://127.0.0.1:8500/healthz")))
	}
	log := telemetry.NewLogger("webserve")
	cfg := config{
		Dist:       envOr("FRAUD_WEB_DIST", "web/dist"),
		API:        envOr("FRAUD_WEB_API_URL", "http://127.0.0.1:8511"),
		Issuer:     envOr("FRAUD_WEB_ISSUER_URL", "http://127.0.0.1:8515"),
		Summarizer: envOr("FRAUD_WEB_SUMMARIZER_URL", "http://127.0.0.1:8513"),
		// Console calls budget 2 s; the proxy gives an upstream a little more before answering 502.
		UpstreamTimeout: 5 * time.Second,
	}
	h, err := newHandler(cfg, log)
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}
	srv := &http.Server{Addr: envOr("FRAUD_WEB_ADDR", ":8500"), Handler: h, ReadHeaderTimeout: 2 * time.Second, IdleTimeout: 60 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	log.Info("webserve listening", "addr", srv.Addr, "dist", cfg.Dist)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("http server", "err", err)
		os.Exit(1)
	}
}
