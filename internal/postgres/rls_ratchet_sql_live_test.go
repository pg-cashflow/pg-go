package postgres

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func runSQLScriptStrippingMetacommands(t *testing.T, relPath string) {
	t.Helper()
	pool, _ := setupLiveTestPool(t, 30*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	candidates := []string{
		filepath.Join("..", "..", relPath),
		filepath.Join(".", relPath),
	}
	var raw []byte
	var err error
	for _, c := range candidates {
		raw, err = os.ReadFile(c)
		if err == nil {
			break
		}
	}
	if err != nil {
		t.Fatalf("could not read SQL script %s: %v", relPath, err)
	}

	var cleanLines []string
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "\\") {
			continue // skip psql metacommands like \set
		}
		cleanLines = append(cleanLines, line)
	}

	cleanSQL := strings.Join(cleanLines, "\n")
	if _, err := pool.Exec(ctx, cleanSQL); err != nil {
		t.Fatalf("SQL script %s failed: %v", relPath, err)
	}
}

func TestLiveSQL_RLSCoverage(t *testing.T) {
	runSQLScriptStrippingMetacommands(t, filepath.Join("scripts", "sql", "rls_coverage_test.sql"))
}

func TestLiveSQL_RLSPropertyScopeRatchet(t *testing.T) {
	runSQLScriptStrippingMetacommands(t, filepath.Join("scripts", "sql", "rls_property_scope_ratchet.sql"))
}
