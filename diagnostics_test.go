package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"testing"
)

const closeRecord = `{"event":"egress_close","hostname":"github.com","peer_ip":"140.82.112.3","port":443,"phase":"relay","error_type":"TimeoutError","close_reason":"idle_timeout","close_direction":"both","bytes_client_to_server":500,"bytes_server_to_client":9000,"eof_directions":[],"duration_seconds":630.5,"last_io_seconds":330.5}`

func logFrame(payload string) []byte {
	b := make([]byte, 8)
	b[0] = 2
	binary.BigEndian.PutUint32(b[4:], uint32(len(payload)))
	return append(b, payload...)
}

func TestDiagnosticChunksAndRedaction(t *testing.T) {
	var record map[string]any
	json.Unmarshal([]byte(closeRecord), &record)
	record["authorization"] = "Bearer secret-should-never-escape"
	record["job"] = "forged-job"
	record["url"] = "https://github.com/private?token=secret"
	body, _ := json.Marshal(record)
	line := append([]byte("2026-10-06T17:39:42.000000000Z "), body...)
	stream := append(logFrame(string(line[:80])), logFrame(string(line[80:])+"\ntraceback raw-secret\n")...)
	var output bytes.Buffer
	n, err := decodeProxyLogs(bytes.NewReader(stream), "ci-job-real", func(b []byte) { output.Write(b) })
	if err != nil || n != 1 {
		t.Fatalf("decode: n=%d err=%v", n, err)
	}
	for _, forbidden := range []string{"secret", "forged", "authorization", "url", "traceback"} {
		if strings.Contains(output.String(), forbidden) {
			t.Fatalf("leaked %s", forbidden)
		}
	}
	if !strings.Contains(output.String(), `"job":"ci-job-real"`) || !strings.Contains(output.String(), `"last_io_seconds":330.5`) {
		t.Fatal("lost diagnostic association")
	}
}

func TestDiagnosticBoundsAndInvalidValues(t *testing.T) {
	for _, extra := range []string{`"hostname":"https://host/token"`, `"phase":"secret"`, `"error_type":"secret"`, `"peer_ip":"secret"`, `"bytes_server_to_client":-1`, `"duration_seconds":1e99`, `"last_io_seconds":9999`, `"eof_directions":["secret"]`} {
		line := strings.TrimSuffix(closeRecord, "}") + "," + extra + "}"
		if _, ok := sanitizeDiagnostic([]byte(line), "job", "2026-10-06T17:39:42Z"); ok {
			t.Fatalf("accepted invalid field %s", extra)
		}
	}
	for _, stream := range [][]byte{[]byte("raw credential text"), logFrame(closeRecord)[:20], logFrame(strings.Repeat("x", 8193))} {
		if _, err := decodeProxyLogs(bytes.NewReader(stream), "job", func([]byte) { t.Fatal("emitted invalid log") }); err == nil {
			t.Fatal("accepted malformed framing")
		}
	}
	stream := bytes.Repeat(logFrame("2026-10-06T17:39:42Z "+closeRecord+"\n"), 201)
	count, err := decodeProxyLogs(bytes.NewReader(stream), "job", func([]byte) {})
	if err != nil || count != 200 {
		t.Fatalf("tail cap count=%d err=%v", count, err)
	}
}

func TestCleanupPreservesDiagnosticsBeforeProxyDeletion(t *testing.T) {
	var order []string
	var output bytes.Buffer
	old := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(old) })
	withDockerHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		order = append(order, r.Method+" "+r.URL.Path)
		if strings.HasSuffix(r.URL.Path, "/logs") {
			if r.URL.Query().Get("tail") != "200" || r.URL.Query().Get("stderr") != "true" {
				t.Error("unbounded log request")
			}
			w.Write(logFrame("2026-10-06T17:39:42Z " + closeRecord + "\n"))
			return
		}
		w.WriteHeader(204)
	})
	f := &fleet{jobs: map[string]string{"job": "net"}}
	f.cleanup("job", "net")
	if len(f.jobs) != 0 {
		t.Fatal("cleanup did not finish")
	}
	joined := strings.Join(order, "\n")
	if strings.Index(joined, "GET /v1.47/containers/job-proxy/logs") > strings.Index(joined, "DELETE /v1.47/containers/job-proxy") {
		t.Fatal("diagnostics read after destruction")
	}
	if !strings.Contains(output.String(), "retained_egress_close") {
		t.Fatal("proxy evidence not retained in controller log")
	}
}

func TestDiagnosticFailureDoesNotBlockCleanupOrRepeatOnRetry(t *testing.T) {
	reads, deletes := 0, 0
	withDockerHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/logs") {
			reads++
			w.WriteHeader(500)
			io.WriteString(w, "secret-response")
			return
		}
		if r.Method == "DELETE" && strings.HasSuffix(r.URL.Path, "job-proxy") {
			deletes++
			if deletes == 1 {
				w.WriteHeader(500)
				return
			}
		}
		w.WriteHeader(204)
	})
	f := &fleet{jobs: map[string]string{"job": "net"}}
	f.cleanup("job", "net")
	if len(f.jobs) != 1 {
		t.Fatal("failed delete forgot job")
	}
	f.cleanup("job", "net")
	if len(f.jobs) != 0 || reads != 1 {
		t.Fatalf("cleanup blocked or duplicated diagnostics: reads=%d", reads)
	}
}
