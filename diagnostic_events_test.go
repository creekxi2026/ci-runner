package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/actions/scaleset"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func diagnosticFixture(t *testing.T, w *bytes.Buffer) *scaleset.Diagnostics {
	t.Helper()
	d, e := scaleset.NewDiagnostics(scaleset.DiagnosticIdentity{WindowID: "test-window", SourceRevision: strings.Repeat("a", 40), ImageDigest: "sha256:" + strings.Repeat("b", 64), ScopeRef: "test-scope"}, [32]byte{1}, w, 4096)
	if e != nil {
		t.Fatal(e)
	}
	return d
}
func TestDiagnosticControllerRetryEquivalence(t *testing.T) {
	type trace struct {
		Gets, Acks, Acquired, Jobs int
		Cancelled                  bool
	}
	run := func(on bool) (trace, []byte) {
		var output bytes.Buffer
		var d *scaleset.Diagnostics
		if on {
			d = diagnosticFixture(t, &output)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		s := &fakeSession{cancel: cancel, msg: &scaleset.RunnerScaleSetMessage{MessageID: 8, JobAvailableMessages: []*scaleset.JobAvailable{{JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: 9, EventName: "workflow_dispatch"}}}}}
		f := &fleet{diagnostics: d, session: s, jobs: map[string]string{"busy": "busy-net"}}
		e := f.listen(ctx, time.Millisecond)
		closeDiagnostics(d)
		return trace{s.gets, s.acks, s.acquired, len(f.jobs), errors.Is(e, context.Canceled)}, output.Bytes()
	}
	a, _ := run(false)
	b, output := run(true)
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("drift %v %v", a, b)
	}
	for _, stage := range []string{"poll", "ack", "scale"} {
		if !bytes.Contains(output, []byte(`"stage":"`+stage+`"`)) {
			t.Fatal("missing", stage)
		}
	}
}
func TestDiagnosticProfilePrivateAndDefaultNil(t *testing.T) {
	var b bytes.Buffer
	if openDiagnostics("", &b) != nil {
		t.Fatal("default enabled")
	}
	dir := t.TempDir()
	old := imageRevision
	imageRevision = strings.Repeat("a", 40)
	defer func() { imageRevision = old }()
	id := scaleset.DiagnosticIdentity{WindowID: "test", SourceRevision: imageRevision, ImageDigest: "sha256:" + strings.Repeat("b", 64), ScopeRef: "scope"}
	data, _ := json.Marshal(id)
	profile := filepath.Join(dir, "profile.json")
	salt := filepath.Join(dir, "salt")
	if e := os.WriteFile(profile, data, 0600); e != nil {
		t.Fatal(e)
	}
	os.WriteFile(salt, bytes.Repeat([]byte{1}, 32), 0600)
	d := openDiagnostics(profile, &b)
	if d == nil {
		t.Fatal("valid private profile rejected")
	}
	_, end := d.Begin(context.Background(), scaleset.InitConfig, 1)
	end(nil)
	closeDiagnostics(d)
	os.Chmod(profile, 0644)
	if openDiagnostics(profile, &b) != nil {
		t.Fatal("public profile accepted")
	}
	os.Chmod(profile, 0600)
	os.WriteFile(salt, make([]byte, 32), 0600)
	if openDiagnostics(profile, &b) != nil {
		t.Fatal("zero salt accepted")
	}
}
func TestDiagnosticCancelledStartEquivalence(t *testing.T) {
	for _, on := range []bool{false, true} {
		var output bytes.Buffer
		var d *scaleset.Diagnostics
		if on {
			d = diagnosticFixture(t, &output)
		}
		f := &fleet{diagnostics: d, jobs: map[string]string{}}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if !errors.Is(f.start(ctx), context.Canceled) || len(f.jobs) != 0 {
			t.Fatal("cancel semantics")
		}
		closeDiagnostics(d)
	}
}

func TestDiagnosticBootstrapDemandRetryEquivalence(t *testing.T) {
	type result struct {
		Demand, Gets, Acks, Acquired, Pending int
		Canceled                              bool
	}
	run := func(on bool) (result, []byte) {
		var output bytes.Buffer
		var d *scaleset.Diagnostics
		if on {
			d = diagnosticFixture(t, &output)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		s := &fakeSession{cancel: cancel, msg: &scaleset.RunnerScaleSetMessage{MessageID: 8}}
		calls := 0
		f := &fleet{diagnostics: d, session: s, jobs: map[string]string{}, demandSource: func(context.Context) (int, error) {
			calls++
			if calls == 1 {
				return 0, errors.New("SECRET-demand")
			}
			return 0, nil
		}}
		err := f.listen(ctx, time.Millisecond)
		closeDiagnostics(d)
		return result{calls, s.gets, s.acks, s.acquired, len(f.messages), errors.Is(err, context.Canceled)}, output.Bytes()
	}
	a, _ := run(false)
	b, events := run(true)
	if !reflect.DeepEqual(a, b) || a.Demand < 2 || !a.Canceled {
		t.Fatalf("bootstrap/demand retry drift: %+v %+v", a, b)
	}
	for _, stage := range []string{"bootstrap", "demand", "reconcile", "poll", "ack"} {
		if !bytes.Contains(events, []byte(`"stage":"`+stage+`"`)) {
			t.Fatal("missing stage", stage)
		}
	}
	if bytes.Contains(events, []byte("SECRET")) {
		t.Fatal("demand secret reached sink")
	}
}
