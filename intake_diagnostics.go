package main

// Diagnostics are observations only: no protocol data, exception text or values.
import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const intakeStages = " prepare_started prepare_ready socket_listening socket_accepted assignment_ready helper_trusted packet_read packet_parsed packet_rejected helper_checked helper_started helper_start_failed helper_ready session_closed helper_exit cli_checked cli_rejected fd_checked frame_read frame_parsed socket_connect socket_connected host_wait host_ready host_rejected worker_start worker_started worker_exit bridge_rejected runner_entry interposer_installed helper_entry installation_ready call_parsed call_rejected scope_checked token_fields_present token_fields_absent token_present identity_check identity_ready provider_read provider_ok provider_rejected http_2xx http_4xx http_5xx http_unknown gate_published ready_sent helper_rejected "

func intakeEnabled() bool { return os.Getenv("CI_INTAKE_DIAGNOSTICS") == "1" }
func safeIntakeStage(s string) bool {
	return s != "" && !strings.ContainsAny(s, " \r\n\t") && strings.Contains(intakeStages, " "+s+" ")
}
func intakeEvent(job, source, stage string) {
	if !intakeEnabled() || !safeIntakeStage(stage) || !serviceID.MatchString(job) || !oneOf(source, "controller", "helper", "worker") {
		return
	}
	// The existing controller log survives job deletion; it remains bounded by the
	// operator's current Docker log retention, not a new store or collection service.
	log.Printf("ci-intake/v1 job=%s source=%s stage=%s", job, source, stage)
}

// A bounded streaming stderr filter. Never buffer arbitrary helper stderr or
// forward it. Validate the WHOLE line; prefixes with appended values are invalid.
type intakeStderr struct {
	mu       sync.Mutex
	job      string
	line     []byte
	overflow bool
	count    int
}

func (w *intakeStderr) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, b := range p {
		if b == '\n' {
			if !w.overflow && w.count < 64 && bytes.HasPrefix(w.line, []byte("ci-intake/v1 ")) {
				stage := string(w.line[len("ci-intake/v1 "):])
				if safeIntakeStage(stage) {
					intakeEvent(w.job, "helper", stage)
					w.count++
				}
			}
			w.line = w.line[:0]
			w.overflow = false
		} else if len(w.line) < 96 && !w.overflow {
			w.line = append(w.line, b)
		} else {
			w.overflow = true
		}
	}
	return len(p), nil
}
func intakeEnvironment() []string {
	env := []string{"PATH=/usr/local/bin:/usr/bin:/bin", "PYTHONDONTWRITEBYTECODE=1"}
	if intakeEnabled() {
		env = append(env, "CI_INTAKE_DIAGNOSTICS=1")
	}
	return env
}
func intakeExit(job, source string, code int) {
	if !intakeEnabled() || !serviceID.MatchString(job) || !oneOf(source, "controller", "worker") || code < -1 || code > 255 {
		return
	}
	log.Printf("ci-intake/v1 job=%s source=%s stage=exit code=%d", job, source, code)
}
func safeIntakeLine(line string) (string, int, bool) {
	if safeIntakeStage(line) {
		return line, 0, true
	}
	if value, ok := strings.CutPrefix(line, "worker_exit code="); ok {
		code, e := strconv.Atoi(value)
		if e == nil && code >= -1 && code <= 255 && strconv.Itoa(code) == value {
			return "exit", code, true
		}
	}
	return "", 0, false
}

func intakeJob(directory string) string { return filepath.Base(directory) }

func decodeIntakeLogs(r io.Reader, job string) error {
	// Docker framing may split records. Total bytes and per-frame allocation are
	// bounded independently. Only complete allowlisted lines reach the safe log.
	data, err := io.ReadAll(io.LimitReader(r, 64*1024+1))
	if err != nil || len(data) > 64*1024 {
		return fmt.Errorf("bounded intake logs unavailable")
	}
	var payload bytes.Buffer
	for len(data) > 0 {
		if len(data) < 8 || data[0] != 2 || data[1] != 0 || data[2] != 0 || data[3] != 0 {
			return fmt.Errorf("invalid framing")
		}
		n := int(binary.BigEndian.Uint32(data[4:8]))
		data = data[8:]
		if n > 8192 || n > len(data) {
			return fmt.Errorf("invalid framing")
		}
		payload.Write(data[:n])
		data = data[n:]
	}
	count := 0
	for _, line := range strings.Split(payload.String(), "\n") {
		parts := strings.SplitN(line, " ", 2)
		if len(parts) != 2 {
			continue
		}
		if _, e := time.Parse(time.RFC3339Nano, parts[0]); e != nil {
			continue
		}
		stage, ok := strings.CutPrefix(parts[1], "ci-intake/v1 ")
		if kind, code, valid := safeIntakeLine(stage); ok && valid {
			if kind == "exit" {
				intakeExit(job, "worker", code)
			} else {
				intakeEvent(job, "worker", kind)
			}
			count++
			if count == 64 {
				break
			}
		}
	}
	return nil
}
func captureIntakeDiagnostics(job string) {
	if !intakeEnabled() || !serviceID.MatchString(job) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	req, e := http.NewRequestWithContext(ctx, "GET", "http://docker/v1.47/containers/"+url.PathEscape(job)+"/logs?stdout=false&stderr=true&tail=128&timestamps=true", nil)
	if e != nil {
		return
	}
	resp, e := engine.Do(req)
	if e != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		_ = decodeIntakeLogs(resp.Body, job)
	}
}
