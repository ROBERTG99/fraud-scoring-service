package model

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "tests", "fixtures", "triton", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func canonical(t *testing.T, b []byte) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(v)
	return string(out)
}

// TestKServeV2RequestShape compares the request with a recorded KServe v2 request and decodes a
// recorded Triton response.
func TestKServeV2RequestShape(t *testing.T) {
	rows := [][]float32{{0, 1, 3, 4200, 94000, 2, 1, 43200, 2, 1, 2, 14, 9.4}, {1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13}}
	got, err := BuildKServeRequest(rows)
	if err != nil {
		t.Fatal(err)
	}
	if canonical(t, got) != canonical(t, readFixture(t, "infer_request.json")) {
		t.Fatalf("request differs from recorded fixture:\n got %s", got)
	}
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		if !bytes.Equal(body, got) {
			http.Error(w, "unexpected body", http.StatusBadRequest)
			return
		}
		_, _ = w.Write(readFixture(t, "infer_response.json"))
	}))
	defer srv.Close()
	p, err := NewTriton(srv.URL, nil).Predict(context.Background(), AliasProduction, rows)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v2/models/fraud_gbdt/infer" {
		t.Fatalf("path %s", gotPath)
	}
	if p.Version != "triton:fraud_gbdt/1" || len(p.Scores) != 2 || p.Scores[0] != float32(0.9927) {
		t.Fatalf("prediction %+v", p)
	}
	if TritonModelName(AliasShadow) != "fraud_gbdt_shadow" {
		t.Fatal("shadow alias must map to fraud_gbdt_shadow")
	}
	if _, err := ParseKServeResponse([]byte(`{"outputs":[{"name":"probability","data":[0.1]}]}`), 2); err == nil {
		t.Fatal("row/score count mismatch must fail")
	}
}
