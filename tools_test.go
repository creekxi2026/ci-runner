package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProtectedToolsMount(t *testing.T) {
	root := t.TempDir()
	work, err := openSharedWork(root, "fixture", "tools-test")
	if err != nil {
		t.Fatal(err)
	}
	defer work.Close()
	seed := strings.Repeat("a", 64)
	base := filepath.Join(root, "templates/tools", seed)
	if err = os.MkdirAll(filepath.Join(base, "files"), 0700); err != nil {
		t.Fatal(err)
	}
	manifest := `{"kind":"tools","schema":1,"platform":"linux-arm64-ubuntu24.04","identity":"` + seed + `"}`
	if err = os.WriteFile(filepath.Join(base, "manifest.json"), []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	f := &fleet{work: work, toolsSeed: seed}
	h := obj{}
	if err = f.mountTools(h); err != nil {
		t.Fatal(err)
	}
	m := h["Mounts"].([]obj)[0]
	if m["ReadOnly"] != true || m["Target"] != "/opt/ci-tools" || m["Source"] != "fixture" {
		t.Fatalf("unsafe tools mount: %v", m)
	}
	if m["VolumeOptions"].(obj)["Subpath"] != "templates/tools/"+seed+"/files" {
		t.Fatal(m)
	}
	f.cache = &dependencyCache{}
	if f.mountTools(obj{}) == nil {
		t.Fatal("mixed tool snapshot accepted with shared writable cache")
	}
	f.cache = nil
	if err = os.Chmod(filepath.Join(base, "files"), 0777); err != nil {
		t.Fatal(err)
	}
	if f.mountTools(obj{}) == nil {
		t.Fatal("accepted writable snapshot")
	}
	os.Chmod(filepath.Join(base, "files"), 0700)
	os.Remove(filepath.Join(base, "manifest.json"))
	os.Symlink("../manifest.json", filepath.Join(base, "manifest.json"))
	if f.mountTools(obj{}) == nil {
		t.Fatal("accepted symlink manifest")
	}
	f.toolsSeed = "../escape"
	if f.mountTools(obj{}) == nil {
		t.Fatal("accepted path escape")
	}
}
