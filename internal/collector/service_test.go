package collector

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/cashfree"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type stubIntents struct {
	created  *domain.PaymentIntent
	existing *domain.PaymentIntent
}

func (s *stubIntents) Create(_ context.Context, p *domain.PaymentIntent) error {
	p.ID = uuid.New()
	s.created = p
	return nil
}
func (s *stubIntents) LatestOpenForDue(context.Context, uuid.UUID) (*domain.PaymentIntent, error) {
	if s.existing != nil {
		return s.existing, nil
	}
	return nil, pgx.ErrNoRows
}

type stubCF struct{ n int }

func (s *stubCF) CreateUPIOrder(ctx context.Context, _ string, _ int64, _ string, _ string, _ ...string) (string, *time.Time, error) {
	s.n++
	exp := time.Now().Add(time.Hour)
	return "sess_1", &exp, nil
}

func (s *stubCF) GetOrder(context.Context, string) (*cashfree.OrderDetails, error) {
	return nil, nil
}

func TestPayIntentManualIncludesVPA(t *testing.T) {
	svc := New(&stubIntents{}, nil)
	due := &domain.Due{ID: uuid.New(), DueCode: "ABC123", Amount: 1500000, Status: domain.DueStatusPending}
	prop := &domain.Property{UPIVPA: "owner@upi", OwnerName: "Owner", PaymentMode: domain.PaymentModeManual}
	intent, png, err := svc.PayIntent(context.Background(), due, prop, "12", "/tenant/dues/x/qr", "")
	if err != nil {
		t.Fatal(err)
	}
	if intent.VPA != "owner@upi" || intent.Note != "PG-ABC123" || !intent.Payable || intent.Mode != domain.PaymentModeManual {
		t.Fatalf("%+v", intent)
	}
	if len(png) == 0 {
		t.Fatal("expected png")
	}
}

func TestPayIntentHidesWhenPaid(t *testing.T) {
	svc := New(&stubIntents{}, nil)
	due := &domain.Due{ID: uuid.New(), DueCode: "ABC123", Amount: 1, Status: domain.DueStatusPaid}
	prop := &domain.Property{UPIVPA: "owner@upi", PaymentMode: domain.PaymentModeManual}
	intent, _, err := svc.PayIntent(context.Background(), due, prop, "", "/x", "")
	if err != nil {
		t.Fatal(err)
	}
	if intent.Payable || intent.VPA != "" {
		t.Fatalf("paid due must not expose pay fields: %+v", intent)
	}
}

func TestPayIntentManualIgnoresCashfreeClient(t *testing.T) {
	cf := &stubCF{}
	svc := New(&stubIntents{}, cf)
	due := &domain.Due{ID: uuid.New(), DueCode: "ABC123", Amount: 100, Status: domain.DueStatusPending}
	prop := &domain.Property{PaymentMode: domain.PaymentModeManual, UPIVPA: "owner@upi", OwnerName: "O"}
	intent, png, err := svc.PayIntent(context.Background(), due, prop, "", "/x", "")
	if err != nil {
		t.Fatal(err)
	}
	if intent.Mode != domain.PaymentModeManual || cf.n != 0 || len(png) == 0 {
		t.Fatalf("manual must not create Cashfree orders: mode=%s n=%d png=%d", intent.Mode, cf.n, len(png))
	}
}

func TestPayIntentCashfreeSession(t *testing.T) {
	cf := &stubCF{}
	svc := New(&stubIntents{}, cf)
	due := &domain.Due{ID: uuid.New(), DueCode: "ABC123", Amount: 100, Status: domain.DueStatusPending}
	prop := &domain.Property{PaymentMode: domain.PaymentModeCashfree, UPIVPA: "hidden@upi"}
	intent, png, err := svc.PayIntent(context.Background(), due, prop, "", "/x", "9999999999")
	if err != nil {
		t.Fatal(err)
	}
	if intent.Mode != domain.PaymentModeCashfree || intent.PaymentSessionID != "sess_1" || intent.VPA != "" {
		t.Fatalf("%+v", intent)
	}
	if png != nil {
		t.Fatal("cashfree should not return personal QR png")
	}
	if cf.n != 1 {
		t.Fatalf("orders=%d", cf.n)
	}
}

func TestPayIntentCashfreeNoPhoneFallsBackToManual(t *testing.T) {
	cf := &stubCF{}
	svc := New(&stubIntents{}, cf)
	due := &domain.Due{ID: uuid.New(), DueCode: "ABC123", Amount: 100, Status: domain.DueStatusPending}
	prop := &domain.Property{PaymentMode: domain.PaymentModeCashfree, UPIVPA: "owner@upi", OwnerName: "O"}
	intent, png, err := svc.PayIntent(context.Background(), due, prop, "", "/x", "")
	if err != nil {
		t.Fatal(err)
	}
	if intent.Mode != domain.PaymentModeManual || cf.n != 0 || len(png) == 0 {
		t.Fatalf("missing phone must fall back to manual: mode=%s n=%d png=%d", intent.Mode, cf.n, len(png))
	}
}

func TestEnsureCashfreeRejectsStaleAmount(t *testing.T) {
	sess := "old_sess"
	existing := &domain.PaymentIntent{AmountPaise: 100, PaymentSessionID: &sess, Status: domain.IntentCreated}
	intents := &stubIntents{existing: existing}
	cf := &stubCF{}
	svc := New(intents, cf)
	due := &domain.Due{ID: uuid.New(), DueCode: "ABC123", Amount: 200, Status: domain.DueStatusPending}
	prop := &domain.Property{PaymentMode: domain.PaymentModeCashfree}
	intent, _, err := svc.PayIntent(context.Background(), due, prop, "", "/x", "9999999999")
	if err != nil {
		t.Fatal(err)
	}
	if cf.n != 1 || intent.PaymentSessionID != "sess_1" {
		t.Fatalf("stale amount must create a new order: n=%d session=%s", cf.n, intent.PaymentSessionID)
	}
}

func TestEnsureCashfreeReusesMatchingAmount(t *testing.T) {
	sess := "keep_sess"
	existing := &domain.PaymentIntent{AmountPaise: 100, PaymentSessionID: &sess, Status: domain.IntentCreated}
	intents := &stubIntents{existing: existing}
	cf := &stubCF{}
	svc := New(intents, cf)
	due := &domain.Due{ID: uuid.New(), DueCode: "ABC123", Amount: 100, Status: domain.DueStatusPending}
	prop := &domain.Property{PaymentMode: domain.PaymentModeCashfree}
	intent, _, err := svc.PayIntent(context.Background(), due, prop, "", "/x", "9999999999")
	if err != nil {
		t.Fatal(err)
	}
	if cf.n != 0 || intent.PaymentSessionID != "keep_sess" {
		t.Fatalf("matching amount must reuse session: n=%d session=%s", cf.n, intent.PaymentSessionID)
	}
}

func TestPNGURL(t *testing.T) {
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	if got := PNGURL("owner", id); got != "/api/owner/dues/11111111-1111-1111-1111-111111111111/qr" {
		t.Fatalf("unexpected owner PNG URL: %s", got)
	}
	if got := PNGURL("tenant", id); got != "/api/tenant/dues/11111111-1111-1111-1111-111111111111/qr" {
		t.Fatalf("unexpected tenant PNG URL: %s", got)
	}
}

type lockingIntentStore struct {
	reusable        *domain.PaymentIntent
	superseded      bool
	createdWithDues bool
}

func (s *lockingIntentStore) Create(context.Context, *domain.PaymentIntent) error { return nil }
func (s *lockingIntentStore) LatestOpenForDue(context.Context, uuid.UUID) (*domain.PaymentIntent, error) {
	return nil, pgx.ErrNoRows
}
func (s *lockingIntentStore) GetReusableIntentUnderLock(_ context.Context, _, _ uuid.UUID, minRemaining time.Duration) (*domain.PaymentIntent, error) {
	if s.reusable != nil {
		if s.reusable.ExpiresAt != nil && s.reusable.ExpiresAt.Before(time.Now().Add(minRemaining)) {
			return nil, pgx.ErrNoRows
		}
		return s.reusable, nil
	}
	return nil, pgx.ErrNoRows
}
func (s *lockingIntentStore) SupersedeOpenIntentsForDue(context.Context, uuid.UUID) error {
	s.superseded = true
	return nil
}
func (s *lockingIntentStore) CreateWithDues(_ context.Context, _ *domain.PaymentIntent, _ []uuid.UUID, _ []int64) error {
	s.createdWithDues = true
	return nil
}

func TestEnsureCashfreeUnderLockReuse(t *testing.T) {
	sess := "reused_sess"
	exp := time.Now().Add(25 * time.Minute)
	store := &lockingIntentStore{
		reusable: &domain.PaymentIntent{AmountPaise: 500000, PaymentSessionID: &sess, Status: domain.IntentCreated, ExpiresAt: &exp},
	}
	cf := &stubCF{}
	svc := New(store, cf)
	due := &domain.Due{ID: uuid.New(), TenantID: uuid.New(), DueCode: "ABC123", Amount: 500000, Status: domain.DueStatusPending}
	prop := &domain.Property{PaymentMode: domain.PaymentModeCashfree}

	intent, _, err := svc.PayIntent(context.Background(), due, prop, "", "", "9876543210")
	if err != nil {
		t.Fatal(err)
	}
	if cf.n != 0 || intent.PaymentSessionID != "reused_sess" {
		t.Fatalf("expected reuse under lock: cf.n=%d session=%s", cf.n, intent.PaymentSessionID)
	}
	if store.superseded {
		t.Fatal("should not supersede when intent is reusable")
	}
}

func TestEnsureCashfreeNearExpiryCreatesNewOrder(t *testing.T) {
	sess := "expiring_soon_sess"
	exp := time.Now().Add(5 * time.Minute) // < 10m remaining!
	store := &lockingIntentStore{
		reusable: &domain.PaymentIntent{AmountPaise: 500000, PaymentSessionID: &sess, Status: domain.IntentCreated, ExpiresAt: &exp},
	}
	cf := &stubCF{}
	svc := New(store, cf)
	due := &domain.Due{ID: uuid.New(), TenantID: uuid.New(), DueCode: "ABC123", Amount: 500000, Status: domain.DueStatusPending}
	prop := &domain.Property{PaymentMode: domain.PaymentModeCashfree}

	intent, _, err := svc.PayIntent(context.Background(), due, prop, "", "", "9876543210")
	if err != nil {
		t.Fatal(err)
	}
	if cf.n != 1 || intent.PaymentSessionID != "sess_1" {
		t.Fatalf("expected new order created when < 10m remaining: cf.n=%d session=%s", cf.n, intent.PaymentSessionID)
	}
	if !store.superseded {
		t.Fatal("expected older intent to be superseded under lock")
	}
	if !store.createdWithDues {
		t.Fatal("expected CreateWithDues to be called")
	}
}

type recoveringIntentMgr struct {
	claimed         *domain.PaymentIntent
	transitioned    bool
	sessionID       string
	failed          bool
	dispatchUnknown bool
}

func (m *recoveringIntentMgr) Create(context.Context, *domain.PaymentIntent) error { return nil }
func (m *recoveringIntentMgr) LatestOpenForDue(context.Context, uuid.UUID) (*domain.PaymentIntent, error) {
	return nil, pgx.ErrNoRows
}
func (m *recoveringIntentMgr) ClaimInitiatingIntent(ctx context.Context, tenantID uuid.UUID, dueIDs []uuid.UUID, amounts []int64, totalAmountPaise int64, orderID string, minRemaining time.Duration) (*domain.PaymentIntent, bool, bool, error) {
	m.claimed = &domain.PaymentIntent{
		ID:              uuid.New(),
		ProviderOrderID: orderID,
		AmountPaise:     totalAmountPaise,
		Status:          domain.IntentInitiating,
		IdempotencyKey:  uuid.New(),
	}
	return m.claimed, false, false, nil
}
func (m *recoveringIntentMgr) TransitionCreated(ctx context.Context, intentID uuid.UUID, sessionID string, expiresAt *time.Time) error {
	m.transitioned = true
	m.sessionID = sessionID
	return nil
}
func (m *recoveringIntentMgr) TransitionFailed(ctx context.Context, intentID uuid.UUID) error {
	m.failed = true
	return nil
}
func (m *recoveringIntentMgr) TransitionDispatchUnknown(ctx context.Context, intentID uuid.UUID) error {
	m.dispatchUnknown = true
	return nil
}
func (m *recoveringIntentMgr) GetByID(ctx context.Context, id uuid.UUID) (*domain.PaymentIntent, error) {
	return m.claimed, nil
}

type timingOutCF struct {
	createCalls   int
	getOrderCalls int
	activeOrder   *cashfree.OrderDetails
	getOrderErr   error
	createErr     error
	capturedIdem  string
}

func (c *timingOutCF) CreateUPIOrder(ctx context.Context, orderID string, amountPaise int64, customerPhone, note string, idempotencyKey ...string) (string, *time.Time, error) {
	c.createCalls++
	if len(idempotencyKey) > 0 {
		c.capturedIdem = idempotencyKey[0]
	}
	if c.createErr != nil {
		return "", nil, c.createErr
	}
	return "", nil, errors.New("gateway timeout 504")
}

func (c *timingOutCF) GetOrder(ctx context.Context, orderID string) (*cashfree.OrderDetails, error) {
	c.getOrderCalls++
	return c.activeOrder, c.getOrderErr
}

func TestEnsureCashfree_RecoversActiveOrderOnCreateTimeout(t *testing.T) {
	exp := time.Now().Add(30 * time.Minute)
	cf := &timingOutCF{
		activeOrder: &cashfree.OrderDetails{
			OrderID:          "test-order-1",
			OrderStatus:      "ACTIVE",
			PaymentSessionID: "recovered_session_123",
			ExpiresAt:        &exp,
		},
	}
	mgr := &recoveringIntentMgr{}
	svc := New(mgr, cf)

	due := &domain.Due{ID: uuid.New(), TenantID: uuid.New(), DueCode: "DUE999", Amount: 120000, Status: domain.DueStatusPending}
	prop := &domain.Property{PaymentMode: domain.PaymentModeCashfree}

	intent, _, err := svc.PayIntent(context.Background(), due, prop, "", "", "9876543210")
	if err != nil {
		t.Fatalf("expected recovered session, got err: %v", err)
	}
	if intent.PaymentSessionID != "recovered_session_123" {
		t.Fatalf("expected recovered_session_123, got: %s", intent.PaymentSessionID)
	}
	if !mgr.transitioned || mgr.failed {
		t.Fatalf("expected transition to created, transitioned=%v failed=%v", mgr.transitioned, mgr.failed)
	}
	if cf.getOrderCalls != 1 {
		t.Fatalf("expected 1 GetOrder call, got: %d", cf.getOrderCalls)
	}
}

func TestEnsureCashfree_PreservesDispatchUnknownOnUnconfirmedNotFound(t *testing.T) {
	cf := &timingOutCF{
		getOrderErr: &cashfree.APIError{StatusCode: 404, Status: "404 Not Found"},
	}
	mgr := &recoveringIntentMgr{}
	svc := New(mgr, cf)

	due := &domain.Due{ID: uuid.New(), TenantID: uuid.New(), DueCode: "DUE888", Amount: 120000, Status: domain.DueStatusPending}
	prop := &domain.Property{PaymentMode: domain.PaymentModeCashfree}

	_, _, err := svc.PayIntent(context.Background(), due, prop, "", "", "9876543210")
	if err == nil {
		t.Fatal("expected error on failed create with 404")
	}
	if !mgr.dispatchUnknown {
		t.Fatal("expected intent to be transitioned to dispatch_unknown on ambiguous 404")
	}
	if mgr.failed {
		t.Fatal("intent must NOT be transitioned to failed on ambiguous 404")
	}
}

func TestEnsureCashfree_MarksFailedOnConfirmedExpiredOrder(t *testing.T) {
	cf := &timingOutCF{
		activeOrder: &cashfree.OrderDetails{
			OrderID:     "test-order-expired",
			OrderStatus: "EXPIRED",
		},
	}
	mgr := &recoveringIntentMgr{}
	svc := New(mgr, cf)

	due := &domain.Due{ID: uuid.New(), TenantID: uuid.New(), DueCode: "DUE777", Amount: 120000, Status: domain.DueStatusPending}
	prop := &domain.Property{PaymentMode: domain.PaymentModeCashfree}

	_, _, err := svc.PayIntent(context.Background(), due, prop, "", "", "9876543210")
	if err == nil {
		t.Fatal("expected error on expired order")
	}
	if !mgr.failed {
		t.Fatal("expected intent to be transitioned to failed when Cashfree authoritatively confirmed order EXPIRED")
	}
}

func TestEnsureCashfree_CreateOrder409ReconcilesExistingOrder(t *testing.T) {
	exp := time.Now().Add(30 * time.Minute)
	cf := &timingOutCF{
		createErr: &cashfree.APIError{StatusCode: 409, Status: "409 Conflict: order_already_exists"},
		activeOrder: &cashfree.OrderDetails{
			OrderID:          "test-order-conflict",
			OrderStatus:      "ACTIVE",
			PaymentSessionID: "conflict_reconciled_session",
			ExpiresAt:        &exp,
		},
	}
	mgr := &recoveringIntentMgr{}
	svc := New(mgr, cf)

	due := &domain.Due{ID: uuid.New(), TenantID: uuid.New(), DueCode: "DUE409", Amount: 200000, Status: domain.DueStatusPending}
	prop := &domain.Property{PaymentMode: domain.PaymentModeCashfree}

	intent, _, err := svc.PayIntent(context.Background(), due, prop, "", "", "9876543210")
	if err != nil {
		t.Fatalf("expected successful reconciliation on 409, got err: %v", err)
	}
	if intent.PaymentSessionID != "conflict_reconciled_session" {
		t.Fatalf("expected conflict_reconciled_session, got: %s", intent.PaymentSessionID)
	}
	if !mgr.transitioned {
		t.Fatal("expected intent to transition to created after 409 reconciliation")
	}
	if mgr.failed || mgr.dispatchUnknown {
		t.Fatalf("unexpected failure or dispatch_unknown state: failed=%v dispatch_unknown=%v", mgr.failed, mgr.dispatchUnknown)
	}
}

func TestEnsureCashfree_CreateOrder400MarksFailed(t *testing.T) {
	cf := &timingOutCF{
		createErr: &cashfree.APIError{StatusCode: 400, Status: "400 Bad Request: invalid_phone_number"},
	}
	mgr := &recoveringIntentMgr{}
	svc := New(mgr, cf)

	due := &domain.Due{ID: uuid.New(), TenantID: uuid.New(), DueCode: "DUE400", Amount: 200000, Status: domain.DueStatusPending}
	prop := &domain.Property{PaymentMode: domain.PaymentModeCashfree}

	_, _, err := svc.PayIntent(context.Background(), due, prop, "", "", "9876543210")
	if err == nil {
		t.Fatal("expected error on 400 client error")
	}
	if !mgr.failed {
		t.Fatal("expected intent to be transitioned to failed on authoritative 400 client rejection")
	}
	if mgr.dispatchUnknown {
		t.Fatal("intent must not be transitioned to dispatch_unknown on authoritative 400")
	}
}

func TestEnsureCashfree_GetOrder404RetainsDispatchUnknown(t *testing.T) {
	cf := &timingOutCF{
		createErr:   errors.New("connection timeout"),
		getOrderErr: &cashfree.APIError{StatusCode: 404, Status: "404 Not Found: order_not_found"},
	}
	mgr := &recoveringIntentMgr{}
	svc := New(mgr, cf)

	due := &domain.Due{ID: uuid.New(), TenantID: uuid.New(), DueCode: "DUERECON404", Amount: 200000, Status: domain.DueStatusPending}
	prop := &domain.Property{PaymentMode: domain.PaymentModeCashfree}

	_, _, err := svc.PayIntent(context.Background(), due, prop, "", "", "9876543210")
	if err == nil {
		t.Fatal("expected error on connection timeout")
	}
	if mgr.failed {
		t.Fatal("GetOrder 404 during reconciliation must NOT mark intent failed (replication lag risk)")
	}
	if !mgr.dispatchUnknown {
		t.Fatal("expected intent to remain in dispatch_unknown for durable poller recovery")
	}
}

func TestEnsureCashfree_PassesPersistedIdempotencyKey(t *testing.T) {
	cf := &timingOutCF{}
	mgr := &recoveringIntentMgr{}
	svc := New(mgr, cf)

	due := &domain.Due{ID: uuid.New(), TenantID: uuid.New(), DueCode: "DUEIDEM", Amount: 150000, Status: domain.DueStatusPending}
	prop := &domain.Property{PaymentMode: domain.PaymentModeCashfree}

	_, _, err := svc.PayIntent(context.Background(), due, prop, "", "", "9876543210")
	// Note: timingOutCF times out, but we check if capturedIdem matches mgr.claimed.IdempotencyKey
	if cf.capturedIdem == "" {
		t.Fatal("expected idempotency key to be passed to CreateUPIOrder")
	}
	if cf.capturedIdem != mgr.claimed.IdempotencyKey.String() {
		t.Fatalf("expected idempotency key %s, got %s", mgr.claimed.IdempotencyKey.String(), cf.capturedIdem)
	}
	_ = err
}
