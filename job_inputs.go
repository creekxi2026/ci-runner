package main

// Generic, opt-in project input adapter. The controller remains the ONLY
// creator and reclaimer of runner, registration, disk, proxy and services.
import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/actions/scaleset"
)

type inputMount struct {
	Name   string `json:"name"`
	Target string `json:"target"`
}
type inputPolicy struct {
	Schema        string              `json:"schema"`
	Command       []string            `json:"command"`
	Digest        string              `json:"sha256"`
	Mounts        []inputMount        `json:"mounts"`
	PrivateMounts []privateInputMount `json:"private_mounts,omitempty"`
}
type jobInputs struct {
	policy        inputPolicy
	root          *os.Root
	base          string
	uid           uint32
	intakes       intakeState
	syncDirectory func(string) error
}

func trustedInputFile(path string) ([]byte, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("absolute input path required")
	}
	for p := filepath.Dir(path); ; p = filepath.Dir(p) {
		st, e := os.Lstat(p)
		if e != nil || !st.IsDir() || !rootOwned(st) || st.Mode().Perm()&0022 != 0 {
			return nil, fmt.Errorf("unsafe input ancestor")
		}
		if p == "/" {
			break
		}
	}
	f, e := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil || !st.Mode().IsRegular() || !rootOwned(st) || st.Mode().Perm()&0022 != 0 || st.Size() > 2<<20 {
		return nil, fmt.Errorf("unsafe input file")
	}
	return io.ReadAll(io.LimitReader(f, 2<<20))
}
func (f *fleet) openJobInputs(path string) error {
	if path == "" {
		return nil
	}
	data, e := trustedInputFile(path)
	if e != nil {
		return e
	}
	var p inputPolicy
	if strictServiceDecode(data, &p) != nil || (p.Schema != "ci-job-inputs/v1" && p.Schema != "ci-job-inputs/v2") || len(p.Command) == 0 || len(p.Mounts) == 0 || len(p.Mounts) > 8 || f.work == nil || f.cleanupLog == nil {
		return fmt.Errorf("invalid job input policy")
	}
	executable, e := trustedInputFile(p.Command[0])
	if e != nil {
		return e
	}
	digest := sha256.Sum256(executable)
	if hex.EncodeToString(digest[:]) != p.Digest {
		return fmt.Errorf("job input adapter digest differs")
	}
	seen := map[string]bool{"/run/ci-input-gate": true, "/run/ci-intake": true, "intake": true}
	for _, m := range p.Mounts {
		if !serviceID.MatchString(m.Name) || m.Name == "gate" || !(strings.HasPrefix(m.Target, "/run/") || p.Schema == "ci-job-inputs/v2" && intakeTarget(m.Target)) || filepath.Clean(m.Target) != m.Target || seen[m.Target] || seen[m.Name] {
			return fmt.Errorf("invalid input mount")
		}
		seen[m.Target] = true
		seen[m.Name] = true
	}
	if err := f.validatePrivateInputs(p); err != nil {
		return err
	}
	base := "inputs-v1/" + f.owner
	for _, dir := range []string{"inputs-v1", base} {
		if e = f.work.root.Mkdir(dir, 0700); e != nil && !errors.Is(e, os.ErrExist) {
			return e
		}
		st, e := f.work.root.Lstat(dir)
		if e != nil || !protectedCleanup(st, 0, 0700, true) {
			return fmt.Errorf("unsafe input root")
		}
	}
	root, e := f.work.root.OpenRoot(base)
	if e != nil {
		return e
	}
	f.inputs = &jobInputs{policy: p, root: root, base: base}
	return nil
}
func (b *jobInputs) write(name string, value any, immutable bool) error {
	data, e := json.Marshal(value)
	if e != nil {
		return e
	}
	if immutable {
		old, e := b.root.ReadFile(name)
		if e == nil {
			if !bytes.Equal(data, old) {
				return fmt.Errorf("sealed input changed")
			}
			return nil
		}
		if !errors.Is(e, os.ErrNotExist) {
			return e
		}
	}
	tmp := name + ".new"
	out, e := b.root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if errors.Is(e, os.ErrExist) {
		if e = b.root.Remove(tmp); e != nil {
			return e
		}
		out, e = b.root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	}
	if e != nil {
		return e
	}
	defer b.root.Remove(tmp)
	if _, e = out.Write(data); e == nil {
		e = out.Chmod(0444)
	}
	if e == nil {
		e = out.Sync()
	}
	ce := out.Close()
	if e != nil {
		return e
	}
	if ce != nil {
		return ce
	}
	if e = b.root.Rename(tmp, name); e != nil {
		return e
	}
	if b.syncDirectory != nil {
		e = b.syncDirectory(filepath.Dir(name))
	} else {
		e = syncCleanupDirectory(b.root, filepath.Dir(name))
	}
	if e != nil {
		return e
	}
	if filepath.Base(name) == "assignment.json" && filepath.Dir(filepath.Dir(name)) == "." {
		intakeEvent(filepath.Dir(name), "controller", "atomic_published")
	}
	return nil
}
func (b *jobInputs) invoke(ctx context.Context, operation, name string) int {
	// Children receive no stdout channel for tokens/spec/JIT; the exit code is
	// the complete response. Project data only travel through sealed private files.
	executable, err := trustedInputFile(b.policy.Command[0])
	if err != nil {
		return 75
	}
	digest := sha256.Sum256(executable)
	if hex.EncodeToString(digest[:]) != b.policy.Digest {
		return 75
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, b.policy.Command[0], b.policy.Command[1:]...)
	cmd.Env = intakeEnvironment()
	body, _ := json.Marshal(obj{"schema": "ci-job-input-call/v1", "operation": operation, "directory": filepath.Join(b.root.Name(), name)})
	cmd.Stdin = bytes.NewReader(body)
	cmd.Stdout = io.Discard
	cmd.Stderr = &intakeStderr{job: name}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	if e := cmd.Run(); e != nil {
		var status *exec.ExitError
		if errors.As(e, &status) && status.ExitCode() == 2 {
			return 2
		}
		return 75
	}
	return 0
}
func (f *fleet) prepareJobInputs(ctx context.Context, name string, spec obj) error {
	b := f.inputs
	if b == nil {
		return nil
	}
	intakeEvent(name, "controller", "prepare_started")
	entries, e := b.entries()
	if e != nil {
		return e
	}
	if len(entries) > 1000 {
		return fmt.Errorf("job input retention required for new admission")
	}
	if e = b.root.Mkdir(name, 0700); e != nil && !errors.Is(e, os.ErrExist) {
		return e
	}
	dirs := append([]inputMount{{Name: "gate", Target: "/run/ci-input-gate"}}, b.policy.Mounts...)
	if b.policy.Schema == "ci-job-inputs/v2" {
		dirs = append(dirs, inputMount{Name: "intake", Target: "/run/ci-intake"})
	}
	host := spec["HostConfig"].(obj)
	mounts, _ := host["Mounts"].([]obj)
	for _, m := range dirs {
		for _, old := range mounts {
			if old["Target"] == m.Target {
				return fmt.Errorf("duplicate job input mount")
			}
		}
		if e = b.root.Mkdir(name+"/"+m.Name, 0755); e != nil && !errors.Is(e, os.ErrExist) {
			return e
		}
		st, e := b.root.Lstat(name + "/" + m.Name)
		if e != nil || !protectedCleanup(st, b.uid, 0755, true) {
			return fmt.Errorf("unsafe job input directory")
		}
		mounts = append(mounts, obj{"Type": "volume", "Source": f.work.volume, "Target": m.Target, "ReadOnly": true, "VolumeOptions": obj{"Subpath": b.base + "/" + name + "/" + m.Name}})
	}
	host["Mounts"] = mounts
	if err := f.mountPrivateInputs(host, b.policy); err != nil {
		return err
	}
	spec["Env"] = append(spec["Env"].([]string), "CI_JOB_INPUTS=1")
	if b.policy.Schema == "ci-job-inputs/v2" {
		spec["Env"] = append(spec["Env"].([]string), "CI_JOB_INTAKE=1")
		spec["OpenStdin"], spec["AttachStdin"], spec["StdinOnce"] = true, true, true
		if intakeEnabled() {
			spec["Env"] = append(spec["Env"].([]string), "CI_INTAKE_DIAGNOSTICS=1")
		}
	}
	if e = b.write(name+"/spec.json", obj{"schema": "ci-job-input-spec/v1", "name": name, "owner": f.owner, "volume": f.work.volume, "spec": spec}, true); e != nil {
		return e
	}
	if b.invoke(ctx, "prepare", name) != 0 {
		return fmt.Errorf("project input preparation unavailable")
	}
	intakeEvent(name, "controller", "prepare_ready")
	return b.startIntake(ctx, name)
}

// Serialize the producer facts and assignment publication with allocation
// cleanup. An old spec or a nil-returning scheduler observer is not association.
func (f *fleet) observeInputAssignment(event *scaleset.JobStarted, fallback bool) error {
	if f.inputs == nil || event == nil {
		return nil
	}
	j := f.state(event.RunnerName)
	if j == nil {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.cleaning {
		return nil
	}
	b := f.inputs
	value, err := inputAssignment(event)
	if err != nil {
		return err
	}
	if _, err = b.root.Stat(event.RunnerName + "/spec.json"); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	data, _ := json.Marshal(value)
	old, err := b.root.ReadFile(event.RunnerName + "/assignment.json")
	missing := errors.Is(err, os.ErrNotExist)
	if err != nil && !missing {
		return err
	}
	if !missing && !bytes.Equal(old, data) {
		return fmt.Errorf("sealed input changed")
	}
	if fallback {
		if !missing {
			return nil
		}
		intakeEvent(event.RunnerName, "controller", "completed_fallback")
	} else {
		intakeEvent(event.RunnerName, "controller", "started_observed")
	}
	return b.write(event.RunnerName+"/assignment.json", value, true)
}
func inputAssignment(event *scaleset.JobStarted) (obj, error) {
	if !strings.HasPrefix(event.RunnerName, "ci-job-") || filepath.Base(event.RunnerName) != event.RunnerName || !serviceID.MatchString(event.RunnerName) {
		return nil, fmt.Errorf("invalid input runner")
	}
	if event.RunnerID <= 0 || event.WorkflowRunID <= 0 || !cacheRepo.MatchString(event.OwnerName+"/"+event.RepositoryName) {
		return nil, fmt.Errorf("invalid input assignment")
	}
	return obj{"schema": "ci-job-assignment/v1", "runner_name": event.RunnerName, "runner_id": event.RunnerID, "repository": event.OwnerName + "/" + event.RepositoryName, "run_id": event.WorkflowRunID, "scheduler_job_id": event.JobID, "event": event.EventName}, nil
}
func (f *fleet) assignJobInputs(event *scaleset.JobStarted) error {
	if f.inputs == nil || event == nil {
		return nil
	}
	b := f.inputs
	if !strings.HasPrefix(event.RunnerName, "ci-job-") || filepath.Base(event.RunnerName) != event.RunnerName {
		return fmt.Errorf("invalid input runner")
	}
	if _, e := b.root.Stat(event.RunnerName + "/spec.json"); errors.Is(e, os.ErrNotExist) {
		return nil
	} else if e != nil {
		return e
	}
	value, e := inputAssignment(event)
	if e != nil {
		return e
	}
	return b.write(event.RunnerName+"/assignment.json", value, true)
}
func (b *jobInputs) reconcile(ctx context.Context, active ...map[string]string) {
	if b.policy.Schema == "ci-job-inputs/v2" {
		return
	}
	// One 60s operation per turn; cursor commits BEFORE the attempt, so crashes,
	// waiting and poisoned scopes cannot monopolize all following scopes.
	entries, e := b.entries()
	if e != nil {
		return
	}
	names := []string{}
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), "ci-job-") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	var cursor string
	if data, e := b.root.ReadFile("cursor.json"); e == nil {
		_ = json.Unmarshal(data, &cursor)
	}
	var priority bool
	if data, e := b.root.ReadFile("priority.json"); e == nil {
		_ = json.Unmarshal(data, &priority)
	}
	// Alternate pre-step admissions with receipt recovery; each lane rotates.
	laneCursor := "cursor.json"
	if priority {
		laneCursor = "admission-cursor.json"
		cursor = ""
		if data, e := b.root.ReadFile(laneCursor); e == nil {
			_ = json.Unmarshal(data, &cursor)
		}
	}
	_ = b.write("priority.json", !priority, false)
	for _, wrap := range []bool{false, true} {
		for _, n := range names {
			if (n <= cursor) != wrap {
				continue
			}
			if priority {
				if len(active) > 0 {
					// All live runners get priority, including a required gate
					// already waiting for PG; old receipt backlog must not
					// consume its finite native wait window.
					if _, ok := active[0][n]; !ok {
						continue
					}
				} else {
					if _, e := b.root.Stat(n + "/gate/ready.json"); e == nil {
						continue
					}
					st, e := b.root.Stat(n + "/assignment.json")
					if e != nil || time.Since(st.ModTime()) > 15*time.Minute {
						continue
					}
				}
			}
			if _, e := b.root.Stat(n + "/settled.json"); e == nil {
				continue
			}
			if _, e := b.root.Stat(n + "/assignment.json"); e != nil {
				continue
			}
			if b.write(laneCursor, n, false) != nil {
				return
			}
			code := b.invoke(ctx, "reconcile", n)
			if code == 0 || code == 2 {
				_ = b.write(n+"/settled.json", obj{"exit": code}, true)
			}
			return
		}
	}
}

func (b *jobInputs) entries() ([]os.DirEntry, error) {
	d, e := b.root.Open(".")
	if e != nil {
		return nil, e
	}
	defer d.Close()
	entries, e := d.ReadDir(10001)
	if e != nil && !errors.Is(e, io.EOF) {
		return nil, e
	}
	if len(entries) > 10000 {
		return nil, fmt.Errorf("input inventory budget exhausted")
	}
	return entries, nil
}
