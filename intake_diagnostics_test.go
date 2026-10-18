package main

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Catch missing stage retention, unsafe helper stderr forwarding, and cleanup ordering.
func TestIntakeStagesRejectWithoutLeakingPacket(t *testing.T) {
	t.Setenv("CI_INTAKE_DIAGNOSTICS", "1")
	var out bytes.Buffer
	old := log.Writer()
	log.SetOutput(&out)
	defer log.SetOutput(old)
	host, client := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer host.Close()
		runIntake(context.Background(), host, filepath.Join(t.TempDir(), "ci-job-diag"), inputPolicy{Command: []string{"/missing"}})
	}()
	client.Write([]byte("{\"secret\":\"SENTINEL_PRIVATE\"}\n"))
	client.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("intake blocked")
	}
	if !strings.Contains(out.String(), "packet_rejected") || strings.Contains(out.String(), "SENTINEL_PRIVATE") {
		t.Fatalf("unsafe/missing stage: %s", out.String())
	}
}

func TestCleanupRetainsOnlySafeWorkerStages(t *testing.T) {
	t.Setenv("CI_INTAKE_DIAGNOSTICS", "1")
	var out bytes.Buffer
	old := log.Writer()
	log.SetOutput(&out)
	defer log.SetOutput(old)
	var order []string
	withDockerHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		order = append(order, r.Method+" "+r.URL.Path)
		if strings.HasSuffix(r.URL.Path, "/logs") {
			w.Write(logFrame("2026-10-06T17:39:42Z ci-intake/v1 frame_parsed\n2026-10-06T17:39:42Z ci-intake/v1 SENTINEL_PRIVATE\n"))
			return
		}
		w.WriteHeader(204)
	})
	f := &fleet{jobs: map[string]string{"ci-job-diag": "net"}}
	f.cleanup("ci-job-diag", "net")
	joined := strings.Join(order, "\n")
	read := strings.Index(joined, "GET /v1.47/containers/ci-job-diag/logs")
	if read < 0 || read > strings.Index(joined, "DELETE /v1.47/containers/ci-job-diag") {
		t.Fatal("worker diagnostics destroyed")
	}
	if !strings.Contains(out.String(), "frame_parsed") || strings.Contains(out.String(), "SENTINEL_PRIVATE") {
		t.Fatal("unsafe/missing retained stage")
	}
	if len(f.jobs) != 0 {
		t.Fatal("diagnostics blocked cleanup")
	}
}

func TestHelperDiagnosticFilterBoundsAndRejectsRawStderr(t *testing.T) {
	t.Setenv("CI_INTAKE_DIAGNOSTICS", "1")
	var out bytes.Buffer
	old := log.Writer()
	log.SetOutput(&out)
	defer log.SetOutput(old)
	w := &intakeStderr{job: "ci-job-diag"}
	for _, s := range []string{"SENTINEL_PRIVATE\n", "ci-intake/v1 helper_entry SENTINEL_PRIVATE\n", strings.Repeat("x", 5000) + "\n", "ci-intake/v1 helper_", "entry\n"} {
		w.Write([]byte(s))
	}
	for i := 0; i < 1000; i++ {
		w.Write([]byte("ci-intake/v1 provider_read\n"))
	}
	if strings.Contains(out.String(), "SENTINEL_PRIVATE") || !strings.Contains(out.String(), "helper_entry") || strings.Count(out.String(), "ci-intake/v2") != 64 {
		t.Fatal("unsafe/unbounded helper diagnostics")
	}
}

type failedIntakeLog struct{}

func (failedIntakeLog) Write(p []byte) (int, error) { return 0, fmt.Errorf("private logging failure") }
func TestIntakeLoggingFailurePreservesRejection(t *testing.T) {
	t.Setenv("CI_INTAKE_DIAGNOSTICS", "1")
	old := log.Writer()
	log.SetOutput(failedIntakeLog{})
	defer log.SetOutput(old)
	host, client := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer host.Close()
		runIntake(context.Background(), host, "/job/ci-job-diag", inputPolicy{Command: []string{"/missing"}})
	}()
	client.Write([]byte("{}\n"))
	client.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("diagnostics changed rejection")
	}
}
func TestWorkerDiagnosticExitAndMalformedFrames(t *testing.T) {
	t.Setenv("CI_INTAKE_DIAGNOSTICS", "1")
	var out bytes.Buffer
	old := log.Writer()
	log.SetOutput(&out)
	defer log.SetOutput(old)
	stream := logFrame("2026-10-06T17:39:42Z ci-intake/v1 worker_exit code=19\n2026-10-06T17:39:42Z ci-intake/v1 worker_exit code=19 SENTINEL_PRIVATE\n")
	if e := decodeIntakeLogs(bytes.NewReader(stream), "ci-job-diag"); e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(out.String(), "code=19") || strings.Contains(out.String(), "SENTINEL_PRIVATE") {
		t.Fatal("unsafe exit diagnostic")
	}
	for _, s := range [][]byte{[]byte("raw-secret"), logFrame(strings.Repeat("x", 8193)), bytes.Repeat([]byte("x"), 65537)} {
		if decodeIntakeLogs(bytes.NewReader(s), "ci-job-diag") == nil {
			t.Fatal("accepted oversized/malformed log")
		}
	}
}
