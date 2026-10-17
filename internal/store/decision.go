package store

import (
	"encoding/json"
	"strings"
	"time"

	fraudv1 "github.com/ROBERTG99/fraud-scoring-service/gen/go/fraud/v1"
	"github.com/ROBERTG99/fraud-scoring-service/internal/events"
)

// DecisionRecord is everything persisted for one decision: the decisions row, the outbox payload and,
// for REVIEW, a review case.
type DecisionRecord struct {
	Decision    *fraudv1.Decision
	AccountID   string
	Features    map[string]float64
	AmountMinor int64
	Currency    string
	Tx          TxAttrs       // decisions.tx: attributes the rule environment and case detail need
	Event       *events.Event // the transaction event published to events.transaction
	TraceParent string        // W3C traceparent of the Score span; the outbox relay sends it as a Kafka header
}

// TxAttrs are the request attributes stored in decisions.tx (no PII: synthetic tokens, /24 prefixes).
type TxAttrs struct {
	AmountMinor  int64  `json:"amount_minor"`
	Currency     string `json:"currency"`
	MerchantID   string `json:"merchant_id"`
	MerchantName string `json:"merchant_name,omitempty"`
	MCC          string `json:"mcc"`
	Channel      string `json:"channel"`
	Country      string `json:"country"`
	IPPrefix     string `json:"ip_prefix"`
	DeviceID     string `json:"device_id"`
	EventTS      string `json:"event_ts"`
}

// ActionName returns APPROVE|REVIEW|DECLINE for the decisions.action column.
func ActionName(a fraudv1.Action) string {
	return strings.TrimPrefix(a.String(), "ACTION_")
}

// DecisionMessage is the fraud.decision.v1 payload (schemas/events/decision.json).
type DecisionMessage struct {
	Schema        string   `json:"schema"`
	DecisionID    string   `json:"decision_id"`
	TransactionID string   `json:"transaction_id"`
	AccountID     string   `json:"account_id"`
	Action        string   `json:"action"`
	Score         float64  `json:"score"`
	ReasonCodes   []string `json:"reason_codes"`
	ModelVersion  string   `json:"model_version"`
	RulesVersion  string   `json:"rules_version"`
	Degraded      bool     `json:"degraded"`
	DecidedAt     string   `json:"decided_at"`
}

// OutboxPayload renders the decision event.
func (r *DecisionRecord) OutboxPayload() []byte {
	d := r.Decision
	codes := d.ReasonCodes
	if codes == nil {
		codes = []string{}
	}
	b, _ := json.Marshal(DecisionMessage{
		Schema: "fraud.decision.v1", DecisionID: d.DecisionId, TransactionID: d.TransactionId, AccountID: r.AccountID,
		Action: ActionName(d.Action), Score: roundScore(d.Score), ReasonCodes: codes, ModelVersion: d.ModelVersion,
		RulesVersion: d.RulesVersion, Degraded: d.Degraded, DecidedAt: d.DecidedAt.AsTime().UTC().Format("2006-01-02T15:04:05.000Z07:00"),
	})
	return b
}

func roundScore(s float32) float64 {
	// float32 → shortest decimal that round-trips, so 0.62 stays 0.62 in JSON.
	b, _ := json.Marshal(s)
	var f float64
	_ = json.Unmarshal(b, &f)
	return f
}

// Priority orders review cases: model score when available, else a bounded function of the amount.
func (r *DecisionRecord) Priority() float32 {
	if r.Decision.Score >= 0 {
		return r.Decision.Score
	}
	return float32(min(1.0, float64(r.AmountMinor)/1_000_000.0))
}

// DecidedAt returns the decision timestamp.
func (r *DecisionRecord) DecidedAt() time.Time { return r.Decision.DecidedAt.AsTime() }
