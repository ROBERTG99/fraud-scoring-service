package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

// BentoML calls POST {base}/predict {"inputs": [[...13 floats]], "alias": "..."}.
type BentoML struct {
	base string
	hc   *http.Client
}

// NewBentoML returns a client for the BentoML model server at base (http://model-server:8514).
func NewBentoML(base string, hc *http.Client) *BentoML {
	if hc == nil {
		hc = NewHTTPClient()
	}
	return &BentoML{base: strings.TrimRight(base, "/"), hc: hc}
}

type predictRequest struct {
	Inputs [][]float32 `json:"inputs"`
	Alias  string      `json:"alias"`
}

type predictResponse struct {
	Scores       []float32 `json:"scores"`
	ModelVersion string    `json:"model_version"`
}

var bufPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}

// Predict scores rows with the model behind alias.
func (b *BentoML) Predict(ctx context.Context, alias string, rows [][]float32) (Prediction, error) {
	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufPool.Put(buf)
	if err := json.NewEncoder(buf).Encode(predictRequest{Inputs: rows, Alias: alias}); err != nil {
		return Prediction{}, fmt.Errorf("%w: encode: %v", ErrUnavailable, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.base+"/predict", buf)
	if err != nil {
		return Prediction{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.hc.Do(req)
	if err != nil {
		return Prediction{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return Prediction{}, fmt.Errorf("%w: status %d", ErrUnavailable, resp.StatusCode)
	}
	var out predictResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return Prediction{}, fmt.Errorf("%w: decode: %v", ErrUnavailable, err)
	}
	if len(out.Scores) != len(rows) {
		return Prediction{}, fmt.Errorf("%w: got %d scores for %d rows", ErrUnavailable, len(out.Scores), len(rows))
	}
	return Prediction{Scores: out.Scores, Version: out.ModelVersion}, nil
}
