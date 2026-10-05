package main

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// A display-only Compose project keeps dynamic jobs out of Docker Desktop and
// OrbStack's root list. It is separate from deployment projects so a deployment
// `compose up --remove-orphans` cannot select active job containers.
func (f *fleet) containerLabels(name string) obj {
	l := f.resourceLabels(jobName(name))
	l["com.docker.compose.project"] = "ci-jobs"
	l["com.docker.compose.service"] = name
	l["com.docker.compose.container-number"] = "1"
	l["com.docker.compose.oneoff"] = "False"
	l["ci-runner.role"] = strings.TrimPrefix(name, jobName(name))
	if name == jobName(name) {
		l["ci-runner.role"] = "runner"
	}
	return l
}

// Refuse a path/volume mismatch: otherwise a controller could count leases on
// one filesystem while Docker mounts data from another. The shared root is only
// visible to trusted controllers, never workers or their companions.
func (f *fleet) verifyWorkMount(ctx context.Context) error {
	name := os.Getenv("CONTROLLER_CONTAINER")
	if name == "" {
		return fmt.Errorf("CONTROLLER_CONTAINER required")
	}
	var c struct {
		Mounts []struct {
			Type, Name, Destination string
			RW                      bool
		}
	}
	if e := dockerContext(ctx, "GET", "/containers/"+name+"/json", nil, &c); e != nil {
		return e
	}
	for _, m := range c.Mounts {
		if m.Destination == f.work.root.Name() && m.Type == "volume" && m.Name == f.work.volume && m.RW {
			var v diskVolume
			if e := dockerContext(ctx, "GET", "/volumes/"+m.Name, nil, &v); e != nil {
				return e
			}
			if v.Driver != "local" || len(v.Options) != 0 {
				return fmt.Errorf("work volume must use local disk without driver options")
			}
			return nil
		}
	}
	return fmt.Errorf("controller work mount does not match configured volume")
}
