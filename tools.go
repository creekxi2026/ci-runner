package main

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"syscall"
)

// Tool distributions are administrator-published immutable snapshots. A mixed
// event pool may read them without mounting any writable dependency cache.
func (f *fleet) mountTools(host obj) error {
	if f.toolsSeed == "" {
		return nil
	}
	if f.work == nil || f.cache != nil || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(f.toolsSeed) {
		return fmt.Errorf("tools seed requires shared work storage, exact SHA256 and writable cache off")
	}
	base := "templates/tools/" + f.toolsSeed
	for _, path := range []string{"templates", "templates/tools", base, base + "/files", base + "/manifest.json"} {
		st, err := f.work.root.Lstat(path)
		if err != nil {
			return err
		}
		uid := st.Sys().(*syscall.Stat_t).Uid
		if st.Mode()&os.ModeSymlink != 0 || uid != uint32(os.Geteuid()) || st.Mode().Perm()&0022 != 0 {
			return fmt.Errorf("tool snapshot is not administrator-owned and protected")
		}
		if path == base+"/manifest.json" {
			if !st.Mode().IsRegular() {
				return fmt.Errorf("invalid tool manifest")
			}
		} else if !st.IsDir() {
			return fmt.Errorf("invalid tool directory")
		}
	}
	b, err := f.work.root.ReadFile(base + "/manifest.json")
	if err != nil {
		return err
	}
	var info struct {
		Kind, Identity, Platform string
		Schema                   int
	}
	if json.Unmarshal(b, &info) != nil || info.Kind != "tools" || info.Identity != f.toolsSeed || info.Schema != 1 || info.Platform != "linux-arm64-ubuntu24.04" {
		return fmt.Errorf("tool snapshot identity mismatch")
	}
	mounts, _ := host["Mounts"].([]obj)
	host["Mounts"] = append(mounts, obj{"Type": "volume", "Source": f.work.volume,
		"Target": "/opt/ci-tools", "ReadOnly": true,
		"VolumeOptions": obj{"NoCopy": true, "Subpath": base + "/files"}})
	return nil
}
