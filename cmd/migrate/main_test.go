package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestMigrate_FailClosedWithoutDB(t *testing.T) {
	if os.Getenv("BE_CMD_MIGRATE_TEST") == "1" {
		main()
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestMigrate_FailClosedWithoutDB")
	cmd.Env = []string{
		"BE_CMD_MIGRATE_TEST=1",
		"SYSTEMROOT=" + os.Getenv("SYSTEMROOT"),
	}
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected migrate to exit non-zero without database URL, but succeeded: %s", string(out))
	}
	outStr := string(out)
	if !strings.Contains(outStr, "DATABASE_URL is required") && !strings.Contains(outStr, "exit status") {
		t.Logf("migrate output: %s", outStr)
	}
}
