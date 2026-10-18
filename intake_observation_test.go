package main

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/actions/scaleset"
)

func observationLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	t.Setenv("CI_INTAKE_DIAGNOSTICS", "1")
	out := new(bytes.Buffer)
	old := log.Writer()
	log.SetOutput(out)
	t.Cleanup(func() { log.SetOutput(old) })
	return out
}

// Catch a handler logging a foreign allocation, or attributing completed to a
// fallback even though started already supplied the immutable assignment.
func TestObservationSchedulerSources(t *testing.T) {
	for _, tc := range []struct {
		name                                string
		known, started, completed, cleaning bool
		observed, fallback, published       int
	}{
		{"unknown", false, true, false, false, 0, 0, 0},
		{"started", true, true, false, false, 1, 0, 1},
		{"completed-only", true, false, true, false, 0, 1, 1},
		{"same-batch", true, true, true, false, 1, 0, 1},
		{"cleaning", true, true, false, true, 0, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := observationLog(t)
			f, _ := inputFixture(t, "cat >/dev/null; exit 0")
			journal := f.cleanupLog
			f.cleanupLog = nil
			t.Cleanup(func() { f.cleanupLog = journal })
			name := "ci-job-observation"
			if err := f.prepareJobInputs(context.Background(), name, inputSpec()); err != nil {
				t.Fatal(err)
			}
			if tc.known {
				f.jobs[name] = "test-network"
				f.state(name).cleaning = tc.cleaning
			}
			out.Reset()
			// Cleanup failure deliberately retains the allocation, without a real Docker call.
			withEngine(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) })
			base := scaleset.JobMessageBase{OwnerName: "example", RepositoryName: "dummy", WorkflowRunID: 7, JobID: "opaque", EventName: "push"}
			msg := &scaleset.RunnerScaleSetMessage{MessageID: 42}
			if tc.started {
				msg.JobStartedMessages = []*scaleset.JobStarted{{RunnerName: name, RunnerID: 9, JobMessageBase: base}}
			}
			if tc.completed {
				msg.JobCompletedMessages = []*scaleset.JobCompleted{{RunnerName: name, RunnerID: 9, JobMessageBase: base, Result: "succeeded"}}
			}
			_ = f.Scale(context.Background(), msg)
			for stage, want := range map[string]int{"started_observed": tc.observed, "completed_fallback": tc.fallback, "atomic_published": tc.published} {
				if got := strings.Count(out.String(), "stage="+stage+" "); got != want {
					t.Errorf("%s count=%d want=%d", stage, got, want)
				}
			}
			before := out.String()
			_ = f.Scale(context.Background(), msg)
			if strings.Count(out.String(), "stage=atomic_published ") != strings.Count(before, "stage=atomic_published ") {
				t.Fatal("retry invented publication")
			}
		})
	}
}

// Catch accepting a controller-only fact from an untrusted stderr stream or
// losing source/clock provenance while retaining split legacy frames.
func TestObservationForwardingAndSchema(t *testing.T) {
	out := observationLog(t)
	w := &intakeStderr{job: "ci-job-observation"}
	for _, line := range []string{"ci-intake/v1 atomic_published\n", "ci-intake/v1 started_observed\n", "ci-intake/v1 frame_parsed\n", "ci-intake/v9 helper_entry\n", "ci-intake/v1 helper_entry SECRET_SENTINEL\n", "ci-intake/v1 helper_", "entry\n"} {
		_, _ = w.Write([]byte(line))
	}
	stream := logFrame("2026-10-06T17:39:42Z ci-intake/v1 atomic_published\n2026-10-06T17:39:42Z ci-intake/v1 provider_ok\n2026-10-06T17:39:42Z ci-intake/v1 frame_parsed\n")
	if err := decodeIntakeLogs(bytes.NewReader(stream), "ci-job-observation"); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if strings.Count(got, "ci-intake/v2 ") != 2 || !strings.Contains(got, "source=helper stage=helper_entry ") || !strings.Contains(got, "source=worker stage=frame_parsed ") || strings.Contains(got, "SECRET_SENTINEL") {
		t.Fatalf("wrong forwarded records: %s", got)
	}
	if strings.Count(got, "evidence=forwarded") != 2 || strings.Count(got, "clock=controller") != 2 {
		t.Fatal("forwarded time presented as producer time")
	}
}

// Catch racy sequence allocation or a clock sampled outside the record lock.
func TestObservationConcurrentRecordsAndDisabled(t *testing.T) {
	out := observationLog(t)
	var wg sync.WaitGroup
	for i := 0; i < 48; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); intakeEvent("ci-job-observation", "controller", "packet_read") }()
	}
	wg.Wait()
	var seq, elapsed uint64
	var segment string
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		offset := strings.Index(line, "ci-intake/v2 ")
		if offset < 0 {
			t.Fatal("missing v2 schema")
		}
		var s string
		var next, ns uint64
		n, _ := fmt.Sscanf(line[offset:], "ci-intake/v2 job=ci-job-observation source=controller stage=packet_read segment=%s seq=%d elapsed_ns=%d", &s, &next, &ns)
		if n != 3 || next <= seq || ns < elapsed || (segment != "" && segment != s) {
			t.Fatal("unordered observation")
		}
		seq, elapsed, segment = next, ns, s
	}
	if len(strings.Split(strings.TrimSpace(out.String()), "\n")) != 48 {
		t.Fatal("lost concurrent record")
	}
	out.Reset()
	t.Setenv("CI_INTAKE_DIAGNOSTICS", "0")
	intakeEvent("ci-job-observation", "controller", "packet_read")
	_, _ = (&intakeStderr{job: "ci-job-observation"}).Write([]byte("ci-intake/v1 helper_entry\n"))
	if out.Len() != 0 {
		t.Fatal("disabled diagnostics emitted records")
	}
}

// Catch reporting equality or a failed write as a new durable publication.
func TestObservationAtomicPublicationAndFailure(t *testing.T) {
	out := observationLog(t)
	_, b := inputFixture(t, "exit 0")
	if err := b.root.Mkdir("ci-job-observation", 0700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := b.write("ci-job-observation/assignment.json", obj{"schema": "ci-job-assignment/v1"}, true); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Count(out.String(), "stage=atomic_published "); got != 1 {
		t.Fatalf("publication count=%d", got)
	}
	out.Reset()
	if b.write("ci-job-missing/assignment.json", obj{}, true) == nil {
		t.Fatal("write unexpectedly succeeded")
	}
	if b.write("ci-job-observation/assignment.json", obj{"changed": true}, true) == nil {
		t.Fatal("immutable mutation succeeded")
	}
	if out.Len() != 0 {
		t.Fatal("failed write published diagnostic")
	}
	// An existing file after an ambiguous durability failure stays unknown on retry.
	if err := os.Chmod(b.root.Name()+"/ci-job-observation/assignment.json", 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b.root.Name()+"/ci-job-observation/assignment.json", []byte(`{"changed":true}`), 0444); err != nil {
		t.Fatal(err)
	}
	if err := b.write("ci-job-observation/assignment.json", obj{"changed": true}, true); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatal("retry invented durability evidence")
	}
}

func TestObservationDirectorySyncFailureIsUnknown(t *testing.T) {
	out := observationLog(t)
	_, b := inputFixture(t, "exit 0")
	if err := b.root.Mkdir("ci-job-sync", 0700); err != nil {
		t.Fatal(err)
	}
	b.syncDirectory = func(string) error { return fmt.Errorf("injected directory sync failure") }
	if b.write("ci-job-sync/assignment.json", obj{"v": 1}, true) == nil {
		t.Fatal("sync error swallowed")
	}
	if _, err := b.root.Stat("ci-job-sync/assignment.json"); err != nil {
		t.Fatal("rename was not reached")
	}
	if out.Len() != 0 {
		t.Fatal("unconfirmed durability reported as publication")
	}
	b.syncDirectory = nil
	if err := b.write("ci-job-sync/assignment.json", obj{"v": 1}, true); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatal("immutable equality invented publication after sync error")
	}
}

func TestObservationRestartSegment(t *testing.T) {
	if os.Getenv("CI_OBSERVATION_SEGMENT_PROBE") == "1" {
		log.SetOutput(os.Stdout)
		intakeEvent("ci-job-probe", "controller", "packet_read")
		return
	}
	var previous string
	for i := 0; i < 2; i++ {
		command := exec.Command(os.Args[0], "-test.run=^TestObservationRestartSegment$")
		command.Env = append(os.Environ(), "CI_INTAKE_DIAGNOSTICS=1", "CI_OBSERVATION_SEGMENT_PROBE=1")
		data, err := command.Output()
		if err != nil {
			t.Fatal(err)
		}
		segment := ""
		for _, field := range strings.Fields(string(data)) {
			if strings.HasPrefix(field, "segment=") {
				segment = strings.TrimPrefix(field, "segment=")
			}
		}
		if segment == "" || segment == previous || !strings.Contains(string(data), "seq=1 ") {
			t.Fatal("restart did not split controller clock")
		}
		previous = segment
	}
}

// Catch stale assignment association and cleanup/publish racing through the
// allocation mutex. These are actual Scale calls, not writes to a log double.
func TestObservationChangedIdentityAndCleanupLock(t *testing.T) {
	out := observationLog(t)
	f, b := inputFixture(t, "cat >/dev/null; exit 0")
	journal := f.cleanupLog
	f.cleanupLog = nil
	t.Cleanup(func() { f.cleanupLog = journal })
	name := "ci-job-current"
	if err := f.prepareJobInputs(context.Background(), name, inputSpec()); err != nil {
		t.Fatal(err)
	}
	f.jobs[name] = "test-network"
	event := &scaleset.JobStarted{RunnerName: name, RunnerID: 9, JobMessageBase: scaleset.JobMessageBase{OwnerName: "example", RepositoryName: "dummy", WorkflowRunID: 7, JobID: "opaque", EventName: "push"}}
	if err := f.assignJobInputs(event); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	event.WorkflowRunID = 8
	if f.Scale(context.Background(), &scaleset.RunnerScaleSetMessage{MessageID: 43, JobStartedMessages: []*scaleset.JobStarted{event}}) == nil {
		t.Fatal("changed scheduler identity accepted")
	}
	if strings.Contains(out.String(), "started_observed") {
		t.Fatal("wrong identity called observed")
	}
	event.WorkflowRunID = 7
	j := f.state(name)
	j.mu.Lock()
	done := make(chan error, 1)
	go func() {
		done <- f.Scale(context.Background(), &scaleset.RunnerScaleSetMessage{MessageID: 44, JobStartedMessages: []*scaleset.JobStarted{event}})
	}()
	j.cleaning = true
	j.mu.Unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "started_observed") {
		t.Fatal("cleaning allocation observed")
	}
	if _, err := b.root.Stat(name + "/assignment.json"); err != nil {
		t.Fatal(err)
	}
}
