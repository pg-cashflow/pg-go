package main

import (
	"os"
	"os/exec"
	"testing"
)

func TestKPISnapshot_FailClosedWithoutConfig(t *testing.T) {
	if os.Getenv("BE_CMD_KPI_SNAPSHOT_TEST") == "1" {
		main()
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestKPISnapshot_FailClosedWithoutConfig")
	cmd.Env = []string{
		"BE_CMD_KPI_SNAPSHOT_TEST=1",
		"SYSTEMROOT=" + os.Getenv("SYSTEMROOT"),
	}
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected kpi-snapshot to exit non-zero with empty environment, but succeeded: %s", string(out))
	}
}
