package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"time"
)

type dependencyCache struct{ volume, owner, repository, lane string }

// Dispatch metadata is not source attestation; opt-in is restricted to a
// dedicated operator-audited manual-only fleet, never a mixed-event fleet.
func dependencyCacheConfig(mode, lane, repository, owner string, events map[string]bool) (*dependencyCache, error) {
	if mode == "" || mode == "off" {
		return nil, nil
	}
	if mode != "trusted-manual" || !regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`).MatchString(lane) || owner == "" {
		return nil, fmt.Errorf("dependency cache requires trusted-manual mode, explicit trust lane and deployment")
	}
	if len(events) != 1 || !events["workflow_dispatch"] {
		return nil, fmt.Errorf("dependency cache requires workflow_dispatch-only fleet")
	}
	u, err := url.Parse(repository)
	if err != nil || u.Scheme != "https" || u.Host != "github.com" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || !regexp.MustCompile(`^/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`).MatchString(u.Path) {
		return nil, fmt.Errorf("dependency cache requires exact GitHub repository URL")
	}
	identity := fmt.Sprintf("v1\x00%s\x00%s\x00linux-arm64\x00trusted-manual\x00%s", owner, repository, lane)
	return &dependencyCache{volume: fmt.Sprintf("ci-deps-linux-arm64-%x", sha256.Sum256([]byte(identity))), owner: owner, repository: repository, lane: lane}, nil
}

// Named volumes survive container removal (including Docker's v=true). No
// volume DELETE path exists here, and cache labels never match job recovery.
func (f *fleet) prepareDependencyCache(ctx context.Context, job string, host obj, env []string) ([]string, error) {
	c := f.cache
	if c == nil {
		return env, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	var volume struct {
		Name, Driver string
		Labels       map[string]string
		Options      map[string]string
	}
	inspect := func() error { return dockerContext(ctx, "GET", "/volumes/"+c.volume, nil, &volume) }
	err := inspect()
	if errors.Is(err, errMissing) {
		if err = dockerContext(ctx, "POST", "/volumes/create", obj{"Name": c.volume, "Driver": "local", "Labels": c.labels()}, nil); err != nil {
			return nil, err
		}
		err = inspect()
	}
	if err != nil {
		return nil, err
	}
	if volume.Name != c.volume || volume.Driver != "local" || len(volume.Options) != 0 || len(volume.Labels) != len(c.labels()) {
		return nil, fmt.Errorf("dependency cache volume identity mismatch")
	}
	for key, value := range c.labels() {
		if volume.Labels[key] != value {
			return nil, fmt.Errorf("dependency cache volume ownership mismatch")
		}
	}
	mounts := []obj{{"Type": "volume", "Source": c.volume, "Target": "/opt/ci-cache", "ReadOnly": false, "VolumeOptions": obj{"NoCopy": true}}}
	h := secure()
	h["ReadonlyRootfs"] = true
	h["CapAdd"] = []string{"CHOWN", "DAC_OVERRIDE", "FOWNER"}
	h["Mounts"] = mounts
	h["Memory"] = 64 * 1024 * 1024
	// Reuse the existing tracked transient helper slot; it is removed before
	// firewall setup, and crash recovery already knows this job companion.
	helper := job + "-fw"
	if err = f.createContext(ctx, helper, f.image, "0", []string{"python3", "/opt/ci/cache-init.py"}, nil, h, "none"); err != nil {
		return nil, err
	}
	var result struct{ StatusCode *int }
	if err = dockerContext(ctx, "POST", "/containers/"+helper+"/wait?condition=not-running", nil, &result); err != nil {
		return nil, err
	}
	if result.StatusCode == nil || *result.StatusCode != 0 {
		return nil, fmt.Errorf("dependency cache initialization failed")
	}
	if err = dockerContext(ctx, "DELETE", "/containers/"+helper+"?force=true", nil, nil); err != nil {
		return nil, err
	}
	host["Mounts"] = mounts
	return append(env, "npm_config_cache=/home/runner/.npm", "PIP_CACHE_DIR=/opt/ci-cache/pip", "GOMODCACHE=/opt/ci-cache/gomod", "GOCACHE=/opt/ci-cache/go-build"), nil
}

func (c *dependencyCache) labels() obj {
	return obj{"ci-runner.cache-owner": c.owner, "ci-runner.cache-repository": c.repository, "ci-runner.cache-lane": c.lane, "ci-runner.cache-schema": "v1-linux-arm64-trusted-manual"}
}
