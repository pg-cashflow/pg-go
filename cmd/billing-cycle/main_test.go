package main

import (
	"os"
	"os/exec"
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
	if err == nil {
		t.Fatalf("expected billing-cycle to exit non-zero with empty environment, but succeeded: %s", string(out))
	}
}
