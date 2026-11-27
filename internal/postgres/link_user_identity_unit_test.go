package postgres

// Unit-level (no-DB) tests for the LinkUserIdentity transactional behaviour.
// These tests use an in-memory fake that implements just enough of the DBTX
// interface to exercise the control-flow of LinkUserIdentity without a live DB.
//
// Live-DB tests (which test SQL correctness) live in
// internal/postgres/*_live_test.go and are skipped when DATABASE_URL is unset.

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// --- in-memory DBTX stub ------------------------------------------------

// fakeTx mimics a pgx.Tx at the query level for LinkUserIdentity's internal
// transaction. It tracks which rows have been "inserted" and enforces the
// single-slot uniqueness rule.
type fakeTx struct {
	// Rows keyed by (projectID+":"+firebaseUID).
	rows         map[string]fakeIdentityRow
	snapshot     map[string]fakeIdentityRow
	userTable    map[uuid.UUID]string // userID -> legacy firebase_uid
	userSnapshot map[uuid.UUID]string

	// Counts for assertions.
	commitCount   int
	rollbackCount int
}

type fakeIdentityRow struct {
	userID uuid.UUID
	status string
}

func newFakeTx(users ...uuid.UUID) *fakeTx {
	f := &fakeTx{
		rows:      make(map[string]fakeIdentityRow),
		userTable: make(map[uuid.UUID]string),
	}
	for _, u := range users {
		f.userTable[u] = ""
	}
	return f
}

func (f *fakeTx) setUserFirebaseUID(userID uuid.UUID, firebaseUID string) {
	f.userTable[userID] = firebaseUID
}

func (f *fakeTx) addIdentityRow(projectID, firebaseUID string, userID uuid.UUID, status string) {
	f.rows[projectID+":"+firebaseUID] = fakeIdentityRow{userID: userID, status: status}
}

// --- fakePool wraps fakeTx and implements the beginner interface --------

type fakePool struct{ tx pgx.Tx }

func (p *fakePool) Begin(_ context.Context) (pgx.Tx, error) {
	if ft, ok := p.tx.(*fakeTx); ok {
		ft.takeSnapshot()
	} else if af, ok := p.tx.(*auditFailTx); ok {
		af.fakeTx.takeSnapshot()
	} else if ci, ok := p.tx.(*conflictOnInsertTx); ok {
		ci.fakeTx.takeSnapshot()
	}
	return p.tx, nil
}

// QueryRow implements DBTX.QueryRow on the pool level (not used directly).
func (p *fakePool) QueryRow(_ context.Context, _ string, _ ...any) pgx.Row { return nil }
func (p *fakePool) Exec(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (p *fakePool) Query(_ context.Context, _ string, _ ...any) (pgx.Rows, error) { return nil, nil }

// --- fakeTx implements pgx.Tx (only the methods called by LinkUserIdentity) -

func (f *fakeTx) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	// Handle "SELECT user_id, status FROM user_identities ... FOR UPDATE"
	projectID, _ := args[0].(string)
	firebaseUID, _ := args[1].(string)
	key := projectID + ":" + firebaseUID
	if row, ok := f.rows[key]; ok {
		return &fakeRow{values: []any{row.userID, row.status}}
	}
	return &fakeRow{err: pgx.ErrNoRows}
}

func (f *fakeTx) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	// Distinguish UPDATE users (args[0]=userID, args[1]=firebaseUID)
	// from UPDATE user_identities and INSERT user_identities.
	// We use a lightweight heuristic: check whether the first arg is a uuid.UUID.
	if len(args) == 0 {
		return pgconn.NewCommandTag("OK 0"), nil
	}

	// INSERT INTO user_identities ... ON CONFLICT DO NOTHING
	if len(args) == 5 {
		userID, _ := args[0].(uuid.UUID)
		projectID, _ := args[1].(string)
		firebaseUID, _ := args[2].(string)
		key := projectID + ":" + firebaseUID
		if _, exists := f.rows[key]; exists {
			// Simulate ON CONFLICT DO NOTHING → 0 rows affected.
			return pgconn.NewCommandTag("INSERT 0 0"), nil
		}
		f.rows[key] = fakeIdentityRow{userID: userID, status: "active"}
		return pgconn.NewCommandTag("INSERT 0 1"), nil
	}

	// UPDATE users SET firebase_uid = COALESCE(NULLIF(firebase_uid, ''), $2) WHERE id = $1
	if len(args) == 2 {
		userID, ok := args[0].(uuid.UUID)
		if ok {
			existingUID, exists := f.userTable[userID]
			if exists {
				newUID, _ := args[1].(string)
				if existingUID == "" {
					f.userTable[userID] = newUID
				}
				return pgconn.NewCommandTag("UPDATE 1"), nil
			}
			return pgconn.NewCommandTag("UPDATE 0"), nil
		}
		// UPDATE user_identities SET last_authenticated_at ... — always OK.
		return pgconn.NewCommandTag("UPDATE 1"), nil
	}

	// Audit INSERT (5 args but caught above) or other writes.
	return pgconn.NewCommandTag("OK 1"), nil
}

func (f *fakeTx) takeSnapshot() {
	f.snapshot = make(map[string]fakeIdentityRow, len(f.rows))
	for k, v := range f.rows {
		f.snapshot[k] = v
	}
	f.userSnapshot = make(map[uuid.UUID]string, len(f.userTable))
	for k, v := range f.userTable {
		f.userSnapshot[k] = v
	}
}

func (f *fakeTx) Query(_ context.Context, _ string, _ ...any) (pgx.Rows, error) { return nil, nil }
func (f *fakeTx) Commit(_ context.Context) error                                { f.commitCount++; return nil }
func (f *fakeTx) Rollback(_ context.Context) error {
	if f.commitCount > 0 {
		return nil
	}
	f.rollbackCount++
	if f.snapshot != nil {
		f.rows = make(map[string]fakeIdentityRow, len(f.snapshot))
		for k, v := range f.snapshot {
			f.rows[k] = v
		}
	}
	if f.userSnapshot != nil {
		f.userTable = make(map[uuid.UUID]string, len(f.userSnapshot))
		for k, v := range f.userSnapshot {
			f.userTable[k] = v
		}
	}
	return nil
}

// pgx.Tx interface stubs (unused paths):
func (f *fakeTx) Begin(_ context.Context) (pgx.Tx, error) { return f, nil }
func (f *fakeTx) SendBatch(_ context.Context, _ *pgx.Batch) pgx.BatchResults {
	return nil
}
func (f *fakeTx) LargeObjects() pgx.LargeObjects { return pgx.LargeObjects{} }
func (f *fakeTx) Prepare(_ context.Context, _, _ string) (*pgconn.StatementDescription, error) {
	return nil, errors.New("not implemented")
}
func (f *fakeTx) Conn() *pgx.Conn { return nil }
func (f *fakeTx) CopyFrom(_ context.Context, _ pgx.Identifier, _ []string, _ pgx.CopyFromSource) (int64, error) {
	return 0, errors.New("not implemented")
}

// --- fakeRow ----------------------------------------------------------------

type fakeRow struct {
	values []any
	err    error
}

func (r *fakeRow) Scan(dests ...any) error {
	if r.err != nil {
		return r.err
	}
	for i, d := range dests {
		if i >= len(r.values) {
			break
		}
		switch dst := d.(type) {
		case *uuid.UUID:
			*dst = r.values[i].(uuid.UUID)
		case *string:
			*dst = r.values[i].(string)
		}
	}
	return nil
}

// --- Tests ------------------------------------------------------------------

func TestLinkUserIdentity_NewUser_CommitsTransaction(t *testing.T) {
	userID := uuid.New()
	tx := newFakeTx(userID)
	pool := &fakePool{tx: tx}

	repo := &UserRepo{db: pool}
	if err := repo.LinkUserIdentity(context.Background(), userID, "proj1", "uid-abc", "firebase", "signup"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tx.commitCount != 1 {
		t.Errorf("expected 1 commit, got %d", tx.commitCount)
	}
	// Row must be in fake store.
	key := "proj1:uid-abc"
	if row, ok := tx.rows[key]; !ok || row.userID != userID {
		t.Errorf("expected identity row for user %s, got %+v", userID, tx.rows[key])
	}
	if tx.userTable[userID] != "uid-abc" {
		t.Errorf("expected legacy users.firebase_uid to be %q, got %q", "uid-abc", tx.userTable[userID])
	}
}

func TestLinkUserIdentity_ExistingLegacyUID_PreservedOnCommit(t *testing.T) {
	userID := uuid.New()
	tx := newFakeTx(userID)
	tx.setUserFirebaseUID(userID, "legacy-uid-1")
	pool := &fakePool{tx: tx}

	repo := &UserRepo{db: pool}
	if err := repo.LinkUserIdentity(context.Background(), userID, "proj1", "uid-new", "firebase", "signup"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tx.userTable[userID] != "legacy-uid-1" {
		t.Errorf("expected existing legacy firebase_uid %q to be preserved, got %q", "legacy-uid-1", tx.userTable[userID])
	}
	if row, ok := tx.rows["proj1:uid-new"]; !ok || row.userID != userID {
		t.Errorf("expected new identity row for user %s, got %+v", userID, tx.rows["proj1:uid-new"])
	}
}

func TestLinkUserIdentity_AlreadyLinkedToAnotherUser_ReturnsError(t *testing.T) {
	ownerID := uuid.New()
	requestorID := uuid.New()
	tx := newFakeTx(ownerID, requestorID)
	tx.addIdentityRow("proj1", "uid-abc", ownerID, "active")
	pool := &fakePool{tx: tx}

	repo := &UserRepo{db: pool}
	err := repo.LinkUserIdentity(context.Background(), requestorID, "proj1", "uid-abc", "firebase", "firebase_link")
	if err == nil {
		t.Fatal("expected error for identity owned by another user, got nil")
	}
	if !errors.Is(err, errors.New(err.Error())) {
		// errors.Is won't match plain fmt.Errorf; check the string instead.
		if err.Error() != "identity already linked to another user" {
			t.Errorf("unexpected error message: %v", err)
		}
	}
	if tx.commitCount != 0 {
		t.Errorf("expected no commit on conflict, got %d", tx.commitCount)
	}
}

func TestLinkUserIdentity_RevokedRow_ReturnsError(t *testing.T) {
	userID := uuid.New()
	tx := newFakeTx(userID)
	tx.addIdentityRow("proj1", "uid-revoked", userID, "revoked")
	pool := &fakePool{tx: tx}

	repo := &UserRepo{db: pool}
	err := repo.LinkUserIdentity(context.Background(), userID, "proj1", "uid-revoked", "firebase", "firebase_link")
	if err == nil {
		t.Fatal("expected error for revoked identity, got nil")
	}
	want := "identity has been revoked and cannot be re-linked automatically"
	if err.Error() != want {
		t.Errorf("expected %q, got %q", want, err.Error())
	}
	if tx.commitCount != 0 {
		t.Errorf("expected no commit for revoked row, got %d", tx.commitCount)
	}
}

func TestLinkUserIdentity_ConcurrentConflictOnInsert_ReturnsError(t *testing.T) {
	// Simulate: FOR UPDATE scan sees no row, but INSERT reports 0 rows affected
	// (another goroutine slipped in between). The function must error, not succeed.
	userID := uuid.New()
	tx := newFakeTx(userID)
	pool := &fakePool{tx: &conflictOnInsertTx{fakeTx: tx}}

	repo := &UserRepo{db: pool}
	err := repo.LinkUserIdentity(context.Background(), userID, "proj2", "uid-conflict", "firebase", "firebase_link")
	if err == nil {
		t.Fatal("expected error on concurrent INSERT conflict, got nil")
	}
	if tx.rollbackCount == 0 {
		t.Errorf("expected rollback on concurrent INSERT conflict, got 0")
	}
	if tx.userTable[userID] != "" {
		t.Errorf("expected legacy users.firebase_uid to be rolled back to empty, got %q", tx.userTable[userID])
	}
}

// conflictOnInsertTx wraps fakeTx but reports 0 rows on INSERT to simulate
// a concurrent claim between the FOR UPDATE read and the INSERT.
type conflictOnInsertTx struct{ *fakeTx }

func (c *conflictOnInsertTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	// If this looks like the user_identities INSERT (5 value args), force 0 rows.
	if len(args) == 5 {
		return pgconn.NewCommandTag("INSERT 0 0"), nil
	}
	return c.fakeTx.Exec(ctx, sql, args...)
}

func (c *conflictOnInsertTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return c.fakeTx.QueryRow(ctx, sql, args...)
}
func (c *conflictOnInsertTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return c.fakeTx.Query(ctx, sql, args...)
}
func (c *conflictOnInsertTx) Begin(ctx context.Context) (pgx.Tx, error) {
	return c.fakeTx.Begin(ctx)
}

func TestLinkUserIdentity_AuditInsertFails_RollsBack(t *testing.T) {
	userID := uuid.New()
	tx := newFakeTx(userID)
	pool := &fakePool{tx: &auditFailTx{fakeTx: tx}}

	repo := &UserRepo{db: pool}
	err := repo.LinkUserIdentity(context.Background(), userID, "proj3", "uid-audit-fail", "firebase", "firebase_link")
	if err == nil {
		t.Fatal("expected error when audit insert fails, got nil")
	}
	if tx.commitCount != 0 {
		t.Errorf("expected 0 commits when audit insert fails, got %d", tx.commitCount)
	}
	if tx.rollbackCount == 0 {
		t.Errorf("expected rollback when audit insert fails, got 0")
	}
	if _, exists := tx.rows["proj3:uid-audit-fail"]; exists {
		t.Errorf("expected identity row to be rolled back when audit insert fails")
	}
	if tx.userTable[userID] != "" {
		t.Errorf("expected legacy users.firebase_uid to be rolled back to empty, got %q", tx.userTable[userID])
	}
}

type auditFailTx struct{ *fakeTx }

func (a *auditFailTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if len(args) == 5 && len(a.fakeTx.rows) > 0 {
		// Second 5-arg Exec in LinkUserIdentity is INSERT INTO user_identity_audit_events
		return pgconn.CommandTag{}, errors.New("simulated audit table failure")
	}
	return a.fakeTx.Exec(ctx, sql, args...)
}

func (a *auditFailTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return a.fakeTx.QueryRow(ctx, sql, args...)
}
func (a *auditFailTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return a.fakeTx.Query(ctx, sql, args...)
}
func (a *auditFailTx) Begin(ctx context.Context) (pgx.Tx, error) {
	return a.fakeTx.Begin(ctx)
}
