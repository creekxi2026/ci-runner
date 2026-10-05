package main

import (
	"context"
	"errors"
	"fmt"
)

const jobStorageSchema = "disk-v1"

func jobVolume(n string) string { return n + "-disk" }

func jobDiskMount(n, subpath, target string) obj {
	return obj{"Type": "volume", "Source": jobVolume(n), "Target": target,
		"VolumeOptions": obj{"NoCopy": true, "Subpath": subpath}}
}

type diskVolume struct {
	Name, Driver string
	Labels       map[string]string
	Options      map[string]string
}

func (f *fleet) ownsJobDisk(n string, v diskVolume) bool {
	return v.Name == jobVolume(n) && v.Driver == "local" && len(v.Options) == 0 &&
		v.Labels["ci-runner.owner"] == f.owner && v.Labels["ci-runner.job"] == n &&
		v.Labels["ci-runner.storage"] == jobStorageSchema
}

// Production uses private subpaths of the shared work volume. The legacy
// per-job volume path remains for lifecycle migration fixtures and recovery.
func (f *fleet) prepareJobDisk(ctx context.Context, n string, host obj) error {
	if f.work != nil {
		if err := f.work.prepare(n); err != nil {
			return err
		}
		host["Mounts"] = []obj{f.diskMount(n, "home", "/home/runner"), f.diskMount(n, "tmp", "/tmp")}
		return nil
	}
	var v diskVolume
	err := dockerContext(ctx, "GET", "/volumes/"+jobVolume(n), nil, &v)
	if err == nil {
		return fmt.Errorf("new job disk already exists")
	}
	if !errors.Is(err, errMissing) {
		return err
	}
	if err = dockerContext(ctx, "POST", "/volumes/create", obj{"Name": jobVolume(n), "Driver": "local", "Labels": f.resourceLabels(n)}, nil); err != nil {
		return err
	}
	if err = dockerContext(ctx, "GET", "/volumes/"+jobVolume(n), nil, &v); err != nil {
		return err
	}
	if !f.ownsJobDisk(n, v) {
		return fmt.Errorf("job disk identity mismatch")
	}
	h := secure()
	h["ReadonlyRootfs"] = true
	h["Memory"] = 64 * 1024 * 1024
	h["MemorySwap"] = h["Memory"]
	h["CapAdd"] = []string{"CHOWN", "DAC_OVERRIDE", "FOWNER"}
	h["Mounts"] = []obj{{"Type": "volume", "Source": jobVolume(n), "Target": "/job-disk", "VolumeOptions": obj{"NoCopy": true}}}
	helper := n + "-fw"
	if err = f.createContext(ctx, helper, f.image, "0", []string{"python3", "/opt/ci/job-disk-init.py"}, nil, h, "none"); err != nil {
		return err
	}
	var result struct{ StatusCode *int }
	if err = dockerContext(ctx, "POST", "/containers/"+helper+"/wait?condition=not-running", nil, &result); err != nil {
		return err
	}
	if result.StatusCode == nil || *result.StatusCode != 0 {
		return fmt.Errorf("job disk initialization failed")
	}
	if err = dockerContext(ctx, "DELETE", "/containers/"+helper+"?force=true", nil, nil); err != nil {
		return err
	}
	host["Mounts"] = []obj{jobDiskMount(n, "home", "/home/runner"), jobDiskMount(n, "tmp", "/tmp")}
	return nil
}

// Remove only our job directory (or legacy owned disk) after containers leave.
// The shared workspace and dependency cache volumes are never removed here.
func (f *fleet) removeJobDisk(n string) error {
	if j := f.state(n); f.work != nil && j != nil && j.shared {
		return f.work.removeData(n)
	}
	var v diskVolume
	err := docker("GET", "/volumes/"+jobVolume(n), nil, &v)
	if errors.Is(err, errMissing) {
		return nil
	}
	if err != nil {
		return err
	}
	if !f.ownsJobDisk(n, v) {
		return fmt.Errorf("refusing foreign job disk cleanup")
	}
	return docker("DELETE", "/volumes/"+jobVolume(n), nil, nil)
}
