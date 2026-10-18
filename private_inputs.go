package main

// Operator-selected persistent resources for a separately authenticated v2 pool.
// They exist in the trusted Listener container before assignment. No Worker may
// execute until the pinned project helper authenticates the original allocation.
// Never enable this policy on a mixed/untrusted pool. This is not hot mounting.
import (
	"fmt"
	"os"
	"path"
	"strings"
	"syscall"
)

type privateInputMount struct {
	Subpath  string `json:"subpath"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only"`
}

func privateTarget(target string) bool {
	if path.Clean(target) != target || !strings.HasPrefix(target, "/") {
		return false
	}
	// Only project resources, never startup binaries, native runner, /proc, /etc,
	// credentials, work/home/tmp or controller/socket namespace overlays.
	return strings.HasPrefix(target, "/opt/project/") || strings.HasPrefix(target, "/var/lib/project/")
}

func (f *fleet) validatePrivateInputs(p inputPolicy) error {
	if len(p.PrivateMounts) == 0 {
		return nil
	}
	if p.Schema != "ci-job-inputs/v2" || f.work == nil || f.cache != nil || len(p.PrivateMounts) > 12 || len(f.allowedEvents) != 1 || !f.allowedEvents["workflow_dispatch"] {
		return fmt.Errorf("private inputs require an isolated dispatch v2 pool with writable cache off")
	}
	for i, m := range p.PrivateMounts {
		if !privateTarget(m.Target) || path.Clean(m.Subpath) != m.Subpath || strings.HasPrefix(m.Subpath, "/") || strings.HasPrefix(m.Subpath, ".") || len(strings.Split(m.Subpath, "/")) < 2 {
			return fmt.Errorf("invalid private input path")
		}
		for _, other := range p.PrivateMounts[:i] {
			if other.Target == m.Target || strings.HasPrefix(other.Target, m.Target+"/") || strings.HasPrefix(m.Target, other.Target+"/") || other.Subpath == m.Subpath {
				return fmt.Errorf("overlapping private inputs")
			}
		}
		for _, other := range p.Mounts {
			if other.Target == m.Target || strings.HasPrefix(other.Target, m.Target+"/") || strings.HasPrefix(m.Target, other.Target+"/") {
				return fmt.Errorf("private input overlaps publication")
			}
		}
		parts := strings.Split(m.Subpath, "/")
		for n := range parts {
			item := strings.Join(parts[:n+1], "/")
			st, e := f.work.root.Lstat(item)
			if e != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("private input directory unavailable")
			}
			uid := st.Sys().(*syscall.Stat_t).Uid
			if n == len(parts)-1 && !m.ReadOnly {
				if uid != 1001 || st.Mode().Perm() != 0700 {
					return fmt.Errorf("private input owner/mode differs")
				}
			} else if uid != 0 || st.Mode().Perm()&0022 != 0 {
				return fmt.Errorf("untrusted private input ancestor")
			}
		}
	}
	return nil
}

func (f *fleet) mountPrivateInputs(host obj, p inputPolicy) error {
	if e := f.validatePrivateInputs(p); e != nil {
		return e
	}
	mounts, _ := host["Mounts"].([]obj)
	for _, m := range p.PrivateMounts {
		for _, old := range mounts {
			target, ok := old["Target"].(string)
			if !ok || target == m.Target || strings.HasPrefix(target, m.Target+"/") || strings.HasPrefix(m.Target, target+"/") {
				return fmt.Errorf("private input mount collision")
			}
		}
		mounts = append(mounts, obj{"Type": "volume", "Source": f.work.volume, "Target": m.Target, "ReadOnly": m.ReadOnly, "VolumeOptions": obj{"NoCopy": true, "Subpath": m.Subpath}})
	}
	host["Mounts"] = mounts
	return nil
}
