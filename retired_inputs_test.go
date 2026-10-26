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

func TestRetiredStageProfileStopsBeforeInitialization(t *testing.T) {
	for _, key := range []string{"CI_DIAGNOSTIC_PROFILE", "CI_DIAGNOSTIC_OWNER_UID"} {
		t.Run(key, func(t *testing.T) {
			t.Setenv("JOB_INPUT_POLICY_FILE", "")
			t.Setenv("CI_DIAGNOSTIC_PROFILE", "")
			t.Setenv("CI_DIAGNOSTIC_OWNER_UID", "")
			t.Setenv(key, "private-value")
			t.Setenv("RUNNER_MAX_JOBS", "invalid")
			err := run()
			if err == nil || !strings.Contains(err.Error(), "stage diagnostics are retired") || strings.Contains(err.Error(), "private-value") {
				t.Fatalf("expected safe migration rejection, got %v", err)
			}
		})
	}
}
