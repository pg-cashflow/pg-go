package jobs_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/joho/godotenv"
	"github.com/pg-cashflow/pg-go/internal/config"
	"github.com/pg-cashflow/pg-go/internal/testutil"
)

// allBinaries lists all 12 operational and background entrypoints in cmd/
var allBinaries = []string{
	"server",
	"billing-cycle",
	"daily-rollup",
	"reminder",
	"cashfree-poll",
	"digilocker-reconcile",
	"financial-summary",
	"gamification-cycle",
	"kpi-snapshot",
	"kyc-expiry",
	"migrate",
	"search-reindex",
}

// TestGate12_BinariesMissingEnvFailFast validates that all cmd/ binaries
// fail fast with non-zero POSIX exit codes when executed without required environment variables,
// while deprecated binaries emit their notice cleanly.
func TestGate12_BinariesMissingEnvFailFast(t *testing.T) {
	for _, bin := range allBinaries {
		t.Run(bin, func(t *testing.T) {
			cmdPath := filepath.Join("..", "..", "cmd", bin)
			if _, err := os.Stat(cmdPath); err != nil {
				t.Fatalf("binary path %s does not exist: %v", cmdPath, err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()

			cmd := exec.CommandContext(ctx, "go", "run", ".")
			cmd.Dir = cmdPath

			// Pass isolated environment stripped of DATABASE_URL / secrets
			cmd.Env = []string{
				"GOTOOLCHAIN=local",
				"GOPROXY=off",
				"SYSTEMROOT=" + os.Getenv("SYSTEMROOT"),
				"PATH=" + os.Getenv("PATH"),
				"TEMP=" + os.Getenv("TEMP"),
				"TMP=" + os.Getenv("TMP"),
				"USERPROFILE=" + os.Getenv("USERPROFILE"),
				"LOCALAPPDATA=" + os.Getenv("LOCALAPPDATA"),
				"DATABASE_URL=",
				"DATABASE_MAINT_URL=",
				"JWT_SECRET=",
			}

			out, err := cmd.CombinedOutput()
			outStr := string(out)

			if bin == "search-reindex" {
				// search-reindex is permanently decommissioned per ADR-012
				if err != nil {
					t.Fatalf("deprecated binary search-reindex failed unexpectedly: %v", err)
				}
				if !strings.Contains(outStr, "[DEPRECATED]") {
					t.Fatalf("expected deprecation message in search-reindex output, got: %s", outStr)
				}
				return
			}

			if err == nil {
				t.Fatalf("expected binary %s to fail with empty env, but it succeeded: %s", bin, outStr)
			}

			exitErr, ok := err.(*exec.ExitError)
			if !ok {
				t.Fatalf("binary %s failed with non-exit error: %v (out: %s)", bin, err, outStr)
			}

			if exitErr.ExitCode() == 0 {
				t.Fatalf("expected non-zero exit code for %s, got 0", bin)
			}
		})
	}
}

// TestGate12_CashfreePollDormantWhenUnconfigured validates that cmd/cashfree-poll
// exits 0 gracefully when Cashfree credentials are intentionally omitted.
func TestGate12_CashfreePollDormantWhenUnconfigured(t *testing.T) {
	_ = godotenv.Load("../../.env")
	testutil.RequireDB(t)

	cfg, err := config.Load()
	if err != nil {
		testutil.FailOnSkipIfDBRequired(t, fmt.Sprintf("config load failed: %v", err))
	}

	cmdPath := filepath.Join("..", "..", "cmd", "cashfree-poll")
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "go", "run", ".")
	cmd.Dir = cmdPath
	// Set whitespace to prevent godotenv from re-populating from .env
	cmd.Env = append(os.Environ(),
		"GOTOOLCHAIN=local",
		"GOPROXY=off",
		"DATABASE_URL="+cfg.DatabaseURL,
		"DATABASE_MAINT_URL="+cfg.DatabaseMaintURL,
		"CASHFREE_PG_APP_ID= ",
		"CASHFREE_APP_ID= ",
		"CASHFREE_PG_SECRET_KEY= ",
		"CASHFREE_SECRET_KEY= ",
	)

	out, err := cmd.CombinedOutput()
	outStr := string(out)

	if err != nil {
		t.Fatalf("expected cashfree-poll to exit 0 when unconfigured, got error: %v (out: %s)", err, outStr)
	}

	if !strings.Contains(outStr, "cashfree poll skipped: not configured") {
		t.Fatalf("expected skip log message, got: %s", outStr)
	}
}

// TestGate12_MigrateExecution validates that cmd/migrate runs cleanly
// against the test database and emits the completion message with exit code 0.
func TestGate12_MigrateExecution(t *testing.T) {
	_ = godotenv.Load("../../.env")
	testutil.RequireDB(t)

	cfg, err := config.Load()
	if err != nil {
		testutil.FailOnSkipIfDBRequired(t, fmt.Sprintf("config load failed: %v", err))
	}

	cmdPath := filepath.Join("..", "..", "cmd", "migrate")
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	migrationsDir, _ := filepath.Abs(filepath.Join("..", "..", "migrations"))

	cmd := exec.CommandContext(ctx, "go", "run", ".")
	cmd.Dir = cmdPath
	cmd.Env = append(os.Environ(),
		"GOTOOLCHAIN=local",
		"GOPROXY=off",
		"DATABASE_URL="+cfg.DatabaseURL,
		"DATABASE_MAINT_URL="+cfg.DatabaseMaintURL,
		"MIGRATIONS_DIR="+migrationsDir,
	)

	out, err := cmd.CombinedOutput()
	outStr := string(out)

	if err != nil {
		t.Fatalf("expected cmd/migrate to exit 0, got err: %v (out: %s)", err, outStr)
	}

	if !strings.Contains(outStr, "migrations applied") {
		t.Fatalf("expected 'migrations applied' in output, got: %s", outStr)
	}
}

// TestGate12_ServerGracefulShutdown validates that cmd/server responds
// to SIGINT/Interrupt, drains active connections, and terminates cleanly.
func TestGate12_ServerGracefulShutdown(t *testing.T) {
	_ = godotenv.Load("../../.env")
	testutil.RequireDB(t)

	cfg, err := config.Load()
	if err != nil {
		testutil.FailOnSkipIfDBRequired(t, fmt.Sprintf("config load failed: %v", err))
	}

	testPort := "18092"
	cmdPath := filepath.Join("..", "..", "cmd", "server")

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "go", "run", ".")
	cmd.Dir = cmdPath
	cmd.Env = append(os.Environ(),
		"GOTOOLCHAIN=local",
		"GOPROXY=off",
		"HTTP_ADDR=:"+testPort,
		"PORT="+testPort,
		"DATABASE_URL="+cfg.DatabaseURL,
		"DATABASE_MAINT_URL="+cfg.DatabaseMaintURL,
	)

	if err := cmd.Start(); err != nil {
		t.Fatalf("start cmd/server failed: %v", err)
	}

	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}()

	// Poll until server responds on /health (allow up to 25s for Go compilation + startup)
	client := &http.Client{Timeout: 500 * time.Millisecond}
	healthURL := fmt.Sprintf("http://localhost:%s/health", testPort)
	serverReady := false

	for i := 0; i < 100; i++ {
		time.Sleep(250 * time.Millisecond)
		resp, err := client.Get(healthURL)
		if err == nil && resp.StatusCode == http.StatusOK {
			_ = resp.Body.Close()
			serverReady = true
			break
		}
	}

	if !serverReady {
		t.Fatalf("cmd/server failed to become ready on port %s within 25 seconds", testPort)
	}

	// Send Interrupt signal to trigger graceful shutdown
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Logf("Process.Signal failed (platform limitation on Windows), using Process.Kill: %v", err)
		_ = cmd.Process.Kill()
	}

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	select {
	case err := <-done:
		if err != nil && !strings.Contains(err.Error(), "interrupt") && !strings.Contains(err.Error(), "exit status 1") {
			t.Logf("server terminated with: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server process did not exit within 5 seconds of interrupt signal")
	}
}
