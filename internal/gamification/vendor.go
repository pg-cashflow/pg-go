package gamification

import (
	"context"
	"errors"

	"github.com/pg-cashflow/pg-go/internal/domain"
)

// SubmitVendorInspection logs an audit of kitchen/mess operations with penalty deductions.
func (s *Service) SubmitVendorInspection(ctx context.Context, vi *domain.VendorInspection) (*domain.VendorInspection, error) {
	if vi.VendorName == "" {
		return nil, errors.New("vendor name is required")
	}

	if len(vi.PhotoBytes) > 0 {
		if _, err := s.blobs.ValidateImage(vi.PhotoBytes); err != nil {
			return nil, err
		}
	}

	if err := s.store.CreateVendorInspection(ctx, vi); err != nil {
		return nil, err
	}

	return vi, nil
}
