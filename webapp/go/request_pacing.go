package main

import (
	"context"
	"time"
)

const nonScoringRequestDelay = 100 * time.Millisecond

// Pace requests without holding a transaction, mutation slot, or stream gate.
func waitNonScoringRequest(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	timer := time.NewTimer(nonScoringRequestDelay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ctx.Err()
	}
}
