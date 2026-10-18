// Command evaldecide is the target of the `decisions` eval suite (spec §11): it reads evalgate dataset rows
// (JSONL on stdin) and decides each one through the real ScorerServer, active rule set and combination
// table, with the feature store and the model stubbed by the case: `features` (13 values, or null for Redis
// down) and `model` ({"score": s} or {"unavailable": true}). One JSON line per input line on stdout, flushed
// per line so a caller can stream cases. Nothing touches Redis, Postgres, Kafka or a model server.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	fraudv1 "github.com/ROBERTG99/fraud-scoring-service/gen/go/fraud/v1"
	"github.com/ROBERTG99/fraud-scoring-service/internal/api"
	"github.com/ROBERTG99/fraud-scoring-service/internal/features"
	"github.com/ROBERTG99/fraud-scoring-service/internal/model"
	"github.com/ROBERTG99/fraud-scoring-service/internal/rules"
	"github.com/ROBERTG99/fraud-scoring-service/internal/store"
)

// Input is the `input` of a decisions dataset row.
type Input struct {
	Transaction struct {
		TransactionID string `json:"transaction_id"`
		AccountID     string `json:"account_id"`
		AmountMinor   int64  `json:"amount_minor"`
		Currency      string `json:"currency"`
		MerchantID    string `json:"merchant_id"`
		MerchantName  string `json:"merchant_name"`
		MCC           string `json:"mcc"`
		Channel       string `json:"channel"`
		Country       string `json:"country"`
		IPPrefix      string `json:"ip_prefix"`
		DeviceID      string `json:"device_id"`
		EventTS       string `json:"event_ts"`
	} `json:"transaction"`
	Features map[string]float64 `json:"features"`
	Model    struct {
		Score       *float32 `json:"score"`
		Unavailable bool     `json:"unavailable"`
	} `json:"model"`
}

// Output is one decided case.
type Output struct {
	ID           string   `json:"id"`
	Action       string   `json:"action,omitempty"`
	ReasonCodes  []string `json:"reason_codes"`
	Degraded     bool     `json:"degraded"`
	Score        float32  `json:"score"`
	ModelVersion string   `json:"model_version,omitempty"`
	RulesVersion string   `json:"rules_version,omitempty"`
	Error        string   `json:"error,omitempty"`
}

type caseFeatures struct {
	spec *features.Spec
	vec  *features.Vector // nil: the store is down
}

func (c caseFeatures) ScoreTx(context.Context, features.Tx) (features.Vector, features.Raw, error) {
	if c.vec == nil {
		return features.Vector{}, features.Raw{}, errors.New("eval: features unavailable")
	}
	return *c.vec, features.Raw{}, nil
}

func (c caseFeatures) Spec() *features.Spec { return c.spec }

type caseModel struct{ score *float32 }

func (m caseModel) Predict(context.Context, string, [][]float32) (model.Prediction, error) {
	if m.score == nil {
		return model.Prediction{}, fmt.Errorf("%w: eval: model unavailable", model.ErrUnavailable)
	}
	return model.Prediction{Scores: []float32{*m.score}, Version: "eval-model"}, nil
}

type memory struct{}

func (memory) Get(context.Context, string) (*fraudv1.Decision, []byte, error) { return nil, nil, nil }
func (memory) Put(context.Context, *fraudv1.Decision, []byte) error           { return nil }
func (memory) Commit(_ context.Context, r store.DecisionRecord) (*fraudv1.Decision, error) {
	return r.Decision, nil
}
func (memory) GetByTransaction(context.Context, string) (*fraudv1.Decision, []byte, error) {
	return nil, nil, store.ErrNotFound
}

func decide(ctx context.Context, eng *rules.Engine, spec *features.Spec, id string, in Input) Output {
	out := Output{ID: id}
	f := caseFeatures{spec: spec}
	if in.Features != nil {
		var v features.Vector
		for i, name := range features.Names() {
			value, ok := in.Features[name]
			if !ok {
				out.Error = "missing feature " + name
				return out
			}
			v[i] = value
		}
		f.vec = &v
	}
	m := caseModel{score: in.Model.Score}
	if in.Model.Unavailable {
		m.score = nil
	}
	ts, err := time.Parse(time.RFC3339Nano, in.Transaction.EventTS)
	if err != nil {
		out.Error = "event_ts: " + err.Error()
		return out
	}
	t := in.Transaction
	srv := api.NewScorerServer(api.ScorerDeps{Features: f, Model: m, Rules: eng, Idempotency: memory{}, Decisions: memory{},
		Log: slog.New(slog.DiscardHandler), Now: func() time.Time { return ts }})
	d, err := srv.Score(ctx, &fraudv1.ScoreRequest{Transaction: &fraudv1.Transaction{
		TransactionId: t.TransactionID, AccountId: t.AccountID, AmountMinor: t.AmountMinor, Currency: t.Currency,
		MerchantId: t.MerchantID, MerchantName: t.MerchantName, Mcc: t.MCC,
		Channel: fraudv1.Channel(fraudv1.Channel_value["CHANNEL_"+t.Channel]), Country: t.Country,
		IpPrefix: t.IPPrefix, DeviceId: t.DeviceID, EventTs: timestamppb.New(ts),
	}})
	if err != nil {
		out.Error = err.Error()
		return out
	}
	out.Action = store.ActionName(d.Action)
	out.ReasonCodes = slices.Sorted(slices.Values(d.ReasonCodes))
	if out.ReasonCodes == nil {
		out.ReasonCodes = []string{}
	}
	out.Degraded, out.Score, out.ModelVersion, out.RulesVersion = d.Degraded, d.Score, d.ModelVersion, d.RulesVersion
	return out
}

func run(r io.Reader, w io.Writer, rulesDir, specPath string) error {
	spec, err := features.LoadSpec(specPath)
	if err != nil {
		return err
	}
	eng, err := rules.NewEngine(rulesDir, nil)
	if err != nil {
		return err
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	bw := bufio.NewWriter(w)
	enc := json.NewEncoder(bw)
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var row struct {
			ID    string `json:"id"`
			Input Input  `json:"input"`
		}
		var out Output
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
			out = Output{ID: row.ID, Error: "invalid row: " + err.Error()}
		} else {
			// Each case gets the full Score budget; the stubs answer at once.
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			out = decide(ctx, eng, spec, row.ID, row.Input)
			cancel()
		}
		if err := enc.Encode(out); err != nil {
			return err
		}
		if err := bw.Flush(); err != nil {
			return err
		}
	}
	return sc.Err()
}

func main() {
	rulesDir, specPath := "rules", "features/spec.yaml"
	if v := os.Getenv("FRAUD_RULES_DIR"); v != "" {
		rulesDir = v
	}
	if v := os.Getenv("FRAUD_FEATURE_SPEC"); v != "" {
		specPath = v
	}
	if err := run(os.Stdin, os.Stdout, rulesDir, specPath); err != nil {
		fmt.Fprintln(os.Stderr, "evaldecide:", err)
		os.Exit(1)
	}
}
