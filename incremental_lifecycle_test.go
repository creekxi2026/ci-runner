package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/actions/scaleset"
)

// Exercise the real cleanup/janitor entrypoints, with disk data on a real
// filesystem and external Docker/GitHub identities represented by servers.
func TestIncrementalCleanupOrdersWritersCacheAndDisk(t *testing.T) {
	for _, mode := range []string{"success", "pending", "missing-event", "worker-delete-failure"} {
		t.Run(mode, func(t *testing.T) {
			n := "ci-job-12345678-abc"
			workPath := t.TempDir()
			work, e := openSharedWork(workPath, "fixture", "test-cache")
			if e != nil {
				t.Fatal(e)
			}
			defer work.Close()
			if e = work.reserve(n, false); e != nil {
				t.Fatal(e)
			}
			home := filepath.Join(workPath, "jobs", n, "home")
			writeCacheFixture(t, home)
			c := testCache(t)
			sha := strings.Repeat("a", 40)
			deleted := map[string]bool{}
			calls := 0
			failDelete := mode == "worker-delete-failure"
			c.http = &http.Client{Transport: handlerTransport{func(w http.ResponseWriter, r *http.Request) {
				calls++
				for _, suffix := range []string{"", "-fw", "-pg", "-proxy"} {
					if !deleted[n+suffix] {
						t.Errorf("read cache identity while writer %s exists", n+suffix)
					}
				}
				if _, e := os.Stat(home); e != nil {
					t.Error("private data removed before publication")
				}
				switch {
				case strings.Contains(r.URL.Path, "/attempts/"):
					status, conclusion := "completed", "success"
					if mode == "pending" {
						status = "in_progress"
						conclusion = ""
					}
					json.NewEncoder(w).Encode(obj{"total_count": 1, "jobs": []obj{{"id": 9, "run_id": 7, "run_attempt": 1, "runner_id": 42, "runner_name": n, "name": "compile", "head_sha": sha, "status": status, "conclusion": conclusion}}})
				case strings.Contains(r.URL.Path, "/actions/runs/"):
					json.NewEncoder(w).Encode(obj{"id": 7, "run_attempt": 1, "event": "push", "path": c.policy.Workflow, "head_sha": sha, "head_branch": "main", "repository": obj{"full_name": c.policy.Repository}, "head_repository": obj{"full_name": c.policy.Repository}})
				default:
					json.NewEncoder(w).Encode(obj{"protected": true, "commit": obj{"sha": sha}})
				}
			}}}
			c.endpoint = "https://api.github.com"
			f := &fleet{owner: "test-cache", work: work, incremental: c, jobs: map[string]string{n: n + "-net"}}
			j := f.state(n)
			j.shared = true
			j.disk = true
			if mode != "missing-event" {
				f.cacheJobEvent(&scaleset.JobStarted{RunnerID: 42, RunnerName: n, JobMessageBase: scaleset.JobMessageBase{WorkflowRunID: 7, EventName: "push", OwnerName: "example", RepositoryName: "project", JobDisplayName: "compile", JobWorkflowRef: "example/project/.github/workflows/check.yml@refs/heads/main"}})
			}
			withEngine(t, func(w http.ResponseWriter, r *http.Request) {
				p := strings.TrimPrefix(r.URL.Path, "/v1.47")
				if r.Method == "GET" && strings.HasSuffix(p, "/json") {
					json.NewEncoder(w).Encode(obj{"State": obj{"Running": false}})
					return
				}
				if r.Method == "DELETE" && strings.HasPrefix(p, "/containers/") {
					name := strings.TrimPrefix(p, "/containers/")
					if failDelete && name == n {
						w.WriteHeader(500)
						return
					}
					deleted[name] = true
				}
				w.WriteHeader(204)
			})
			// Janitor sees stopped runner, even without a JobCompleted message.
			f.reap()
			if failDelete {
				if calls != 0 {
					t.Fatal("published before worker deletion")
				}
				failDelete = false
				f.reap()
			}
			if len(f.snapshot()) != 0 {
				t.Fatal("cache failure prevented cleanup")
			}
			if _, e = os.Stat(home); !os.IsNotExist(e) {
				t.Fatal("private job data retained", e)
			}
			idx, e := c.index()
			if e != nil {
				t.Fatal(e)
			}
			want := mode == "success" || mode == "worker-delete-failure"
			if (idx.Current != "") != want {
				t.Fatalf("publication=%v want=%v", idx.Current != "", want)
			}
			old := calls
			f.reap()
			if calls != old {
				t.Fatal("replayed publication after cleanup")
			}
		})
	}
}
