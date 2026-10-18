package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Explicit offline transport server. The external probe supplies its own
// orchestrator and optional sealed helper/directory; no provider is built in.
// The dummy helper grants no project/provider authority. Production startIntake
// and scheduler publication are separately exercised by assignment_wait_test.go.
func TestNativeAssignmentProbeServer(t *testing.T) {
	mode := os.Getenv("CI_NATIVE_ASSIGNMENT_CASE")
	if mode == "" {
		t.Skip("explicit offline native assignment probe only")
	}
	if os.Getuid() != 0 {
		t.Fatal("controller fixture requires isolated root")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "helper-ran")
	command := filepath.Join(dir, "helper.sh")
	body := []byte("#!/bin/sh\ncat >/dev/null\ntouch '" + marker + "'\nprintf 0\nsleep 60\n")
	if err := os.WriteFile(command, body, 0555); err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(body)
	external := os.Getenv("CI_NATIVE_ASSIGNMENT_DIRECTORY") != ""
	if external {
		dir = os.Getenv("CI_NATIVE_ASSIGNMENT_DIRECTORY")
		command = os.Getenv("CI_NATIVE_ASSIGNMENT_COMMAND")
		raw, err := os.ReadFile(command)
		if err != nil {
			t.Fatal(err)
		}
		h = sha256.Sum256(raw)
	}
	assignment := func() {
		if err := os.WriteFile(filepath.Join(dir, "assignment.json"), []byte(`{"synthetic":true}`), 0444); err != nil {
			t.Error(err)
		}
	}
	if mode == "before" {
		assignment()
	}
	address := "/run/ci-intake/session.sock"
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: address, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := os.Chmod(address, 0666); err != nil {
		t.Fatal(err)
	}
	fmt.Println("listening")
	conn, err := listener.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	listener.Close() // retain production's single-use/no-reconnect boundary
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	if mode == "late" {
		done := make(chan struct{})
		defer func() { <-done }()
		go func() {
			defer close(done)
			if pause(ctx, 31500*time.Millisecond) {
				assignment()
			}
		}()
	}
	if mode == "missing" {
		timer := time.AfterFunc(200*time.Millisecond, cancel)
		defer timer.Stop()
	}
	runIntake(ctx, conn, dir, inputPolicy{Command: []string{command}, Digest: hex.EncodeToString(h[:])})
	_, err = os.Stat(marker)
	if external {
		if _, err := os.Stat(filepath.Join(dir, "gate/ready.json")); err != nil {
			t.Fatal("external helper did not publish its gate")
		}
	} else if (err == nil) != (mode == "before" || mode == "late") {
		t.Fatal("helper crossed assignment/revocation boundary")
	}
}
