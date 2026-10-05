package main

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// Only this root initializer sees protected templates and one empty job home.
// Workers see neither the seed mount nor other jobs; cached writes stay private.
func (f *fleet) prepareWorkspace(ctx context.Context, n string) error {
	if f.work == nil {
		return nil
	}
	var image struct{ ID string }
	if err := dockerContext(ctx, "GET", "/images/"+f.image+"/json", nil, &image); err != nil {
		return err
	}
	id := strings.TrimPrefix(image.ID, "sha256:")
	if !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(id) {
		return fmt.Errorf("invalid runner image identity")
	}
	cmd := []string{"python3", "/opt/ci/workspace-init.py", "prepare", "--image-id", id}
	if f.seed != "" {
		cmd = append(cmd, "--seed", f.seed)
	}
	h := secure()
	h["ReadonlyRootfs"] = true
	h["Memory"] = 256 * 1024 * 1024
	h["MemorySwap"] = h["Memory"]
	h["CapAdd"] = []string{"CHOWN", "DAC_OVERRIDE", "FOWNER"}
	h["Mounts"] = []obj{
		{"Type": "volume", "Source": f.work.volume, "Target": "/templates", "VolumeOptions": obj{"NoCopy": true, "Subpath": "templates"}},
		f.diskMount(n, "home", "/job-home"),
	}
	helper := n + "-fw"
	if err := f.createContext(ctx, helper, f.image, "0", cmd, nil, h, "none"); err != nil {
		return err
	}
	var result struct{ StatusCode *int }
	if err := dockerContext(ctx, "POST", "/containers/"+helper+"/wait?condition=not-running", nil, &result); err != nil {
		return err
	}
	if result.StatusCode == nil || *result.StatusCode != 0 {
		return fmt.Errorf("private workspace initialization failed")
	}
	return dockerContext(ctx, "DELETE", "/containers/"+helper+"?force=true", nil, nil)
}
