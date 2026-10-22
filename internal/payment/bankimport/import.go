package bankimport

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/csv"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/payment"
)

// BankWriter stores statement rows.
type BankWriter interface {
	InsertTransaction(ctx context.Context, tx pgx.Tx, txn *domain.BankTransaction) (bool, error)
	UpdateStatus(ctx context.Context, tx pgx.Tx, id uuid.UUID, status domain.BankTransactionStatus, matchedDueID *uuid.UUID, matchedBy *uuid.UUID, matchedAt *time.Time, journalEntryID *uuid.UUID) error
}

// Ledger posts the quarantine journal for a credit row.
type Ledger interface {
	MirrorBankStatementCredit(ctx context.Context, propertyID, bankTxnID uuid.UUID, amountPaise int64, at time.Time) error
}

// Matcher suggests a due for a credit row. It does not settle the due.
type Matcher interface {
	SuggestMatch(ctx context.Context, propertyID uuid.UUID, amountPaise int64, txnDate time.Time, note string) (*payment.MatchResult, error)
}

// Input is one statement import.
type Input struct {
	PropertyID     uuid.UUID
	BankAccountID  *uuid.UUID
	Rows           []csv.Row
	FinanceEnabled bool
}

// Result counts rows by outcome.
type Result struct {
	Matched    int
	Suggested  int
	Unmatched  int
	Duplicates int
	Debits     int
}

// Error is a failed import step. Message is safe to return to the client.
type Error struct {
	Message string
}

func (e *Error) Error() string { return e.Message }

// Import records debit and credit rows. It does not write the import log.
func Import(ctx context.Context, bank BankWriter, ledger Ledger, matcher Matcher, in Input) (Result, error) {
	var out Result
	for _, row := range in.Rows {
		if row.Type == csv.RowTypeDebit {
			if err := importDebit(ctx, bank, in, row, &out); err != nil {
				return out, err
			}
			continue
		}
		if err := importCredit(ctx, bank, ledger, matcher, in, row, &out); err != nil {
			return out, err
		}
	}
	return out, nil
}

func importDebit(ctx context.Context, bank BankWriter, in Input, row csv.Row, out *Result) error {
	classification := "unclassified"
	if row.IsInternalTransfer {
		classification = "internal_transfer"
	}
	debitHash := domain.ComputeBankTxnDedupHash(in.PropertyID, in.BankAccountID, row.Date, row.AmountPaise, "debit", row.TxnID, row.BalancePaise, row.Occurrence)
	debitTxn := &domain.BankTransaction{
		ID:                  uuid.NewSHA1(uuid.NameSpaceOID, []byte(debitHash)),
		PropertyID:          in.PropertyID,
		BankAccountID:       in.BankAccountID,
		TxnID:               row.TxnID,
		AmountPaise:         row.AmountPaise,
		RowType:             "debit",
		TxnDate:             row.Date,
		Narration:           row.Note,
		ClosingBalancePaise: row.BalancePaise,
		OccurrenceIndex:     row.Occurrence,
		DedupHash:           debitHash,
		Status:              domain.BankTxnIgnoredDebit,
		Classification:      classification,
		IsInternalTransfer:  row.IsInternalTransfer,
		PayerPhone:          row.PayerPhone,
		PayerVPA:            row.PayerVPA,
	}
	if bank != nil {
		inserted, err := bank.InsertTransaction(ctx, nil, debitTxn)
		if err != nil {
			slog.Error("ImportStatements saving debit transaction failed", "error", err)
			return &Error{Message: "failed to record debit transaction"}
		}
		if !inserted {
			out.Duplicates++
			return nil
		}
	}
	out.Debits++
	return nil
}

func importCredit(ctx context.Context, bank BankWriter, ledger Ledger, matcher Matcher, in Input, row csv.Row, out *Result) error {
	classification := "unclassified"
	if row.IsReversal {
		classification = "reversal"
	} else if row.IsInternalTransfer {
		classification = "internal_transfer"
	}
	dedupHash := domain.ComputeBankTxnDedupHash(in.PropertyID, in.BankAccountID, row.Date, row.AmountPaise, "credit", row.TxnID, row.BalancePaise, row.Occurrence)
	sourceID := uuid.NewSHA1(uuid.NameSpaceOID, []byte(dedupHash))
	creditTxn := &domain.BankTransaction{
		ID:                  sourceID,
		PropertyID:          in.PropertyID,
		BankAccountID:       in.BankAccountID,
		TxnID:               row.TxnID,
		AmountPaise:         row.AmountPaise,
		RowType:             "credit",
		TxnDate:             row.Date,
		Narration:           row.Note,
		ClosingBalancePaise: row.BalancePaise,
		OccurrenceIndex:     row.Occurrence,
		DedupHash:           dedupHash,
		Status:              domain.BankTxnUnmatched,
		Classification:      classification,
		IsReversal:          row.IsReversal,
		IsInternalTransfer:  row.IsInternalTransfer,
		PayerPhone:          row.PayerPhone,
		PayerVPA:            row.PayerVPA,
	}
	// Calculate suggested match before database insertion so that SuggestedDueID and
	// ConfidenceScore are persisted directly in the INSERT statement.
	isSuggested := false
	if !row.IsReversal && !row.IsInternalTransfer && matcher != nil {
		matchRes, err := matcher.SuggestMatch(ctx, in.PropertyID, row.AmountPaise, row.Date, row.Note)
		if err == nil && matchRes != nil && matchRes.Due != nil {
			isSuggested = true
			creditTxn.Status = domain.BankTxnSuggestedMatch
			creditTxn.SuggestedDueID = &matchRes.Due.ID
			if matchRes.IsDeterministic {
				creditTxn.ConfidenceScore = 1.00
			} else {
				creditTxn.ConfidenceScore = 0.80
			}
		}
	}

	if bank != nil {
		inserted, err := bank.InsertTransaction(ctx, nil, creditTxn)
		if err != nil {
			slog.Error("ImportStatements inserting bank transaction failed", "error", err)
			return &Error{Message: "failed to record bank transaction"}
		}
		if !inserted {
			out.Duplicates++
			return nil
		}
	}
	if ledger != nil && in.FinanceEnabled {
		if err := ledger.MirrorBankStatementCredit(ctx, in.PropertyID, sourceID, row.AmountPaise, row.Date); err != nil {
			slog.Error("ImportStatements ledger mirror quarantine failed", "error", err)
			return &Error{Message: "failed to record financial entry"}
		}
	}
	if isSuggested {
		out.Suggested++
		return nil
	}
	out.Unmatched++
	return nil
}
