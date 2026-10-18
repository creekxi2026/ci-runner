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

	"github.com/google/uuid"
)

// Source/stage pairs are disjoint. Legacy producer lines carry no authority.
const controllerIntakeStages = " session_bound session_rejected started_observed completed_fallback atomic_published prepare_started prepare_ready socket_listening socket_accepted assignment_wait assignment_unavailable assignment_ready helper_trusted packet_read packet_parsed packet_rejected helper_checked helper_started helper_start_failed helper_ready session_closed helper_exit exit "
const helperIntakeStages = " helper_entry installation_ready call_parsed call_rejected scope_checked token_fields_present token_fields_absent token_present identity_check identity_ready provider_read provider_ok provider_rejected http_2xx http_4xx http_5xx http_unknown gate_published ready_sent helper_rejected provider_decode_ready provider_decode_rejected run_id_diff run_attempt_diff run_status_diff run_conclusion_diff run_workflow_diff run_repository_diff run_repository_id_diff run_context_repository_id_diff run_event_diff run_actor_diff run_shape_rejected run_identity_ready "
const workerIntakeStages = " bootstrap_started bootstrap_received bootstrap_child_started bootstrap_issued bootstrap_closed cli_checked cli_rejected fd_checked frame_read frame_parsed socket_connect socket_connected host_wait host_ready host_rejected worker_start worker_started worker_exit bridge_rejected session_closed runner_entry interposer_installed exit "

// A segment identifies this controller process only, never a credential or a
// persistent allocation identity. Sequence and elapsed share the output lock.
var intakeRecords = struct {
	sync.Mutex
	segment  string
	started  time.Time
	sequence uint64
}{segment: uuid.NewString(), started: time.Now()}

func intakeEnabled() bool { return os.Getenv("CI_INTAKE_DIAGNOSTICS") == "1" }
func safeSourceIntakeStage(source, stage string) bool {
	if stage == "" || strings.ContainsAny(stage, " \r\n\t") {
		return false
	}
	stages := ""
	switch source {
	case "controller":
		stages = controllerIntakeStages
	case "helper":
		stages = helperIntakeStages
	case "worker":
		stages = workerIntakeStages
	default:
		return false
	}
	return strings.Contains(stages, " "+stage+" ")
}
func safeIntakeStage(stage string) bool {
	return safeSourceIntakeStage("controller", stage) || safeSourceIntakeStage("helper", stage) || safeSourceIntakeStage("worker", stage)
}
func intakeRecord(job, source, stage string, code *int) {
	if !intakeEnabled() || !serviceID.MatchString(job) || !safeSourceIntakeStage(source, stage) {
		return
	}
	if code != nil && (stage != "exit" || *code < -1 || *code > 255) {
		return
	}
	evidence := "forwarded"
	if source == "controller" {
		evidence = "observed"
	}
	intakeRecords.Lock()
	defer intakeRecords.Unlock()
	intakeRecords.sequence++
	suffix := ""
	if code != nil {
		suffix = " code=" + strconv.Itoa(*code)
	}
	log.Printf("ci-intake/v2 job=%s source=%s stage=%s segment=%s seq=%d elapsed_ns=%d clock=controller evidence=%s%s", job, source, stage, intakeRecords.segment, intakeRecords.sequence, time.Since(intakeRecords.started).Nanoseconds(), evidence, suffix)
}
func intakeEvent(job, source, stage string) { intakeRecord(job, source, stage, nil) }

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
				if safeSourceIntakeStage("helper", stage) {
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
	if !oneOf(source, "controller", "worker") {
		return
	}
	intakeRecord(job, source, "exit", &code)
}
func safeIntakeLine(line string) (string, int, bool) {
	if line != "exit" && safeSourceIntakeStage("worker", line) {
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
