package postgres

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

func TestLivePostgresRefreshTokenSessionCeiling(t *testing.T) {
	pool, _ := setupLiveTestPool(t, 30*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Apply migrations 030, 033, 034
	for _, m := range []string{
		"030_refresh_tokens.sql",
		"033_refresh_tokens_revoked_at.sql",
		"034_refresh_tokens_family_started_at.sql",
	} {
		mPath := filepath.Join("..", "..", "migrations", m)
		mSQL, err := os.ReadFile(mPath)
		if err != nil {
			t.Fatalf("failed reading migration %s: %v", m, err)
		}
		if _, err := pool.Exec(ctx, string(mSQL)); err != nil {
			t.Fatalf("failed applying migration %s: %v", m, err)
		}
	}

	repo := NewRefreshTokenRepo(pool)
	userID := uuid.New()
	familyID := uuid.New()

	_, err := pool.Exec(ctx, `
		INSERT INTO users (id, phone, role)
		VALUES ($1, $2, 'tenant')
		ON CONFLICT (id) DO NOTHING`,
		userID, "+91"+strconv.FormatInt(time.Now().UnixNano()%10000000000, 10),
	)
	if err != nil {
		t.Fatalf("failed inserting test user: %v", err)
	}

	t.Run("rotation fails if family_started_at exceeds 90 days", func(t *testing.T) {
		oldStarted := time.Now().UTC().Add(-91 * 24 * time.Hour)
		tokenHash := "expired_ceiling_hash_" + uuid.New().String()[:8]
		rt := &domain.RefreshToken{
			ID:              uuid.New(),
			UserID:          userID,
			FamilyID:        familyID,
			TokenHash:       tokenHash,
			ExpiresAt:       time.Now().UTC().Add(30 * 24 * time.Hour),
			Revoked:         false,
			FamilyStartedAt: oldStarted,
			CreatedAt:       oldStarted,
		}
		if err := repo.StoreRefreshToken(ctx, rt); err != nil {
			t.Fatalf("store token failed: %v", err)
		}

		newRT := &domain.RefreshToken{
			TokenHash: "new_hash_" + uuid.New().String()[:8],
			ExpiresAt: time.Now().UTC().Add(30 * 24 * time.Hour),
		}

		_, err := repo.RotateTokenTx(ctx, tokenHash, newRT)
		if !errors.Is(err, ErrRefreshTokenExpired) {
			t.Fatalf("expected ErrRefreshTokenExpired for 90-day ceiling breach, got %v", err)
		}
	})

	t.Run("session ceiling is immutable and survives purge of root token", func(t *testing.T) {
		familyID2 := uuid.New()
		startedAt := time.Now().UTC().Add(-10 * 24 * time.Hour).Truncate(time.Microsecond)
		rootHash := "root_hash_" + uuid.New().String()[:8]
		rootRT := &domain.RefreshToken{
			ID:              uuid.New(),
			UserID:          userID,
			FamilyID:        familyID2,
			TokenHash:       rootHash,
			ExpiresAt:       time.Now().UTC().Add(30 * 24 * time.Hour),
			Revoked:         false,
			FamilyStartedAt: startedAt,
			CreatedAt:       startedAt,
		}
		if err := repo.StoreRefreshToken(ctx, rootRT); err != nil {
			t.Fatalf("store root token: %v", err)
		}

		// Rotate root -> child 1
		child1Hash := "child1_hash_" + uuid.New().String()[:8]
		child1RT := &domain.RefreshToken{
			TokenHash: child1Hash,
			ExpiresAt: time.Now().UTC().Add(30 * 24 * time.Hour),
		}
		rotatedChild1, err := repo.RotateTokenTx(ctx, rootHash, child1RT)
		if err != nil {
			t.Fatalf("rotate root token: %v", err)
		}
		if !rotatedChild1.FamilyStartedAt.Equal(startedAt) {
			t.Fatalf("expected child1 to inherit family_started_at %v, got %v", startedAt, rotatedChild1.FamilyStartedAt)
		}

		// Purge revoked tokens older than 1 second (this deletes rootRT)
		time.Sleep(10 * time.Millisecond)
		_, err = pool.Exec(ctx, `DELETE FROM refresh_tokens WHERE token_hash = $1`, rootHash)
		if err != nil {
			t.Fatalf("purge root token: %v", err)
		}

		// Rotate child 1 -> child 2 (even though root token is permanently deleted from database)
		child2Hash := "child2_hash_" + uuid.New().String()[:8]
		child2RT := &domain.RefreshToken{
			TokenHash: child2Hash,
			ExpiresAt: time.Now().UTC().Add(30 * 24 * time.Hour),
		}
		rotatedChild2, err := repo.RotateTokenTx(ctx, child1Hash, child2RT)
		if err != nil {
			t.Fatalf("rotate child1 after root purged: %v", err)
		}
		if !rotatedChild2.FamilyStartedAt.Equal(startedAt) {
			t.Fatalf("expected child2 to still have immutable root family_started_at %v, got %v", startedAt, rotatedChild2.FamilyStartedAt)
		}
	})
}

func TestLivePostgresConcurrentRotation(t *testing.T) {
	pool, _ := setupLiveTestPool(t, 30*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	repo := NewRefreshTokenRepo(pool)
	userID := uuid.New()
	familyID := uuid.New()

	_, err := pool.Exec(ctx, `
		INSERT INTO users (id, phone, role)
		VALUES ($1, $2, 'tenant')
		ON CONFLICT (id) DO NOTHING`,
		userID, "+91"+strconv.FormatInt(time.Now().UnixNano()%10000000000, 10),
	)
	if err != nil {
		t.Fatalf("failed inserting test user: %v", err)
	}

	rootHash := "concurrent_root_" + uuid.New().String()[:8]

	rootRT := &domain.RefreshToken{
		ID:              uuid.New(),
		UserID:          userID,
		FamilyID:        familyID,
		TokenHash:       rootHash,
		ExpiresAt:       time.Now().UTC().Add(30 * 24 * time.Hour),
		Revoked:         false,
		FamilyStartedAt: time.Now().UTC(),
		CreatedAt:       time.Now().UTC(),
	}
	if err := repo.StoreRefreshToken(ctx, rootRT); err != nil {
		t.Fatalf("store root token: %v", err)
	}

	concurrency := 8
	var wg sync.WaitGroup
	results := make([]error, concurrency)
	tokens := make([]*domain.RefreshToken, concurrency)

	startBarrier := make(chan struct{})

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-startBarrier
			newRT := &domain.RefreshToken{
				TokenHash: "conc_child_" + strconv.Itoa(idx) + "_" + uuid.New().String()[:8],
				ExpiresAt: time.Now().UTC().Add(30 * 24 * time.Hour),
			}
			rotated, err := repo.RotateTokenTx(ctx, rootHash, newRT)
			results[idx] = err
			tokens[idx] = rotated
		}(i)
	}

	// Trigger all goroutines simultaneously
	close(startBarrier)
	wg.Wait()

	// Verify results:
	// Exactly one goroutine does the primary rotation.
	// Other goroutines hitting within 15s grace window get grace-window child tokens.
	// If any hit after or during race, no errors other than nil (success) or ErrReplayDetected are allowed.
	// Crucially: NO deadlocks, NO sql crashes, NO data corruption.
	successCount := 0
	for i := 0; i < concurrency; i++ {
		if results[i] == nil {
			successCount++
			if tokens[i] == nil || tokens[i].ID == uuid.Nil {
				t.Errorf("worker %d returned nil or empty token on success", i)
			}
		} else if !errors.Is(results[i], ErrReplayDetected) && !errors.Is(results[i], domain.ErrGraceWindow) {
			t.Errorf("worker %d returned unexpected error: %v", i, results[i])
		}
	}

	if successCount == 0 {
		t.Fatalf("expected at least 1 successful rotation among concurrent workers, got 0")
	}

	// 1. Verify that the original root token is definitely marked revoked in DB
	storedRoot, err := repo.GetRefreshTokenByHash(ctx, rootHash)
	if err != nil {
		t.Fatalf("fetch stored root: %v", err)
	}
	if !storedRoot.Revoked {
		t.Fatalf("expected root token to be revoked after concurrent rotation")
	}

	// 2. Verify that exactly 1 active (unrevoked) successor token exists in DB,
	// preventing multiple unrevoked child tokens from being minted on concurrent retransmit (M-01).
	var activeTokensInDB int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM refresh_tokens WHERE family_id = $1 AND revoked = FALSE`, familyID).Scan(&activeTokensInDB); err != nil {
		t.Fatalf("count active tokens in db: %v", err)
	}
	if activeTokensInDB != 1 {
		t.Fatalf("expected exactly 1 active successor token in DB, got %d", activeTokensInDB)
	}

	// Verify all successful workers received the same successor token ID
	var firstTokenID uuid.UUID
	for i := 0; i < concurrency; i++ {
		if results[i] == nil && tokens[i] != nil {
			if firstTokenID == uuid.Nil {
				firstTokenID = tokens[i].ID
			} else if tokens[i].ID != firstTokenID {
				t.Errorf("worker %d got different token ID %s, expected %s", i, tokens[i].ID, firstTokenID)
			}
		}
	}

	// 3. Verify that all tokens in the family (root and children) strictly share the root's family_started_at
	expectedStartedAt := rootRT.FamilyStartedAt.Truncate(time.Microsecond)
	rows, err := pool.Query(ctx, `SELECT id, revoked, family_started_at FROM refresh_tokens WHERE family_id = $1`, familyID)
	if err != nil {
		t.Fatalf("query family tokens: %v", err)
	}
	defer rows.Close()

	totalRows := 0
	for rows.Next() {
		var tID uuid.UUID
		var isRevoked bool
		var famStarted time.Time
		if err := rows.Scan(&tID, &isRevoked, &famStarted); err != nil {
			t.Fatalf("scan family token: %v", err)
		}
		totalRows++
		if !famStarted.Truncate(time.Microsecond).Equal(expectedStartedAt) {
			t.Fatalf("token %v in DB has family_started_at %v, expected %v", tID, famStarted, expectedStartedAt)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows error: %v", err)
	}
	if totalRows != 2 {
		t.Fatalf("expected total tokens in family to be 2 (1 root + 1 successor), got %d", totalRows)
	}

	// 4. Verify all worker-returned tokens have the identical family_started_at
	for i := 0; i < concurrency; i++ {
		if results[i] == nil {
			if !tokens[i].FamilyStartedAt.Truncate(time.Microsecond).Equal(expectedStartedAt) {
				t.Errorf("worker %d returned token with family_started_at %v, expected %v", i, tokens[i].FamilyStartedAt, expectedStartedAt)
			}
		}
	}
}

// TestLivePostgresGraceWindowChainRevocation tests the specific defect scenario where a third
// concurrent duplicate arrives *after* the grace path has already been executed once.
//
// Sequence:
//
//	A) Worker 0 rotates R → S1 (normal path, commits).
//	B) Worker 1 uses R within grace window; repo walks chain, finds S1 active, returns (S1, ErrGraceWindow). No new row minted.
//	C) Worker 2 uses R within grace window *after* B; must ALSO return (S1, ErrGraceWindow), NOT revoke family.
//
// Invariant: family is never revoked inside the grace window for a legitimate duplicate.
// Invariant: exactly 1 active successor exists at all times.
func TestLivePostgresGraceWindowChainRevocation(t *testing.T) {
	pool, _ := setupLiveTestPool(t, 30*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for _, m := range []string{
		"030_refresh_tokens.sql",
		"033_refresh_tokens_revoked_at.sql",
		"034_refresh_tokens_family_started_at.sql",
		"052_refresh_tokens_replaced_by.sql",
	} {
		mPath := filepath.Join("..", "..", "migrations", m)
		mSQL, err := os.ReadFile(mPath)
		if err != nil {
			t.Fatalf("failed reading migration %s: %v", m, err)
		}
		if _, err := pool.Exec(ctx, string(mSQL)); err != nil {
			t.Fatalf("failed applying migration %s: %v", m, err)
		}
	}

	repo := NewRefreshTokenRepo(pool)
	userID := uuid.New()
	familyID := uuid.New()

	_, err := pool.Exec(ctx, `
		INSERT INTO users (id, phone, role)
		VALUES ($1, $2, 'tenant')
		ON CONFLICT (id) DO NOTHING`,
		userID, "+91"+strconv.FormatInt(time.Now().UnixNano()%10000000000, 10),
	)
	if err != nil {
		t.Fatalf("failed inserting test user: %v", err)
	}

	rootHash := "grace_chain_root_" + uuid.New().String()[:8]
	rootRT := &domain.RefreshToken{
		ID:              uuid.New(),
		UserID:          userID,
		FamilyID:        familyID,
		TokenHash:       rootHash,
		ExpiresAt:       time.Now().UTC().Add(30 * 24 * time.Hour),
		Revoked:         false,
		FamilyStartedAt: time.Now().UTC(),
		CreatedAt:       time.Now().UTC(),
	}
	if err := repo.StoreRefreshToken(ctx, rootRT); err != nil {
		t.Fatalf("store root token: %v", err)
	}

	countActive := func(label string) int {
		t.Helper()
		var n int
		if scanErr := pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM refresh_tokens WHERE family_id = $1 AND revoked = FALSE AND expires_at > NOW()`,
			familyID,
		).Scan(&n); scanErr != nil {
			t.Fatalf("%s: countActive query: %v", label, scanErr)
		}
		return n
	}

	// Step A: Normal rotation R → S1
	rotatedS1, err := repo.RotateTokenTx(ctx, rootHash, &domain.RefreshToken{
		TokenHash: "grace_chain_s1_" + uuid.New().String()[:8],
		ExpiresAt: time.Now().UTC().Add(30 * 24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("step A: normal rotation R→S1 failed: %v", err)
	}
	t.Logf("Step A: S1 minted ID=%s", rotatedS1.ID)
	if n := countActive("after step A"); n != 1 {
		t.Fatalf("after step A: expected 1 active token, got %d", n)
	}

	// Step B: Duplicate use of R within grace window → (leaf, ErrGraceWindow), zero new rows
	bLeaf, bErr := repo.RotateTokenTx(ctx, rootHash, &domain.RefreshToken{
		TokenHash: "grace_chain_b_" + uuid.New().String()[:8],
		ExpiresAt: time.Now().UTC().Add(30 * 24 * time.Hour),
	})
	if !errors.Is(bErr, domain.ErrGraceWindow) {
		t.Fatalf("step B: expected ErrGraceWindow, got err=%v leaf=%v", bErr, bLeaf)
	}
	if bLeaf == nil || bLeaf.UserID == uuid.Nil {
		t.Fatal("step B: ErrGraceWindow must return non-nil leaf with valid UserID")
	}
	if bLeaf.UserID != userID {
		t.Errorf("step B: leaf.UserID=%s want %s", bLeaf.UserID, userID)
	}
	if n := countActive("after step B"); n != 1 {
		t.Fatalf("after step B: expected 1 active token, got %d — grace path minted an extra row", n)
	}
	t.Logf("Step B: grace returned leaf ID=%s (ErrGraceWindow)", bLeaf.ID)

	// Step C: Third duplicate, also within grace window; must return same leaf as B
	cLeaf, cErr := repo.RotateTokenTx(ctx, rootHash, &domain.RefreshToken{
		TokenHash: "grace_chain_c_" + uuid.New().String()[:8],
		ExpiresAt: time.Now().UTC().Add(30 * 24 * time.Hour),
	})
	if !errors.Is(cErr, domain.ErrGraceWindow) {
		t.Fatalf("step C: expected ErrGraceWindow, got err=%v leaf=%v", cErr, cLeaf)
	}
	if cLeaf == nil || cLeaf.UserID == uuid.Nil {
		t.Fatal("step C: ErrGraceWindow must return non-nil leaf with valid UserID")
	}
	if bLeaf.ID != cLeaf.ID {
		t.Errorf("B and C returned different leaf IDs: B=%s C=%s — idempotency broken", bLeaf.ID, cLeaf.ID)
	}
	if n := countActive("after step C"); n != 1 {
		t.Fatalf("after step C: expected 1 active token, got %d", n)
	}
	t.Logf("Step C: grace returned same leaf ID=%s (ErrGraceWindow)", cLeaf.ID)

	// Final: family must NOT be fully revoked
	var allRevoked bool
	if scanErr := pool.QueryRow(ctx,
		`SELECT bool_and(revoked) FROM refresh_tokens WHERE family_id = $1`, familyID,
	).Scan(&allRevoked); scanErr != nil {
		t.Fatalf("check family revoke state: %v", scanErr)
	}
	if allRevoked {
		t.Fatal("family was incorrectly fully revoked inside the grace window — P1-A defect not fixed")
	}
}
