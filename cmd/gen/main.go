// Command gen writes the synthetic dataset: events.jsonl, labels.csv and manifest.json.
//
//	go run ./cmd/gen -size demo -seed 7 -out artifacts/data/demo
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

func main() {
	size := flag.String("size", "demo", "demo (200k events), eval (2M) or parity (5k)")
	seed := flag.Uint64("seed", 7, "random seed")
	out := flag.String("out", "", "output directory (default artifacts/data/<size>)")
	flag.Parse()
	if *out == "" {
		*out = filepath.Join("artifacts", "data", *size)
	}
	if err := run(*size, *seed, *out); err != nil {
		fmt.Fprintln(os.Stderr, "gen:", err)
		os.Exit(1)
	}
}

func run(size string, seed uint64, out string) error {
	start := time.Now()
	cfg, err := ConfigFor(size, seed)
	if err != nil {
		return err
	}
	ds, err := Generate(cfg)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(out, 0o750); err != nil {
		return err
	}
	evSHA, err := writeFile(filepath.Join(out, "events.jsonl"), ds.WriteEvents)
	if err != nil {
		return err
	}
	lbSHA, err := writeFile(filepath.Join(out, "labels.csv"), ds.WriteLabels)
	if err != nil {
		return err
	}
	fraud, splits := 0, map[string]int{}
	for _, l := range ds.Labels {
		if l.Fraud {
			fraud++
		}
		splits[l.Split]++
	}
	manifest := map[string]any{
		"gen_version": GenVersion, "seed": seed, "size": size, "events": len(ds.Events),
		"transactions": len(ds.Labels), "fraud_transactions": fraud,
		"fraud_rate": float64(fraud) / float64(len(ds.Labels)), "accounts": cfg.Accounts,
		"days": cfg.Days, "warmup_days": cfg.WarmupDays, "start": time.UnixMilli(cfg.StartMillis).UTC().Format(time.RFC3339),
		"split_counts": splits, "events_sha256": evSHA, "labels_sha256": lbSHA,
	}
	b, _ := json.MarshalIndent(manifest, "", "  ")
	if err := os.WriteFile(filepath.Join(out, "manifest.json"), append(b, '\n'), 0o600); err != nil {
		return err
	}
	fmt.Printf("gen: %d events, %d transactions (%.4f fraud) in %s -> %s (sha256 %s)\n",
		len(ds.Events), len(ds.Labels), float64(fraud)/float64(len(ds.Labels)), time.Since(start).Round(time.Millisecond), out, evSHA[:12])
	return nil
}

func writeFile(path string, write func(io.Writer) error) (string, error) {
	f, err := os.Create(path) // #nosec G304 -- output path from the CLI
	if err != nil {
		return "", err
	}
	h := sha256.New()
	if err := write(io.MultiWriter(f, h)); err != nil {
		_ = f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
