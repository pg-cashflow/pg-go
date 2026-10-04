package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/joho/godotenv"
	"github.com/pg-cashflow/pg-go/internal/attendance"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/config"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/postgres"
)

func TestLiveAttendanceAndPayrollHTTPFlow(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live postgres test in short mode")
	}
	_ = godotenv.Load("../../.env")
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set, skipping live Postgres test")
	}

	cfg, err := config.Load()
	if err != nil {
		t.Skip("config load failed, skipping live Postgres test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	pool, err := postgres.NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		t.Skipf("cannot connect to Postgres (%v), skipping live test", err)
	}
	defer pool.Close()

	_ = postgres.Migrate(ctx, pool, filepath.Join("..", "..", "migrations"))
	_, _ = pool.Exec(ctx, `ALTER TABLE wage_calculations ADD COLUMN IF NOT EXISTS days_unrecorded NUMERIC(4, 1) NOT NULL DEFAULT 0`)

	propID := uuid.New()
	inviteCode := fmt.Sprintf("A%s", uuid.New().String()[:7])
	ownerPhone := fmt.Sprintf("+91%010d", (time.Now().UnixNano()+4)%10000000000)
	_, err = pool.Exec(ctx, `
		INSERT INTO properties (id, name, address, owner_phone, upi_vpa, owner_name, owner_email, invite_code)
		VALUES ($1, 'Attendance HTTP Property', '789 Staff Ave', $2, 'owner@upi', 'Staff Owner', 'owner@test.com', $3)`,
		propID, ownerPhone, inviteCode,
	)
	if err != nil {
		t.Fatalf("insert property: %v", err)
	}
	defer func() {
		_, _ = pool.Exec(ctx, `DELETE FROM properties WHERE id = $1`, propID)
	}()

	ownerID := uuid.New()
	userPhone := fmt.Sprintf("+91%010d", (time.Now().UnixNano()+5)%10000000000)
	_, err = pool.Exec(ctx, `
		INSERT INTO users (id, phone, role, property_id)
		VALUES ($1, $2, 'owner', $3)`, ownerID, userPhone, propID,
	)
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
	defer func() {
		_, _ = pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, ownerID)
	}()

	// Create payee
	payeeID := uuid.New()
	dummyHash := domain.ComputeAccountHash([]byte("test_salt_123"), uuid.New().String())
	payeePhone := fmt.Sprintf("+91%010d", (time.Now().UnixNano()+6)%10000000000)
	_, err = pool.Exec(ctx, `
		INSERT INTO payout_payees (
			id, property_id, payee_type, name, phone, account_number_hash, upi_vpa
		) VALUES ($1, $2, 'staff', 'Sunil Security', $3, $4, 'sunil@upi')
		ON CONFLICT DO NOTHING;
	`, payeeID, propID, payeePhone, dummyHash)
	if err != nil {
		t.Fatalf("insert payee: %v", err)
	}
	defer func() {
		_, _ = pool.Exec(ctx, `DELETE FROM payout_payees WHERE id = $1`, payeeID)
	}()

	attendanceRepo := postgres.NewAttendanceRepo(pool)
	payoutRepo := postgres.NewPayoutRepo(pool)
	attendanceSvc := attendance.NewService(pool, attendanceRepo, payoutRepo)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	h := &Handlers{
		Deps: Deps{
			Pool:           pool,
			PayoutRepo:     payoutRepo,
			AttendanceRepo: attendanceRepo,
			AttendanceSvc:  attendanceSvc,
		},
	}

	owner := router.Group("/owner", func(c *gin.Context) {
		c.Set(auth.ContextClaimsKey, &auth.Claims{
			UserID:     ownerID,
			PropertyID: &propID,
			Role:       domain.RoleOwner,
		})
	})
	{
		owner.POST("/staff", h.OwnerCreateStaffProfile)
		owner.GET("/staff", h.OwnerListStaffProfiles)
		owner.PUT("/staff/:id/status", h.OwnerUpdateStaffProfileStatus)

		owner.GET("/attendance/leave-policy", h.OwnerGetLeavePolicy)
		owner.PUT("/attendance/leave-policy", h.OwnerUpdateLeavePolicy)
		owner.POST("/attendance/daily", h.OwnerMarkDailyAttendance)
		owner.GET("/attendance/monthly", h.OwnerListMonthlyAttendance)

		owner.POST("/payroll/calculate", h.OwnerPreviewPayroll)
		owner.POST("/payroll/finalize", h.OwnerFinalizePayroll)
	}

	// 1a. Attempt to create staff profile with non-existent or cross-property payee_id (IDOR guard)
	foreignPayeeBody := map[string]interface{}{
		"payee_id":                uuid.New().String(),
		"name":                    "Imposter Staff",
		"role":                    "Infiltrator",
		"base_monthly_wage_paise": 100000,
		"effective_from":          "2026-09-01",
	}
	foreignPayload, _ := json.Marshal(foreignPayeeBody)
	wBadPayee := httptest.NewRecorder()
	reqBadPayee, _ := http.NewRequest(http.MethodPost, "/owner/staff", bytes.NewReader(foreignPayload))
	reqBadPayee.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(wBadPayee, reqBadPayee)
	if wBadPayee.Code != http.StatusNotFound {
		t.Fatalf("expected 404 Not Found on cross-property/non-existent payee, got %d (body: %s)", wBadPayee.Code, wBadPayee.Body.String())
	}

	// 1b. Create Staff Profile
	createBody := map[string]interface{}{
		"payee_id":                payeeID.String(),
		"name":                    "Sunil Security",
		"role":                    "Night Watchman",
		"base_monthly_wage_paise": 2400000, // Rs 24,000 / month
		"effective_from":          "2026-09-01",
	}
	payload, _ := json.Marshal(createBody)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/owner/staff", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created on POST /owner/staff, got %d (body: %s)", w.Code, w.Body.String())
	}
	var createdStaff domain.StaffProfile
	_ = json.Unmarshal(w.Body.Bytes(), &createdStaff)
	if createdStaff.Role != "Night Watchman" {
		t.Errorf("expected role 'Night Watchman', got %s", createdStaff.Role)
	}

	// 2. Update Leave Policy
	policyBody := map[string]interface{}{
		"monthly_free_leave_days": 2,
		"paid_holidays":           []string{"2026-09-05"},
		"working_days_basis":      "calendar_days",
	}
	payload, _ = json.Marshal(policyBody)
	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodPut, "/owner/attendance/leave-policy", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on PUT /owner/attendance/leave-policy, got %d", w.Code)
	}

	// 3a. Reject attendance for non-existent or foreign staff
	badAttReq := domain.MarkDailyAttendanceRequest{
		WorkDate: "2026-09-01",
		Entries: []domain.DailyAttendanceEntry{
			{StaffID: uuid.New(), Status: domain.AttendancePresent},
		},
	}
	pBad, _ := json.Marshal(badAttReq)
	wBadAtt := httptest.NewRecorder()
	rBadAtt, _ := http.NewRequest(http.MethodPost, "/owner/attendance/daily", bytes.NewReader(pBad))
	rBadAtt.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(wBadAtt, rBadAtt)
	if wBadAtt.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request on foreign staff attendance, got %d", wBadAtt.Code)
	}

	// 3b. Mark Daily Attendance (28 days present, 2 days absent)
	for day := 1; day <= 28; day++ {
		wDate := fmt.Sprintf("2026-09-%02d", day)
		attReq := domain.MarkDailyAttendanceRequest{
			WorkDate: wDate,
			Entries: []domain.DailyAttendanceEntry{
				{
					StaffID: createdStaff.ID,
					Status:  domain.AttendancePresent,
				},
			},
		}
		p, _ := json.Marshal(attReq)
		w = httptest.NewRecorder()
		r, _ := http.NewRequest(http.MethodPost, "/owner/attendance/daily", bytes.NewReader(p))
		r.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 OK on POST /owner/attendance/daily, got %d (day %d)", w.Code, day)
		}
	}
	// Day 29 & 30 absent
	for day := 29; day <= 30; day++ {
		wDate := fmt.Sprintf("2026-09-%02d", day)
		attReq := domain.MarkDailyAttendanceRequest{
			WorkDate: wDate,
			Entries: []domain.DailyAttendanceEntry{
				{
					StaffID: createdStaff.ID,
					Status:  domain.AttendanceAbsent,
				},
			},
		}
		p, _ := json.Marshal(attReq)
		w = httptest.NewRecorder()
		r, _ := http.NewRequest(http.MethodPost, "/owner/attendance/daily", bytes.NewReader(p))
		r.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 OK on POST /owner/attendance/daily, got %d (day %d)", w.Code, day)
		}
	}

	// 4. Preview Payroll for 2026-09 (2 absences <= 2 free leaves -> full wage)
	cycleReq := map[string]string{"cycle_month": "2026-09"}
	payload, _ = json.Marshal(cycleReq)
	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodPost, "/owner/payroll/calculate", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on POST /owner/payroll/calculate, got %d", w.Code)
	}

	var previewResp struct {
		Calculations []domain.WageCalculation `json:"calculations"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &previewResp)
	if len(previewResp.Calculations) != 1 {
		t.Fatalf("expected 1 calculation preview, got %d", len(previewResp.Calculations))
	}
	if previewResp.Calculations[0].NetWagePaise != 2400000 {
		t.Errorf("expected 2400000 net wage, got %d", previewResp.Calculations[0].NetWagePaise)
	}

	// 5. Finalize Payroll for 2026-09 -> Should generate an unbatched payout_items entry!
	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodPost, "/owner/payroll/finalize", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on POST /owner/payroll/finalize, got %d (body: %s)", w.Code, w.Body.String())
	}

	var finalizeResp struct {
		Finalized []domain.WageCalculation `json:"finalized"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &finalizeResp)
	if len(finalizeResp.Finalized) != 1 {
		t.Fatalf("expected 1 finalized wage calculation, got %d", len(finalizeResp.Finalized))
	}
	finalCalc := finalizeResp.Finalized[0]
	if finalCalc.PayoutItemID == nil {
		t.Fatal("expected payout_item_id to be populated after finalization")
	}
	if finalCalc.Status != domain.WageStatusBatched {
		t.Errorf("expected status 'batched', got %s", finalCalc.Status)
	}

	// 6. Verify payout_item was created in DB and is unbatched
	item, err := payoutRepo.GetPayoutItemByID(ctx, *finalCalc.PayoutItemID)
	if err != nil {
		t.Fatalf("failed to retrieve generated payout item: %v", err)
	}
	if item.BatchID != nil {
		t.Errorf("expected batch_id to be nil (unbatched), got %v", item.BatchID)
	}
	if item.AmountPaise != 2400000 {
		t.Errorf("expected amount 2400000, got %d", item.AmountPaise)
	}
	if item.PayeeID != payeeID {
		t.Errorf("expected payee %s, got %s", payeeID, item.PayeeID)
	}
	if item.Status != domain.PayoutPending {
		t.Errorf("expected status 'pending', got %s", item.Status)
	}
}
