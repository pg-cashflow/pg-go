package jobs

import (
	"context"

	"github.com/pg-cashflow/pg-go/internal/slotinvite"
)

// SlotInviteExpiryJob marks overdue open invites as expired and releases bed reservations.
type SlotInviteExpiryJob struct {
	svc *slotinvite.Service
}

func NewSlotInviteExpiryJob(svc *slotinvite.Service) *SlotInviteExpiryJob {
	return &SlotInviteExpiryJob{svc: svc}
}

func (j *SlotInviteExpiryJob) Run(ctx context.Context) (int64, error) {
	return j.svc.ExpireDue(ctx)
}
