package main

import (
	"os"
	"os/exec"
	"testing"
)

func TestReminder_FailClosedWithoutConfig(t *testing.T) {
	if os.Getenv("BE_CMD_REMINDER_TEST") == "1" {
		main()
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestReminder_FailClosedWithoutConfig")
	cmd.Env = []string{
		"BE_CMD_REMINDER_TEST=1",
		"SYSTEMROOT=" + os.Getenv("SYSTEMROOT"),
	}
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected reminder to exit non-zero with empty environment, but succeeded: %s", string(out))
	}
}
