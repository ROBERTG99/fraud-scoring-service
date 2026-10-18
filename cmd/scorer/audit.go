package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/ROBERTG99/fraud-scoring-service/internal/auth"
	"github.com/ROBERTG99/fraud-scoring-service/internal/store"
)

// auditKeyFromEnv returns the audit-chain HMAC key (D-050): FRAUD_AUDIT_KEY, hex, at least 32 bytes, held
// outside the database. Local stacks (FRAUD_ENV=dev) fall back to a fixed development key, which is public
// and protects nothing; elsewhere a missing key stops the scorer.
func auditKeyFromEnv() ([]byte, error) {
	raw := os.Getenv("FRAUD_AUDIT_KEY")
	if raw == "" {
		if !auth.DevMode() {
			return nil, errors.New("FRAUD_AUDIT_KEY is required outside FRAUD_ENV=dev")
		}
		k := sha256.Sum256([]byte("fraud-scoring-service development audit key"))
		return k[:], nil
	}
	key, err := hex.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("FRAUD_AUDIT_KEY: %w", err)
	}
	if len(key) < store.MinAuditKeyBytes {
		return nil, fmt.Errorf("FRAUD_AUDIT_KEY: %d bytes, need at least %d", len(key), store.MinAuditKeyBytes)
	}
	return key, nil
}

// auditVerify verifies the chain and the anchors in FRAUD_AUDIT_ANCHORS; with anchor it then appends the
// current head there. Exit codes: 0 intact, 1 broken, 2 could not run.
func auditVerify(anchor bool) int {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := "audit-verify"
	if anchor {
		cmd = "audit-anchor"
	}
	fail := func(code int, err error) int {
		fmt.Fprintf(os.Stderr, "%s: %v\n", cmd, err)
		return code
	}
	key, err := auditKeyFromEnv()
	if err != nil {
		return fail(2, err)
	}
	path := os.Getenv("FRAUD_AUDIT_ANCHORS")
	if anchor && path == "" {
		return fail(2, errors.New("FRAUD_AUDIT_ANCHORS (a file outside the database) is not set"))
	}
	anchors, err := readAnchors(path, anchor) // the first audit-anchor creates the file
	if err != nil {
		return fail(2, err)
	}
	pool, err := openPool(ctx, envOr("FRAUD_PG_DSN", "postgres://fraud_app:fraud_app@127.0.0.1:8520/fraud?sslmode=disable"), 2)
	if err != nil {
		return fail(2, err)
	}
	defer pool.Close()
	n, head, err := store.VerifyAudit(ctx, pool, key, anchors)
	if err != nil {
		return fail(1, fmt.Errorf("%w (after %d verified entries)", err, n))
	}
	fmt.Printf("audit chain intact: %d entries verified, %d anchors checked, head seq %d\n", n, len(anchors), head.Seq)
	if !anchor || n == 0 {
		return 0
	}
	line, err := json.Marshal(store.NewAuditAnchor(key, head, time.Now()))
	if err != nil {
		return fail(2, err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fail(2, err)
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		_ = f.Close()
		return fail(2, err)
	}
	if err := f.Close(); err != nil {
		return fail(2, err)
	}
	fmt.Printf("anchored seq %d %s\n", head.Seq, head.Hash)
	return 0
}

func readAnchors(path string, allowMissing bool) ([]store.AuditAnchor, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path) // #nosec G304 -- an operator-supplied path
	if errors.Is(err, os.ErrNotExist) && allowMissing {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return store.ReadAuditAnchors(io.Reader(f))
}
