package payment

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

var (
	ErrAmbiguous = errors.New("payment: ambiguous amount/date match")
	ErrNoMatch   = errors.New("payment: no matching due")
)

var dueCodeInNote = regexp.MustCompile(`(?i)PG-([0-9A-Z]{6})`)

// DueCodeMatcher extracts PG-XXXXXX from a bank note and looks up the due.
type DueCodeMatcher struct {
	dues DueRepository
}

func NewDueCodeMatcher(dues DueRepository) *DueCodeMatcher {
	return &DueCodeMatcher{dues: dues}
}

// ExtractDueCode returns the 6-char due code from note, or empty if absent.
func ExtractDueCode(note string) string {
	m := dueCodeInNote.FindStringSubmatch(note)
	if len(m) < 2 {
		return ""
	}
	return strings.ToUpper(m[1])
}

// Match looks up a due by PG-XXXXXX in note. Returns (nil, nil) if no code present.
func (m *DueCodeMatcher) Match(ctx context.Context, note string) (*domain.Due, error) {
	code := ExtractDueCode(note)
	if code == "" {
		return nil, nil
	}
	due, err := m.dues.GetByDueCode(ctx, code)
	if err != nil {
		return nil, fmt.Errorf("payment: due_code lookup: %w", err)
	}
	return due, nil
}

// AmountDateWindowMatcher finds open dues with matching amount and due_date within ±3 days.
type AmountDateWindowMatcher struct {
	dues DueRepository
}

func NewAmountDateWindowMatcher(dues DueRepository) *AmountDateWindowMatcher {
	return &AmountDateWindowMatcher{dues: dues}
}

const amountDateWindowDays = 3

// Match returns a single candidate due, ErrAmbiguous if more than one, or ErrNoMatch if none.
func (m *AmountDateWindowMatcher) Match(ctx context.Context, propertyID uuid.UUID, amountPaise int64, txnDate time.Time) (*domain.Due, error) {
	day := dateOnly(txnDate)
	from := day.AddDate(0, 0, -amountDateWindowDays)
	to := day.AddDate(0, 0, amountDateWindowDays)
	candidates, err := m.dues.FindByAmountAndDateWindow(ctx, propertyID, amountPaise, from, to)
	if err != nil {
		return nil, err
	}
	switch len(candidates) {
	case 0:
		return nil, ErrNoMatch
	case 1:
		d := candidates[0]
		return &d, nil
	default:
		return nil, ErrAmbiguous
	}
}

// Matcher tries due_code first, then amount/date window (co-primary).
type Matcher struct {
	dueCode    *DueCodeMatcher
	amountDate *AmountDateWindowMatcher
}

func NewMatcher(dues DueRepository) *Matcher {
	return &Matcher{
		dueCode:    NewDueCodeMatcher(dues),
		amountDate: NewAmountDateWindowMatcher(dues),
	}
}

// MatchResult is a resolved due with matched_by strategy.
type MatchResult struct {
	Due             *domain.Due
	MatchedBy       domain.MatchedBy
	IsDeterministic bool
}

// Match runs matching: due_code (deterministic), then amount_date_window (heuristic).
func (m *Matcher) Match(ctx context.Context, propertyID uuid.UUID, amountPaise int64, txnDate time.Time, note string) (*MatchResult, error) {
	if due, err := m.dueCode.Match(ctx, note); err != nil {
		return nil, err
	} else if due != nil {
		if due.PropertyID != propertyID {
			return nil, ErrNoMatch
		}
		if due.Status != domain.DueStatusPending && due.Status != domain.DueStatusPartial {
			return nil, ErrNoMatch
		}
		return &MatchResult{Due: due, MatchedBy: domain.MatchedByDueCode, IsDeterministic: true}, nil
	}

	due, err := m.amountDate.Match(ctx, propertyID, amountPaise, txnDate)
	if err != nil {
		return nil, err
	}
	return &MatchResult{Due: due, MatchedBy: domain.MatchedByAmountDateWindow, IsDeterministic: false}, nil
}

func dateOnly(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}
