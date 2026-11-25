package domain

import "testing"

func TestEvaluatePayoutTransition(t *testing.T) {
	tests := []struct {
		name string
		from PayoutItemStatus
		to   PayoutItemStatus
		want PayoutTransitionDecision
	}{
		{
			name: "processing to succeeded enqueues settlement",
			from: PayoutProcessing,
			to:   PayoutSucceeded,
			want: PayoutTransitionDecision{Accepted: true, EnqueueSettlement: true},
		},
		{
			name: "processing to failed accepts without ledger event",
			from: PayoutProcessing,
			to:   PayoutFailed,
			want: PayoutTransitionDecision{Accepted: true},
		},
		{
			name: "processing to retriable_failed accepts without ledger event",
			from: PayoutProcessing,
			to:   PayoutRetriableFailed,
			want: PayoutTransitionDecision{Accepted: true},
		},
		{
			name: "processing to rejected accepts without ledger event",
			from: PayoutProcessing,
			to:   PayoutRejected,
			want: PayoutTransitionDecision{Accepted: true},
		},
		{
			name: "processing to reversed enqueues settlement before reversal",
			from: PayoutProcessing,
			to:   PayoutReversed,
			want: PayoutTransitionDecision{Accepted: true, EnqueueSettlement: true, EnqueueReversal: true},
		},
		{
			name: "failed to late succeeded is rejected with conflict and no ledger event",
			from: PayoutFailed,
			to:   PayoutSucceeded,
			want: PayoutTransitionDecision{Conflict: true},
		},
		{
			name: "rejected to late succeeded is rejected with conflict and no ledger event",
			from: PayoutRejected,
			to:   PayoutSucceeded,
			want: PayoutTransitionDecision{Conflict: true},
		},
		{
			name: "retriable_failed to late succeeded is rejected with conflict and no ledger event",
			from: PayoutRetriableFailed,
			to:   PayoutSucceeded,
			want: PayoutTransitionDecision{Conflict: true},
		},
		{
			name: "failed to reversed is rejected with conflict and no ledger event",
			from: PayoutFailed,
			to:   PayoutReversed,
			want: PayoutTransitionDecision{Conflict: true},
		},
		{
			name: "succeeded to reversed enqueues only reversal",
			from: PayoutSucceeded,
			to:   PayoutReversed,
			want: PayoutTransitionDecision{Accepted: true, EnqueueReversal: true},
		},
		{
			name: "succeeded to failed is rejected with conflict",
			from: PayoutSucceeded,
			to:   PayoutFailed,
			want: PayoutTransitionDecision{Conflict: true},
		},
		{
			name: "reversed to late succeeded is rejected with conflict",
			from: PayoutReversed,
			to:   PayoutSucceeded,
			want: PayoutTransitionDecision{Conflict: true},
		},
		{
			name: "reversed to failed is rejected with conflict",
			from: PayoutReversed,
			to:   PayoutFailed,
			want: PayoutTransitionDecision{Conflict: true},
		},
		{
			name: "duplicate succeeded is no-op without ledger event",
			from: PayoutSucceeded,
			to:   PayoutSucceeded,
			want: PayoutTransitionDecision{NoOp: true},
		},
		{
			name: "duplicate reversed is no-op without ledger event",
			from: PayoutReversed,
			to:   PayoutReversed,
			want: PayoutTransitionDecision{NoOp: true},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := EvaluatePayoutTransition(tc.from, tc.to)
			if got != tc.want {
				t.Fatalf("EvaluatePayoutTransition(%s, %s) = %+v, want %+v", tc.from, tc.to, got, tc.want)
			}
		})
	}
}
