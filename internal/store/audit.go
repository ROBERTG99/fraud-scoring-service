package store

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// auditLockKey serialises appends so the hash chain never forks (also enforced by UNIQUE(prev_hash)).
const auditLockKey = 724411

// Audit hash algorithms (audit_log.alg, migration 00010, D-050).
const (
	// AuditAlgSHA256 is the first chain: unkeyed sha256 of '|'-joined fields. Verified only before the first
	// keyed entry, so nobody can append unkeyed rows to a keyed chain.
	AuditAlgSHA256 = 1
	// AuditAlgHMAC is HMAC-SHA256, under a key held outside the database, of length-prefixed fields.
	AuditAlgHMAC = 2
)

// MinAuditKeyBytes is the shortest accepted HMAC key.
const MinAuditKeyBytes = 32

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
	Alg      int
}

// AuditHash is the AuditAlgSHA256 hash: sha256(prev | ts | actor | role | action | target | details JSON |).
// Fields may contain '|', so two different entries can hash alike; kept only to verify old rows.
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

// AuditMAC is the AuditAlgHMAC hash: HMAC-SHA256(key, "fraud.audit.v2" and each field, every one prefixed by
// its length as a big-endian uint64), so field boundaries are unambiguous.
func AuditMAC(key []byte, prev string, ts time.Time, actor, role, action, target string, details map[string]any) (string, error) {
	canon, err := json.Marshal(details)
	if err != nil {
		return "", err
	}
	return mac(key, "fraud.audit.v2", prev, ts.UTC().Format(time.RFC3339Nano), actor, role, action, target, string(canon)), nil
}

func mac(key []byte, parts ...string) string {
	h := hmac.New(sha256.New, key)
	var n [8]byte
	for _, p := range parts {
		binary.BigEndian.PutUint64(n[:], uint64(len(p)))
		h.Write(n[:])
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func checkKey(key []byte) error {
	if len(key) < MinAuditKeyBytes {
		return fmt.Errorf("audit key: %d bytes, need at least %d", len(key), MinAuditKeyBytes)
	}
	return nil
}

// AppendAudit appends a keyed, hash-chained entry inside tx.
func AppendAudit(ctx context.Context, tx pgx.Tx, key []byte, actor, role, action, target string, details map[string]any) (AuditEntry, error) {
	if err := checkKey(key); err != nil {
		return AuditEntry{}, err
	}
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
	hash, err := AuditMAC(key, prev, ts, actor, role, action, target, norm)
	if err != nil {
		return AuditEntry{}, err
	}
	e := AuditEntry{TS: ts, ActorSub: actor, Role: role, Action: action, Target: target, Details: norm, PrevHash: prev, Hash: hash, Alg: AuditAlgHMAC}
	err = tx.QueryRow(ctx, `INSERT INTO audit_log (ts, actor_sub, role, action, target, details, prev_hash, hash, alg)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING seq`, ts, actor, role, action, target, string(raw), prev, hash, AuditAlgHMAC).Scan(&e.Seq)
	return e, err
}

// AuditAnchor records the chain head outside the database, so cutting off the newest entries is detected
// (`scorer audit-anchor` appends one per run; `scorer audit-verify -anchors` checks them all). MAC binds the
// anchor to the audit key.
type AuditAnchor struct {
	Seq  int64     `json:"seq"`
	Hash string    `json:"hash"`
	At   time.Time `json:"at"`
	MAC  string    `json:"mac"`
}

func anchorMAC(key []byte, seq int64, hash string, at time.Time) string {
	return mac(key, "fraud.audit.anchor.v1", strconv.FormatInt(seq, 10), hash, at.UTC().Format(time.RFC3339Nano))
}

// NewAuditAnchor returns a signed anchor for the head entry.
func NewAuditAnchor(key []byte, head AuditEntry, at time.Time) AuditAnchor {
	at = at.UTC()
	return AuditAnchor{Seq: head.Seq, Hash: head.Hash, At: at, MAC: anchorMAC(key, head.Seq, head.Hash, at)}
}

// ReadAuditAnchors parses one JSON anchor per line.
func ReadAuditAnchors(r io.Reader) ([]AuditAnchor, error) {
	var out []AuditAnchor
	sc := bufio.NewScanner(r)
	for line := 1; sc.Scan(); line++ {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var a AuditAnchor
		if err := json.Unmarshal(sc.Bytes(), &a); err != nil {
			return nil, fmt.Errorf("anchor line %d: %w", line, err)
		}
		out = append(out, a)
	}
	return out, sc.Err()
}

// VerifyAudit recomputes the whole chain under key and checks every anchor against it. It returns the
// number of verified entries and the head (the zero entry when the log is empty).
func VerifyAudit(ctx context.Context, pool *pgxpool.Pool, key []byte, anchors []AuditAnchor) (int, AuditEntry, error) {
	if err := checkKey(key); err != nil {
		return 0, AuditEntry{}, err
	}
	want := map[int64]string{}
	var maxAnchored int64
	for _, a := range anchors {
		if !hmac.Equal([]byte(a.MAC), []byte(anchorMAC(key, a.Seq, a.Hash, a.At))) {
			return 0, AuditEntry{}, fmt.Errorf("anchor for seq %d at %s: MAC does not match the audit key", a.Seq, a.At.Format(time.RFC3339))
		}
		want[a.Seq] = a.Hash
		maxAnchored = max(maxAnchored, a.Seq)
	}
	rows, err := pool.Query(ctx, `SELECT seq, ts, actor_sub, role, action, target, details, prev_hash, hash, alg FROM audit_log ORDER BY seq`)
	if err != nil {
		return 0, AuditEntry{}, err
	}
	defer rows.Close()
	var head AuditEntry
	keyed := false
	n := 0
	for rows.Next() {
		var e AuditEntry
		var details []byte
		if err := rows.Scan(&e.Seq, &e.TS, &e.ActorSub, &e.Role, &e.Action, &e.Target, &details, &e.PrevHash, &e.Hash, &e.Alg); err != nil {
			return n, head, err
		}
		if err := json.Unmarshal(details, &e.Details); err != nil {
			return n, head, err
		}
		if e.PrevHash != head.Hash {
			return n, head, fmt.Errorf("audit chain broken at seq %d: prev_hash does not match the previous entry", e.Seq)
		}
		var hash string
		switch {
		case e.Alg == AuditAlgHMAC:
			keyed = true
			hash, err = AuditMAC(key, e.PrevHash, e.TS, e.ActorSub, e.Role, e.Action, e.Target, e.Details)
		case e.Alg == AuditAlgSHA256 && !keyed:
			hash, err = AuditHash(e.PrevHash, e.TS, e.ActorSub, e.Role, e.Action, e.Target, e.Details)
		case e.Alg == AuditAlgSHA256:
			return n, head, fmt.Errorf("audit chain broken at seq %d: unkeyed entry after keyed entries", e.Seq)
		default:
			return n, head, fmt.Errorf("audit chain broken at seq %d: unknown alg %d", e.Seq, e.Alg)
		}
		if err != nil {
			return n, head, err
		}
		if !hmac.Equal([]byte(hash), []byte(e.Hash)) {
			return n, head, fmt.Errorf("audit chain broken at seq %d: hash mismatch", e.Seq)
		}
		if h, ok := want[e.Seq]; ok {
			if h != e.Hash {
				return n, head, fmt.Errorf("audit chain broken at seq %d: hash differs from its anchor", e.Seq)
			}
			delete(want, e.Seq)
		}
		head = e
		n++
	}
	if err := rows.Err(); err != nil {
		return n, head, err
	}
	if maxAnchored > head.Seq {
		return n, head, fmt.Errorf("audit chain ends at seq %d but seq %d was anchored: entries were removed", head.Seq, maxAnchored)
	}
	for seq := range want {
		return n, head, fmt.Errorf("anchored seq %d is missing from the chain", seq)
	}
	return n, head, nil
}
