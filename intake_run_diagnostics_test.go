package main

import (
	"strings"
	"testing"
)

// Catch lost helper rejection reasons, or accepting them as worker/controller
// facts or with appended private data. Exercise the actual streaming filter.
func TestRunDiagnosticForwarding(t *testing.T) {
	stages := strings.Fields("provider_decode_ready provider_decode_rejected run_id_diff run_attempt_diff run_status_diff run_conclusion_diff run_workflow_diff run_repository_diff run_repository_id_diff run_context_repository_id_diff run_event_diff run_actor_diff run_shape_rejected run_identity_ready")
	out := observationLog(t)
	w := &intakeStderr{job: "ci-job-run-diag"}
	for _, stage := range stages {
		out.Reset()
		_, _ = w.Write([]byte("ci-intake/v1 " + stage[:4]))
		_, _ = w.Write([]byte(stage[4:] + "\n"))
		if !strings.Contains(out.String(), "source=helper stage="+stage+" ") || !strings.Contains(out.String(), "evidence=forwarded") {
			t.Fatalf("lost helper classification %s", stage)
		}
		out.Reset()
		_, _ = w.Write([]byte("ci-intake/v1 " + stage + " SENTINEL_PRIVATE\n"))
		intakeEvent("ci-job-run-diag", "worker", stage)
		intakeEvent("ci-job-run-diag", "controller", stage)
		if out.Len() != 0 {
			t.Fatal("unsafe classification or source accepted")
		}
	}
	t.Setenv("CI_INTAKE_DIAGNOSTICS", "0")
	_, _ = w.Write([]byte("ci-intake/v1 provider_decode_ready\n"))
	if out.Len() != 0 {
		t.Fatal("disabled diagnostic emitted")
	}
}
