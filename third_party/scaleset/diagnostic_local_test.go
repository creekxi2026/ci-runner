package scaleset

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestDiagnosticScaleLocalClosedValueCopy(t *testing.T) {
	var b bytes.Buffer
	d := diagTest(t, &b, 64)
	ctx, end := d.Begin(context.Background(), Scale, 1)
	n := uint64(2)
	f := false
	v := DiagnosticScaleLocal{Origin: "cleanup_journal", Reason: "invalid_cleanup_identity", Handler: "started", Started: &n, EventIndex: &n, NameValid: &f}
	d.SetScaleLocal(ctx, v)
	n = 999
	f = true
	v.Reason = "SECRET"
	end(errors.New("SECRET"))
	if !finishDiag(t, d) {
		t.Fatal("loss")
	}
	var found int
	for _, line := range bytes.Split(bytes.TrimSpace(b.Bytes()), []byte("\n")) {
		if len(line)+1 > 2048 {
			t.Fatal("cap")
		}
		var e DiagnosticEvent
		if json.Unmarshal(line, &e) != nil {
			t.Fatal("schema")
		}
		if e.Local != nil {
			found++
			if e.Phase != "end" || e.Local.Reason != "invalid_cleanup_identity" || *e.Local.EventIndex != 2 || *e.Local.NameValid || e.Local.JournalExists != nil {
				t.Fatal(e)
			}
		}
	}
	if found != 1 || bytes.Contains(b.Bytes(), []byte("SECRET")) {
		t.Fatal("count/privacy")
	}
}
func TestDiagnosticScaleLocalRejectsInvalidEnums(t *testing.T) {
	base := DiagnosticScaleLocal{Origin: "unknown", Reason: "unknown", Handler: "unknown"}
	cases := []DiagnosticScaleLocal{base, base, base, base, base}
	cases[0].Origin = "SECRET"
	cases[1].Reason = "SECRET"
	cases[2].Handler = "SECRET"
	cases[3].Origin = "input_assignment"
	cases[3].Reason = "unsafe_cleanup_record"
	zero := uint64(0)
	cases[4].EventIndex = &zero
	for _, v := range cases {
		var b bytes.Buffer
		d := diagTest(t, &b, 64)
		ctx, end := d.Begin(context.Background(), Scale, 1)
		d.SetScaleLocal(ctx, v)
		end(nil)
		if finishDiag(t, d) || d.Loss() != 1 {
			t.Fatal("not rejected")
		}
		if bytes.Contains(b.Bytes(), []byte("SECRET")) {
			t.Fatal("leak")
		}
	}
	var nilD *Diagnostics
	nilD.SetScaleLocal(context.Background(), base)
}
func TestDiagnosticScaleLocalReasonPairs(t *testing.T) {
	pairs := map[string][]string{
		"cleanup_journal":       {"invalid_cleanup_identity", "cleanup_open_failed", "unsafe_cleanup_record", "invalid_cleanup_record_size", "invalid_cleanup_record", "foreign_cleanup_record", "cleanup_update_failed"},
		"scheduler_observation": {"invalid_scheduler_identity", "scheduler_identity_changed"},
		"input_assignment":      {"invalid_input_runner", "invalid_input_assignment", "input_spec_stat_failed", "input_assignment_read_failed", "sealed_input_changed", "input_assignment_write_failed"},
		"completion_cleanup":    {"completion_cleanup_pending"}, "unknown": {"unknown"}, "none": {"none"},
	}
	for origin, reasons := range pairs {
		for _, reason := range reasons {
			if !(DiagnosticScaleLocal{Origin: origin, Reason: reason, Handler: "started"}).valid() {
				t.Fatal(origin, reason)
			}
		}
	}
}
