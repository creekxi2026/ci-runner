package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/actions/scaleset"
)

type localRejectTransport struct{ trace *[]string }

func (r localRejectTransport) RoundTrip(q *http.Request) (*http.Response, error) {
	*r.trace = append(*r.trace, q.Method+" "+q.URL.Path)
	return nil, errors.New("SECRET-transport")
}

type localAcquireFailure struct {
	fakeSession
	err error
}

func (s *localAcquireFailure) AcquireJobs(context.Context, []int64) ([]int64, error) {
	s.acquired++
	return nil, s.err
}

func localRecords(t *testing.T, b []byte) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(b), []byte("\n")) {
		if len(line)+1 > 2048 {
			t.Fatal("line cap")
		}
		var v map[string]any
		if json.Unmarshal(line, &v) != nil {
			t.Fatal("invalid JSON")
		}
		out = append(out, v)
	}
	if bytes.Contains(b, []byte("SECRET")) || bytes.Contains(b, []byte("ci-job-12345678-abc")) || bytes.Contains(b, []byte("workflow_dispatch")) || bytes.Contains(b, []byte("example")) {
		t.Fatal("secret in sink")
	}
	return out
}

// Real Scale and filesystem handlers, no live Docker or GitHub. Every failure
// runs twice to exercise the same message's acquired/retry state. Fault sinks
// include an actually full, blocked queue; it is released and joined before exit.
func TestDiagnosticScaleLocalBranches(t *testing.T) {
	cases := []struct{ name, origin, reason, handler string }{
		{"started-invalid", "cleanup_journal", "invalid_cleanup_identity", "started"},
		{"completed-invalid", "cleanup_journal", "invalid_cleanup_identity", "completed"},
		{"unsafe", "cleanup_journal", "unsafe_cleanup_record", "started"},
		{"malformed", "cleanup_journal", "invalid_cleanup_record", "started"},
		{"foreign", "cleanup_journal", "foreign_cleanup_record", "started"},
		{"open", "cleanup_journal", "cleanup_open_failed", "started"},
		{"scheduler-invalid", "scheduler_observation", "invalid_scheduler_identity", "started"},
		{"scheduler-changed", "scheduler_observation", "scheduler_identity_changed", "started"},
		{"update", "cleanup_journal", "cleanup_update_failed", "started"},
		{"input-runner", "input_assignment", "invalid_input_runner", "started"},
		{"input-identity", "input_assignment", "invalid_input_assignment", "started"},
		{"input-stat", "input_assignment", "input_spec_stat_failed", "started"},
		{"input-read", "input_assignment", "input_assignment_read_failed", "started"},
		{"input-sealed", "input_assignment", "sealed_input_changed", "started"},
		{"input-write", "input_assignment", "input_assignment_write_failed", "started"},
		{"completion-pending", "completion_cleanup", "completion_cleanup_pending", "completed"},
		{"acquire-unknown", "unknown", "unknown", "unknown"},
		{"missing-success", "none", "none", "unknown"},
		{"demand-unknown", "unknown", "unknown", "unknown"},
	}
	sentinel := &os.PathError{Op: "SECRET-op", Path: "SECRET-path", Err: context.DeadlineExceeded}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			type result struct {
				Errors         []string
				Deadline       []bool
				Trace          []string
				Jobs, Acquires int
				Done, Acquired bool
			}
			run := func(sink string) (result, []byte) {
				f, n, p := journalFixture(t)
				root := filepath.Dir(p)
				f.jobs = map[string]string{}
				event := &scaleset.JobStarted{RunnerName: n, RunnerID: 42, JobMessageBase: scaleset.JobMessageBase{OwnerName: "example", RepositoryName: "SECRET-repo", WorkflowRunID: 7, JobID: "SECRET-job", EventName: "workflow_dispatch"}}
				msg := &scaleset.RunnerScaleSetMessage{MessageID: 31, JobStartedMessages: []*scaleset.JobStarted{nil, event}, JobAvailableMessages: []*scaleset.JobAvailable{nil}}
				write := func(b []byte, mode os.FileMode) {
					t.Helper()
					if e := os.WriteFile(p, b, mode); e != nil {
						t.Fatal(e)
					}
				}
				switch tc.name {
				case "started-invalid", "completed-invalid":
					event.RunnerName = "../SECRET-invalid"
				case "unsafe":
					write([]byte("SECRET"), 0644)
				case "malformed":
					write([]byte("{SECRET}"), 0444)
				case "foreign":
					b, _ := json.Marshal(cleanupObservation{Schema: "foreign", Owner: "unit", Job: n})
					write(b, 0444)
				case "open":
					if e := os.Symlink(filepath.Base(p), p); e != nil {
						t.Fatal(e)
					}
				case "scheduler-invalid":
					f.jobs[n] = "net"
					event.RunnerID = 0
				case "scheduler-changed":
					f.jobs[n] = "net"
					if e := f.observeScheduler(n, 42, event.JobMessageBase, ""); e != nil {
						t.Fatal(e)
					}
					delete(f.jobs, n)
					event.WorkflowRunID++
				case "update":
					f.jobs[n] = "net"
					f.cleanupLog.syncDir = func() error { return sentinel }
				}
				if strings.HasPrefix(tc.name, "input-") {
					f.cleanupLog = nil
					f.jobs[n] = "net"
					dir := t.TempDir()
					r, e := os.OpenRoot(dir)
					if e != nil {
						t.Fatal(e)
					}
					defer r.Close()
					f.inputs = &jobInputs{root: r, uid: uint32(os.Getuid()), syncDirectory: func(string) error { return nil }}
					if e = os.Mkdir(filepath.Join(dir, n), 0700); e != nil {
						t.Fatal(e)
					}
					if e = os.WriteFile(filepath.Join(dir, n, "spec.json"), []byte("{}"), 0444); e != nil {
						t.Fatal(e)
					}
					switch tc.name {
					case "input-runner":
						delete(f.jobs, n)
						event.RunnerName = "SECRET-invalid"
						f.jobs[event.RunnerName] = "net"
					case "input-identity":
						event.WorkflowRunID = 0
					case "input-stat":
						os.Remove(filepath.Join(dir, n, "spec.json"))
						os.Symlink("spec.json", filepath.Join(dir, n, "spec.json"))
					case "input-read":
						os.Mkdir(filepath.Join(dir, n, "assignment.json"), 0700)
					case "input-sealed":
						os.WriteFile(filepath.Join(dir, n, "assignment.json"), []byte("SECRET-old"), 0444)
					case "input-write":
						f.inputs.syncDirectory = func(string) error { return sentinel }
					}
					root = dir
				}
				if tc.name == "completion-pending" {
					f.cleanupLog = nil
					f.jobs[n] = "net"
				}
				if tc.handler == "completed" {
					msg.JobStartedMessages = nil
					msg.JobCompletedMessages = []*scaleset.JobCompleted{nil, {RunnerName: event.RunnerName, RunnerID: event.RunnerID, JobMessageBase: event.JobMessageBase, Result: "failed"}}
				}
				if tc.name == "acquire-unknown" {
					msg.JobAvailableMessages = append(msg.JobAvailableMessages, &scaleset.JobAvailable{JobMessageBase: scaleset.JobMessageBase{EventName: "workflow_dispatch", RunnerRequestID: 1}})
					f.session = &localAcquireFailure{err: sentinel}
				}
				var output bytes.Buffer
				var d *scaleset.Diagnostics
				gate := make(chan struct{})
				if sink != "nil" {
					var w io.Writer = &output
					capacity := 512
					if sink != "healthy" {
						w = diagnosticFaultWriter{sink, gate}
						capacity = 1
					}
					var e error
					d, e = scaleset.NewDiagnostics(scaleset.DiagnosticIdentity{WindowID: "local-scale", SourceRevision: strings.Repeat("a", 40), ImageDigest: "sha256:" + strings.Repeat("b", 64), ScopeRef: "fixture"}, [32]byte{1}, w, capacity)
					if e != nil {
						t.Fatal(e)
					}
				}
				f.diagnostics = d
				demandCalls := 0
				if tc.name == "missing-success" || tc.name == "demand-unknown" {
					f.demandSource = func(context.Context) (int, error) {
						demandCalls++
						if tc.name == "demand-unknown" {
							return 0, sentinel
						}
						return 0, nil
					}
				}
				var got result
				old := engine.Transport
				engine.Transport = localRejectTransport{&got.Trace}
				defer func() { engine.Transport = old }()
				for i := 0; i < 2; i++ {
					if i > 0 && tc.name == "input-write" {
						if e := f.inputs.root.Remove(n + "/assignment.json"); e != nil {
							t.Fatal(e)
						}
					}
					ctx, end := d.Begin(context.Background(), scaleset.Scale, uint64(i+1))
					e := f.Scale(ctx, msg)
					end(e)
					if e != nil {
						// Injected sentinels survive unchanged, not merely via errors.Is.
						if (tc.name == "update" || tc.name == "input-write" || tc.name == "acquire-unknown" || tc.name == "demand-unknown") && e != sentinel {
							t.Fatal("error identity changed")
						}
						if e == sentinel {
							var pe *os.PathError
							if !errors.As(e, &pe) || pe != sentinel {
								t.Fatal("errors.As identity")
							}
						}
						got.Errors = append(got.Errors, strings.ReplaceAll(e.Error(), root, "<root>"))
						got.Deadline = append(got.Deadline, errors.Is(e, context.DeadlineExceeded))
					} else {
						got.Errors = append(got.Errors, "")
					}
				}
				if sink == "blocked" {
					close(gate)
				}
				closeDiagnostics(d)
				if tc.name == "missing-success" && demandCalls != 1 {
					t.Fatal("success did not reach demand")
				}
				if tc.name == "demand-unknown" && demandCalls != 2 {
					t.Fatal("unknown did not preserve demand retry")
				}
				got.Jobs = len(f.jobs)
				got.Done = f.messages[31].done
				got.Acquired = f.messages[31].acquired
				if s, ok := f.session.(*localAcquireFailure); ok {
					got.Acquires = s.acquired
				}
				return got, output.Bytes()
			}
			baseline, _ := run("nil")
			for _, sink := range []string{"healthy", "io", "short", "panic", "blocked"} {
				got, b := run(sink)
				if !reflect.DeepEqual(got, baseline) {
					t.Fatalf("%s behavior drift: %+v / %+v", sink, got, baseline)
				}
				if sink != "healthy" {
					continue
				}
				records := localRecords(t, b)
				var locals []map[string]any
				for _, r := range records {
					if v, ok := r["local"].(map[string]any); ok {
						if r["phase"] != "end" || r["parent_op_id"] == nil {
							t.Fatal("not inner end")
						}
						locals = append(locals, v)
					}
				}
				want := 2
				if tc.name == "missing-success" {
					want = 2
				}
				if len(locals) != want {
					t.Fatalf("local classification count %d", len(locals))
				}
				for i, v := range locals {
					if v["origin"] != tc.origin || v["reason"] != tc.reason || v["handler"] != tc.handler {
						t.Fatalf("classification %+v", v)
					}
					if tc.name == "started-invalid" || tc.name == "completed-invalid" {
						if v["name_valid"] != false || v["state_exists"] != false || v["journal_exists"] != nil || v["journal_safe"] != nil || v["event_index"] != float64(2) {
							t.Fatalf("unknown conflated %+v", v)
						}
					}
					if i == 0 && tc.name == "acquire-unknown" && v["acquired"] != false {
						t.Fatal("acquire state")
					}
					if tc.name != "acquire-unknown" && tc.name != "missing-success" && v["acquired"] != true {
						t.Fatal("acquired")
					}
				}
				// Synthetic, safe output feeds the real offline archive in the next layer.
				if dir := os.Getenv("R33_EVENT_OUTPUT"); dir != "" {
					if e := os.WriteFile(filepath.Join(dir, tc.name+".ndjson"), b, 0600); e != nil {
						t.Fatal(e)
					}
				}
			}
		})
	}
}

func TestDiagnosticScaleUnknownCancellationAndNil(t *testing.T) {
	var output bytes.Buffer
	d := diagnosticFixture(t, &output)
	f := &fleet{diagnostics: d}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if e := f.Scale(ctx, &scaleset.RunnerScaleSetMessage{MessageID: 1}); e != context.Canceled {
		t.Fatal("cancel identity")
	}
	if e := f.Scale(context.Background(), nil); e != nil {
		t.Fatal(e)
	}
	closeDiagnostics(d)
	rs := localRecords(t, output.Bytes())
	var vs []map[string]any
	for _, r := range rs {
		if v, ok := r["local"].(map[string]any); ok {
			vs = append(vs, v)
		}
	}
	if len(vs) != 2 || vs[0]["reason"] != "unknown" || vs[0]["acquired"] != nil || vs[1]["available"] != nil {
		t.Fatal(vs)
	}
}

func TestDiagnosticScaleLocalLossDoesNotWait(t *testing.T) {
	gate := make(chan struct{})
	d, e := scaleset.NewDiagnostics(scaleset.DiagnosticIdentity{WindowID: "full", SourceRevision: strings.Repeat("a", 40), ImageDigest: "sha256:" + strings.Repeat("b", 64), ScopeRef: "fixture"}, [32]byte{1}, diagnosticFaultWriter{"blocked", gate}, 1)
	if e != nil {
		t.Fatal(e)
	}
	f, n, _ := journalFixture(t)
	f.jobs = map[string]string{}
	n = "../SECRET"
	f.diagnostics = d
	start := time.Now()
	for i := 0; i < 100; i++ {
		if f.Scale(context.Background(), &scaleset.RunnerScaleSetMessage{MessageID: 8, JobStartedMessages: []*scaleset.JobStarted{{RunnerName: n}}}) == nil {
			t.Fatal("returned success")
		}
	}
	if time.Since(start) > time.Second || d.Loss() == 0 {
		t.Fatal("producer blocked or no queue pressure")
	}
	close(gate)
	closeDiagnostics(d)
}

func TestDiagnosticCleanupRecordReadBoundary(t *testing.T) {
	f, n, _ := journalFixture(t)
	for _, r := range []io.Reader{strings.NewReader(strings.Repeat("x", 65537)), localFaultReader{}} {
		o := &scaleObservation{scaleset.DiagnosticScaleLocal{Origin: "unknown", Reason: "unknown", Handler: "started"}}
		if _, e := f.cleanupLog.decodeRecord(n, r, o); e == nil || o.Origin != "cleanup_journal" || o.Reason != "invalid_cleanup_record_size" {
			t.Fatal(o)
		}
	}
}

type localFaultReader struct{}

func (localFaultReader) Read([]byte) (int, error) { return 0, errors.New("SECRET-read") }

type localListenSession struct{ fakeSession }

func (s *localListenSession) GetMessage(context.Context, int, int) (*scaleset.RunnerScaleSetMessage, error) {
	s.gets++
	return s.msg, nil
}
func TestDiagnosticLocalListenPropagation(t *testing.T) {
	f, n, _ := journalFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sentinel := errors.New("SECRET-update")
	calls := 0
	f.cleanupLog.syncDir = func() error {
		calls++
		if calls == 2 {
			cancel()
		}
		return sentinel
	}
	s := &localListenSession{fakeSession: fakeSession{msg: &scaleset.RunnerScaleSetMessage{MessageID: 9, JobStartedMessages: []*scaleset.JobStarted{{RunnerName: n, RunnerID: 42, JobMessageBase: scaleset.JobMessageBase{OwnerName: "example", RepositoryName: "SECRET-repo", WorkflowRunID: 7}}}, JobAvailableMessages: []*scaleset.JobAvailable{{JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: 7, EventName: "workflow_dispatch"}}}}}}
	var b bytes.Buffer
	d := diagnosticFixture(t, &b)
	f.diagnostics = d
	f.session = s
	if e := f.listen(ctx, time.Millisecond); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	closeDiagnostics(d)
	if s.gets != 1 || s.acks != 0 || s.acquired != 1 || calls != 2 {
		t.Fatal(s, calls)
	}
	rs := localRecords(t, b.Bytes())
	parents := map[float64]map[string]any{}
	var locals int
	for _, r := range rs {
		if r["stage"] == "scale" && r["phase"] == "end" {
			parents[r["op_id"].(float64)] = r
		}
	}
	for _, r := range rs {
		if v, ok := r["local"].(map[string]any); ok {
			locals++
			p := parents[r["parent_op_id"].(float64)]
			if v["reason"] != "cleanup_update_failed" || p["local"] != nil || p["outcome"] != r["outcome"] || p["logical_attempt"] != float64(locals) {
				t.Fatal(r, p)
			}
		}
	}
	if locals != 2 {
		t.Fatal(locals)
	}
}

func TestDiagnosticJournalUnknownDoesNotRetainEarlierRead(t *testing.T) {
	f, n, _ := journalFixture(t)
	if e := f.cleanupLog.update(n, func(v *cleanupObservation) error { v.StartedAt = "2026-10-25T00:00:00Z"; return nil }); e != nil {
		t.Fatal(e)
	}
	o := &scaleObservation{scaleset.DiagnosticScaleLocal{Origin: "unknown", Reason: "unknown", Handler: "started"}}
	if _, e := f.cleanupLog.readObserved(n, o); e != nil || o.JournalSafe == nil || !*o.JournalSafe {
		t.Fatal("initial read")
	}
	if e := f.cleanupLog.root.Remove(n + ".json"); e != nil {
		t.Fatal(e)
	}
	if _, e := f.cleanupLog.readObserved(n, o); !errors.Is(e, os.ErrNotExist) || o.JournalExists == nil || *o.JournalExists || o.JournalSafe != nil {
		t.Fatal("stale journal observation", o)
	}
}
