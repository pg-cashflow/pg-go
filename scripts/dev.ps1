param(
    [Parameter(Position = 0)]
    [ValidateSet("migrate", "server", "test", "test-db", "coverage", "golden", "vet")]
    [string]$Task = "server"
)

$ErrorActionPreference = "Stop"
Set-Location (Split-Path $PSScriptRoot -Parent)

switch ($Task) {
    "migrate" { go run ./cmd/migrate/ }
    "server" { go run ./cmd/server/ }
    "test" { go test ./... }
    "test-db" {
        go clean -testcache
        $env:REQUIRE_DB = "1"
        go test -count=1 ./...
    }
    "coverage" {
        go test -coverprofile=coverage.out ./...
        go run ./scripts/check-coverage.go -profile=coverage.out -require-all
    }
    "golden" {
        go clean -testcache
        $env:REQUIRE_DB = "1"
        go test -count=1 -run TestE2EMoneyLifecycle_DoubleEntryConservation ./internal/api/
    }
    "vet" { go vet ./... }
}
