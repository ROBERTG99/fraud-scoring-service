// Command feature-updater consumes events.* (one goroutine per partition) and keeps Redis features
// current; invalid events go to events.dlq. HTTP :8512 serves /healthz, /readyz and /metrics.
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

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/ROBERTG99/fraud-scoring-service/internal/events"
	"github.com/ROBERTG99/fraud-scoring-service/internal/features"
	"github.com/ROBERTG99/fraud-scoring-service/internal/store"
	"github.com/ROBERTG99/fraud-scoring-service/internal/stream"
	"github.com/ROBERTG99/fraud-scoring-service/internal/telemetry"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

type multiSink []Sink

func (m multiSink) Enqueue(evs []events.Event) {
	for _, s := range m {
		s.Enqueue(evs)
	}
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(telemetry.Healthcheck(envOr("FRAUD_HEALTHCHECK_URL", "http://127.0.0.1:8512/healthz")))
	}
	log := telemetry.NewLogger("feature-updater")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, log); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("feature-updater failed", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, log *slog.Logger) error {
	brokers := stream.Brokers(envOr("FRAUD_KAFKA_BROKERS", "127.0.0.1:8540"))
	spec, err := features.LoadSpec(os.Getenv("FRAUD_FEATURE_SPEC"))
	if err != nil {
		return err
	}
	rdb := features.NewRedisClient(envOr("FRAUD_REDIS_ADDR", "127.0.0.1:8530"))
	defer rdb.Close()
	fs := features.NewStore(rdb, "", spec)

	var sinks multiSink
	if dsn := os.Getenv("FRAUD_PG_DSN"); dsn != "" {
		pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		pool, err := pgxpool.New(pctx, dsn)
		cancel()
		if err != nil {
			return err
		}
		defer pool.Close()
		w := store.NewAccountEventWriter(pool, log)
		defer w.Close()
		sinks = append(sinks, w)
	}

	admin, err := kgo.NewClient(kgo.SeedBrokers(brokers...))
	if err != nil {
		return err
	}
	tctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	err = stream.EnsureTopics(tctx, admin, stream.Partitions, 1, stream.AllTopics...)
	cancel()
	admin.Close()
	if err != nil {
		return err
	}

	u, err := New(Config{Brokers: brokers}, fs, sinks, log)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		rctx, cancel := context.WithTimeout(r.Context(), 200*time.Millisecond)
		defer cancel()
		if err := rdb.Ping(rctx).Err(); err != nil {
			http.Error(w, "redis: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ready"))
	})
	mux.Handle("GET /metrics", promhttp.HandlerFor(telemetry.Registry, promhttp.HandlerOpts{}))
	srv := &http.Server{Addr: envOr("FRAUD_UPDATER_HTTP_ADDR", ":8512"), Handler: mux, ReadHeaderTimeout: 2 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http", "err", err)
		}
	}()
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	log.Info("feature-updater started", "brokers", brokers)
	return u.Run(ctx)
}
