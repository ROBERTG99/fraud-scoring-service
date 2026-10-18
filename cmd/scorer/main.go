// Command scorer serves fraud decisions over gRPC (:8510) and HTTP/JSON (:8511).
//
//	scorer               run the service
//	scorer healthcheck   probe /healthz (container HEALTHCHECK)
//	scorer audit-verify  recompute the audit_log hash chain and exit non-zero when it is broken
//	scorer migrate       apply goose migrations with FRAUD_PG_MIGRATE_DSN (owner role)
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/ROBERTG99/fraud-scoring-service/internal/api"
	"github.com/ROBERTG99/fraud-scoring-service/internal/auth"
	"github.com/ROBERTG99/fraud-scoring-service/internal/decide"
	"github.com/ROBERTG99/fraud-scoring-service/internal/features"
	"github.com/ROBERTG99/fraud-scoring-service/internal/model"
	"github.com/ROBERTG99/fraud-scoring-service/internal/rules"
	"github.com/ROBERTG99/fraud-scoring-service/internal/store"
	"github.com/ROBERTG99/fraud-scoring-service/internal/stream"
	"github.com/ROBERTG99/fraud-scoring-service/internal/telemetry"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "healthcheck":
			os.Exit(telemetry.Healthcheck(envOr("FRAUD_HEALTHCHECK_URL", "http://127.0.0.1:8511/healthz")))
		case "audit-verify":
			os.Exit(auditVerify())
		case "migrate":
			os.Exit(migrate())
		}
	}
	log := telemetry.NewLogger("scorer")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, log); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("scorer failed", "err", err)
		os.Exit(1)
	}
}

// eventAge parses FRAUD_MAX_EVENT_AGE: unset → the 24 h default; "0" → no bound (only for replaying
// historical synthetic data, as the seed and demo stacks do; D-041); otherwise a Go duration.
func eventAge(v string) (time.Duration, error) {
	switch v {
	case "":
		return 0, nil
	case "0":
		return -1, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("FRAUD_MAX_EVENT_AGE: want a positive duration or 0, got %q", v)
	}
	return d, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func openPool(ctx context.Context, dsn string, maxConns int32) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	cfg.MaxConns = maxConns
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(pctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(pctx); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

func modelClient(backend string) (model.Client, error) {
	hc := model.NewHTTPClient()
	switch backend {
	case "", "bentoml":
		return model.NewBentoML(envOr("FRAUD_MODEL_URL", "http://127.0.0.1:8514"), hc), nil
	case "triton":
		return model.NewTriton(envOr("FRAUD_TRITON_URL", "http://127.0.0.1:8000"), hc), nil
	}
	return nil, fmt.Errorf("FRAUD_MODEL_BACKEND must be bentoml or triton, got %q", backend)
}

func run(ctx context.Context, log *slog.Logger) error {
	serviceToken := os.Getenv("FRAUD_SERVICE_TOKEN")
	if serviceToken == "" {
		return errors.New("FRAUD_SERVICE_TOKEN is required")
	}
	shutdownTracing, err := telemetry.InitTracing(ctx, "scorer")
	if err != nil {
		return err
	}
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdownTracing(sctx) // flush buffered spans
	}()
	spec, err := features.LoadSpec(envOr("FRAUD_FEATURE_SPEC", "features/spec.yaml"))
	if err != nil {
		return err
	}
	eng, err := rules.NewEngine(envOr("FRAUD_RULES_DIR", "rules"), log)
	if err != nil {
		return err
	}
	// Rules read the feature env, so a rule set written for another feature spec must not run.
	if got := eng.Active().FeatureSpec; got != spec.Version {
		return fmt.Errorf("rule set %s targets feature spec %s, scorer uses %s", eng.Active().Version, got, spec.Version)
	}
	eng.RequireFeatureSpec(spec.Version) // hot reloads too

	rdb := features.NewRedisClient(envOr("FRAUD_REDIS_ADDR", "127.0.0.1:8530"))
	defer func() { _ = rdb.Close() }()
	// Pub/sub for rule reloads holds a connection without a read deadline; keep it off the scoring pool.
	user, pass := features.RedisCredentials()
	sub := redis.NewClient(&redis.Options{Addr: envOr("FRAUD_REDIS_ADDR", "127.0.0.1:8530"), Username: user, Password: pass})
	defer func() { _ = sub.Close() }()

	pool, err := openPool(ctx, envOr("FRAUD_PG_DSN", "postgres://fraud_app:fraud_app@127.0.0.1:8520/fraud?sslmode=disable"), 32)
	if err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	defer pool.Close()

	writer, err := store.NewGroupWriter(pool, store.WriterConfig{SpoolDir: envOr("FRAUD_SPOOL_DIR", "var/spool")}, log)
	if err != nil {
		return err
	}
	defer writer.Close()

	kcl, err := kgo.NewClient(kgo.SeedBrokers(stream.Brokers(envOr("FRAUD_KAFKA_BROKERS", "127.0.0.1:8540"))...),
		kgo.MaxBufferedRecords(100_000), kgo.ProducerLinger(5*time.Millisecond), kgo.RecordPartitioner(kgo.StickyKeyPartitioner(nil)))
	if err != nil {
		return err
	}
	defer func() {
		fctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = kcl.Flush(fctx)
		cancel()
		kcl.Close()
	}()

	base, err := modelClient(os.Getenv("FRAUD_MODEL_BACKEND"))
	if err != nil {
		return err
	}
	// Primary and shadow predictions share one batcher per alias (docs/DECISIONS.md D-024).
	inflight, err := strconv.Atoi(envOr("FRAUD_MODEL_MAX_INFLIGHT", "1"))
	if err != nil {
		return fmt.Errorf("FRAUD_MODEL_MAX_INFLIGHT: %w", err)
	}
	batcher := model.NewBatcher(base, model.BatchConfig{MaxInFlight: inflight})
	defer batcher.Close()
	var mc model.Client = batcher

	cases := store.NewCaseStore(pool)
	saveRules := func(rs *rules.RuleSet) {
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := cases.SaveRuleSet(sctx, rs.Version, rs.SHA256, rs.Body); err != nil {
			log.Error("record rule set", "version", rs.Version, "err", err)
		}
	}
	eng.OnLoad(saveRules)
	saveRules(eng.Active())

	// Owned goroutines: all stop when ctx is cancelled; wait for them before closing their dependencies.
	bg, cancelBG := context.WithCancel(ctx)
	done := make(chan struct{}, 2)
	relay := store.NewRelay(pool, kcl, log, 500)
	go func() { relay.Run(bg, 100*time.Millisecond); done <- struct{}{} }()
	go func() { eng.Watch(bg, 5*time.Second, sub); done <- struct{}{} }()
	defer func() { cancelBG(); <-done; <-done }()

	var shadow *model.ShadowRunner
	var shadowSub api.ShadowSubmitter
	shadowDropped := func() int64 { return 0 }
	if v, _ := strconv.ParseBool(envOr("FRAUD_SHADOW_ENABLED", "true")); v {
		shadow = model.NewShadowRunner(mc, store.NewShadowStore(pool), func(s float64) string { return decide.BandOf(eng.Active(), s) },
			model.ShadowConfig{}, log)
		defer shadow.Close()
		shadowSub = shadow
		shadowDropped = shadow.Dropped
	}

	maxEventAge, err := eventAge(os.Getenv("FRAUD_MAX_EVENT_AGE"))
	if err != nil {
		return err
	}
	prefix := os.Getenv("FRAUD_REDIS_PREFIX") // shared with the feature-updater; default "fr"
	live := api.NewLiveStats(nil)
	scorer := api.NewScorerServer(api.ScorerDeps{
		Features: features.NewStore(rdb, prefix, spec), Model: mc, Rules: eng, Idempotency: store.NewRedisIdempotency(rdb, prefix),
		Decisions: writer, Publisher: kafkaPublisher{cl: kcl}, Shadow: shadowSub, Live: live, Log: log,
		MaxEventAge: maxEventAge,
	})
	caseSrv := &api.CaseServer{Backend: cases, Rules: eng, Live: live, ShadowDropped: shadowDropped, Log: log}

	rps, err := strconv.ParseFloat(envOr("FRAUD_RATE_LIMIT_RPS", strconv.Itoa(api.DefaultUserRPS)), 64)
	if err != nil {
		return fmt.Errorf("FRAUD_RATE_LIMIT_RPS: %w", err)
	}
	verifier := &auth.Verifier{Issuer: envOr("FRAUD_ISSUER", "http://127.0.0.1:8515"),
		Keys: &auth.RemoteKeys{URL: envOr("FRAUD_JWKS_URL", "http://127.0.0.1:8515/.well-known/jwks.json")}}
	srv, err := newServers(ctx, serverConfig{
		GRPCAddr: envOr("FRAUD_GRPC_ADDR", ":8510"),
		HTTPAddr: envOr("FRAUD_HTTP_ADDR", ":8511"),
		Auth:     api.AuthConfig{ServiceToken: serviceToken, Verifier: api.JWTVerifier{V: verifier}, Authorize: api.RBAC},
		Limiter:  api.NewRateLimiter(rps, nil),
		Scorer:   scorer,
		Cases:    caseSrv,
		Ready: func(ctx context.Context) error {
			if err := rdb.Ping(ctx).Err(); err != nil {
				return fmt.Errorf("redis: %w", err)
			}
			if err := pool.Ping(ctx); err != nil {
				return fmt.Errorf("postgres: %w", err)
			}
			return nil
		},
		Log: log,
	})
	if err != nil {
		return err
	}
	log.Info("scorer listening", "grpc", srv.grpcLis.Addr().String(), "http", srv.httpLis.Addr().String(),
		"rules_version", eng.Active().Version, "shadow", shadow != nil)
	return srv.serve(ctx)
}

func auditVerify() int {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, err := openPool(ctx, envOr("FRAUD_PG_DSN", "postgres://fraud_app:fraud_app@127.0.0.1:8520/fraud?sslmode=disable"), 2)
	if err != nil {
		fmt.Fprintln(os.Stderr, "audit-verify:", err)
		return 2
	}
	defer pool.Close()
	n, err := store.VerifyAudit(ctx, pool)
	if err != nil {
		fmt.Fprintf(os.Stderr, "audit-verify: %v (after %d verified entries)\n", err, n)
		return 1
	}
	fmt.Printf("audit chain intact: %d entries verified\n", n)
	return 0
}

func migrate() int {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	dsn := envOr("FRAUD_PG_MIGRATE_DSN", "postgres://fraud:fraud@127.0.0.1:8520/fraud?sslmode=disable")
	if err := store.MigrateUp(ctx, dsn); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		return 1
	}
	v, err := store.MigrationVersion(ctx, dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		return 1
	}
	fmt.Printf("migrations applied: version %d\n", v)
	return 0
}
