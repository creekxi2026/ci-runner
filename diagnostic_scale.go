package main

import (
	"github.com/actions/scaleset"
)

// Stack-owned and synchronous under the existing Scale lock. All methods accept
// nil so legacy callers and diagnostics-off execute the same business paths.
type scaleObservation struct{ scaleset.DiagnosticScaleLocal }

func boolValue(v bool) *bool     { return &v }
func uintValue(v uint64) *uint64 { return &v }
func (o *scaleObservation) fail(origin, reason string) {
	if o != nil && o.Origin == "unknown" {
		o.Origin, o.Reason = origin, reason
	}
}
func (o *scaleObservation) missingJournal() {
	if o != nil {
		o.Origin, o.Reason = "unknown", "unknown"
	}
}
func (o *scaleObservation) event(handler string, i int) {
	if o != nil {
		o.Handler, o.EventIndex = handler, uintValue(uint64(i+1))
		o.NameValid, o.StateExists, o.JournalExists, o.JournalSafe = nil, nil, nil, nil
	}
}
