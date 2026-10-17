// Command replay feeds a generated dataset through the running system: history to Kafka, then the last
// -window of transactions to the scorer over gRPC at -rate per second (see internal/replay).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	fraudv1 "github.com/ROBERTG99/fraud-scoring-service/gen/go/fraud/v1"
	"github.com/ROBERTG99/fraud-scoring-service/internal/replay"
	"github.com/ROBERTG99/fraud-scoring-service/internal/stream"
	"github.com/ROBERTG99/fraud-scoring-service/internal/telemetry"
)

// options are shared with cmd/seed through flags of the same names.
type options struct {
	data        string
	window      time.Duration
	rate        float64
	workers     int
	scorer      string
	brokers     string
	token       string
	waitGroup   string
	waitTimeout time.Duration
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func parse(args []string) (options, error) {
	var o options
	fs := flag.NewFlagSet("replay", flag.ContinueOnError)
	fs.StringVar(&o.data, "data", "artifacts/data/demo", "dataset directory (cmd/gen output)")
	fs.DurationVar(&o.window, "window", 24*time.Hour, "score the transactions in the last window of the dataset")
	fs.Float64Var(&o.rate, "rate", 500, "transactions scored per second (0 = unlimited)")
	fs.IntVar(&o.workers, "workers", 16, "concurrent Score calls")
	fs.StringVar(&o.scorer, "scorer", envOr("FRAUD_SCORER_GRPC", "127.0.0.1:8510"), "scorer gRPC address")
	fs.StringVar(&o.brokers, "brokers", envOr("FRAUD_KAFKA_BROKERS", "127.0.0.1:8540"), "Kafka brokers")
	fs.StringVar(&o.token, "token", os.Getenv("FRAUD_SERVICE_TOKEN"), "service bearer token (default $FRAUD_SERVICE_TOKEN)")
	fs.StringVar(&o.waitGroup, "wait-group", "feature-updater", "consumer group to wait for before scoring (empty: no wait)")
	fs.DurationVar(&o.waitTimeout, "wait-timeout", 5*time.Minute, "how long to wait for the feature-updater")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if o.token == "" {
		return o, errors.New("a service token is required (-token or FRAUD_SERVICE_TOKEN)")
	}
	return o, nil
}

func main() {
	log := telemetry.NewLogger("replay")
	o, err := parse(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "replay:", err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	st, err := run(ctx, o, log)
	if err != nil {
		log.Error("replay failed", "err", err)
		os.Exit(1)
	}
	fmt.Println(summary(st))
}

func summary(st replay.Stats) string {
	return fmt.Sprintf("replay: produced %d events, scored %d transactions in %s (APPROVE %d, REVIEW %d, DECLINE %d, degraded %d, errors %d %v)",
		st.Produced, st.Scored, st.Elapsed.Round(time.Millisecond), st.Actions["APPROVE"], st.Actions["REVIEW"],
		st.Actions["DECLINE"], st.Degraded, st.Errors, st.ByCode)
}

func run(ctx context.Context, o options, log *slog.Logger) (replay.Stats, error) {
	evs, err := replay.Load(filepath.Join(o.data, "events.jsonl"))
	if err != nil {
		return replay.Stats{}, err
	}
	warm, live := replay.Split(evs, o.window)
	log.Info("replay: dataset loaded", "events", len(evs), "warm", len(warm), "live", len(live))

	kcl, err := kgo.NewClient(kgo.SeedBrokers(stream.Brokers(o.brokers)...), kgo.ProducerLinger(5*time.Millisecond),
		kgo.ProducerBatchMaxBytes(1<<20), kgo.RecordPartitioner(kgo.StickyKeyPartitioner(nil)))
	if err != nil {
		return replay.Stats{}, err
	}
	defer kcl.Close()
	tctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	err = stream.EnsureTopics(tctx, kcl, stream.Partitions, 1, stream.AllTopics...)
	cancel()
	if err != nil {
		return replay.Stats{}, err
	}
	conn, err := grpc.NewClient(o.scorer, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return replay.Stats{}, err
	}
	defer func() { _ = conn.Close() }()

	cfg := replay.Config{Rate: o.rate, Workers: o.workers, Token: o.token, Log: log}
	if o.waitGroup != "" {
		wait := replay.CaughtUp(kadm.NewClient(kcl), o.waitGroup, []string{stream.TopicLogin, stream.TopicDevice, stream.TopicTransaction}, 250*time.Millisecond)
		cfg.WaitCaughtUp = func(ctx context.Context) error {
			wctx, cancel := context.WithTimeout(ctx, o.waitTimeout)
			defer cancel()
			return wait(wctx)
		}
	}
	return replay.Run(ctx, cfg, kcl, fraudv1.NewScorerClient(conn), warm, live)
}
