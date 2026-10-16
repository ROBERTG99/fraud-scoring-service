package telemetry

import (
	"log/slog"
	"net/http"
	"os"
	"time"
)

// NewLogger returns a JSON logger tagged with the service name. Never log PII at INFO.
func NewLogger(service string) *slog.Logger {
	level := slog.LevelInfo
	if os.Getenv("FRAUD_LOG_LEVEL") == "debug" {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})).With("service", service)
}

// Healthcheck probes url and returns a process exit code (used by distroless HEALTHCHECK).
func Healthcheck(url string) int {
	client := http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(url) // #nosec G107 -- local probe URL from configuration
	if err != nil {
		return 1
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
