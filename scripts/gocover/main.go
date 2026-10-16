// Command gocover enforces line-coverage floors on a Go cover profile (spec §10).
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

type counts struct{ covered, total int64 }

func main() {
	profile := flag.String("profile", "", "cover profile path")
	core := flag.String("core", "", "comma-separated package dirs that form the core")
	minCore := flag.Float64("min-core", 85, "minimum core coverage percent")
	minTotal := flag.Float64("min-total", 70, "minimum overall coverage percent")
	flag.Parse()
	byPkg, err := parse(*profile)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	ok := report(byPkg, strings.Split(*core, ","), *minCore, *minTotal)
	if !ok {
		os.Exit(1)
	}
}

const module = "github.com/ROBERTG99/fraud-scoring-service/"

func parse(path string) (map[string]*counts, error) {
	f, err := os.Open(path) // #nosec G304 -- path is a CLI argument
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	// Blocks can appear several times (one per test binary); keep the max hit count per block.
	seen := map[string]int64{}
	stmts := map[string]int64{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "mode:") || line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			continue
		}
		n, _ := strconv.ParseInt(fields[1], 10, 64)
		c, _ := strconv.ParseInt(fields[2], 10, 64)
		key := fields[0]
		stmts[key] = n
		if c > seen[key] {
			seen[key] = c
		} else if _, ok := seen[key]; !ok {
			seen[key] = c
		}
	}
	byPkg := map[string]*counts{}
	for key, n := range stmts {
		file := strings.SplitN(key, ":", 2)[0]
		file = strings.TrimPrefix(file, module)
		if strings.HasPrefix(file, "gen/") {
			continue
		}
		pkg := file[:strings.LastIndex(file, "/")]
		c := byPkg[pkg]
		if c == nil {
			c = &counts{}
			byPkg[pkg] = c
		}
		c.total += n
		if seen[key] > 0 {
			c.covered += n
		}
	}
	return byPkg, sc.Err()
}

func pct(c counts) float64 {
	if c.total == 0 {
		return 100
	}
	return 100 * float64(c.covered) / float64(c.total)
}

func report(byPkg map[string]*counts, core []string, minCore, minTotal float64) bool {
	var all, coreSum counts
	pkgs := make([]string, 0, len(byPkg))
	for p := range byPkg {
		pkgs = append(pkgs, p)
	}
	sort.Strings(pkgs)
	for _, p := range pkgs {
		c := byPkg[p]
		all.covered += c.covered
		all.total += c.total
		for _, prefix := range core {
			if prefix != "" && (p == prefix || strings.HasPrefix(p, prefix+"/")) {
				coreSum.covered += c.covered
				coreSum.total += c.total
			}
		}
		fmt.Printf("%-50s %6.1f%%\n", p, pct(*c))
	}
	fmt.Printf("core  %6.1f%% (min %.0f%%)\ntotal %6.1f%% (min %.0f%%)\n", pct(coreSum), minCore, pct(all), minTotal)
	return pct(coreSum) >= minCore && pct(all) >= minTotal
}
