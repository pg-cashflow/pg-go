package testutil

import (
	"os"
	"testing"
)

type mockTB struct {
	testing.TB
	failed  bool
	skipped bool
	msg     string
}

func (m *mockTB) Helper() {}
func (m *mockTB) Fatalf(format string, args ...any) {
	m.failed = true
}
func (m *mockTB) Skip(args ...any) {
	m.skipped = true
}

func TestRequireDB_WhenUnsetAndRequireDB(t *testing.T) {
	origURL := os.Getenv("DATABASE_URL")
	origReq := os.Getenv("REQUIRE_DB")
	defer func() {
		os.Setenv("DATABASE_URL", origURL)
		os.Setenv("REQUIRE_DB", origReq)
	}()

	os.Unsetenv("DATABASE_URL")
	os.Setenv("REQUIRE_DB", "1")

	mock := &mockTB{}
	RequireDB(mock)

	if !mock.failed {
		t.Fatalf("expected test to fail when REQUIRE_DB=1 and DATABASE_URL is empty")
	}
	if mock.skipped {
		t.Fatalf("did not expect test to skip when REQUIRE_DB=1")
	}
}

func TestRequireDB_WhenUnsetAndNoRequireDB(t *testing.T) {
	origURL := os.Getenv("DATABASE_URL")
	origReq := os.Getenv("REQUIRE_DB")
	defer func() {
		os.Setenv("DATABASE_URL", origURL)
		os.Setenv("REQUIRE_DB", origReq)
	}()

	os.Unsetenv("DATABASE_URL")
	os.Unsetenv("REQUIRE_DB")

	mock := &mockTB{}
	RequireDB(mock)

	if !mock.skipped {
		t.Fatalf("expected test to skip when REQUIRE_DB is not set")
	}
	if mock.failed {
		t.Fatalf("did not expect test to fail when REQUIRE_DB is not set")
	}
}

func TestRequireDB_WhenSet(t *testing.T) {
	origURL := os.Getenv("DATABASE_URL")
	origReq := os.Getenv("REQUIRE_DB")
	defer func() {
		os.Setenv("DATABASE_URL", origURL)
		os.Setenv("REQUIRE_DB", origReq)
	}()

	expected := "postgres://user:pass@localhost:5432/db"
	os.Setenv("DATABASE_URL", expected)
	os.Setenv("REQUIRE_DB", "1")

	mock := &mockTB{}
	url := RequireDB(mock)

	if url != expected {
		t.Fatalf("got %q, want %q", url, expected)
	}
	if mock.failed || mock.skipped {
		t.Fatalf("did not expect test to fail or skip when DATABASE_URL is set")
	}
}

func TestFailOnSkipIfDBRequired_WhenRequireDB(t *testing.T) {
	origReq := os.Getenv("REQUIRE_DB")
	defer os.Setenv("REQUIRE_DB", origReq)

	os.Setenv("REQUIRE_DB", "1")
	mock := &mockTB{}
	FailOnSkipIfDBRequired(mock, "network error")

	if !mock.failed {
		t.Fatalf("expected failure when REQUIRE_DB=1")
	}
	if mock.skipped {
		t.Fatalf("did not expect skip when REQUIRE_DB=1")
	}
}

func TestFailOnSkipIfDBRequired_WhenNoRequireDB(t *testing.T) {
	origReq := os.Getenv("REQUIRE_DB")
	defer os.Setenv("REQUIRE_DB", origReq)

	os.Unsetenv("REQUIRE_DB")
	mock := &mockTB{}
	FailOnSkipIfDBRequired(mock, "network error")

	if !mock.skipped {
		t.Fatalf("expected skip when REQUIRE_DB not set")
	}
	if mock.failed {
		t.Fatalf("did not expect failure when REQUIRE_DB not set")
	}
}

func TestFailOnSkipfIfDBRequired_WhenRequireDB(t *testing.T) {
	origReq := os.Getenv("REQUIRE_DB")
	defer os.Setenv("REQUIRE_DB", origReq)

	os.Setenv("REQUIRE_DB", "1")
	mock := &mockTB{}
	FailOnSkipfIfDBRequired(mock, "error code: %d", 500)

	if !mock.failed {
		t.Fatalf("expected failure when REQUIRE_DB=1")
	}
	if mock.skipped {
		t.Fatalf("did not expect skip when REQUIRE_DB=1")
	}
}

func TestFailOnSkipfIfDBRequired_WhenNoRequireDB(t *testing.T) {
	origReq := os.Getenv("REQUIRE_DB")
	defer os.Setenv("REQUIRE_DB", origReq)

	os.Unsetenv("REQUIRE_DB")
	mock := &mockTB{}
	FailOnSkipfIfDBRequired(mock, "error code: %d", 500)

	if !mock.skipped {
		t.Fatalf("expected skip when REQUIRE_DB not set")
	}
	if mock.failed {
		t.Fatalf("did not expect failure when REQUIRE_DB not set")
	}
}
