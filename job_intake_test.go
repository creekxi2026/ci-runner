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
	directory := filepath.Join(dir, "ci-job-dummy")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "assignment.json"), []byte(`{"fixture":true}`), 0444); err != nil {
		t.Fatal(err)
	}
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
		runIntake(ctx, host, directory, policy)
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
	if len(entries) != 2 || entries[0].Name() != "adapter.py" || entries[1].Name() != "ci-job-dummy" {
		t.Fatal("session left files")
	}
	entries, _ = os.ReadDir(directory)
	if len(entries) != 1 || entries[0].Name() != "assignment.json" {
		t.Fatal("allocation persisted data")
	}
}

func TestIntakeRejectsMalformedPacketsBeforeAdapter(t *testing.T) {
	for _, payload := range []string{
		`{"schema":"foreign","message":{}}` + "\n",
		strings.Repeat("x", intakeLimit+1),
		`{"schema":"ci-job-intake/v1","message":{},"extra":true}` + "\n",
		`{"schema":"ci-job-intake/v1","message":{"id":1,"id":2}}` + "\n",
		`{"schema":"ci-job-intake/v1","schema":"ci-job-intake/v1","message":{}}` + "\n",
		`{"Schema":"ci-job-intake/v1","message":{}}` + "\n",
		`{"schema":"ci-job-intake/v1","message":null}` + "\n",
		`{"schema":"ci-job-intake/v1","message":[]}` + "\n",
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

func TestIntakePreservesUnicodeAllocationThroughActualAdapter(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "assignment.json"), []byte(`{"fixture":true}`), 0444); err != nil {
		t.Fatal(err)
	}
	command := filepath.Join(dir, "adapter.py")
	body := []byte("#!/usr/bin/env python3\nimport json,sys,time\nx=json.loads(sys.stdin.readline())\nassert x['message']['jobDisplayName']=='登记验证结论'\nassert x['message']['contextData']['github']['event']['ref']=='分支/修复-🧪'\nprint('0',end='',flush=True)\ntime.sleep(30)\n")
	if err := os.WriteFile(command, body, 0700); err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(body)
	host, worker := net.Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	defer worker.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer host.Close()
		runIntake(ctx, host, dir, inputPolicy{Command: []string{command}, Digest: hex.EncodeToString(h[:])})
	}()
	message := obj{"jobDisplayName": "登记验证结论", "contextData": obj{"github": obj{"event": obj{"ref": "分支/修复-🧪"}}}}
	if err := json.NewEncoder(worker).Encode(obj{"schema": "ci-job-intake/v1", "message": message}); err != nil {
		t.Fatal(err)
	}
	if value, err := bufio.NewReader(worker).ReadByte(); err != nil || value != '0' {
		t.Fatalf("Unicode allocation did not reach the actual adapter: %c %v", value, err)
	}
	worker.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Unicode allocation retained its helper")
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
	material, err := b.takeBootstrap(name)
	if err != nil {
		t.Fatal("fixture bootstrap unavailable")
	}
	defer clear(material)
	conn, err := net.DialTimeout("unix", fmt.Sprintf("/proc/self/fd/%d/session.sock", socketDir.Fd()), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	testAuthenticateIntake(t, conn, material)
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

// Invoked explicitly by tests/native_worker_probe.py in an offline runner image.
// The adapter is a transport fixture, not project authentication or an Actions API.
func TestNativeWorkerProbeServer(t *testing.T) {
	address := os.Getenv("CI_NATIVE_PROBE_SOCKET")
	if address == "" {
		t.Skip("explicit offline .NET probe only")
	}
	dir := t.TempDir()
	// runIntake observes the allocation directory beside the socket. Publishing
	// into the unrelated Go temp dir leaves the real Worker waiting forever.
	if err := os.WriteFile(filepath.Join(filepath.Dir(address), "assignment.json"), []byte(`{"fixture":true}`), 0444); err != nil {
		t.Fatal(err)
	}
	command := filepath.Join(dir, "adapter.py")
	body := []byte("#!/usr/bin/env python3\nimport json,sys,time\nx=json.loads(sys.stdin.readline())\nassert x['schema']=='ci-job-input-session/v1'\nassert x['message']['requestId']==131\nassert x['message']['jobId']=='7f5675bc-929c-4d5c-bf58-41f07d452abc'\nassert x['directory'].startswith('/tmp/native-intake-')\nprint(" + fmt.Sprintf("%q", os.Getenv("CI_NATIVE_PROBE_REPLY")) + ",end='',flush=True)\ntime.sleep(30)\n")
	if err := os.WriteFile(command, body, 0700); err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(body)
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: address, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	fmt.Println("listening")
	conn, err := listener.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	runIntake(ctx, conn, filepath.Dir(address), inputPolicy{Command: []string{command}, Digest: hex.EncodeToString(h[:])})
}
