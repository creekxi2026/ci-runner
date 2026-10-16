package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/actions/scaleset"
)

func TestIntakeDisconnectKillsAdapterAndNeverPersistsJobMessage(t *testing.T) {
	dir := t.TempDir()
	command := filepath.Join(dir, "adapter.py")
	body := []byte("#!/usr/bin/env python3\nimport json,sys,time\nx=json.loads(sys.stdin.readline())\nassert x['message']['variables']['system.github.token']['value']=='fixture-secret'\nassert x['directory'].endswith('ci-job-dummy')\nprint('0',end='',flush=True)\ntime.sleep(30)\n")
	if e := os.WriteFile(command, body, 0700); e != nil {
		t.Fatal(e)
	}
	h := sha256.Sum256(body)
	policy := inputPolicy{Command: []string{command}, Digest: hex.EncodeToString(h[:])}
	// Paths under a developer checkout are intentionally not treated as an
	// installed root policy. runIntake itself owns no file access or policy trust.
	host, worker := net.Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer host.Close()
		runIntake(ctx, host, filepath.Join(dir, "ci-job-dummy"), policy)
	}()
	msg := obj{"schema": "ci-job-intake/v1", "message": obj{"variables": obj{"system.github.token": obj{"value": "fixture-secret"}}}}
	json.NewEncoder(worker).Encode(msg)
	if b, e := bufio.NewReader(worker).ReadByte(); e != nil || b != '0' {
		t.Fatalf("no ready: %c %v", b, e)
	}
	worker.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("disconnected helper survived")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != "adapter.py" {
		t.Fatal("session left files")
	}
}

func TestIntakeRejectsMalformedPacketsBeforeAdapter(t *testing.T) {
	for _, payload := range []string{
		`{"schema":"foreign","message":{}}` + "\n",
		strings.Repeat("x", intakeLimit+1),
		`{"schema":"ci-job-intake/v1","message":{},"extra":true}` + "\n",
		`{"schema":"ci-job-intake/v1","message":{"id":1,"id":2}}` + "\n",
	} {
		t.Run(payload[:12], func(t *testing.T) {
			dir := t.TempDir()
			command := filepath.Join(dir, "adapter.sh")
			sentinel := filepath.Join(dir, "ran")
			source := []byte("#!/bin/sh\ntouch " + sentinel + "\nprintf 0\n")
			if err := os.WriteFile(command, source, 0700); err != nil {
				t.Fatal(err)
			}
			h := sha256.Sum256(source)
			host, worker := net.Pipe()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			done := make(chan struct{})
			go func() {
				defer close(done)
				defer host.Close()
				runIntake(ctx, host, dir,
					inputPolicy{Command: []string{command}, Digest: hex.EncodeToString(h[:])})
			}()
			go func() { worker.Write([]byte(payload)); worker.Close() }()
			<-done
			if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
				t.Fatal("malformed packet executed adapter")
			}
		})
	}
}

func TestIntakeMountPolicyAllowsSealedLauncherButRejectsRuntimeShadow(t *testing.T) {
	for _, target := range []string{"/opt/ci-release/bin", "/run/dummy"} {
		if !intakeTarget(target) {
			t.Fatalf("sealed launcher rejected: %s", target)
		}
	}
	for _, target := range []string{"/", "/opt", "/opt/ci", "/opt/actions-runner/bin", "/opt/go/bin", "/usr/bin", "/run/../opt/test", "/opt/ci-tools"} {
		if intakeTarget(target) {
			t.Fatalf("runtime shadow accepted: %s", target)
		}
	}
}

func TestIntakeCancellationBeforePacketDoesNotRetainConnection(t *testing.T) {
	dir := t.TempDir()
	command := filepath.Join(dir, "adapter.sh")
	source := []byte("#!/bin/sh\nexit 0\n")
	os.WriteFile(command, source, 0700)
	h := sha256.Sum256(source)
	host, worker := net.Pipe()
	defer worker.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer host.Close()
		runIntake(ctx, host, dir, inputPolicy{Command: []string{command}, Digest: hex.EncodeToString(h[:])})
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancelled intake retained its pre-authentication connection")
	}
}

func TestInputV2SingleUseAllocationAndControllerTeardown(t *testing.T) {
	f, b := inputFixture(t, `IFS= read -r call || [ -n "$call" ]
case "$call" in *ci-job-input-call/v1*) exit 0;; *) printf 0; sleep 30;; esac`)
	b.policy.Schema = "ci-job-inputs/v2"
	b.policy.Mounts = append(b.policy.Mounts, inputMount{Name: "launcher", Target: "/opt/dummy/bin"})
	t.Cleanup(b.closeIntakes)
	name := "ci-job-intake-test"
	spec := inputSpec()
	if err := f.prepareJobInputs(context.Background(), name, spec); err != nil {
		t.Fatal(err)
	}
	event := &scaleset.JobStarted{RunnerName: name, RunnerID: 42, JobMessageBase: scaleset.JobMessageBase{OwnerName: "example", RepositoryName: "dummy", WorkflowRunID: 7, JobID: "opaque", EventName: "push"}}
	if err := f.assignJobInputs(event); err != nil {
		t.Fatal(err)
	}
	address := filepath.Join(b.root.Name(), name, "intake/session.sock")
	socketDir, err := os.Open(filepath.Dir(address))
	if err != nil {
		t.Fatal(err)
	}
	defer socketDir.Close()
	conn, err := net.DialTimeout("unix", fmt.Sprintf("/proc/self/fd/%d/session.sock", socketDir.Fd()), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	if err = json.NewEncoder(conn).Encode(obj{"schema": "ci-job-intake/v1", "message": obj{"jobId": "opaque"}}); err != nil {
		t.Fatal(err)
	}
	if value, err := bufio.NewReader(conn).ReadByte(); err != nil || value != '0' {
		t.Fatalf("allocation rejected: %c %v", value, err)
	}
	if _, err := os.Stat(address); !os.IsNotExist(err) {
		t.Fatal("single-use socket retained its name")
	}
	b.closeIntakes()
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("controller exit retained helper connection")
	}
}
