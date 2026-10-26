package main

// This is infrastructure evidence, NOT a service-specific terminal receipt.
// In particular, removing a service cannot attest revocation of its credentials.
import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/actions/scaleset"
)

const cleanupSchema = "ci-cleanup-observation/v1"

type cleanupNative struct {
	Repository     string `json:"repository"`
	RunID          int64  `json:"run_id"`
	SchedulerJobID string `json:"scheduler_job_id"`
	RunnerID       int    `json:"runner_id"`
	Result         string `json:"scheduler_result"`
}
type cleanupRunner struct {
	Observed   bool   `json:"observed"`
	Running    bool   `json:"running"`
	OOMKilled  bool   `json:"oom_killed"`
	ExitCode   int    `json:"exit_code"`
	FinishedAt string `json:"finished_at"`
}
type cleanupObservation struct {
	Admissions map[string]string               `json:"admission_digests,omitempty"`
	Proofs     map[string]serviceTerminalProof `json:"service_proofs,omitempty"`
	Schema     string                          `json:"schema"`
	Owner      string                          `json:"owner_id"`
	Job        string                          `json:"runner_name"`
	Native     *cleanupNative                  `json:"native"`
	Services   *serviceIndex                   `json:"service_index"`
	Runner     cleanupRunner                   `json:"runner_before_cleanup"`
	StartedAt  string                          `json:"cleanup_started_at"`
	ReleasedAt string                          `json:"allocation_released_at"`
}
type cleanupJournal struct {
	root    *os.Root
	owner   string
	uid     uint32
	mu      sync.Mutex
	syncDir func() error
}

func syncCleanupDirectory(root *os.Root, path string) error {
	d, e := root.Open(path)
	if e != nil {
		return e
	}
	defer d.Close()
	return d.Sync()
}

func protectedCleanup(st os.FileInfo, uid uint32, mode os.FileMode, dir bool) bool {
	s, ok := st.Sys().(*syscall.Stat_t)
	return ok && s.Uid == uid && st.Mode().Perm() == mode &&
		((dir && st.IsDir()) || (!dir && st.Mode().IsRegular() && s.Nlink == 1))
}

// Production always requires UID 0. Tests use their isolated directory's UID.
func openCleanupJournal(work *sharedWork, uid uint32) (*cleanupJournal, error) {
	if work == nil || !serviceID.MatchString(work.owner) {
		return nil, fmt.Errorf("cleanup journal requires safe shared-work owner")
	}
	for _, p := range []string{"cleanup-v1", "cleanup-v1/" + work.owner} {
		if e := work.root.Mkdir(p, 0700); e != nil && !errors.Is(e, os.ErrExist) {
			return nil, e
		}
		st, e := work.root.Lstat(p)
		if e != nil || !protectedCleanup(st, uid, 0700, true) {
			return nil, fmt.Errorf("unsafe cleanup journal directory")
		}
		// Retry this sync even if mkdir previously succeeded before a crash/error.
		if e = syncCleanupDirectory(work.root, filepath.Dir(p)); e != nil {
			return nil, e
		}
	}
	r, e := work.root.OpenRoot("cleanup-v1/" + work.owner)
	if e != nil {
		return nil, e
	}
	return &cleanupJournal{root: r, owner: work.owner, uid: uid, syncDir: func() error { return syncCleanupDirectory(r, ".") }}, nil
}

func (c *cleanupJournal) read(n string) (cleanupObservation, error) {
	v := cleanupObservation{}
	valid := validJobName.MatchString(n)
	if !valid {
		return v, fmt.Errorf("invalid cleanup identity")
	}
	f, e := c.root.OpenFile(n+".json", os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if e != nil {
		return v, e
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil || !protectedCleanup(st, c.uid, 0444, false) || st.Size() < 1 || st.Size() > 65536 {
		return v, fmt.Errorf("unsafe cleanup record")
	}
	return c.decodeRecord(n, f)
}

// The stream is the already opened, validated descriptor. Keeping the bounded
// decode separate permits deterministic growth/read-fault tests without racing
// real filesystem mutations or adding a business-path read.
func (c *cleanupJournal) decodeRecord(n string, r io.Reader) (cleanupObservation, error) {
	v := cleanupObservation{}
	b, e := io.ReadAll(io.LimitReader(r, 65537))
	if e != nil || len(b) > 65536 {
		return v, fmt.Errorf("invalid cleanup record size")
	}
	if e = strictServiceDecode(b, &v); e != nil {
		return v, fmt.Errorf("invalid cleanup record")
	}
	if v.Schema != cleanupSchema || v.Owner != c.owner || v.Job != n {
		return v, fmt.Errorf("foreign cleanup record")
	}
	return v, nil
}

// Only the controller writes this directory; the existing owner flock excludes
// other controllers. In-process writers serialize updates. fsync the file and
// directory before acknowledging a scheduler event or deleting its inputs.
func (c *cleanupJournal) update(n string, change func(*cleanupObservation) error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, e := c.read(n)
	if errors.Is(e, os.ErrNotExist) {
		v = cleanupObservation{Schema: cleanupSchema, Owner: c.owner, Job: n}
	} else if e != nil {
		return e
	}
	if !validJobName.MatchString(n) {
		return fmt.Errorf("invalid cleanup identity")
	}
	before, _ := json.Marshal(v)
	if e = change(&v); e != nil {
		return e
	}
	b, e := json.Marshal(v)
	if e != nil || len(b) > 65536 {
		return fmt.Errorf("cleanup record too large")
	}
	if bytes.Equal(before, b) {
		return c.syncDir()
	}
	tmp := n + ".new"
	// A crash may leave an uncommitted file. Unlink that name, never follow it.
	if e = c.root.Remove(tmp); e != nil && !errors.Is(e, os.ErrNotExist) {
		return e
	}
	f, e := c.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return e
	}
	defer c.root.Remove(tmp)
	_, e = f.Write(b)
	if e == nil {
		e = f.Chmod(0444)
	}
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e != nil {
		return e
	}
	if ce != nil {
		return ce
	}
	if e = c.root.Rename(tmp, n+".json"); e != nil {
		return e
	}
	return c.syncDir()
}

func (f *fleet) observeScheduler(n string, id int, event scaleset.JobMessageBase, result string) error {
	if f.cleanupLog == nil {
		return nil
	}
	// The SDK JobID is opaque, not the numeric GitHub Actions job ID. Do not
	// manufacture an attempt, source SHA or project producer identity here.
	state := f.state(n)
	if state == nil {
		if _, e := f.cleanupLog.read(n); errors.Is(e, os.ErrNotExist) {
			return nil
		} else if e != nil {
			return e
		}
	}
	if !cacheRepo.MatchString(event.OwnerName+"/"+event.RepositoryName) || event.WorkflowRunID <= 0 || id <= 0 || len(event.JobID) > 256 || !ascii(event.JobID) {
		return fmt.Errorf("invalid scheduler cleanup identity")
	}
	if !oneOf(result, "", "succeeded", "failed", "canceled", "skipped", "abandoned") {
		result = "unknown"
	}
	native := cleanupNative{event.OwnerName + "/" + event.RepositoryName, event.WorkflowRunID, event.JobID, id, result}
	return f.cleanupLog.update(n, func(v *cleanupObservation) error {
		if v.Native != nil {
			old := *v.Native
			old.Result = native.Result
			if old != native || (v.Native.Result != "" && result != "" && v.Native.Result != result) {
				return fmt.Errorf("scheduler cleanup identity changed")
			}
			if result == "" {
				return nil
			}
		}
		v.Native = &native
		return nil
	})
}

// Called before deleting any owned resources. Unknown inspect state is kept
// unknown; raw Docker errors, environment, labels and logs never enter the file.
func (f *fleet) beginCleanupObservation(n string) error {
	if f.cleanupLog == nil {
		return nil
	}
	return f.cleanupLog.update(n, func(v *cleanupObservation) error {
		if v.StartedAt != "" {
			return nil
		}
		v.StartedAt = time.Now().UTC().Format(time.RFC3339Nano)
		var state struct {
			State struct {
				Running, OOMKilled bool
				ExitCode           int
				FinishedAt         string
			}
			Config struct{ Labels map[string]string }
		}
		if e := docker("GET", "/containers/"+n+"/json", nil, &state); e == nil && state.Config.Labels["ci-runner.owner"] == f.owner && state.Config.Labels["ci-runner.job"] == n {
			finished := ""
			if t, e := time.Parse(time.RFC3339Nano, state.State.FinishedAt); e == nil && t.Year() > 1 {
				finished = t.UTC().Format(time.RFC3339Nano)
			}
			v.Runner = cleanupRunner{true, state.State.Running, state.State.OOMKilled, state.State.ExitCode, finished}
		}
		b, e := f.serviceRead(serviceBase(n)+"/index.json", 65536)
		if e == nil {
			var index serviceIndex
			if strictServiceDecode(b, &index) == nil && index.Owner == f.owner && index.Job == n && serviceHex.MatchString(index.Lease) && serviceHex.MatchString(index.Fingerprint) {
				// The private index was produced by this controller, never by an adapter.
				v.Services = &index
			}
		}
		return nil
	})
}

func (f *fleet) finishCleanupObservation(n string) error {
	if f.cleanupLog == nil {
		return nil
	}
	if err := f.verifyCleanupAbsent(n); err != nil {
		return err
	}
	return f.cleanupLog.update(n, func(v *cleanupObservation) error {
		if v.StartedAt == "" {
			return fmt.Errorf("cleanup start evidence missing")
		}
		if v.ReleasedAt == "" {
			v.ReleasedAt = time.Now().UTC().Format(time.RFC3339Nano)
		}
		return nil
	})
}

func (f *fleet) verifyCleanupAbsent(n string) error {
	for _, p := range []string{"jobs/" + n, "leases/" + n, f.work.leaseDir + "/" + n} {
		if _, err := f.work.root.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("cleanup allocation still present or unknown")
		}
	}
	filter, _ := json.Marshal(obj{"label": []string{"ci-runner.owner=" + f.owner, "ci-runner.job=" + n}})
	for _, path := range []string{"/containers/json?all=true&", "/networks?"} {
		var resources []json.RawMessage
		if err := docker("GET", path+"filters="+url.QueryEscape(string(filter)), nil, &resources); err != nil || resources == nil || len(resources) != 0 {
			return fmt.Errorf("cleanup resources still present or unknown")
		}
	}
	return nil
}

// A process can die after deleting the last allocation but before committing
// ReleasedAt. Recover those journals even when Docker and pool leases are gone.
func (f *fleet) recoverCleanupObservations() error {
	if f.cleanupLog == nil {
		return nil
	}
	d, e := f.cleanupLog.root.Open(".")
	if e != nil {
		return e
	}
	defer d.Close()
	for {
		entries, e := d.ReadDir(100)
		if e != nil && e != io.EOF {
			return e
		}
		for _, entry := range entries {
			if filepath.Ext(entry.Name()) != ".json" {
				continue
			}
			n := entry.Name()[:len(entry.Name())-5]
			v, err := f.cleanupLog.read(n)
			if err != nil {
				return err
			}
			if v.StartedAt == "" || v.ReleasedAt != "" {
				continue
			}
			if f.state(n) != nil {
				continue
			}
			// Only reconcile already-absent allocations. Never recreate a lease or
			// delete resources from a journal name alone.
			for _, p := range []string{"jobs/" + n, "leases/" + n, f.work.leaseDir + "/" + n} {
				if _, err = f.work.root.Lstat(p); !errors.Is(err, os.ErrNotExist) {
					return fmt.Errorf("cleanup journal has unreconciled allocation")
				}
			}
			if err = f.finalizeCleanup(n); err != nil {
				return err
			}
		}
		if e == io.EOF {
			return nil
		}
	}
}
