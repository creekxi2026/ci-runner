package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/actions/scaleset"
)

// A slow, independently delivered scheduler observation must not revoke a valid
// allocation already received on Listener's pipe. This crosses the OLD 30s
// cutoff in real time; no clock or intake implementation is replaced.
func TestAssignmentArrivesAfterThirtySeconds(t *testing.T) {
	f, b, name, conn := assignmentSession(t)
	if err := json.NewEncoder(conn).Encode(obj{"schema": "ci-job-intake/v1", "message": obj{"jobId": "synthetic"}}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(31500 * time.Millisecond)
	publishAssignment(t, f, name)
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if v, err := bufio.NewReader(conn).ReadByte(); err != nil || v != '0' {
		t.Fatalf("valid delayed assignment rejected: %q %v", v, err)
	}
	b.closeIntakes()
}

// The peer can close after its packet but before the scheduler observation.
// Waiting must not leave a goroutine/connection alive until an unrelated timer.
func TestAssignmentWaitPeerDisconnectIsPrompt(t *testing.T) {
	_, b, _, conn := assignmentSession(t)
	if err := json.NewEncoder(conn).Encode(obj{"schema": "ci-job-intake/v1", "message": obj{"jobId": "synthetic"}}); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	done := make(chan struct{})
	go func() { b.intakes.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("assignment wait retained disconnected peer")
	}
}

func TestAssignmentWaitCancellationAndUnexpectedData(t *testing.T) {
	for _, reason := range []string{"cleanup", "second-packet", "empty-probe"} {
		t.Run(reason, func(t *testing.T) {
			_, b, name, conn := assignmentSession(t)
			if reason != "empty-probe" {
				if err := json.NewEncoder(conn).Encode(obj{"schema": "ci-job-intake/v1", "message": obj{}}); err != nil {
					t.Fatal(err)
				}
			}
			if reason == "cleanup" {
				b.cancelIntake(name)
			} else if reason == "empty-probe" {
				conn.Close()
			} else {
				conn.Write([]byte("x"))
			}
			done := make(chan struct{})
			go func() { b.intakes.wg.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("revoked assignment wait survived")
			}
			if _, err := b.root.Stat(name + "/assignment.json"); !os.IsNotExist(err) {
				t.Fatal("wait invented assignment")
			}
		})
	}
}

func TestAssignmentBeforePacketAndShortDelay(t *testing.T) {
	for _, before := range []bool{true, false} {
		t.Run(fmt.Sprint(before), func(t *testing.T) {
			f, b, name, conn := assignmentSession(t)
			if before {
				publishAssignment(t, f, name)
			}
			if err := json.NewEncoder(conn).Encode(obj{"schema": "ci-job-intake/v1", "message": obj{}}); err != nil {
				t.Fatal(err)
			}
			if !before {
				time.Sleep(150 * time.Millisecond)
				publishAssignment(t, f, name)
			}
			conn.SetReadDeadline(time.Now().Add(3 * time.Second))
			if v, err := bufio.NewReader(conn).ReadByte(); err != nil || v != '0' {
				t.Fatalf("assignment failed: %q %v", v, err)
			}
			b.closeIntakes()
		})
	}
}

func TestAssignmentMissingHonorsParentDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if waitIntakeAssignment(ctx, t.TempDir()) {
		t.Fatal("missing assignment became ready")
	}
}

func TestAssignmentMissingCannotExceedAdmissionBudget(t *testing.T) {
	directory := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		if waitIntakeAssignment(context.Background(), directory) {
			t.Fatal("missing assignment became ready")
		}
		if time.Since(start) != 10*time.Minute {
			t.Fatal("assignment wait exceeded initial admission budget")
		}
	})
}

func assignmentSession(t *testing.T) (*fleet, *jobInputs, string, net.Conn) {
	t.Helper()
	f, b := inputFixture(t, `IFS= read -r call || [ -n "$call" ]
case "$call" in *ci-job-input-call/v1*) exit 0;; *) printf 0; sleep 30;; esac`)
	b.policy.Schema = "ci-job-inputs/v2"
	t.Cleanup(b.closeIntakes)
	name := "ci-job-assignment-test"
	if err := f.prepareJobInputs(context.Background(), name, inputSpec()); err != nil {
		t.Fatal(err)
	}
	d, err := os.Open(filepath.Join(b.root.Name(), name, "intake"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	material, err := b.takeBootstrap(name)
	if err != nil {
		t.Fatal("fixture bootstrap unavailable")
	}
	defer clear(material)
	conn, err := net.DialTimeout("unix", fmt.Sprintf("/proc/self/fd/%d/session.sock", d.Fd()), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	testAuthenticateIntake(t, conn, material)
	return f, b, name, conn
}

func publishAssignment(t *testing.T, f *fleet, name string) {
	t.Helper()
	event := &scaleset.JobStarted{RunnerName: name, RunnerID: 42, JobMessageBase: scaleset.JobMessageBase{OwnerName: "example", RepositoryName: "dummy", WorkflowRunID: 7, JobID: "synthetic", EventName: "push"}}
	if err := f.assignJobInputs(event); err != nil {
		t.Fatal(err)
	}
}
