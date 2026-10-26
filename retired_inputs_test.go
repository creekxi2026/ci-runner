package main

import (
	"strings"
	"testing"
)

func TestRetiredInputPolicyStopsBeforeInitialization(t *testing.T) {
	t.Setenv("JOB_INPUT_POLICY_FILE", "/missing/private-policy.json")
	// Neither credentials nor any Docker/GitHub initialization may precede the
	// migration rejection, even when the old policy cannot be read.
	t.Setenv("RUNNER_MAX_JOBS", "invalid")
	err := run()
	if err == nil || !strings.Contains(err.Error(), "JOB_INPUT_POLICY_FILE is retired") || strings.Contains(err.Error(), "/missing/") {
		t.Fatalf("expected a path-free migration rejection, got %v", err)
	}
}
