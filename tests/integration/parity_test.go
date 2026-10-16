//go:build integration

// Package integration holds cross-component tests that need Redis, Postgres or Kafka.
package integration

import (
	"bufio"
	"context"
	"encoding/csv"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/ROBERTG99/fraud-scoring-service/internal/events"
	"github.com/ROBERTG99/fraud-scoring-service/internal/features"
	"github.com/ROBERTG99/fraud-scoring-service/internal/testutil"
)

func repoPath(parts ...string) string {
	return filepath.Join(append([]string{"..", ".."}, parts...)...)
}

func readOffline(t *testing.T, path string) (map[string][]float64, []string) {
	t.Helper()
	f, err := os.Open(path) // #nosec G304 -- test fixture path
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]float64{}
	for _, row := range rows[1:] {
		vals := make([]float64, len(row)-1)
		for i, v := range row[1:] {
			vals[i], err = strconv.ParseFloat(v, 64)
			if err != nil {
				t.Fatal(err)
			}
		}
		out[row[0]] = vals
	}
	return out, rows[0][1:]
}

// TestOnlineOfflineParity replays the 5,000-event parity fixture through the Go online path (Redis Lua,
// in event order) and requires every transaction's features to equal the Python offline values exactly.
func TestOnlineOfflineParity(t *testing.T) {
	offlinePath := os.Getenv("FRAUD_PARITY_OFFLINE")
	if offlinePath == "" {
		offlinePath = repoPath("tests", "fixtures", "parity", "offline_features.csv")
	}
	offline, names := readOffline(t, offlinePath)
	if len(names) != features.NumFeatures {
		t.Fatalf("offline file has %d features", len(names))
	}
	for i, n := range features.Names() {
		if names[i] != n {
			t.Fatalf("feature %d: offline %s, online %s", i, names[i], n)
		}
	}

	rdb := redis.NewClient(&redis.Options{Addr: testutil.RedisAddr(t)})
	defer func() { _ = rdb.Close() }()
	store := features.NewStore(rdb, testutil.UniqueName("parity"), nil)
	f, err := os.Open(repoPath("tests", "fixtures", "parity", "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	online := map[string]features.Vector{}
	var order []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	nEvents := 0
	for sc.Scan() {
		var e events.Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		nEvents++
		if e.Kind == events.KindTransaction {
			v, _, err := store.ScoreTx(ctx, features.TxFromEvent(&e))
			if err != nil {
				t.Fatal(err)
			}
			online[e.EventID] = v
			order = append(order, e.EventID)
			continue
		}
		if err := store.ApplyEvent(ctx, &e); err != nil {
			t.Fatal(err)
		}
	}
	if nEvents != 5000 {
		t.Fatalf("fixture has %d events, want 5000", nEvents)
	}
	if len(online) != len(offline) {
		t.Fatalf("online %d transactions, offline %d", len(online), len(offline))
	}
	mismatches := 0
	for id, v := range online {
		want, ok := offline[id]
		if !ok {
			t.Fatalf("transaction %s missing offline", id)
		}
		for i := range v {
			if v[i] != want[i] {
				mismatches++
				if mismatches <= 10 {
					t.Errorf("%s %s: online %v offline %v", id, names[i], v[i], want[i])
				}
			}
		}
	}
	if out := os.Getenv("FRAUD_PARITY_ONLINE_OUT"); out != "" {
		writeOnline(t, out, names, order, online)
	}
	t.Logf("parity: %d transactions x %d features, %d mismatches", len(online), features.NumFeatures, mismatches)
	if mismatches > 0 {
		t.Fatalf("%d feature mismatches", mismatches)
	}
}

func writeOnline(t *testing.T, path string, names, order []string, online map[string]features.Vector) {
	t.Helper()
	f, err := os.Create(path) // #nosec G304 -- path from the parity script
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	w := csv.NewWriter(f)
	_ = w.Write(append([]string{"transaction_id"}, names...))
	for _, id := range order {
		v := online[id]
		row := []string{id}
		for _, x := range v {
			row = append(row, strconv.FormatFloat(x, 'g', -1, 64))
		}
		_ = w.Write(row)
	}
	w.Flush()
	if err := w.Error(); err != nil {
		t.Fatal(err)
	}
}
