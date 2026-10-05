package domain

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Floor represents a physical level of a building.
type Floor struct {
	ID          uuid.UUID `json:"id"`
	PropertyID  uuid.UUID `json:"property_id"`
	FloorNumber int       `json:"floor_number"`
	Name        string    `json:"name"`
	CreatedAt   time.Time `json:"created_at"`
}

// Room represents an accommodation unit on a floor.
type Room struct {
	ID            uuid.UUID `json:"id"`
	PropertyID    uuid.UUID `json:"property_id"`
	FloorId       uuid.UUID `json:"floor_id"`
	RoomNumber    string    `json:"room_number"`
	Capacity      int16     `json:"capacity"`
	IncludedUnits int       `json:"included_units"` // included monthly kWh
	CreatedAt     time.Time `json:"created_at"`
}

// PropertyGamificationSettings holds property-wide gamification rules and budget caps.
type PropertyGamificationSettings struct {
	PropertyID             uuid.UUID `json:"property_id"`
	PointValuePaise        int64     `json:"point_value_paise"`
	MonthlyBudgetPaise     int64     `json:"monthly_budget_paise"`
	EarnCapPerTenant       int       `json:"earn_cap_per_tenant"`
	RSVPSubCap             int       `json:"rsvp_sub_cap"`
	ExpiryDays             int       `json:"expiry_days"`
	FloorBonusThreshold    int       `json:"floor_bonus_threshold"` // percentage e.g. 85
	ElectricityTariffPaise int64     `json:"electricity_tariff_paise"`
	GraceDays              int       `json:"grace_days"`
	LatePenaltyPointsPerDay int      `json:"late_penalty_points_per_day"`
	LatePenaltyMaxPoints   int       `json:"late_penalty_max_points"`
	CreatedAt              time.Time `json:"created_at"`
	UpdatedAt              time.Time `json:"updated_at"`
}

// PointRule defines a rule under which points can be awarded.
type PointRule struct {
	ID          uuid.UUID `json:"id"`
	PropertyID  uuid.UUID `json:"property_id"`
	Code        string    `json:"code"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Points      int       `json:"points"`
	MonthlyCap  int       `json:"monthly_cap"`
	IsRSVP      bool      `json:"is_rsvp"`
	Active      bool      `json:"active"`
	CreatedAt   time.Time `json:"created_at"`
}

// PointsLedgerEntry represents an immutable point debit or credit.
type PointsLedgerEntry struct {
	ID         int64      `json:"id"`
	TenantID   uuid.UUID  `json:"tenant_id"`
	PropertyID uuid.UUID  `json:"property_id"`
	RuleCode   string     `json:"rule_code"`
	Delta      int        `json:"delta"`
	RefType    *string    `json:"ref_type,omitempty"`
	RefID      *string    `json:"ref_id,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"` // null for negative deltas
	CreatedBy  *uuid.UUID `json:"created_by,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
}

// TenantStreak records on-time payment streaks and freeze state.
type TenantStreak struct {
	TenantID          uuid.UUID  `json:"tenant_id"`
	PropertyID        uuid.UUID  `json:"property_id"`
	OnTimeMonths      int        `json:"on_time_months"`
	CachedBalance     int        `json:"cached_balance"`
	LastOnTimeDueID   *uuid.UUID `json:"last_on_time_due_id,omitempty"`
	FreezesAvailable  int16      `json:"freezes_available"`
	LastFreezeUsedAt  *time.Time `json:"last_freeze_used_at,omitempty"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

// RewardsCatalogItem is a redeemable perk or credit.
type RewardsCatalogItem struct {
	ID              uuid.UUID       `json:"id"`
	PropertyID      uuid.UUID       `json:"property_id"`
	Code            string          `json:"code"`
	Title           string          `json:"title"`
	Description     string          `json:"description"`
	Category        string          `json:"category"` // cash_credit, food_coupon, perk
	PointsCost      int             `json:"points_cost"`
	MinTenureMonths int             `json:"min_tenure_months"`
	IsActive        bool            `json:"is_active"`
	Metadata        json.RawMessage `json:"metadata"`
	CreatedAt       time.Time       `json:"created_at"`
}

// Redemption records points spent on a reward.
type Redemption struct {
	ID           uuid.UUID       `json:"id"`
	TenantID     uuid.UUID       `json:"tenant_id"`
	PropertyID   uuid.UUID       `json:"property_id"`
	RewardID     uuid.UUID       `json:"reward_id"`
	PointsSpent  int             `json:"points_spent"`
	Status       string          `json:"status"` // completed, cancelled
	AppliedDueID *uuid.UUID      `json:"applied_due_id,omitempty"`
	CouponCode   *string         `json:"coupon_code,omitempty"`
	Metadata     json.RawMessage `json:"metadata,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at"`
}

// Inspection is a room, floor, or common area review.
type Inspection struct {
	ID              uuid.UUID        `json:"id"`
	PropertyID      uuid.UUID        `json:"property_id"`
	RoomID          *uuid.UUID       `json:"room_id,omitempty"`
	FloorID         *uuid.UUID       `json:"floor_id,omitempty"`
	InspectorUserID uuid.UUID        `json:"inspector_user_id"`
	InspectionType  string           `json:"inspection_type"` // room, floor, common_bathroom
	ScorePercent    int              `json:"score_percent"`
	Passed          bool             `json:"passed"`
	Notes           string           `json:"notes"`
	InspectedAt     time.Time        `json:"inspected_at"`
	CreatedAt       time.Time        `json:"created_at"`
	Items           []InspectionItem `json:"items,omitempty"`
}

// InspectionItem is a single checklist line item in an inspection.
type InspectionItem struct {
	ID               uuid.UUID  `json:"id"`
	InspectionID     uuid.UUID  `json:"inspection_id"`
	ItemKey          string     `json:"item_key"`
	Description      string     `json:"description"`
	Passed           bool       `json:"passed"`
	HasPhoto         bool       `json:"has_photo"`
	PhotoBytes       []byte     `json:"-"`
	Notes            string     `json:"notes"`
	DisputedAt       *time.Time `json:"disputed_at,omitempty"`
	DisputeNote      *string    `json:"dispute_note,omitempty"`
	ResolvedAt       *time.Time `json:"resolved_at,omitempty"`
	ResolvedBy       *uuid.UUID `json:"resolved_by,omitempty"`
	ResolutionStatus string     `json:"resolution_status"` // none, disputed, upheld, overturned
}

// VendorInspection audits kitchen / mess operations.
type VendorInspection struct {
	ID              uuid.UUID `json:"id"`
	PropertyID      uuid.UUID `json:"property_id"`
	InspectorUserID uuid.UUID `json:"inspector_user_id"`
	VendorName      string    `json:"vendor_name"`
	InspectionType  string    `json:"inspection_type"`
	ScorePercent    int       `json:"score_percent"`
	Notes           string    `json:"notes"`
	HasPhoto        bool      `json:"has_photo"`
	PhotoBytes      []byte    `json:"-"`
	PenaltyPaise    int64     `json:"penalty_paise"`
	InspectedAt     time.Time `json:"inspected_at"`
	CreatedAt       time.Time `json:"created_at"`
}

// Violation records a house rule infraction.
type Violation struct {
	ID             uuid.UUID  `json:"id"`
	TenantID       uuid.UUID  `json:"tenant_id"`
	PropertyID     uuid.UUID  `json:"property_id"`
	RuleCode       string     `json:"rule_code"`
	Severity       string     `json:"severity"` // safety, lifestyle
	Step           int16      `json:"step"`     // 1: notice, 2: deduction, 3: warning
	Description    string     `json:"description"`
	HasEvidence    bool       `json:"has_evidence"`
	EvidenceBytes  []byte     `json:"-"`
	AcknowledgedAt *time.Time `json:"acknowledged_at,omitempty"`
	CreatedBy      uuid.UUID  `json:"created_by"`
	CreatedAt      time.Time  `json:"created_at"`
}

// HazardReport is a private/anonymous hazard or maintenance ticket.
type HazardReport struct {
	ID                  uuid.UUID  `json:"id"`
	PropertyID          uuid.UUID  `json:"property_id"`
	ReportedByTenantID  uuid.UUID  `json:"-"` // anonymous to peers
	Category            string     `json:"category"`
	Description         string     `json:"description"`
	HasPhoto            bool       `json:"has_photo"`
	PhotoBytes          []byte     `json:"-"`
	Status              string     `json:"status"` // open, in_progress, resolved, rejected
	ResolvedAt          *time.Time `json:"resolved_at,omitempty"`
	ResolvedBy          *uuid.UUID `json:"resolved_by,omitempty"`
	PointsAwarded       bool       `json:"points_awarded"`
	CreatedAt           time.Time  `json:"created_at"`
}

// MeterReading records a room electricity or floor water meter metric.
type MeterReading struct {
	ID           uuid.UUID `json:"id"`
	PropertyID   uuid.UUID `json:"property_id"`
	RoomID       *uuid.UUID `json:"room_id,omitempty"`
	FloorID      *uuid.UUID `json:"floor_id,omitempty"`
	Kind         string    `json:"kind"` // electricity, water
	ReadingValue float64   `json:"reading_value"`
	ReadingAt    time.Time `json:"reading_at"`
	Source       string    `json:"source"` // manual, device
	RecordedBy   uuid.UUID `json:"recorded_by"`
	CreatedAt    time.Time `json:"created_at"`
}

// MealRSVP tracks student presence for mess headcount and food waste reduction.
type MealRSVP struct {
	ID         uuid.UUID `json:"id"`
	TenantID   uuid.UUID `json:"tenant_id"`
	PropertyID uuid.UUID `json:"property_id"`
	MealDate   time.Time `json:"meal_date"` // date only
	MealSlot   string    `json:"meal_slot"` // breakfast, lunch, dinner
	Attending  bool      `json:"attending"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// MenuPoll allows residents to vote on monthly mess menu options.
type MenuPoll struct {
	ID         uuid.UUID       `json:"id"`
	PropertyID uuid.UUID       `json:"property_id"`
	MonthYear  string          `json:"month_year"` // YYYY-MM
	Title      string          `json:"title"`
	Options    json.RawMessage `json:"options"`
	ClosedAt   *time.Time      `json:"closed_at,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
}

// MenuVote records a tenant's vote on a menu poll.
type MenuVote struct {
	ID        uuid.UUID `json:"id"`
	PollID    uuid.UUID `json:"poll_id"`
	TenantID  uuid.UUID `json:"tenant_id"`
	OptionID  string    `json:"option_id"`
	CreatedAt time.Time `json:"created_at"`
}

// Referral tracks tenant referrals for move-in bonuses.
type Referral struct {
	ID               uuid.UUID  `json:"id"`
	PropertyID       uuid.UUID  `json:"property_id"`
	ReferrerTenantID uuid.UUID  `json:"referrer_tenant_id"`
	ReferredTenantID *uuid.UUID `json:"referred_tenant_id,omitempty"`
	Phone            string     `json:"phone"`
	Name             string     `json:"name"`
	Status           string     `json:"status"` // pending, moved_in, rewarded
	PointsAwarded    int        `json:"points_awarded"`
	CreatedAt        time.Time  `json:"created_at"`
	RewardedAt       *time.Time `json:"rewarded_at,omitempty"`
}
