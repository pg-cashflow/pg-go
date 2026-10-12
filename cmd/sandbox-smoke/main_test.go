package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestSandboxSmoke_FailClosedWithoutConfig(t *testing.T) {
	if os.Getenv("BE_CMD_SANDBOX_SMOKE_TEST") == "1" {
		if arg := os.Getenv("CMD_ARG"); arg != "" {
			os.Args = []string{os.Args[0], arg}
		}
		main()
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestSandboxSmoke_FailClosedWithoutConfig")
	cmd.Env = []string{
		"BE_CMD_SANDBOX_SMOKE_TEST=1",
		"SYSTEMROOT=" + os.Getenv("SYSTEMROOT"),
	}
	out, err := cmd.CombinedOutput()
	outStr := string(out)
	if err == nil {
		t.Fatalf("expected sandbox-smoke to exit non-zero with empty environment, but succeeded: %s", outStr)
	}
	if strings.Contains(outStr, "panic:") {
		t.Fatalf("sandbox-smoke panicked instead of failing closed: %s", outStr)
	}
	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() == 0 {
		t.Fatalf("expected non-zero exit code, got err: %v, out: %s", err, outStr)
	}
}

func TestSandboxSmoke_HelpFlag(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=TestSandboxSmoke_FailClosedWithoutConfig")
	cmd.Env = []string{
		"BE_CMD_SANDBOX_SMOKE_TEST=1",
		"CMD_ARG=-help",
		"SYSTEMROOT=" + os.Getenv("SYSTEMROOT"),
	}
	out, err := cmd.CombinedOutput()
	outStr := string(out)
	if err != nil {
		t.Fatalf("expected -help to exit 0, got err: %v, out: %s", err, outStr)
	}
	if !strings.Contains(outStr, "Usage") {
		t.Fatalf("expected help output to contain Usage, got: %s", outStr)
	}
}

func TestSandboxSmoke_InvalidFlag(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=TestSandboxSmoke_FailClosedWithoutConfig")
	cmd.Env = []string{
		"BE_CMD_SANDBOX_SMOKE_TEST=1",
		"CMD_ARG=-unknown-flag-12345",
		"SYSTEMROOT=" + os.Getenv("SYSTEMROOT"),
	}
	out, err := cmd.CombinedOutput()
	outStr := string(out)
	if err == nil {
		t.Fatalf("expected invalid flag to exit non-zero, out: %s", outStr)
	}
	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 2 {
		t.Fatalf("expected exit code 2 on flag parse error, got: %v, out: %s", err, outStr)
	}
}
