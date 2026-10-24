package scaleset

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

func diagTest(t *testing.T, w io.Writer, n int) *Diagnostics {
	t.Helper()
	d, e := NewDiagnostics(DiagnosticIdentity{"window", "0123456789012345678901234567890123456789", "sha256:" + strings.Repeat("a", 64), "scope"}, [32]byte{1}, w, n)
	if e != nil {
		t.Fatal(e)
	}
	return d
}
func finishDiag(t *testing.T, d *Diagnostics) bool {
	t.Helper()
	ctx, c := context.WithTimeout(context.Background(), time.Second)
	defer c()
	return d.Close(ctx)
}
func TestDiagnosticSchemaPrivacy(t *testing.T) {
	var b bytes.Buffer
	d := diagTest(t, &b, 100)
	ctx, end := d.Begin(context.Background(), Poll, 1)
	ctx = d.sessionContext(ctx, "SECRET-session")
	diagnosticHTTPBegin(ctx)
	diagnosticHTTPEnd(ctx, 202, nil, false)
	end(errors.New("SECRET-error/URL"))
	if !finishDiag(t, d) {
		t.Fatal("loss")
	}
	if strings.Contains(b.String(), "SECRET") {
		t.Fatal("leak")
	}
	var seq uint64
	for _, line := range bytes.Split(bytes.TrimSpace(b.Bytes()), []byte("\n")) {
		if len(line)+1 > 2048 {
			t.Fatal("line cap")
		}
		var e DiagnosticEvent
		if json.Unmarshal(line, &e) != nil || e.Seq <= seq || e.Schema != "ci-stage-v1" {
			t.Fatal(string(line))
		}
		seq = e.Seq
	}
}

type badDiagnosticWriter struct {
	kind string
	gate chan struct{}
}

func (w badDiagnosticWriter) Write(b []byte) (int, error) {
	switch w.kind {
	case "panic":
		panic("SECRET")
	case "short":
		return 1, nil
	case "block":
		<-w.gate
		return len(b), nil
	default:
		return 0, io.ErrClosedPipe
	}
}
func TestDiagnosticWriterFaultIsolation(t *testing.T) {
	for _, k := range []string{"panic", "short", "io", "block"} {
		t.Run(k, func(t *testing.T) {
			gate := make(chan struct{})
			d := diagTest(t, badDiagnosticWriter{k, gate}, 1)
			start := time.Now()
			for i := 0; i < 100; i++ {
				_, end := d.Begin(context.Background(), Poll, 1)
				end(nil)
			}
			if time.Since(start) > time.Second {
				t.Fatal("blocked producer")
			}
			ctx, c := context.WithTimeout(context.Background(), 10*time.Millisecond)
			defer c()
			if d.Close(ctx) {
				t.Fatal("incomplete accepted")
			}
			close(gate)
			select {
			case <-d.done:
			case <-time.After(time.Second):
				t.Fatal("writer not recovered")
			}
		})
	}
}
func TestDiagnosticConcurrentLossAndHMAC(t *testing.T) {
	var b bytes.Buffer
	d := diagTest(t, &b, 4096)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Go(func() {
			ctx, end := d.Begin(context.Background(), SessionRefresh, 1)
			a := d.sessionContext(ctx, "secret").Value(diagnosticSessionKey{}).(*string)
			c := d.sessionContext(ctx, "secret").Value(diagnosticSessionKey{}).(*string)
			if *a != *c || len(*a) != 64 {
				t.Error("HMAC")
			}
			end(context.Canceled)
		})
	}
	wg.Wait()
	if !finishDiag(t, d) {
		t.Fatal("unexpected loss")
	}
}
func TestDiagnosticInvalidIdentityAndSalt(t *testing.T) {
	_, err := NewDiagnostics(DiagnosticIdentity{}, [32]byte{}, io.Discard, 1)
	if err == nil {
		t.Fatal("invalid accepted")
	}
	var b bytes.Buffer
	d := diagTest(t, &b, 10)
	_, end := d.Begin(context.Background(), DiagnosticStage("SECRET"), 1)
	end(nil)
	if finishDiag(t, d) {
		t.Fatal("invalid stage")
	}
	if strings.Contains(b.String(), "SECRET") {
		t.Fatal("leak")
	}
}
