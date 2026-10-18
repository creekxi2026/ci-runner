package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPrivateInputsRequireSeparateAuthenticatedPool(t *testing.T) {
	f, b := inputFixture(t, "exit 0")
	f.allowedEvents = map[string]bool{"workflow_dispatch": true}
	for _, dir := range []string{"operator", "operator/sealed", "operator/state"} {
		if e := f.work.root.Mkdir(dir, 0755); e != nil {
			t.Fatal(e)
		}
	}
	state := filepath.Join(f.work.root.Name(), "operator/state")
	if e := os.Chown(state, 1001, 1001); e != nil {
		t.Fatal(e)
	}
	os.Chmod(state, 0700)
	policy := b.policy
	policy.Schema = "ci-job-inputs/v2"
	policy.PrivateMounts = []privateInputMount{{"operator/state", "/var/lib/project/dummy", false}, {"operator/sealed", "/opt/project/dummy", true}}
	host := obj{"Mounts": []obj{{"Type": "volume", "Source": "ci-work-linux-arm64", "Target": "/tmp", "VolumeOptions": obj{"Subpath": "jobs/dummy/tmp"}}}}
	if e := f.mountPrivateInputs(host, policy); e != nil {
		t.Fatal(e)
	}
	mounts := host["Mounts"].([]obj)
	if len(mounts) != 3 || mounts[1]["ReadOnly"] != false || mounts[2]["ReadOnly"] != true || mounts[1]["Source"] != "ci-work-linux-arm64" {
		t.Fatal("private mount boundary changed")
	}
	// Defaults never acquire these resources.
	empty := obj{}
	if e := f.mountPrivateInputs(empty, b.policy); e != nil || len(empty["Mounts"].([]obj)) != 0 {
		t.Fatal("default gained mounts")
	}
	for _, event := range []string{"pull_request", "push", "workflow_run"} {
		f.allowedEvents[event] = true
		if f.validatePrivateInputs(policy) == nil {
			t.Fatal("mixed event pool accepted")
		}
		delete(f.allowedEvents, event)
	}
	old := policy.Schema
	policy.Schema = "ci-job-inputs/v1"
	if f.validatePrivateInputs(policy) == nil {
		t.Fatal("v1 accepted private state")
	}
	policy.Schema = old
	for _, bad := range []privateInputMount{
		{"/", "/opt/project/data", true}, {"operator/../sealed", "/opt/project/data", true},
		{"operator", "/opt/project/data", true}, {"operator/sealed", "/opt/actions-runner", true},
		{"operator/sealed", "/run/ci-intake", true}, {"operator/sealed", "/opt/project/../ci/x", true},
		{"operator/sealed", "/var/lib/project/state", false},
	} {
		p := policy
		p.PrivateMounts = []privateInputMount{bad}
		if f.validatePrivateInputs(p) == nil {
			t.Fatalf("invalid private input accepted: %+v", bad)
		}
	}
	os.Chmod(state, 0755)
	if f.validatePrivateInputs(policy) == nil {
		t.Fatal("writable state permission change accepted")
	}
	os.Chmod(state, 0700)
	if e := os.Symlink("sealed", filepath.Join(f.work.root.Name(), "operator/link")); e != nil {
		t.Fatal(e)
	}
	p := policy
	p.PrivateMounts = []privateInputMount{{"operator/link", "/opt/project/data", true}}
	if f.validatePrivateInputs(p) == nil {
		t.Fatal("symlink input accepted")
	}
	// Reject both nested private mounts and overlap with an existing host mount.
	p = policy
	p.PrivateMounts = append(append([]privateInputMount{}, policy.PrivateMounts...), privateInputMount{"operator/sealed", "/opt/project/dummy/sub", true})
	if f.validatePrivateInputs(p) == nil {
		t.Fatal("nested private mount accepted")
	}
	collision := obj{"Mounts": []obj{{"Target": "/opt/project"}}}
	if f.mountPrivateInputs(collision, policy) == nil {
		t.Fatal("existing parent mount overlaid")
	}
}
