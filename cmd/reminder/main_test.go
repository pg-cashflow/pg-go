package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestReminder_FailClosedWithoutConfig(t *testing.T) {
	if os.Getenv("BE_CMD_REMINDER_TEST") == "1" {
		arg := os.Getenv("CMD_ARG")
		if arg != "" {
			os.Args = []string{os.Args[0], arg}
		} else {
			os.Args = []string{os.Args[0]}
		}
		main()
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestReminder_FailClosedWithoutConfig")
	cmd.Env = []string{
		"BE_CMD_REMINDER_TEST=1",
		"SYSTEMROOT=" + os.Getenv("SYSTEMROOT"),
	}
	out, err := cmd.CombinedOutput()
	outStr := string(out)
	if err == nil {
		t.Fatalf("expected reminder to exit non-zero with empty environment, but succeeded: %s", outStr)
	}
	if strings.Contains(outStr, "panic:") {
		t.Fatalf("reminder panicked instead of failing closed: %s", outStr)
	}
	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() == 0 {
		t.Fatalf("expected non-zero exit code, got err: %v, out: %s", err, outStr)
	}
}

func TestReminder_HelpFlag(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=TestReminder_FailClosedWithoutConfig")
	cmd.Env = []string{
		"BE_CMD_REMINDER_TEST=1",
		"CMD_ARG=-help",
		"SYSTEMROOT=" + os.Getenv("SYSTEMROOT"),
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("expected reminder -help to exit 0, got err: %v, out: %s", err, string(out))
	}
}

func TestReminder_InvalidFlag(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=TestReminder_FailClosedWithoutConfig")
	cmd.Env = []string{
		"BE_CMD_REMINDER_TEST=1",
		"CMD_ARG=-invalid-flag-xyz",
		"SYSTEMROOT=" + os.Getenv("SYSTEMROOT"),
	}
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected reminder with invalid flag to exit non-zero, but got success: %s", string(out))
	}
	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 2 {
		t.Fatalf("expected exit code 2 for flag error, got %v (out: %s)", err, string(out))
	}
}
