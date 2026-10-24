package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/actions/scaleset"
	"time"
)

// Source statistics come from the authenticated scale-set API, not a frozen
// queue message. A nil source exists only for isolated message-handler tests.
func (f *fleet) reconcileCurrent(ctx context.Context) (diagnosticErr error) {
	ctx, diagnosticEnd := f.diagnostics.Begin(ctx, scaleset.Reconcile, 1)
	defer func() { diagnosticEnd(diagnosticErr) }()
	for range f.jobLimit() {
		f.mu.Lock()
		stopped := f.stopping
		f.mu.Unlock()
		if stopped || ctx.Err() != nil {
			return context.Canceled
		}
		if f.demandSource == nil {
			return nil
		}
		demandCtx, end := f.diagnostics.Begin(ctx, scaleset.Demand, 1)
		want, err := f.demandSource(demandCtx)
		if err == nil && want >= 0 {
			f.diagnostics.Count(demandCtx, scaleset.DiagnosticCounts{Demand: uint64(want)})
		}
		end(err)
		if err != nil {
			return err
		}
		if additions(want, len(f.snapshot()), f.jobLimit()) == 0 {
			return nil
		}
		if err = f.start(ctx); err != nil {
			if errors.Is(err, errCapacity) {
				f.changed()
				return nil
			}
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

// Scheduling can block on GitHub/Docker; never run it in the local janitor.
func (f *fleet) coordinate(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for ctx.Err() == nil {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-f.changes:
			if f.demandSource != nil && f.scaleMu.TryLock() {
				err := f.reconcileCurrent(ctx)
				f.scaleMu.Unlock()
				if err != nil && ctx.Err() == nil {
					f.changed()
				}
			} else if ctx.Err() == nil {
				f.changed()
			}
		}
		if f.inputs != nil {
			f.inputs.reconcile(ctx, f.snapshot())
		}
		if !pause(ctx, interval) {
			return
		}
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
