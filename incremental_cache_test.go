package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/actions/scaleset"
)

func cacheFixturePolicy() incrementalPolicy {
	return incrementalPolicy{Version: 1, Repository: "example/project", Branch: "main", Workflow: ".github/workflows/check.yml", Job: "compile", Paths: []string{".cache/compiler", ".cache/transform"}, MaxBytes: 1 << 20, MaxEntries: 50, VolumeLabels: map[string]string{"ci-runner.cache-schema": "v2-linux-arm64-trusted-manual", "ci-runner.cache-owner": "operator", "ci-runner.cache-repository": "https://github.com/example/runtime", "ci-runner.cache-lane": "reviewed-main"}}
}
func TestIncrementalPolicyBounds(t *testing.T) {
	good := cacheFixturePolicy()
	check := func(p incrementalPolicy, want bool) {
		t.Helper()
		b, _ := json.Marshal(p)
		_, e := parseIncrementalPolicy(b)
		if (e == nil) != want {
			t.Fatalf("accepted=%v error=%v", e == nil, e)
		}
	}
	check(good, true)
	for _, paths := range [][]string{{"../secret"}, {".ssh"}, {".cache/a/../../secret"}, {".cache/a", ".cache/a/b"}, {".cache/a", ".cache/a"}, {".cache/a\\b"}} {
		bad := good
		bad.Paths = paths
		check(bad, false)
	}
	bad := good
	bad.MaxBytes = 1<<30 + 1
	check(bad, false)
	bad = good
	bad.MaxEntries = 50001
	check(bad, false)
}
func TestIncrementalAttestationRejectsUntrustedAndStaleJobs(t *testing.T) {
	p := cacheFixturePolicy()
	sha := strings.Repeat("a", 40)
	event := scaleset.JobStarted{RunnerID: 42, RunnerName: "ci-job-12345678-abc", JobMessageBase: scaleset.JobMessageBase{WorkflowRunID: 7, EventName: "push", OwnerName: "example", RepositoryName: "project", JobDisplayName: "compile", JobWorkflowRef: "example/project/.github/workflows/check.yml@refs/heads/main"}}
	makeData := func() map[string]any {
		return map[string]any{
			"/repos/example/project/actions/runs/7":                 obj{"id": 7, "run_attempt": 2, "event": "push", "path": p.Workflow, "head_sha": sha, "head_branch": "main", "repository": obj{"full_name": p.Repository}, "head_repository": obj{"full_name": p.Repository}},
			"/repos/example/project/actions/runs/7/attempts/2/jobs": obj{"total_count": 1, "jobs": []any{obj{"id": 9, "run_id": 7, "run_attempt": 2, "runner_id": 42, "runner_name": event.RunnerName, "name": "compile", "head_sha": sha, "status": "completed", "conclusion": "success"}}},
			"/repos/example/project/branches/main":                  obj{"protected": true, "commit": obj{"sha": sha}},
		}
	}
	for _, test := range []struct {
		name   string
		mutate func(map[string]any, *scaleset.JobStarted)
	}{
		{"valid", func(map[string]any, *scaleset.JobStarted) {}},
		{"pr", func(_ map[string]any, e *scaleset.JobStarted) { e.EventName = "pull_request" }},
		{"spoofed-ref", func(_ map[string]any, e *scaleset.JobStarted) { e.JobWorkflowRef = "evil/workflow@main" }},
		{"fork", func(d map[string]any, _ *scaleset.JobStarted) {
			d["/repos/example/project/actions/runs/7"].(obj)["head_repository"] = obj{"full_name": "fork/project"}
		}},
		{"old-attempt", func(d map[string]any, _ *scaleset.JobStarted) {
			d["/repos/example/project/actions/runs/7/attempts/2/jobs"].(obj)["jobs"].([]any)[0].(obj)["run_attempt"] = 1
		}},
		{"different-runner", func(d map[string]any, _ *scaleset.JobStarted) {
			d["/repos/example/project/actions/runs/7/attempts/2/jobs"].(obj)["jobs"].([]any)[0].(obj)["runner_id"] = 99
		}},
		{"failed", func(d map[string]any, _ *scaleset.JobStarted) {
			d["/repos/example/project/actions/runs/7/attempts/2/jobs"].(obj)["jobs"].([]any)[0].(obj)["conclusion"] = "failure"
		}},
		{"cancelled", func(d map[string]any, _ *scaleset.JobStarted) {
			d["/repos/example/project/actions/runs/7/attempts/2/jobs"].(obj)["jobs"].([]any)[0].(obj)["conclusion"] = "cancelled"
		}},
		{"pending", func(d map[string]any, _ *scaleset.JobStarted) {
			d["/repos/example/project/actions/runs/7/attempts/2/jobs"].(obj)["jobs"].([]any)[0].(obj)["status"] = "in_progress"
		}},
		{"truncated", func(d map[string]any, _ *scaleset.JobStarted) {
			d["/repos/example/project/actions/runs/7/attempts/2/jobs"].(obj)["total_count"] = 101
		}},
		{"duplicate", func(d map[string]any, _ *scaleset.JobStarted) {
			j := d["/repos/example/project/actions/runs/7/attempts/2/jobs"].(obj)
			j["total_count"] = 2
			j["jobs"] = append(j["jobs"].([]any), j["jobs"].([]any)[0])
		}},
		{"unprotected", func(d map[string]any, _ *scaleset.JobStarted) {
			d["/repos/example/project/branches/main"].(obj)["protected"] = false
		}},
		{"superseded", func(d map[string]any, _ *scaleset.JobStarted) {
			d["/repos/example/project/branches/main"].(obj)["commit"] = obj{"sha": strings.Repeat("b", 40)}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			data := makeData()
			ev := event
			test.mutate(data, &ev)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer test-token" {
					t.Error("missing controller credential")
				}
				v, ok := data[r.URL.Path]
				if !ok {
					http.NotFound(w, r)
					return
				}
				json.NewEncoder(w).Encode(v)
			}))
			defer server.Close()
			c := &incrementalCache{policy: p, token: "test-token", endpoint: server.URL, http: server.Client()}
			result, e := c.attest(context.Background(), &ev)
			if test.name == "valid" {
				if e != nil || result != sha {
					t.Fatal(result, e)
				}
			} else if e == nil {
				t.Fatal("accepted invalid provenance")
			}
		})
	}
}
func cacheTestRoot(t *testing.T) (*os.Root, string) {
	t.Helper()
	p := t.TempDir()
	r, e := os.OpenRoot(p)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { r.Close() })
	return r, p
}
func writeCacheFixture(t *testing.T, home string) {
	t.Helper()
	for _, p := range cacheFixturePolicy().Paths {
		if e := os.MkdirAll(filepath.Join(home, p), 0700); e != nil {
			t.Fatal(e)
		}
		if e := os.WriteFile(filepath.Join(home, p, "entry"), []byte("cached transform"), 0600); e != nil {
			t.Fatal(e)
		}
	}
}
