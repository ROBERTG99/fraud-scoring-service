package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestScoreReturnsRulesOnlyDecision(t *testing.T) {
	srv := httptest.NewServer(newSkeletonMux())
	defer srv.Close()

	body := []byte(`{"transaction":{"transaction_id":"0192f000-0000-7000-8000-000000000001","account_id":"acc-1","amount_minor":94000,"currency":"USD","mcc":"5732","channel":"CHANNEL_ECOMMERCE"}}`)
	resp, err := http.Post(srv.URL+"/v1/score", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var d struct {
		Action       string   `json:"action"`
		ModelVersion string   `json:"modelVersion"`
		ReasonCodes  []string `json:"reasonCodes"`
		Degraded     bool     `json:"degraded"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		t.Fatal(err)
	}
	if d.ModelVersion != "none" || !d.Degraded {
		t.Fatalf("expected rules-only degraded decision, got %+v", d)
	}
	if d.Action != "ACTION_REVIEW" {
		t.Fatalf("amount above the degraded review floor must be REVIEW, got %s", d.Action)
	}
	if len(d.ReasonCodes) == 0 || d.ReasonCodes[len(d.ReasonCodes)-1] != "MODEL_UNAVAILABLE" {
		t.Fatalf("expected MODEL_UNAVAILABLE reason, got %v", d.ReasonCodes)
	}

	health, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	health.Body.Close()
	if health.StatusCode != http.StatusOK {
		t.Fatalf("healthz status %d", health.StatusCode)
	}
}
