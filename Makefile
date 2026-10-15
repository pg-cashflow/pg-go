.PHONY: migrate server test test-db coverage golden vet

migrate:
	go run ./cmd/migrate/

server:
	go run ./cmd/server/

test:
	go test ./...

test-db:
	go clean -testcache
	set REQUIRE_DB=1&& go test -count=1 ./...

coverage:
	go test -coverprofile=coverage.out ./...
	go run ./scripts/check-coverage.go -profile=coverage.out -require-all

golden:
	go clean -testcache
	set REQUIRE_DB=1&& go test -count=1 -run TestE2EMoneyLifecycle_DoubleEntryConservation ./internal/api/

vet:
	go vet ./...

# PR slice for admission module (Gate 06 + join/slot/waitlist/referral packages).
test-admission:
	go test ./internal/auth ./internal/join ./internal/referral ./internal/waitlist -run "Patch|Bps|LiveWaitlist" -count=1
	go test ./internal/postgres -run "Patch|LivePropertyAdmission|LiveSlotInvite" -count=1
	go test ./internal/api -run "TestRouteRoleAuthorizationMatrix|JoinProfile|SlotInvite|Waitlist|OnboardingSettings" -count=1
