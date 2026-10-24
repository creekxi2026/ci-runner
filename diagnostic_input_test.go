package main

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/actions/scaleset"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A separate process proves FIFO open cannot strand the controller entry point.
func TestDiagnosticInputChild(t *testing.T) {
	p := os.Getenv("DIAGNOSTIC_TEST_PATH")
	if p == "" {
		return
	}
	imageRevision = strings.Repeat("a", 40)
	var out bytes.Buffer
	d := openDiagnostics(p, &out)
	enabled := d != nil
	closeDiagnostics(d)
	if enabled != (os.Getenv("DIAGNOSTIC_TEST_VALID") == "1") {
		t.Fatal("unexpected diagnostic availability")
	}
}

func TestDiagnosticInputTypesBounded(t *testing.T) {
	for _, target := range []string{"profile", "salt"} {
		for _, kind := range []string{"valid", "fifo", "directory", "symlink", "hardlink", "mode", "oversize"} {
			t.Run(target+"-"+kind, func(t *testing.T) {
				dir := t.TempDir()
				p := filepath.Join(dir, "profile.json")
				salt := filepath.Join(dir, "salt")
				id := scaleset.DiagnosticIdentity{WindowID: "test", SourceRevision: strings.Repeat("a", 40), ImageDigest: "sha256:" + strings.Repeat("b", 64), ScopeRef: "scope"}
				data, _ := json.Marshal(id)
				if err := os.WriteFile(p, data, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(salt, bytes.Repeat([]byte{1}, 32), 0600); err != nil {
					t.Fatal(err)
				}
				name := p
				if target == "salt" {
					name = salt
				}
				var err error
				switch kind {
				case "fifo":
					os.Remove(name)
					err = syscall.Mkfifo(name, 0600)
				case "directory":
					os.Remove(name)
					err = os.Mkdir(name, 0600)
				case "symlink":
					os.Rename(name, name+".real")
					err = os.Symlink(name+".real", name)
				case "hardlink":
					err = os.Link(name, name+".link")
				case "mode":
					err = os.Chmod(name, 0644)
				case "oversize":
					err = os.WriteFile(name, bytes.Repeat([]byte{'x'}, 4097), 0600)
				}
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestDiagnosticInputChild$")
				cmd.Env = append(os.Environ(), "GORACE=atexit_sleep_ms=0", "DIAGNOSTIC_TEST_PATH="+p, "CI_DIAGNOSTIC_OWNER_UID="+strconv.Itoa(os.Geteuid()), "DIAGNOSTIC_TEST_VALID="+map[bool]string{true: "1", false: "0"}[kind == "valid"])
				out, err := cmd.CombinedOutput()
				if ctx.Err() != nil {
					t.Fatal("diagnostic input blocked before business entry")
				}
				if err != nil {
					t.Fatalf("%v: %s", err, out)
				}
			})
		}
	}
}
