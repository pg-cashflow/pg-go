package finance

import (
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type LineSpec struct {
	Account  string
	Debit    int64
	Credit   int64
	LineKind string
}

func MakeLines(propertyID, sourceID uuid.UUID, sourceType string, occurredAt time.Time, specs []LineSpec) ([]domain.JournalLine, error) {
	var debit, credit int64
	out := make([]domain.JournalLine, 0, len(specs))
	for _, s := range specs {
		if (s.Debit > 0 && s.Credit > 0) || (s.Debit == 0 && s.Credit == 0) {
			return nil, ErrUnbalancedJournal
		}
		debit += s.Debit
		credit += s.Credit
		out = append(out, domain.JournalLine{
			ID:          uuid.New(),
			PropertyID:  propertyID,
			AccountCode: s.Account,
			DebitPaise:  s.Debit,
			CreditPaise: s.Credit,
			SourceType:  sourceType,
			SourceID:    sourceID,
			LineKind:    s.LineKind,
			OccurredAt:  occurredAt,
		})
	}
	if debit != credit || debit == 0 {
		return nil, ErrUnbalancedJournal
	}
	return out, nil
}

func AccountSum(lines []domain.JournalLine, account string) (debit, credit int64) {
	for _, l := range lines {
		if l.AccountCode == account {
			debit += l.DebitPaise
			credit += l.CreditPaise
		}
	}
	return
}

func nextCapitalRef(n int) string {
	return fmt.Sprintf("INV-%04d", n+1)
}
