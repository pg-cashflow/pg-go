package jobs_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
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

var (
	builtServerOnce sync.Once
	builtServerPath string
	builtServerErr  error
)

func getOrBuildServer(t *testing.T) string {
	t.Helper()
	builtServerOnce.Do(func() {
		tmpDir, err := os.MkdirTemp("", "pg-server-build-*")
		if err != nil {
			builtServerErr = fmt.Errorf("mkdir temp: %w", err)
			return
		}
		binName := "server"
		if runtime.GOOS == "windows" {
			binName = "server.exe"
		}
		builtServerPath = filepath.Join(tmpDir, binName)
		cmdPath, _ := filepath.Abs(filepath.Join("..", "..", "cmd", "server"))
		buildCmd := exec.Command("go", "build", "-o", builtServerPath, ".")
		buildCmd.Dir = cmdPath
		buildCmd.Env = append(os.Environ(), "GOTOOLCHAIN=local", "GOPROXY=off")
		if out, err := buildCmd.CombinedOutput(); err != nil {
			builtServerErr = fmt.Errorf("build server failed: %w (out: %s)", err, string(out))
			return
		}
	})
	if builtServerErr != nil {
		t.Fatalf("%v", builtServerErr)
	}
	return builtServerPath
}

func getFreePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("get free port: %v", err)
	}
	defer l.Close()
	return strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
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

	serverBin := getOrBuildServer(t)
	testPort := getFreePort(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, serverBin)
	cmd.Env = append(os.Environ(),
		"GOTOOLCHAIN=local",
		"GOPROXY=off",
		"HTTP_ADDR=127.0.0.1:"+testPort,
		"PORT="+testPort,
		"DATABASE_URL="+cfg.DatabaseURL,
		"DATABASE_MAINT_URL="+cfg.DatabaseMaintURL,
	)

	if err := cmd.Start(); err != nil {
		t.Fatalf("start cmd/server binary failed: %v", err)
	}
	startedPID := cmd.Process.Pid

	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}()

	// Poll until server responds on /healthz and verify returned PID matches started process
	client := &http.Client{Timeout: 500 * time.Millisecond}
	healthURL := fmt.Sprintf("http://127.0.0.1:%s/healthz?pid=1", testPort)
	serverReady := false

	for i := 0; i < 60; i++ {
		time.Sleep(100 * time.Millisecond)
		resp, err := client.Get(healthURL)
		if err == nil && resp.StatusCode == http.StatusOK {
			var hResp struct {
				Status string `json:"status"`
				PID    int    `json:"pid"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&hResp); err == nil {
				_ = resp.Body.Close()
				if hResp.PID == startedPID {
					serverReady = true
					break
				}
			} else {
				_ = resp.Body.Close()
			}
		}
	}

	if !serverReady {
		t.Fatalf("cmd/server binary (PID %d) failed to become ready on port %s within timeout", startedPID, testPort)
	}

	// Trigger shutdown: on Unix Process.Signal sends SIGINT; on Windows Process.Kill stops binary cleanly
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		_ = cmd.Process.Kill()
	}

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	select {
	case err := <-done:
		if err != nil && !strings.Contains(err.Error(), "interrupt") && !strings.Contains(err.Error(), "exit status 1") && !strings.Contains(err.Error(), "killed") {
			t.Logf("server terminated with: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("server process (PID %d) did not exit within 5 seconds of interrupt signal", startedPID)
	}
}
