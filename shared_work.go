package main

// One disk volume is shared by controllers, never by job roots. An OS advisory
// lock serializes durable leases across controller processes (not just fleets).
import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

const sharedWorkSchema = "shared-disk-v2"
const globalJobLimit = 3

var errCapacity = errors.New("global job capacity full")

type workLease struct {
	Owner   string
	Created time.Time
	Shared  bool
}
type sharedWork struct {
	root            *os.Root
	lock, ownerLock *os.File
	mu              sync.Mutex
	volume, owner   string
}

func openSharedWork(path, volume, owner string) (*sharedWork, error) {
	if path == "" || volume == "" || owner == "" || filepath.Base(owner) != owner || owner == "." || owner == ".." {
		return nil, fmt.Errorf("shared work path, volume and safe deployment owner required")
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	s := &sharedWork{root: root, volume: volume, owner: owner}
	fail := func(e error) (*sharedWork, error) { s.Close(); return nil, e }
	// Controllers run as root. Jobs cannot see these root-owned directories.
	for _, p := range []string{"leases", "owners", "jobs"} {
		if err = root.Mkdir(p, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return fail(err)
		}
	}
	s.lock, err = root.OpenFile("admission.lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fail(err)
	}
	s.ownerLock, err = root.OpenFile("owners/"+owner, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fail(err)
	}
	if err = syscall.Flock(int(s.ownerLock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fail(fmt.Errorf("controller owner already active: %w", err))
	}
	err = s.locked(func() error {
		want := sharedWorkSchema + "\n" + volume + "\nlimit=3\n"
		got, e := root.ReadFile("schema")
		if errors.Is(e, os.ErrNotExist) {
			return s.writeAtomic("schema", []byte(want))
		}
		if e != nil {
			return e
		}
		if string(got) != want {
			return fmt.Errorf("shared work schema/volume/budget mismatch")
		}
		return nil
	})
	if err != nil {
		return fail(err)
	}
	return s, nil
}
func (s *sharedWork) Close() {
	if s.ownerLock != nil {
		s.ownerLock.Close()
	}
	if s.lock != nil {
		s.lock.Close()
	}
	if s.root != nil {
		s.root.Close()
	}
}
func (s *sharedWork) locked(fn func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := syscall.Flock(int(s.lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
	return fn()
}
func (s *sharedWork) writeAtomic(p string, b []byte) error {
	f, e := s.root.OpenFile(p+".new", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	_, e = f.Write(b)
	if e == nil {
		e = f.Sync()
	}
	c := f.Close()
	if e != nil {
		return e
	}
	if c != nil {
		return c
	}
	if e = s.root.Rename(p+".new", p); e != nil {
		return e
	}
	d, e := s.root.Open(filepath.Dir(p))
	if e != nil {
		return e
	}
	defer d.Close()
	return d.Sync()
}
func (s *sharedWork) leases() (map[string]workLease, error) {
	d, e := s.root.Open("leases")
	if e != nil {
		return nil, e
	}
	defer d.Close()
	entries, e := d.ReadDir(-1)
	if e != nil {
		return nil, e
	}
	result := map[string]workLease{}
	for _, entry := range entries {
		n := entry.Name()
		if filepath.Ext(n) == ".new" {
			continue
		} // uncommitted journal: no resources created yet
		if !validJobName.MatchString(n) || !entry.Type().IsRegular() {
			return nil, fmt.Errorf("invalid admission record")
		}
		b, e := s.root.ReadFile("leases/" + n)
		if e != nil {
			return nil, e
		}
		var l workLease
		if e = json.Unmarshal(b, &l); e != nil || l.Owner == "" || l.Created.IsZero() {
			return nil, fmt.Errorf("invalid admission lease")
		}
		result[n] = l
	}
	return result, nil
}
func (s *sharedWork) reserve(n string, adopt bool) error {
	if !validJobName.MatchString(n) {
		return fmt.Errorf("invalid job name")
	}
	return s.locked(func() error {
		all, e := s.leases()
		if e != nil {
			return e
		}
		if l, ok := all[n]; ok {
			if l.Owner != s.owner {
				return fmt.Errorf("foreign job lease")
			}
			if adopt {
				return nil
			}
			return fmt.Errorf("job lease already exists")
		}
		if !adopt && len(all) >= globalJobLimit {
			return errCapacity
		}
		b, _ := json.Marshal(workLease{s.owner, time.Now().UTC(), !adopt})
		return s.writeAtomic("leases/"+n, b)
	})
}
func (s *sharedWork) owned() (map[string]workLease, error) {
	result := map[string]workLease{}
	e := s.locked(func() error {
		all, e := s.leases()
		if e != nil {
			return e
		}
		for n, l := range all {
			if l.Owner == s.owner {
				result[n] = l
			}
		}
		return nil
	})
	return result, e
}
func (s *sharedWork) checkOwner(n string) error {
	b, e := s.root.ReadFile("leases/" + n)
	if e != nil {
		return e
	}
	var l workLease
	if json.Unmarshal(b, &l) != nil || l.Owner != s.owner {
		return fmt.Errorf("foreign lease")
	}
	return nil
}
func (s *sharedWork) prepare(n string) error {
	if !validJobName.MatchString(n) {
		return fmt.Errorf("invalid job name")
	}
	if e := s.checkOwner(n); e != nil {
		return e
	}
	var usage syscall.Statfs_t
	if e := syscall.Statfs(s.root.Name(), &usage); e != nil {
		return e
	}
	if uint64(usage.Bavail)*uint64(usage.Bsize) < 5*1024*1024*1024 {
		return fmt.Errorf("less than 5 GiB free on work filesystem")
	}
	// Write the lease before any directory or Docker resource: crash recovery can
	// always identify the owner, even if initialization is interrupted.
	if e := s.root.Mkdir("jobs/"+n, 0700); e != nil {
		return e
	}
	for _, d := range []struct {
		name string
		uid  int
		mode os.FileMode
	}{{"home", 1001, 0700}, {"tmp", 1001, 01777}, {"postgres", 999, 0700}, {"postgres-run", 999, 0700}} {
		p := "jobs/" + n + "/" + d.name
		if e := s.root.Mkdir(p, 0700); e != nil {
			return e
		}
		if e := s.root.Chown(p, d.uid, d.uid); e != nil {
			return e
		}
		mode := d.mode
		if d.name == "tmp" {
			mode = 0777 | os.ModeSticky
		}
		if e := s.root.Chmod(p, mode); e != nil {
			return e
		}
	}
	return nil
}
func (s *sharedWork) removeData(n string) error {
	if !validJobName.MatchString(n) {
		return fmt.Errorf("invalid job name")
	}
	if e := s.checkOwner(n); e != nil {
		return e
	}
	// Root.RemoveAll confines traversal, including hostile symlinks from a job.
	return s.root.RemoveAll("jobs/" + n)
}
func (s *sharedWork) release(n string) error {
	return s.locked(func() error {
		if e := s.checkOwner(n); e != nil {
			return e
		}
		if _, e := s.root.Lstat("jobs/" + n); !errors.Is(e, os.ErrNotExist) {
			return fmt.Errorf("job data must be removed before release")
		}
		if e := s.root.Remove("leases/" + n); e != nil {
			return e
		}
		d, e := s.root.Open("leases")
		if e != nil {
			return e
		}
		defer d.Close()
		return d.Sync()
	})
}
func (f *fleet) diskMount(n, sub, target string) obj {
	if j := f.state(n); f.work != nil && j != nil && j.shared {
		return obj{"Type": "volume", "Source": f.work.volume, "Target": target, "VolumeOptions": obj{"NoCopy": true, "Subpath": "jobs/" + n + "/" + sub}}
	}
	return jobDiskMount(n, sub, target)
}
