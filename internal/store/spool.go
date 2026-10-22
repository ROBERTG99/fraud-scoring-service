package store

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	fraudv1 "github.com/ROBERTG99/fraud-scoring-service/gen/go/fraud/v1"
	"github.com/ROBERTG99/fraud-scoring-service/internal/events"
)

// Spool is an append-only, fsync'd JSONL file of decisions that could not reach Postgres in time.
// Replay is idempotent: decisions are inserted with ON CONFLICT DO NOTHING.
type Spool struct {
	dir   string
	mu    sync.Mutex
	f     *os.File
	depth int
	// syncFile makes appended lines durable; nil means (*os.File).Sync. Tests wrap it to time the disk.
	syncFile func(*os.File) error
}

type spoolLine struct {
	Decision    json.RawMessage    `json:"decision"`
	AccountID   string             `json:"account_id"`
	Features    map[string]float64 `json:"features"`
	AmountMinor int64              `json:"amount_minor"`
	Currency    string             `json:"currency"`
	Tx          TxAttrs            `json:"tx"`
	Event       json.RawMessage    `json:"event,omitempty"`
	TraceParent string             `json:"traceparent,omitempty"`
	RequestHash []byte             `json:"request_sha256,omitempty"`
}

const activeSpool = "spool.jsonl"

// OpenSpool opens (or creates) the spool in dir and counts what is already waiting.
func OpenSpool(dir string) (*Spool, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	s := &Spool{dir: dir}
	n, err := s.countPending()
	if err != nil {
		return nil, err
	}
	s.depth = n
	return s, nil
}

func (s *Spool) countPending() (int, error) {
	files, err := s.pendingFiles(true)
	if err != nil {
		return 0, err
	}
	total := 0
	for _, f := range files {
		recs, err := readSpoolFile(f)
		if err != nil {
			return 0, err
		}
		total += len(recs)
	}
	return total, nil
}

// Depth is the number of spooled decisions not yet replayed.
func (s *Spool) Depth() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.depth
}

// Append writes records and fsyncs before returning.
func (s *Spool) Append(recs []DecisionRecord) error {
	var buf strings.Builder
	for i := range recs {
		line, err := encodeSpoolLine(&recs[i])
		if err != nil {
			return err
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		path := filepath.Join(s.dir, activeSpool)
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600) // #nosec G304 -- spool dir from config
		if err != nil {
			return err
		}
		// A crash mid-write can leave a torn last line; start on a fresh line so the next record stays readable.
		torn, err := endsTorn(path)
		if err == nil && torn {
			_, err = f.WriteString("\n")
		}
		if err != nil {
			_ = f.Close()
			return err
		}
		s.f = f
	}
	if _, err := s.f.WriteString(buf.String()); err != nil {
		return err
	}
	syncFn := (*os.File).Sync
	if s.syncFile != nil {
		syncFn = s.syncFile
	}
	if err := syncFn(s.f); err != nil {
		return err
	}
	s.depth += len(recs)
	return nil
}

// endsTorn reports whether the file at path is non-empty and does not end with a newline.
func endsTorn(path string) (bool, error) {
	f, err := os.Open(path) // #nosec G304 -- spool dir from config
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil || st.Size() == 0 {
		return false, err
	}
	last := make([]byte, 1)
	if _, err := f.ReadAt(last, st.Size()-1); err != nil {
		return false, err
	}
	return last[0] != '\n', nil
}

// Drain rotates the active file and hands every pending record to commit; files are deleted only after
// commit succeeds, so a crash during replay replays again (idempotently).
func (s *Spool) Drain(commit func([]DecisionRecord) error) (int, error) {
	s.mu.Lock()
	if s.f != nil {
		_ = s.f.Close()
		s.f = nil
	}
	// Rotate the active file even when this process never opened it: a scorer killed while Postgres was down
	// leaves its fsync'd decisions there, and the restarted process must replay them (D-075).
	active := filepath.Join(s.dir, activeSpool)
	if _, err := os.Stat(active); err == nil {
		rotated := filepath.Join(s.dir, fmt.Sprintf("spool.%d.replay", time.Now().UnixNano()))
		if err := os.Rename(active, rotated); err != nil {
			s.mu.Unlock()
			return 0, err
		}
	}
	s.mu.Unlock()
	files, err := s.pendingFiles(false)
	if err != nil {
		return 0, err
	}
	done := 0
	for _, f := range files {
		recs, err := readSpoolFile(f)
		if err != nil {
			return done, err
		}
		for start := 0; start < len(recs); start += 256 {
			if err := commit(recs[start:min(start+256, len(recs))]); err != nil {
				return done, err
			}
		}
		if err := os.Remove(f); err != nil {
			return done, err
		}
		done += len(recs)
		s.mu.Lock()
		s.depth = max(0, s.depth-len(recs))
		s.mu.Unlock()
	}
	return done, nil
}

// pendingFiles lists rotated files (and the active file when includeActive).
func (s *Spool) pendingFiles(includeActive bool) ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".replay") || (name == activeSpool && includeActive) {
			out = append(out, filepath.Join(s.dir, name))
		}
	}
	sort.Strings(out)
	return out, nil
}

// Close closes the active file.
func (s *Spool) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f != nil {
		err := s.f.Close()
		s.f = nil
		return err
	}
	return nil
}

func encodeSpoolLine(r *DecisionRecord) ([]byte, error) {
	d, err := protojson.Marshal(r.Decision)
	if err != nil {
		return nil, err
	}
	line := spoolLine{Decision: d, AccountID: r.AccountID, Features: r.Features, AmountMinor: r.AmountMinor, Currency: r.Currency, Tx: r.Tx, TraceParent: r.TraceParent, RequestHash: r.RequestHash}
	if r.Event != nil {
		line.Event = r.Event.AppendJSON(nil)
	}
	return json.Marshal(line)
}

func readSpoolFile(path string) ([]DecisionRecord, error) {
	f, err := os.Open(path) // #nosec G304 -- spool dir from config
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var out []DecisionRecord
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var line spoolLine
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			// A torn last line (crash mid-write) is skipped; earlier lines were fsync'd.
			continue
		}
		var d fraudv1.Decision
		if err := protojson.Unmarshal(line.Decision, &d); err != nil {
			continue
		}
		rec := DecisionRecord{Decision: &d, AccountID: line.AccountID, Features: line.Features, AmountMinor: line.AmountMinor, Currency: line.Currency, Tx: line.Tx, TraceParent: line.TraceParent, RequestHash: line.RequestHash}
		if len(line.Event) > 0 {
			var ev events.Event
			if err := json.Unmarshal(line.Event, &ev); err == nil {
				rec.Event = &ev
			}
		}
		out = append(out, rec)
	}
	return out, sc.Err()
}
