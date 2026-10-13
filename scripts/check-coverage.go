//go:build ignore

package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type threshold struct {
	pkg      string
	minCover float64
}

var aspirationalThresholds = []threshold{
	{pkg: "github.com/pg-cashflow/pg-go/internal/finance", minCover: 85.0},
	{pkg: "github.com/pg-cashflow/pg-go/internal/payment", minCover: 85.0},
	{pkg: "github.com/pg-cashflow/pg-go/internal/auth", minCover: 80.0},
	{pkg: "github.com/pg-cashflow/pg-go/internal/api", minCover: 75.0},
}

var regressionFloors = []threshold{
	{pkg: "github.com/pg-cashflow/pg-go/internal/finance", minCover: 76.0},
	{pkg: "github.com/pg-cashflow/pg-go/internal/auth", minCover: 64.0},
	{pkg: "github.com/pg-cashflow/pg-go/internal/payment", minCover: 52.4},
	{pkg: "github.com/pg-cashflow/pg-go/internal/api", minCover: 62.0},
	{pkg: "github.com/pg-cashflow/pg-go/internal/jobs", minCover: 60.0},
	{pkg: "github.com/pg-cashflow/pg-go/internal/gamification", minCover: 35.0},
	{pkg: "github.com/pg-cashflow/pg-go/internal/postgres", minCover: 35.0},
	{pkg: "github.com/pg-cashflow/pg-go/internal/notification", minCover: 14.0},
	{pkg: "github.com/pg-cashflow/pg-go/internal/tenant", minCover: 10.0},
	{pkg: "github.com/pg-cashflow/pg-go/internal/billing", minCover: 46.1},
	{pkg: "github.com/pg-cashflow/pg-go/internal/cashfree", minCover: 63.9},
	{pkg: "github.com/pg-cashflow/pg-go/internal/kyc", minCover: 57.0},
	{pkg: "github.com/pg-cashflow/pg-go/internal/search", minCover: 74.7},
	{pkg: "github.com/pg-cashflow/pg-go/internal/attendance", minCover: 37.5},
	{pkg: "github.com/pg-cashflow/pg-go/internal/crypto", minCover: 77.0},
	{pkg: "github.com/pg-cashflow/pg-go/internal/domain", minCover: 31.3},
	{pkg: "github.com/pg-cashflow/pg-go/internal/collector", minCover: 27.4},
	{pkg: "github.com/pg-cashflow/pg-go/internal/csv", minCover: 86.0},
	{pkg: "github.com/pg-cashflow/pg-go/internal/aadhaar", minCover: 83.0},
	{pkg: "github.com/pg-cashflow/pg-go/internal/magiclink", minCover: 65.1},
	{pkg: "github.com/pg-cashflow/pg-go/internal/join", minCover: 45.5},
}

type pkgStats struct {
	totalStmts   int64
	coveredStmts int64
}

func main() {
	profilePath := flag.String("profile", "coverage.out", "Path to coverage profile file (comma-separated list supported)")
	strict := flag.Bool("strict", false, "Enforce aspirational coverage targets rather than regression floors")
	requireAll := flag.Bool("require-all", false, "Fail if any tracked package is missing from coverage profiles")
	reportAspirational := flag.Bool("report-aspirational", false, "Report gaps to aspirational coverage targets (non-blocking)")
	flag.Parse()

	thresholds := regressionFloors
	if *strict {
		thresholds = aspirationalThresholds
	}

	paths := strings.Split(*profilePath, ",")
	stats := make(map[string]*pkgStats)

	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		f, err := os.Open(p)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error opening coverage profile %s: %v\n", p, err)
			os.Exit(1)
		}

		scanner := bufio.NewScanner(f)
		lineNum := 0
		for scanner.Scan() {
			lineNum++
			line := strings.TrimSpace(scanner.Text())
			if lineNum == 1 && strings.HasPrefix(line, "mode:") {
				continue
			}
			if line == "" {
				continue
			}

			// Format: file.go:startLine.col,endLine.col numStatements count
			fields := strings.Fields(line)
			if len(fields) != 3 {
				continue
			}

			filePart := fields[0]
			colonIdx := strings.LastIndex(filePart, ":")
			if colonIdx == -1 {
				continue
			}
			filePath := filePart[:colonIdx]
			pkgDir := filepath.Dir(filePath)
			pkgDir = strings.ReplaceAll(pkgDir, "\\", "/")

			numStmts, err1 := strconv.ParseInt(fields[1], 10, 64)
			count, err2 := strconv.ParseInt(fields[2], 10, 64)
			if err1 != nil || err2 != nil {
				continue
			}

			s, ok := stats[pkgDir]
			if !ok {
				s = &pkgStats{}
				stats[pkgDir] = s
			}

			s.totalStmts += numStmts
			if count > 0 {
				s.coveredStmts += numStmts
			}
		}
		_ = f.Close()

		if err := scanner.Err(); err != nil {
			fmt.Fprintf(os.Stderr, "Error reading coverage profile %s: %v\n", p, err)
			os.Exit(1)
		}
	}

	fmt.Println("==================================================")
	fmt.Println("       GATE 02: PACKAGE COVERAGE REPORT")
	fmt.Println("==================================================")

	hasFailure := false
	for _, t := range thresholds {
		s, found := stats[t.pkg]
		if !found || s.totalStmts == 0 {
			if *requireAll {
				fmt.Printf("FAIL: %-45s (Target: %.1f%%) - NO DATA OR 0 STATEMENTS\n", t.pkg, t.minCover)
				hasFailure = true
			} else {
				fmt.Printf("SKIP: %-45s (Target: %.1f%%) - Not present in profile\n", t.pkg, t.minCover)
			}
			continue
		}

		pct := (float64(s.coveredStmts) / float64(s.totalStmts)) * 100.0
		if pct < t.minCover {
			fmt.Printf("FAIL: %-45s Actual: %5.1f%% < Floor: %5.1f%% (%d/%d stmts)\n",
				t.pkg, pct, t.minCover, s.coveredStmts, s.totalStmts)
			hasFailure = true
		} else {
			fmt.Printf("PASS: %-45s Actual: %5.1f%% >= Floor: %5.1f%% (%d/%d stmts)\n",
				t.pkg, pct, t.minCover, s.coveredStmts, s.totalStmts)
		}
	}
	fmt.Println("==================================================")

	if *reportAspirational {
		fmt.Println("==================================================")
		fmt.Println("    ASPIRATIONAL COVERAGE TARGETS & GAPS (NON-BLOCKING)")
		fmt.Println("==================================================")
		for _, a := range aspirationalThresholds {
			shortPkg := a.pkg[strings.LastIndex(a.pkg, "/")+1:]
			s, found := stats[a.pkg]
			if !found || s.totalStmts == 0 {
				fmt.Printf("ASPIRATIONAL: %-10s Target: %5.1f%% - NO DATA\n", shortPkg, a.minCover)
				continue
			}
			pct := (float64(s.coveredStmts) / float64(s.totalStmts)) * 100.0
			gap := a.minCover - pct
			if gap < 0 {
				gap = 0
			}
			fmt.Printf("ASPIRATIONAL: %-10s Actual: %5.1f%%, Target: %5.1f%%, Gap: %4.1f points\n",
				shortPkg, pct, a.minCover, gap)
		}
		fmt.Println("==================================================")
	}

	if hasFailure {
		fmt.Println("Gate 02 Status: FAILED (Coverage floor violated)")
		os.Exit(1)
	}

	fmt.Println("Gate 02 Status: PASSED (All package coverage floors satisfied)")
}
