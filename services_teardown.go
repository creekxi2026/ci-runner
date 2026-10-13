package main

// A project supplies the meaning of checks. The core only accepts the exact
// operator-declared boolean keys, so adapter diagnostics/credentials cannot be
// copied into the durable journal.
import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"
)

type serviceTerminalOutput struct {
	Version int             `json:"version"`
	Checks  map[string]bool `json:"checks"`
}
type serviceTerminalProof struct {
	Adapter string          `json:"adapter_image"`
	Config  string          `json:"config_digest"`
	Checks  map[string]bool `json:"checks"`
}

func (f *fleet) collectServiceTeardown(index *serviceIndex, id string) error {
	if f.cleanupLog == nil {
		return nil
	}
	// Never run a privileged cleanup adapter until the untrusted worker is gone.
	var worker any
	if e := docker("GET", "/containers/"+index.Job+"/json", nil, &worker); e != errMissing {
		return fmt.Errorf("worker absence unverified")
	}
	v, e := f.cleanupLog.read(index.Job)
	if e != nil {
		return e
	}
	if _, ok := v.Proofs[id]; ok {
		return f.cleanupLog.update(index.Job, func(*cleanupObservation) error { return nil })
	}
	// The immutable, private per-lease spec survives catalog changes and restart.
	b, e := f.serviceRead(serviceDir(index.Job, id)+"/spec.json", 65536)
	if os.IsNotExist(e) {
		return nil
	} // Legacy leases have no terminal capability.
	if e != nil {
		return e
	}
	var spec serviceSpec
	if e = strictServiceDecode(b, &spec); e != nil {
		return e
	}
	if spec.Teardown == nil {
		return nil
	}
	item := index.Services[id]
	if spec.Image != item.Image || spec.Adapter != item.Adapter || serviceDigest(spec.Config) != item.ConfigDigest {
		return fmt.Errorf("teardown declaration changed")
	}
	// A successful stopped teardown container is the durable completion witness.
	// Transfer its proof even after the execution deadline (e.g. a journal fsync
	// failure followed by restart); never accept output from an abandoned run.
	completed := func() (bool, error) {
		var state serviceContainer
		e := docker("GET", "/containers/"+serviceName(index.Job, id, "teardown")+"/json", nil, &state)
		if errors.Is(e, errMissing) {
			return false, nil
		}
		if e != nil {
			return false, e
		}
		return serviceMatches(state.Config.Labels, index, id, "teardown") && state.Config.Image == spec.Adapter && !state.State.Running && state.State.Status == "exited" && state.State.ExitCode != nil && *state.State.ExitCode == 0, nil
	}
	success, e := completed()
	if e != nil {
		return e
	}
	if !success {
		r, e := f.readServiceRecord(index.Job, id)
		if os.IsNotExist(e) {
			return nil
		}
		if e != nil {
			return e
		}
		if r.Endpoint.Host == "" {
			return nil
		}
		started, e := time.Parse(time.RFC3339Nano, v.StartedAt)
		if e != nil {
			return fmt.Errorf("missing cleanup start")
		}
		deadline := started.Add(time.Duration(spec.Resources.Startup) * time.Second)
		if time.Now().After(deadline) {
			return nil
		}
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		defer cancel()
		var server serviceContainer
		e = dockerContext(ctx, "GET", "/containers/"+serviceName(index.Job, id, "server")+"/json", nil, &server)
		if errors.Is(e, errMissing) {
			return nil
		}
		if e != nil {
			return e // Unknown availability must retain the source for bounded retry.
		}
		if !serviceMatches(server.Config.Labels, index, id, "server") || server.Config.Image != spec.Image {
			return fmt.Errorf("teardown server identity mismatch")
		}
		if !server.State.Running {
			return nil
		}
		if e = f.runServiceAdapter(ctx, index, id, spec, r.Endpoint, "teardown"); e != nil {
			success, inspectError := completed()
			if inspectError != nil {
				return inspectError
			}
			if !success {
				// Failed setup/observation and partial adapter failure are retryable
				// only inside the original persisted execution bound. Keep inputs.
				return e
			}
		}
	}
	path := serviceDir(index.Job, id) + "/terminal/proof.json"
	st, e := f.work.root.Lstat(path)
	if os.IsNotExist(e) {
		return nil
	}
	if e != nil {
		return e
	}
	if !protectedCleanup(st, 0, 0444, false) {
		return nil
	}
	// serviceRead adds O_NOFOLLOW and bounds the read; root-private ancestors are
	// not mounted into the worker or another lease's adapter.
	b, e = f.serviceRead(path, 65536)
	if os.IsNotExist(e) {
		return nil
	}
	if e != nil {
		return e
	}
	var output serviceTerminalOutput
	if strictServiceDecode(b, &output) != nil || output.Version != 1 || len(output.Checks) != len(spec.Teardown.Checks) {
		return nil
	}
	for _, key := range spec.Teardown.Checks {
		if !output.Checks[key] {
			return nil
		}
	}
	// Persist before deleting server, bootstrap secret, private outputs or disk.
	return f.cleanupLog.update(index.Job, func(v *cleanupObservation) error {
		if v.Proofs == nil {
			v.Proofs = map[string]serviceTerminalProof{}
		}
		v.Proofs[id] = serviceTerminalProof{spec.Adapter, item.ConfigDigest, output.Checks}
		return nil
	})
}
