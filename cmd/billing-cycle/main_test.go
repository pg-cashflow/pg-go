package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestBillingCycle_FailClosedWithoutConfig(t *testing.T) {
	if os.Getenv("BE_CMD_BILLING_CYCLE_TEST") == "1" {
		main()
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestBillingCycle_FailClosedWithoutConfig")
	cmd.Env = []string{
		"BE_CMD_BILLING_CYCLE_TEST=1",
		"SYSTEMROOT=" + os.Getenv("SYSTEMROOT"),
	}
	out, err := cmd.CombinedOutput()
	outStr := string(out)
	if err == nil {
		t.Fatalf("expected billing-cycle to exit non-zero with empty environment, but succeeded: %s", outStr)
	}
	if strings.Contains(outStr, "panic:") {
		t.Fatalf("billing-cycle panicked instead of failing closed: %s", outStr)
	}
	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() == 0 {
		t.Fatalf("expected non-zero exit code, got err: %v, out: %s", err, outStr)
	}
}
