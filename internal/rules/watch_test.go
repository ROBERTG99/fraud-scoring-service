//go:build integration

package rules

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/ROBERTG99/fraud-scoring-service/internal/testutil"
)

func TestWatchReloadsOnPubSub(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: testutil.RedisAddr(t)})
	defer rdb.Close()
	dir := t.TempDir()
	writeRules(t, dir, "2026.10.1", validYAML)
	eng, err := NewEngine(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go eng.Watch(ctx, time.Hour, rdb) // polling effectively off: only pub/sub can trigger the reload
	writeRules(t, dir, "2026.10.4", strings.Replace(validYAML, "2026.10.1", "2026.10.4", 1))
	deadline := time.Now().Add(5 * time.Second)
	for eng.Active().Version != "2026.10.4" && time.Now().Before(deadline) {
		_ = rdb.Publish(ctx, ChangedChannel, "2026.10.4").Err()
		time.Sleep(20 * time.Millisecond)
	}
	if eng.Active().Version != "2026.10.4" {
		t.Fatalf("pub/sub did not reload: %s", eng.Active().Version)
	}
}
