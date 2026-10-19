package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Triton calls the KServe v2 inference protocol: POST {base}/v2/models/{name}/infer.
// Alias production maps to model fraud_gbdt, shadow to fraud_gbdt_shadow.
type Triton struct {
	base string
	hc   *http.Client
}

// NewTriton returns a KServe v2 client for Triton at base (http://triton:8000).
func NewTriton(base string, hc *http.Client) *Triton {
	if hc == nil {
		hc = NewHTTPClient()
	}
	return &Triton{base: strings.TrimRight(base, "/"), hc: hc}
}

// TritonModelName maps an alias to the Triton model name.
func TritonModelName(alias string) string {
	if alias == AliasShadow {
		return "fraud_gbdt_shadow"
	}
	return "fraud_gbdt"
}

type kservTensor struct {
	Name     string    `json:"name"`
	Shape    []int     `json:"shape,omitempty"`
	Datatype string    `json:"datatype,omitempty"`
	Data     []float32 `json:"data,omitempty"`
}

type kservOutputSpec struct {
	Name string `json:"name"`
}

type kservRequest struct {
	Inputs  []kservTensor     `json:"inputs"`
	Outputs []kservOutputSpec `json:"outputs"`
}

type kservResponse struct {
	ModelName    string        `json:"model_name"`
	ModelVersion string        `json:"model_version"`
	Outputs      []kservTensor `json:"outputs"`
}

// BuildKServeRequest flattens rows into one FP32 tensor named "features" with shape [n, 13].
func BuildKServeRequest(rows [][]float32) ([]byte, error) {
	if len(rows) == 0 {
		return nil, fmt.Errorf("no rows")
	}
	width := len(rows[0])
	data := make([]float32, 0, len(rows)*width)
	for _, r := range rows {
		if len(r) != width {
			return nil, fmt.Errorf("ragged rows")
		}
		data = append(data, r...)
	}
	return json.Marshal(kservRequest{
		Inputs:  []kservTensor{{Name: "features", Shape: []int{len(rows), width}, Datatype: "FP32", Data: data}},
		Outputs: []kservOutputSpec{{Name: "probability"}},
	})
}

// ParseKServeResponse extracts the probability output (one score per row).
func ParseKServeResponse(body []byte, rows int) (Prediction, error) {
	var resp kservResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return Prediction{}, err
	}
	for _, o := range resp.Outputs {
		if o.Name != "probability" {
			continue
		}
		if len(o.Data) != rows {
			return Prediction{}, fmt.Errorf("got %d scores for %d rows", len(o.Data), rows)
		}
		return Prediction{Scores: o.Data, Version: fmt.Sprintf("triton:%s/%s", resp.ModelName, resp.ModelVersion)}, nil
	}
	return Prediction{}, fmt.Errorf("no probability output")
}

// Predict scores rows on Triton.
func (c *Triton) Predict(ctx context.Context, alias string, rows [][]float32) (Prediction, error) {
	ctx, cancel := withDefaultDeadline(ctx)
	defer cancel()
	body, err := BuildKServeRequest(rows)
	if err != nil {
		return Prediction{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	url := c.base + "/v2/models/" + TritonModelName(alias) + "/infer"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return Prediction{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return Prediction{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Prediction{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if resp.StatusCode != http.StatusOK {
		return Prediction{}, fmt.Errorf("%w: status %d", ErrUnavailable, resp.StatusCode)
	}
	p, err := ParseKServeResponse(raw, len(rows))
	if err != nil {
		return Prediction{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return p, nil
}
