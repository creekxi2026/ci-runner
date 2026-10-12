package main

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/google/uuid"
)

// Runs only inside an isolated root-owned subdirectory of the existing fixed
// work volume. The parent harness owns and removes that one subdirectory.
func TestCleanupJournalRuntime(t *testing.T) {
	image := os.Getenv("CI_CLEANUP_TEST_IMAGE")
	if image == "" {
		t.Skip("requires explicit isolated Docker runtime harness")
	}
	w, e := openSharedWork("/work", "ci-work-linux-arm64", "ruif106")
	if e != nil {
		t.Fatal(e)
	}
	defer w.Close()
	c, e := openCleanupJournal(w, 0)
	if e != nil {
		t.Fatal(e)
	}
	defer c.root.Close()
	f := &fleet{owner: "ruif106", image: image, work: w, cleanupLog: c, jobs: map[string]string{}}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	for _, outcome := range []string{"succeeded", "failed", "canceled"} {
		t.Run(outcome, func(t *testing.T) {
			n := "ci-job-" + uuid.NewString()[:12]
			if e := w.reserve(n, false); e != nil {
				t.Fatal(e)
			}
			if e := w.root.Mkdir("jobs/"+n, 0700); e != nil {
				t.Fatal(e)
			}
			f.jobs[n] = n + "-net"
			j := f.state(n)
			j.disk = true
			j.shared = true
			defer f.cleanup(n, n+"-net")
			if e := dockerContext(ctx, "POST", "/networks/create", obj{"Name": n + "-net", "Internal": true, "Labels": f.resourceLabels(n)}, nil); e != nil {
				t.Fatal(e)
			}
			command := []string{"/bin/sh", "-c", "exit 0"}
			if outcome == "failed" {
				command[2] = "exit 7"
			}
			if outcome == "canceled" {
				command = []string{"/bin/sleep", "60"}
			}
			h := secure()
			h["ReadonlyRootfs"] = true
			if e := f.createContext(ctx, n, image, "1001", command, nil, h, n+"-net"); e != nil {
				t.Fatal(e)
			}
			if outcome == "canceled" {
				if e := dockerContext(ctx, "POST", "/containers/"+n+"/kill", nil, nil); e != nil {
					t.Fatal(e)
				}
			}
			var wait struct{ StatusCode int }
			if e := dockerContext(ctx, "POST", "/containers/"+n+"/wait?condition=not-running", nil, &wait); e != nil {
				t.Fatal(e)
			}
			// Synthetic scheduler identity is labeled as simulation, not native CI.
			event := scaleset.JobMessageBase{OwnerName: "example", RepositoryName: "fixture", WorkflowRunID: 106, JobID: outcome}
			if e := f.observeScheduler(n, 106, event, outcome); e != nil {
				t.Fatal(e)
			}
			f.cleanup(n, n+"-net")
			if f.state(n) != nil {
				t.Fatal("cleanup remained pending")
			}
			v, e := c.read(n)
			if e != nil {
				t.Fatal(e)
			}
			if !v.Runner.Observed || v.Runner.Running || v.Runner.ExitCode != wait.StatusCode || v.ReleasedAt == "" || v.Native.Result != outcome {
				t.Fatalf("incorrect observed lifecycle: %+v", v)
			}
			b, _ := json.Marshal(v)
			t.Log(string(b))
			if _, e := w.root.Lstat("jobs/" + n); !os.IsNotExist(e) {
				t.Fatal("private data still present")
			}
		})
	}
}
