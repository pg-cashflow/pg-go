package jobs

import (
	"context"

	"github.com/pg-cashflow/pg-go/internal/waitlist"
)

type WaitlistOfferExpiryJob struct {
	svc *waitlist.Service
}

func NewWaitlistOfferExpiryJob(svc *waitlist.Service) *WaitlistOfferExpiryJob {
	return &WaitlistOfferExpiryJob{svc: svc}
}

func (j *WaitlistOfferExpiryJob) Run(ctx context.Context) (int, error) {
	if j.svc == nil {
		return 0, nil
	}
	return j.svc.ExpireOffers(ctx)
}
