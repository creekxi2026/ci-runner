package main

// The runner image intercepts Listener's inherited ProcessChannel BEFORE the
// Worker starts. A single-use socket is exposed only in this job's RO subpath.
// The controller never stores, logs, or grants operations from the raw message.
import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

const intakeLimit = 8 * 1024 * 1024

// Match the proxy's initial admission budget. The proxy starts its clock first
// and disconnects on expiry, so this can never extend its authority or lifetime.
const intakeAssignmentWait = 600 * time.Second

func waitIntakeAssignment(ctx context.Context, directory string) bool {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return false
	}
	defer root.Close()
	ctx, cancel := context.WithTimeout(ctx, intakeAssignmentWait)
	defer cancel()
	for ctx.Err() == nil {
		if _, err := root.Stat("assignment.json"); err == nil {
			return true
		}
		if !pause(ctx, 100*time.Millisecond) {
			return false
		}
	}
	return false
}

type intakeState struct {
	mu       sync.Mutex
	jobs     map[string]context.CancelFunc
	material map[string]*intakeMaterial
	wg       sync.WaitGroup
}

// Allocation messages are opaque upstream JSON, including Unicode job names
// and contexts. Service configuration's ASCII/canonical-value restrictions do
// not apply. Retain duplicate-key, envelope and bounded-depth validation before
// invoking the adapter, and keep the original message bytes for forwarding.
func validIntakePacket(data []byte) bool {
	if !utf8.Valid(data) {
		return false
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var value func(int) bool
	value = func(depth int) bool {
		if depth > 128 {
			return false
		}
		token, err := d.Token()
		if err != nil {
			return false
		}
		if delimiter, ok := token.(json.Delim); ok {
			switch delimiter {
			case '{':
				seen := map[string]bool{}
				for d.More() {
					key, err := d.Token()
					name, ok := key.(string)
					if err != nil || !ok || seen[name] || (depth == 0 && name != "schema" && name != "message") {
						return false
					}
					seen[name] = true
					if !value(depth + 1) {
						return false
					}
				}
				_, err = d.Token()
				return err == nil
			case '[':
				if depth == 0 {
					return false
				}
				for d.More() {
					if !value(depth + 1) {
						return false
					}
				}
				_, err = d.Token()
				return err == nil
			}
			return false
		}
		return depth != 0
	}
	if !value(0) {
		return false
	}
	_, err := d.Token()
	return err == io.EOF
}

func runIntake(ctx context.Context, conn net.Conn, directory string, policy inputPolicy) {
	job := intakeJob(directory)
	intakeEvent(job, "controller", "packet_read")
	defer intakeEvent(job, "controller", "session_closed")
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	closed := make(chan struct{})
	defer close(closed)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-closed:
		}
	}()
	if len(policy.Command) == 0 {
		return
	}
	conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	reader := bufio.NewReaderSize(conn, 4096)
	var data []byte
	var e error
	for len(data) <= intakeLimit {
		part, err := reader.ReadSlice('\n')
		data = append(data, part...)
		e = err
		if err != bufio.ErrBufferFull {
			break
		}
	}
	if e != nil || len(data) > intakeLimit {
		intakeEvent(job, "controller", "packet_rejected")
		return
	}
	var packet struct {
		Schema  string          `json:"schema"`
		Message json.RawMessage `json:"message"`
	}
	if !validIntakePacket(data) || json.Unmarshal(data, &packet) != nil || packet.Schema != "ci-job-intake/v1" {
		intakeEvent(job, "controller", "packet_rejected")
		return
	}
	message := bytes.TrimSpace(packet.Message)
	if len(message) == 0 || message[0] != '{' {
		intakeEvent(job, "controller", "packet_rejected")
		return
	}
	intakeEvent(job, "controller", "packet_parsed")
	conn.SetReadDeadline(time.Time{})
	// No reconnect or second request. Closing the client or sending even one byte
	// cancels the process group, including while initial provider reads block.
	go func() { var b [1]byte; reader.Read(b[:]); cancel() }()
	// Consume/validate the packet before waiting for the independently delivered
	// scheduler observation. EOF, cancellation or another byte now revokes the
	// wait as well as the helper; an empty readiness probe cannot reach a helper.
	intakeEvent(job, "controller", "assignment_wait")
	if !waitIntakeAssignment(ctx, directory) {
		intakeEvent(job, "controller", "assignment_unavailable")
		return
	}
	intakeEvent(job, "controller", "assignment_ready")
	source, e := os.ReadFile(policy.Command[0])
	if e != nil {
		return
	}
	h := sha256.Sum256(source)
	if hex.EncodeToString(h[:]) != policy.Digest {
		return
	}
	cmd := exec.CommandContext(ctx, policy.Command[0], policy.Command[1:]...)
	intakeEvent(job, "controller", "helper_checked")
	cmd.Env = intakeEnvironment()
	call, _ := json.Marshal(obj{"schema": "ci-job-input-session/v1", "directory": directory, "message": packet.Message})
	stdin, e := cmd.StdinPipe()
	if e != nil {
		return
	}
	stdout, e := cmd.StdoutPipe()
	if e != nil {
		return
	}
	cmd.Stderr = &intakeStderr{job: job}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	if cmd.Start() != nil {
		intakeEvent(job, "controller", "helper_start_failed")
		return
	}
	intakeEvent(job, "controller", "helper_started")
	go func() { defer stdin.Close(); stdin.Write(append(call, '\n')) }()
	var status [1]byte
	if _, e = io.ReadFull(stdout, status[:]); e == nil && status[0] == '0' && ctx.Err() == nil {
		intakeEvent(job, "controller", "helper_ready")
		// This single byte is the ONLY response exposed to the unprivileged proxy.
		conn.SetWriteDeadline(time.Now().Add(time.Second))
		if _, e = conn.Write(status[:]); e == nil {
			io.Copy(io.Discard, stdout)
		}
	}
	cancel()
	cmd.Wait()
	if cmd.ProcessState != nil {
		intakeExit(job, "controller", cmd.ProcessState.ExitCode())
	}
	intakeEvent(job, "controller", "helper_exit")
}

func (b *jobInputs) startIntake(_ context.Context, name string) error {
	if b.policy.Schema != "ci-job-inputs/v2" {
		return nil
	}
	b.intakes.mu.Lock()
	defer b.intakes.mu.Unlock()
	if b.intakes.jobs == nil {
		b.intakes.jobs = map[string]context.CancelFunc{}
	}
	if b.intakes.jobs[name] != nil {
		return nil
	}
	dir := filepath.Join(b.root.Name(), name, "intake")
	// This directory is controller-owned. Never adopt or replace an old socket:
	// controller recovery cannot revive a token or a consumed job connection.
	address := filepath.Join(dir, "session.sock")
	// sockaddr_un has a small fixed pathname limit. Bind through the retained
	// directory FD; the worker reaches the same inode through its short mount.
	listenAddress := address
	socketDir, e := b.root.Open(name + "/intake")
	if e != nil {
		return e
	}
	defer socketDir.Close()
	if runtime.GOOS == "linux" {
		listenAddress = fmt.Sprintf("/proc/self/fd/%d/session.sock", socketDir.Fd())
	}
	listener, e := net.ListenUnix("unix", &net.UnixAddr{Name: listenAddress, Net: "unix"})
	if e != nil {
		return e
	}
	listener.SetUnlinkOnClose(false)
	if e = os.Chmod(address, 0666); e != nil {
		listener.Close()
		os.Remove(address)
		return e
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	b.intakes.jobs[name] = cancel
	material, e := newIntakeMaterial()
	if e != nil {
		cancel()
		listener.Close()
		os.Remove(address)
		return e
	}
	if b.intakes.material == nil {
		b.intakes.material = map[string]*intakeMaterial{}
	}
	b.intakes.material[name] = material
	expiry := time.AfterFunc(time.Until(material.expires), func() {
		b.intakes.mu.Lock()
		defer b.intakes.mu.Unlock()
		if m := b.intakes.material[name]; m != nil {
			m.clear()
			delete(b.intakes.material, name)
			cancel()
		}
	})
	intakeEvent(name, "controller", "socket_listening")
	b.intakes.wg.Add(1)
	go func() {
		defer b.intakes.wg.Done()
		defer expiry.Stop()
		defer b.eraseBootstrap(name)
		defer cancel()
		defer listener.Close()
		defer os.Remove(address)
		done := make(chan struct{})
		defer close(done)
		go func() {
			select {
			case <-ctx.Done():
				listener.Close()
			case <-done:
			}
		}()
		conn, e := listener.AcceptUnix()
		if e != nil {
			return
		}
		defer conn.Close()
		intakeEvent(name, "controller", "socket_accepted")
		listener.Close()
		os.Remove(address) // one successful accept, for this job only
		if !b.bindIntake(ctx, name, conn) {
			intakeEvent(name, "controller", "session_rejected")
			return
		}
		intakeEvent(name, "controller", "session_bound")
		if _, e = trustedInputFile(b.policy.Command[0]); e != nil {
			return
		}
		intakeEvent(name, "controller", "helper_trusted")
		runIntake(ctx, conn, filepath.Join(b.root.Name(), name), b.policy)
	}()
	return nil
}

func (b *jobInputs) cancelIntake(name string) {
	if b == nil {
		return
	}
	b.intakes.mu.Lock()
	defer b.intakes.mu.Unlock()
	if m := b.intakes.material[name]; m != nil {
		m.clear()
		delete(b.intakes.material, name)
	}
	if cancel := b.intakes.jobs[name]; cancel != nil {
		cancel()
		delete(b.intakes.jobs, name)
	}
}

func (b *jobInputs) closeIntakes() {
	b.intakes.mu.Lock()
	for name, cancel := range b.intakes.jobs {
		if m := b.intakes.material[name]; m != nil {
			m.clear()
			delete(b.intakes.material, name)
		}
		cancel()
		delete(b.intakes.jobs, name)
	}
	b.intakes.mu.Unlock()
	b.intakes.wg.Wait()
}

func intakeTarget(target string) bool {
	if filepath.Clean(target) != target || strings.ContainsAny(target, "\x00,:\n") {
		return false
	}
	if strings.HasPrefix(target, "/run/") {
		return true
	}
	if !strings.HasPrefix(target, "/opt/") || len(strings.Split(target, "/")) < 4 {
		return false
	}
	for _, protected := range []string{"/opt/ci", "/opt/actions-runner", "/opt/go", "/opt/ci-tools"} {
		if target == protected || strings.HasPrefix(target, protected+"/") {
			return false
		}
	}
	return true
}
