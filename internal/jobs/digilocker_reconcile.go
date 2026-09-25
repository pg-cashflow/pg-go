package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/cashfree"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

// DigiLockerReconcileRepo defines the storage operations required for reconciling pending DigiLocker sessions.
type DigiLockerReconcileRepo interface {
	ListPendingDigiLockerVerifications(ctx context.Context) ([]domain.KYCVerification, error)
	FailVerificationTx(ctx context.Context, verificationID uuid.UUID, reason, actor string) error
}

// DigiLockerStatusChecker defines the vendor status poll operation.
type DigiLockerStatusChecker interface {
	GetDigiLockerStatus(ctx context.Context, verificationID string) (*cashfree.DigiLockerStatus, error)
}

// DigiLockerCompleter defines the completion pipeline orchestrator.
type DigiLockerCompleter interface {
	ProcessDigiLockerCompletion(ctx context.Context, vendorRefID, failedReason, actor string) error
}

// DigiLockerReconcileJob performs a periodic sweep of pending DigiLocker sessions:
//   - Sessions aged 5–60 minutes undergo an active status check via Cashfree.
//   - Sessions aged >60 minutes are force-failed with 'consent_window_expired'.
//   - Sessions aged <5 minutes are skipped to grant tenants time to complete the flow.
type DigiLockerReconcileJob struct {
	Repo      DigiLockerReconcileRepo
	Cashfree  DigiLockerStatusChecker
	Completer DigiLockerCompleter
	Now       func() time.Time
	Log       *slog.Logger
}

func (j *DigiLockerReconcileJob) Run(ctx context.Context) error {
	if j.Repo == nil {
		return nil
	}
	log := j.Log
	if log == nil {
		log = slog.Default()
	}

	now := time.Now().UTC()
	if j.Now != nil {
		now = j.Now()
	}

	pending, err := j.Repo.ListPendingDigiLockerVerifications(ctx)
	if err != nil {
		return fmt.Errorf("digilocker reconcile: list pending: %w", err)
	}

	var activeChecked, forceFailed int
	for _, v := range pending {
		age := now.Sub(v.CreatedAt)
		if age > 60*time.Minute {
			// Force-fail sessions older than 60 minutes
			if err := j.Repo.FailVerificationTx(ctx, v.ID, "consent_window_expired", "digilocker_reconcile"); err != nil {
				log.Error("digilocker reconcile: force fail expired session",
					"verification_id", v.ID,
					"vendor_ref_id", v.VendorReferenceID,
					"err", err,
				)
				continue
			}
			forceFailed++
		} else if age >= 5*time.Minute {
			// Active check on sessions aged 5 to 60 minutes
			if j.Cashfree == nil || j.Completer == nil {
				continue
			}
			st, err := j.Cashfree.GetDigiLockerStatus(ctx, v.VendorReferenceID)
			if err != nil {
				log.Warn("digilocker reconcile: status check error",
					"vendor_ref_id", v.VendorReferenceID,
					"err", err,
				)
				continue
			}
			if st == nil {
				continue
			}

			activeChecked++
			if st.Status == "COMPLETED" {
				if err := j.Completer.ProcessDigiLockerCompletion(ctx, v.VendorReferenceID, "", "digilocker_reconcile"); err != nil {
					log.Error("digilocker reconcile: complete session",
						"vendor_ref_id", v.VendorReferenceID,
						"err", err,
					)
				}
			} else if st.Status == "FAILED" {
				reason := st.Message
				if reason == "" {
					reason = "digilocker verification failed"
				}
				if err := j.Completer.ProcessDigiLockerCompletion(ctx, v.VendorReferenceID, reason, "digilocker_reconcile"); err != nil {
					log.Error("digilocker reconcile: fail session",
						"vendor_ref_id", v.VendorReferenceID,
						"err", err,
					)
				}
			}
		}
	}

	log.Info("digilocker reconcile sweep completed",
		"pending_found", len(pending),
		"active_checked", activeChecked,
		"force_failed", forceFailed,
	)
	return nil
}
