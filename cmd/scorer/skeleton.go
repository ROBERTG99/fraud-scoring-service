package main

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// Walking skeleton (T-05): a rules-only decision over HTTP, replaced by the full gRPC pipeline in M3.
// The degraded band mirrors rules/*.yaml degraded.model_unavailable.review_min_amount_minor.
const skeletonReviewMinAmountMinor = 20000

type skeletonTx struct {
	TransactionID string      `json:"transaction_id"`
	AmountMinor   json.Number `json:"amount_minor"` // number or string (proto int64 JSON mapping)
	MCC           string      `json:"mcc"`
}

type skeletonDecision struct {
	DecisionID    string   `json:"decisionId"`
	TransactionID string   `json:"transactionId"`
	Action        string   `json:"action"`
	Score         float32  `json:"score"`
	ReasonCodes   []string `json:"reasonCodes"`
	ModelVersion  string   `json:"modelVersion"`
	RulesVersion  string   `json:"rulesVersion"`
	Degraded      bool     `json:"degraded"`
	DecidedAt     string   `json:"decidedAt"`
}

func newSkeletonMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("POST /v1/score", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Transaction json.RawMessage `json:"transaction"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		var tx skeletonTx
		if err := json.Unmarshal(req.Transaction, &tx); err != nil {
			http.Error(w, "invalid transaction", http.StatusBadRequest)
			return
		}
		amount, err := tx.AmountMinor.Int64()
		if err != nil || amount <= 0 {
			http.Error(w, "amount_minor must be a positive integer", http.StatusBadRequest)
			return
		}
		action := "ACTION_APPROVE"
		reasons := []string{}
		if tx.MCC == "7995" {
			action = "ACTION_DECLINE"
			reasons = append(reasons, "BLOCKED_MCC")
		} else if amount >= skeletonReviewMinAmountMinor {
			action = "ACTION_REVIEW"
		}
		reasons = append(reasons, "MODEL_UNAVAILABLE")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(skeletonDecision{
			DecisionID:    uuid.NewString(),
			TransactionID: tx.TransactionID,
			Action:        action,
			Score:         -1,
			ReasonCodes:   reasons,
			ModelVersion:  "none",
			RulesVersion:  "skeleton",
			Degraded:      true,
			DecidedAt:     time.Now().UTC().Format(time.RFC3339Nano),
		})
	})
	return mux
}
