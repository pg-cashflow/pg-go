# Ticket 26: Track R.10 — Gate 10: Native Go Fuzzing for Parser Boundaries

- **Type**: `wayfinder:task`
- **Status**: Open
- **Parent**: [Wayfinder Map](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/map.md)
- **Specification**: [Test Readiness Specification](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/TEST_READINESS_SPECIFICATION.md)
- **Dependencies**: None
- **Tier**: PR Short (30s) + Nightly Full

## Objective

Find unhandled edge cases, panics, and out-of-bounds memory accesses in untrusted input parsers.
Implement Go native fuzz tests (`testing.F`) across all four core parser boundaries.
Run short fuzz tests in PR gates and extended fuzz tests on nightly schedules.

## Target Parser Boundaries

1. **Cashfree Webhook Parser** (`internal/cashfree`): Untrusted JSON payloads, polymorphic settlement webhooks, nested dispute objects.
2. **Bank Statement CSV Parsers** (`internal/finance`): SBI and HDFC bank statement CSV files, malformed commas, quote injection, unicode BOM.
3. **UIDAI Aadhaar QR Parser** (`internal/aadhaar`): Secure byte decompresion, variable-length V2/V3 structures, public key verification boundary.
4. **Phone & UTR Normalizers** (`internal/domain`): Raw mobile number strings, dirty UTR strings, international prefixes.

## Karpathy Test Discipline

1. **Look at data first:**
   Inspect genuine sample payloads for each parser.
   Review historical parser error logs for real-world malformed inputs.

2. **Check state at initialization:**
   Add valid corpus files to `testdata/fuzz/` for each target parser.

3. **Overfit one example:**
   Write `FuzzNormalizePhone` in `internal/domain/phone_test.go`.
   Seed with valid Indian numbers (`+919876543210`, `09876543210`, `9876543210`).
   Run `go test -fuzz=FuzzNormalizePhone -fuzztime=5s`.
   Assert zero panics occur.

4. **Compare with dumb baseline:**
   Assert that every successful normalized phone string matches `^[6-9]\d{9}$`.
   Assert that invalid strings return clean errors without throwing exceptions.

5. **Fix seeds:**
   Save crashing inputs to `testdata/fuzz/` as permanent regression tests.

6. **Change one thing at a time:**
   Fuzz phone and UTR parsers first.
   Fuzz webhook JSON deserializer second.
   Fuzz CSV bank statement parsers third.
   Fuzz Aadhaar QR decompressor fourth.

## STE-100 Implementation Steps

1. Create fuzz tests:
   - `internal/domain/phone_fuzz_test.go` (`FuzzNormalizePhone`)
   - `internal/domain/utr_fuzz_test.go` (`FuzzNormalizeUTR`)
   - `internal/cashfree/webhook_fuzz_test.go` (`FuzzParseWebhook`)
   - `internal/finance/bank_csv_fuzz_test.go` (`FuzzParseBankStatementCSV`)
   - `internal/aadhaar/qr_fuzz_test.go` (`FuzzParseSecureQR`)
2. In each fuzz test, seed the corpus (`f.Add(...)`) with known valid inputs.
3. Call `f.Fuzz(func(t *testing.T, data []byte) { ... })`.
4. Assert that parsers return either valid parsed data or a typed error.
5. Assert that parsers never panic on arbitrary byte slices.
6. Configure CI:
   - PR Gate: Run each fuzz test for 10 seconds (`-fuzztime=10s`).
   - Nightly Workflow: Run each fuzz test for 5 minutes (`-fuzztime=5m`).

## Acceptance Criteria

- All four parser boundaries have native `testing.F` implementations.
- Zero panics occur across 1,000,000 generated inputs.
- All crashing inputs are committed to version control as regression tests.
- PR fuzz gate completes in under 1 minute.
