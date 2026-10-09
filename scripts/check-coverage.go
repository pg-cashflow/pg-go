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
	pkg       string
	minCover  float64
}

var defaultThresholds = []threshold{
	{pkg: "github.com/pg-cashflow/pg-go/internal/finance", minCover: 85.0},
	{pkg: "github.com/pg-cashflow/pg-go/internal/payment", minCover: 85.0},
	{pkg: "github.com/pg-cashflow/pg-go/internal/auth", minCover: 80.0},
	{pkg: "github.com/pg-cashflow/pg-go/internal/api", minCover: 75.0},
}

type pkgStats struct {
	totalStmts   int64
	coveredStmts int64
}

func main() {
	profilePath := flag.String("profile", "coverage.out", "Path to coverage profile file")
	flag.Parse()

	f, err := os.Open(*profilePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening coverage profile %s: %v\n", *profilePath, err)
		os.Exit(1)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	stats := make(map[string]*pkgStats)

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
		// normalize backslashes to forward slashes for go package paths
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

	if err := scanner.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "Error reading coverage profile: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("==================================================")
	fmt.Println("       GATE 02: PACKAGE COVERAGE REPORT")
	fmt.Println("==================================================")

	hasFailure := false
	for _, t := range defaultThresholds {
		s, found := stats[t.pkg]
		if !found || s.totalStmts == 0 {
			fmt.Printf("FAIL: %-45s (Target: %.1f%%) - NO DATA OR 0 STATEMENTS\n", t.pkg, t.minCover)
			hasFailure = true
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

	if hasFailure {
		fmt.Println("Gate 02 Status: FAILED (Coverage floor violated)")
		os.Exit(1)
	}

	fmt.Println("Gate 02 Status: PASSED (All package coverage floors satisfied)")
}
