package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
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

// Real Scale and filesystem handlers, no live Docker or GitHub.
// Out-of-namespace events now commit routing dispositions and complete; the
// lower-level cleanup reader still rejects these names (cleanup_journal_test). Every failure
// runs twice to exercise the same message's acquired/retry state.
func TestScaleLocalFailureBranches(t *testing.T) {
	cases := []struct {
		name, handler string
		failure       bool
	}{
		{"started-invalid", "unknown", false},
		{"completed-invalid", "unknown", false},
		{"unsafe", "started", true},
		{"malformed", "started", true},
		{"foreign", "started", true},
		{"open", "started", true},
		{"scheduler-invalid", "started", true},
		{"scheduler-changed", "started", true},
		{"update", "started", true},
		{"completion-pending", "completed", true},
		{"acquire-unknown", "unknown", true},
		{"missing-success", "unknown", false},
		{"demand-unknown", "unknown", true},
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
			run := func() result {
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
				if tc.name == "completion-pending" {
					f.cleanupLog = nil
					f.jobs[n] = "net"
				}
				if tc.handler == "completed" || tc.name == "completed-invalid" {
					msg.JobStartedMessages = nil
					msg.JobCompletedMessages = []*scaleset.JobCompleted{nil, {RunnerName: event.RunnerName, RunnerID: event.RunnerID, JobMessageBase: event.JobMessageBase, Result: "failed"}}
				}
				if tc.name == "acquire-unknown" {
					msg.JobAvailableMessages = append(msg.JobAvailableMessages, &scaleset.JobAvailable{JobMessageBase: scaleset.JobMessageBase{EventName: "workflow_dispatch", RunnerRequestID: 1}})
					f.session = &localAcquireFailure{err: sentinel}
				}
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
					e := f.Scale(context.Background(), msg)
					if e != nil {
						// Injected sentinels survive unchanged, not merely via errors.Is.
						if (tc.name == "update" || tc.name == "acquire-unknown" || tc.name == "demand-unknown") && e != sentinel {
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
				return got
			}
			got := run()
			for _, message := range got.Errors {
				if (message != "") != tc.failure {
					t.Fatalf("expected failure=%v, got %+v", tc.failure, got)
				}
			}
			if got.Done == tc.failure {
				t.Fatalf("message completion changed: %+v", got)
			}
			if tc.name == "acquire-unknown" {
				if got.Acquired || got.Acquires != 2 {
					t.Fatalf("acquire failure was committed or not retried: %+v", got)
				}
			} else if tc.failure && !got.Acquired {
				t.Fatalf("successful acquire lost on handler failure: %+v", got)
			}
			if tc.name == "completion-pending" && got.Jobs != 1 {
				t.Fatalf("pending cleanup lost its job: %+v", got)
			}

		})
	}
}

func TestScaleCancellationAndNil(t *testing.T) {
	f := &fleet{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if e := f.Scale(ctx, &scaleset.RunnerScaleSetMessage{MessageID: 1}); e != context.Canceled {
		t.Fatal(e)
	}
	if e := f.Scale(context.Background(), nil); e != nil {
		t.Fatal(e)
	}
}

func TestCleanupRecordReadBoundary(t *testing.T) {
	f, n, _ := journalFixture(t)
	for _, r := range []io.Reader{strings.NewReader(strings.Repeat("x", 65537)), localFaultReader{}} {
		if _, e := f.cleanupLog.decodeRecord(n, r); e == nil {
			t.Fatal("unbounded or faulty read accepted")
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
func TestLocalListenFailurePropagation(t *testing.T) {
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
	f.session = s
	if e := f.listen(ctx, time.Millisecond); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	if s.gets != 1 || s.acks != 0 || s.acquired != 1 || calls != 2 {
		t.Fatal(s, calls)
	}
}
