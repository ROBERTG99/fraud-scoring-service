package rules

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// ChangedChannel is the Redis pub/sub channel announcing a new rule-set version.
const ChangedChannel = "fr:rules:changed"

var fileRE = regexp.MustCompile(`^rules\.v(\d{4}\.\d{1,2}\.\d{1,3})\.yaml$`)

// Engine holds the active rule set and reloads it when the rules directory changes.
// The active set is swapped atomically; an invalid file never replaces a valid set.
type Engine struct {
	dir      string
	log      *slog.Logger
	active   atomic.Pointer[RuleSet]
	lastSeen atomic.Value // string: fingerprint of the directory
	onLoad   func(*RuleSet)
}

// NewEngine loads the highest-versioned rules.v*.yaml in dir.
func NewEngine(dir string, log *slog.Logger) (*Engine, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	e := &Engine{dir: dir, log: log}
	if err := e.Reload(); err != nil {
		return nil, err
	}
	return e, nil
}

// OnLoad registers a callback run after each successful (re)load.
func (e *Engine) OnLoad(f func(*RuleSet)) {
	e.onLoad = f
	if rs := e.Active(); rs != nil && f != nil {
		f(rs)
	}
}

// Active returns the current rule set (never nil after NewEngine succeeds).
func (e *Engine) Active() *RuleSet { return e.active.Load() }

func versionLess(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) && i < len(pb); i++ {
		x, _ := strconv.Atoi(pa[i])
		y, _ := strconv.Atoi(pb[i])
		if x != y {
			return x < y
		}
	}
	return len(pa) < len(pb)
}

func (e *Engine) latestFile() (string, string, error) {
	entries, err := os.ReadDir(e.dir)
	if err != nil {
		return "", "", err
	}
	var versions []string
	byVersion := map[string]string{}
	var fp strings.Builder
	for _, ent := range entries {
		m := fileRE.FindStringSubmatch(ent.Name())
		if m == nil {
			continue
		}
		info, err := ent.Info()
		if err != nil {
			continue
		}
		fmt.Fprintf(&fp, "%s:%d:%d;", ent.Name(), info.Size(), info.ModTime().UnixNano())
		versions = append(versions, m[1])
		byVersion[m[1]] = filepath.Join(e.dir, ent.Name())
	}
	if len(versions) == 0 {
		return "", fp.String(), errors.New("rules: no rules.v*.yaml in " + e.dir)
	}
	sort.Slice(versions, func(i, j int) bool { return versionLess(versions[i], versions[j]) })
	return byVersion[versions[len(versions)-1]], fp.String(), nil
}

// Reload parses the highest-versioned file; on any error the active set is kept and the error returned.
func (e *Engine) Reload() error {
	path, fp, err := e.latestFile()
	e.lastSeen.Store(fp)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(path) // #nosec G304 -- operator-controlled rules directory
	if err != nil {
		return err
	}
	rs, err := Parse(raw)
	if err != nil {
		e.log.Warn("rule set rejected; keeping previous version", "file", path, "err", err)
		return err
	}
	if cur := e.Active(); cur != nil && cur.SHA256 == rs.SHA256 {
		return nil
	}
	e.active.Store(rs)
	e.log.Info("rule set active", "version", rs.Version, "rules", len(rs.Rules), "sha256", rs.SHA256[:12])
	if e.onLoad != nil {
		e.onLoad(rs)
	}
	return nil
}

// Watch polls the directory every interval and, when sub is non-nil, reloads on fr:rules:changed.
func (e *Engine) Watch(ctx context.Context, interval time.Duration, sub *redis.Client) {
	var msgs <-chan *redis.Message
	if sub != nil {
		ps := sub.Subscribe(ctx, ChangedChannel)
		defer ps.Close()
		msgs = ps.Channel()
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_, fp, _ := e.latestFile()
			if prev, _ := e.lastSeen.Load().(string); fp != prev {
				_ = e.Reload()
			}
		case m, ok := <-msgs:
			if !ok {
				msgs = nil
				continue
			}
			e.log.Info("rules change announced", "payload", m.Payload)
			_ = e.Reload()
		}
	}
}
