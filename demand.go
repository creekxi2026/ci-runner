package main

import (
	"context"
	"fmt"
	"time"
)

// Source statistics come from the authenticated scale-set API, not a frozen
// queue message. A nil source exists only for isolated message-handler tests.
func (f *fleet) reconcileCurrent(ctx context.Context) error {
	for range 3 {
		f.mu.Lock()
		stopped := f.stopping
		f.mu.Unlock()
		if stopped || ctx.Err() != nil {
			return context.Canceled
		}
		if f.demandSource == nil {
			return nil
		}
		want, err := f.demandSource(ctx)
		if err != nil {
			return err
		}
		if additions(want, len(f.snapshot())) == 0 {
			return nil
		}
		if err = f.start(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (f *fleet) changed() {
	if f.changes == nil {
		return
	}
	select {
	case f.changes <- struct{}{}:
	default:
	}
}

func scaleSetDemand(client scaleSets, name string, expectedID func() int) func(context.Context) (int, error) {
	return func(ctx context.Context) (int, error) {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		set, err := client.GetRunnerScaleSet(ctx, 1, name)
		if err != nil {
			return 0, err
		}
		if set == nil || set.ID != expectedID() || set.Statistics == nil {
			return 0, fmt.Errorf("scale-set demand unavailable or identity mismatch")
		}
		return set.Statistics.TotalAssignedJobs, nil
	}
}
