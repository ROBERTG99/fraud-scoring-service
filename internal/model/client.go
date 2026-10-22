// Package model holds model-serving clients (BentoML, Triton KServe v2) and the shadow runner.
package model

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"
)

// Aliases served by the model server.
const (
	AliasProduction = "production"
	AliasShadow     = "shadow"
)

// ErrUnavailable means no score could be obtained in time; the scorer falls back to rules-only.
var ErrUnavailable = errors.New("model unavailable")

// Prediction is the calibrated probability per input row and the serving model version.
type Prediction struct {
	Scores  []float32
	Version string
}

// Client scores feature rows. Implementations must honour ctx deadlines and wrap every failure
// (timeout, transport, status, decoding) in ErrUnavailable.
type Client interface {
	Predict(ctx context.Context, alias string, rows [][]float32) (Prediction, error)
}

// DefaultTimeout bounds a model call whose context carries no deadline.
const DefaultTimeout = 20 * time.Millisecond

func withDefaultDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, DefaultTimeout)
}

// NewHTTPClient returns an HTTP client sized for the scoring path (512 connections per host).
func NewHTTPClient() *http.Client {
	tr := &http.Transport{
		Proxy:               nil, // never route model calls through an egress proxy
		DialContext:         (&net.Dialer{Timeout: 200 * time.Millisecond, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:        512,
		MaxIdleConnsPerHost: 512,
		MaxConnsPerHost:     512,
		IdleConnTimeout:     90 * time.Second,
		ForceAttemptHTTP2:   false,
	}
	return &http.Client{Transport: tr}
}
