// Command scorer serves fraud decisions over gRPC (:8510) and HTTP/JSON (:8511).
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck(envOr("FRAUD_HEALTHCHECK_URL", "http://127.0.0.1:8511/healthz")))
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	srv := &http.Server{Addr: envOr("FRAUD_HTTP_ADDR", ":8511"), Handler: newSkeletonMux(), ReadHeaderTimeout: 2 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	logger.Info("scorer listening", "http", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("http server", "err", err)
		os.Exit(1)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func healthcheck(url string) int {
	client := http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(url) //nolint:noctx // short-lived CLI probe
	if err != nil {
		return 1
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
