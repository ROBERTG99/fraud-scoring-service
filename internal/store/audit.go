package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// auditLockKey serialises appends so the hash chain never forks (also enforced by UNIQUE(prev_hash)).
const auditLockKey = 724411

// AuditEntry is one audit_log row.
type AuditEntry struct {
	Seq      int64
	TS       time.Time
	ActorSub string
	Role     string
	Action   string
	Target   string
	Details  map[string]any
	PrevHash string
	Hash     string
}

// AuditHash is sha256(prev_hash | ts | actor | role | action | target | canonical details JSON).
func AuditHash(prev string, ts time.Time, actor, role, action, target string, details map[string]any) (string, error) {
	canon, err := json.Marshal(details) // map keys are sorted by encoding/json
	if err != nil {
		return "", err
	}
	h := sha256.New()
	for _, part := range []string{prev, ts.UTC().Format(time.RFC3339Nano), actor, role, action, target, string(canon)} {
		h.Write([]byte(part))
		h.Write([]byte{'|'})
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// AppendAudit appends a hash-chained entry inside tx.
func AppendAudit(ctx context.Context, tx pgx.Tx, actor, role, action, target string, details map[string]any) (AuditEntry, error) {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, auditLockKey); err != nil {
		return AuditEntry{}, err
	}
	var prev string
	err := tx.QueryRow(ctx, `SELECT hash FROM audit_log ORDER BY seq DESC LIMIT 1`).Scan(&prev)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return AuditEntry{}, err
	}
	ts := time.Now().UTC().Truncate(time.Microsecond) // Postgres timestamptz precision
	// Round-trip details through JSON so the hash covers exactly what jsonb will return.
	raw, err := json.Marshal(details)
	if err != nil {
		return AuditEntry{}, err
	}
	var norm map[string]any
	if err := json.Unmarshal(raw, &norm); err != nil {
		return AuditEntry{}, err
	}
	hash, err := AuditHash(prev, ts, actor, role, action, target, norm)
	if err != nil {
		return AuditEntry{}, err
	}
	e := AuditEntry{TS: ts, ActorSub: actor, Role: role, Action: action, Target: target, Details: norm, PrevHash: prev, Hash: hash}
	err = tx.QueryRow(ctx, `INSERT INTO audit_log (ts, actor_sub, role, action, target, details, prev_hash, hash)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING seq`, ts, actor, role, action, target, string(raw), prev, hash).Scan(&e.Seq)
	return e, err
}

// VerifyAudit recomputes the whole chain and returns the number of verified entries.
func VerifyAudit(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	rows, err := pool.Query(ctx, `SELECT seq, ts, actor_sub, role, action, target, details, prev_hash, hash FROM audit_log ORDER BY seq`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	prev := ""
	n := 0
	for rows.Next() {
		var e AuditEntry
		var details []byte
		if err := rows.Scan(&e.Seq, &e.TS, &e.ActorSub, &e.Role, &e.Action, &e.Target, &details, &e.PrevHash, &e.Hash); err != nil {
			return n, err
		}
		if err := json.Unmarshal(details, &e.Details); err != nil {
			return n, err
		}
		if e.PrevHash != prev {
			return n, fmt.Errorf("audit chain broken at seq %d: prev_hash does not match the previous entry", e.Seq)
		}
		want, err := AuditHash(e.PrevHash, e.TS, e.ActorSub, e.Role, e.Action, e.Target, e.Details)
		if err != nil {
			return n, err
		}
		if want != e.Hash {
			return n, fmt.Errorf("audit chain broken at seq %d: hash mismatch", e.Seq)
		}
		prev = e.Hash
		n++
	}
	return n, rows.Err()
}
