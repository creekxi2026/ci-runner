package scaleset

import "context"

// DiagnosticScaleLocal is a closed, value-only snapshot of observations already
// made by the controller. nil means not observed, never a negative observation.
// It is attached only to the owning inner Scale end, not its propagation parent.
type DiagnosticScaleLocal struct {
	Origin        string  `json:"origin"`
	Reason        string  `json:"reason"`
	Handler       string  `json:"handler"`
	Available     *uint64 `json:"available"`
	Started       *uint64 `json:"started"`
	Completed     *uint64 `json:"completed"`
	EventIndex    *uint64 `json:"event_index"`
	NameValid     *bool   `json:"name_valid"`
	StateExists   *bool   `json:"state_exists"`
	JournalExists *bool   `json:"journal_exists"`
	JournalSafe   *bool   `json:"journal_safe"`
	Acquired      *bool   `json:"acquired"`
}

func (v DiagnosticScaleLocal) valid() bool {
	switch v.Handler {
	case "started", "completed", "unknown", "none":
	default:
		return false
	}
	if v.EventIndex != nil && (*v.EventIndex == 0 || (v.Handler != "started" && v.Handler != "completed")) {
		return false
	}
	switch v.Origin {
	case "none":
		return v.Reason == "none"
	case "unknown":
		return v.Reason == "unknown"
	case "cleanup_journal":
		switch v.Reason {
		case "invalid_cleanup_identity", "cleanup_open_failed", "unsafe_cleanup_record", "invalid_cleanup_record_size", "invalid_cleanup_record", "foreign_cleanup_record", "cleanup_update_failed":
			return true
		}
	case "scheduler_observation":
		return v.Reason == "invalid_scheduler_identity" || v.Reason == "scheduler_identity_changed"
	case "input_assignment":
		switch v.Reason {
		case "invalid_input_runner", "invalid_input_assignment", "input_spec_stat_failed", "input_assignment_read_failed", "sealed_input_changed", "input_assignment_write_failed":
			return true
		}
	case "completion_cleanup":
		return v.Reason == "completion_cleanup_pending"
	}
	return false
}

// SetScaleLocal copies a validated snapshot. No caller memory, callback, error
// text or arbitrary string is retained by the asynchronous sink. Call on the
// operation's goroutine immediately before its end, after child operations end.
func (d *Diagnostics) SetScaleLocal(ctx context.Context, v DiagnosticScaleLocal) {
	if d == nil {
		return
	}
	c, _ := ctx.Value(diagnosticKey{}).(*diagnosticCall)
	if c == nil || c.d != d || c.stage != Scale || !v.valid() {
		d.loss.Add(1)
		return
	}
	copyU := func(p *uint64) *uint64 {
		if p == nil {
			return nil
		}
		n := *p
		return &n
	}
	copyB := func(p *bool) *bool {
		if p == nil {
			return nil
		}
		b := *p
		return &b
	}
	v.Available, v.Started, v.Completed, v.EventIndex = copyU(v.Available), copyU(v.Started), copyU(v.Completed), copyU(v.EventIndex)
	v.NameValid, v.StateExists = copyB(v.NameValid), copyB(v.StateExists)
	v.JournalExists, v.JournalSafe, v.Acquired = copyB(v.JournalExists), copyB(v.JournalSafe), copyB(v.Acquired)
	c.local = &v
}
