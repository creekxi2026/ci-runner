package main

// The runner image intercepts Listener's inherited ProcessChannel BEFORE the
// Worker starts. A single-use socket is exposed only in this job's RO subpath.
// The controller never stores, logs, or grants operations from the raw message.
import (
	"bufio"
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
)

const intakeLimit = 8 * 1024 * 1024

type intakeState struct {
	mu   sync.Mutex
	jobs map[string]context.CancelFunc
	wg   sync.WaitGroup
}

func runIntake(ctx context.Context, conn net.Conn, directory string, policy inputPolicy) {
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
		return
	}
	var packet struct {
		Schema  string          `json:"schema"`
		Message json.RawMessage `json:"message"`
	}
	if strictServiceDecode(data, &packet) != nil || packet.Schema != "ci-job-intake/v1" || len(packet.Message) == 0 {
		return
	}
	conn.SetReadDeadline(time.Time{})
	// No reconnect or second request. Closing the client or sending even one byte
	// cancels the process group, including while initial provider reads block.
	go func() { var b [1]byte; reader.Read(b[:]); cancel() }()
	source, e := os.ReadFile(policy.Command[0])
	if e != nil {
		return
	}
	h := sha256.Sum256(source)
	if hex.EncodeToString(h[:]) != policy.Digest {
		return
	}
	cmd := exec.CommandContext(ctx, policy.Command[0], policy.Command[1:]...)
	cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "PYTHONDONTWRITEBYTECODE=1"}
	call, _ := json.Marshal(obj{"schema": "ci-job-input-session/v1", "directory": directory, "message": packet.Message})
	stdin, e := cmd.StdinPipe()
	if e != nil {
		return
	}
	stdout, e := cmd.StdoutPipe()
	if e != nil {
		return
	}
	cmd.Stderr = io.Discard
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	if cmd.Start() != nil {
		return
	}
	go func() { defer stdin.Close(); stdin.Write(append(call, '\n')) }()
	var status [1]byte
	if _, e = io.ReadFull(stdout, status[:]); e == nil && status[0] == '0' && ctx.Err() == nil {
		// This single byte is the ONLY response exposed to the unprivileged proxy.
		conn.SetWriteDeadline(time.Now().Add(time.Second))
		if _, e = conn.Write(status[:]); e == nil {
			io.Copy(io.Discard, stdout)
		}
	}
	cancel()
	cmd.Wait()
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
	b.intakes.wg.Add(1)
	go func() {
		defer b.intakes.wg.Done()
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
		listener.Close()
		os.Remove(address) // one successful accept, for this job only
		// assignment events and the pipe may race. Wait only inside the job lifetime.
		end := time.Now().Add(30 * time.Second)
		for {
			if _, e = b.root.Stat(name + "/assignment.json"); e == nil {
				break
			}
			if !time.Now().Before(end) || !pause(ctx, 100*time.Millisecond) {
				return
			}
		}
		if _, e = trustedInputFile(b.policy.Command[0]); e != nil {
			return
		}
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
	if cancel := b.intakes.jobs[name]; cancel != nil {
		cancel()
		delete(b.intakes.jobs, name)
	}
}

func (b *jobInputs) closeIntakes() {
	b.intakes.mu.Lock()
	for name, cancel := range b.intakes.jobs {
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
