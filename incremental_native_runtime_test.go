package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Explicit runtime fixture; no external service, download, or custom hit logic.
// The Go tool decides whether it can reuse a cached test result.
func TestIncrementalNativeGoRuntime(t *testing.T) {
	if os.Getenv("CI_NATIVE_CACHE_TEST") != "1" {
		t.Skip("native toolchain runtime fixture not requested")
	}
	c := testCache(t)
	c.policy.Paths = []string{".cache/compiler"}
	c.policy.MaxBytes = 1 << 30
	c.policy.MaxEntries = 50000
	home, p := cacheTestRoot(t)
	workspace := t.TempDir()
	if e := os.WriteFile(filepath.Join(workspace, "go.mod"), []byte("module example/nativecache\n\ngo 1.26\n"), 0600); e != nil {
		t.Fatal(e)
	}
	source := filepath.Join(workspace, "value.go")
	os.WriteFile(source, []byte("package nativecache\nfunc Value() int {return 7}\n"), 0600)
	os.WriteFile(filepath.Join(workspace, "value_test.go"), []byte("package nativecache\nimport \"testing\"\nfunc TestValue(t *testing.T) {if Value()!=7 {t.Fatal(\"changed source was executed\")}}\n"), 0600)
	run := func() (string, error) {
		cmd := exec.Command("go", "test", "./...")
		cmd.Dir = workspace
		cmd.Env = append(os.Environ(), "GOCACHE="+filepath.Join(p, ".cache/compiler"), "GOTOOLCHAIN=local", "GOPROXY=off", "GOSUMDB=off", "CGO_ENABLED=0")
		start := time.Now()
		b, e := cmd.CombinedOutput()
		t.Logf("native go test %.3fs: %s", time.Since(start).Seconds(), b)
		return string(b), e
	}
	if out, e := run(); e != nil || strings.Contains(out, "(cached)") {
		t.Fatal("cold native test", out, e)
	}
	start := time.Now()
	if e := c.save(context.Background(), home, strings.Repeat("a", 40)); e != nil {
		t.Fatal(e)
	}
	t.Logf("local directory save %.3fs", time.Since(start).Seconds())
	// Destroy the private task cache. Only the controller snapshot remains.
	if e := os.RemoveAll(filepath.Join(p, ".cache")); e != nil {
		t.Fatal(e)
	}
	start = time.Now()
	if e := c.restore(context.Background(), home); e != nil {
		t.Fatal(e)
	}
	t.Logf("local directory restore %.3fs", time.Since(start).Seconds())
	if out, e := run(); e != nil || !strings.Contains(out, "(cached)") {
		t.Fatal("Go did not reuse its own persisted result", out, e)
	}
	os.WriteFile(source, []byte("package nativecache\nfunc Value() int {return 8}\n"), 0600)
	if out, e := run(); e == nil || strings.Contains(out, "(cached)") || !strings.Contains(out, "changed source was executed") {
		t.Fatal("Go did not invalidate changed source", out, e)
	}
}

func TestIncrementalNativeJestRuntime(t *testing.T) {
	binary := os.Getenv("CI_JEST_BINARY")
	if binary == "" {
		t.Skip("native Jest fixture not requested")
	}
	c := testCache(t)
	c.policy.Paths = []string{".cache/transform"}
	home, p := cacheTestRoot(t)
	workspace := t.TempDir()
	cache := filepath.Join(p, ".cache/transform")
	config := map[string]any{"rootDir": workspace, "cacheDirectory": cache, "testEnvironment": "node", "transform": map[string]string{"\\.js$": filepath.Join(workspace, "transform.cjs")}}
	b, _ := json.Marshal(config)
	os.WriteFile(filepath.Join(workspace, "jest.config.json"), b, 0600)
	os.WriteFile(filepath.Join(workspace, "transform.cjs"), []byte(`const fs=require('fs'); module.exports={process(source){fs.appendFileSync(__dirname+'/transform-count','x'); return {code:source};}};`), 0600)
	source := filepath.Join(workspace, "value.js")
	os.WriteFile(source, []byte("module.exports=7;"), 0600)
	os.WriteFile(filepath.Join(workspace, "value.test.js"), []byte(`const fs=require('fs'); test('value',()=>{fs.appendFileSync(__dirname+'/test-count','x');expect(require('./value.js')).toBe(7);});`), 0600)
	run := func() error {
		cmd := exec.Command("node", binary, "--config", filepath.Join(workspace, "jest.config.json"), "--runInBand")
		cmd.Dir = workspace
		out, e := cmd.CombinedOutput()
		t.Logf("native Jest: %s", out)
		return e
	}
	if e := run(); e != nil {
		t.Fatal(e)
	}
	before, _ := os.ReadFile(filepath.Join(workspace, "transform-count"))
	if e := c.save(context.Background(), home, strings.Repeat("a", 40)); e != nil {
		t.Fatal(e)
	}
	os.RemoveAll(filepath.Join(p, ".cache"))
	if e := c.restore(context.Background(), home); e != nil {
		t.Fatal(e)
	}
	if e := run(); e != nil {
		t.Fatal(e)
	}
	after, _ := os.ReadFile(filepath.Join(workspace, "transform-count"))
	tests, _ := os.ReadFile(filepath.Join(workspace, "test-count"))
	if string(after) != string(before) || string(tests) != "xx" {
		t.Fatalf("transform not reused or test skipped: transforms %d->%d tests=%s", len(before), len(after), tests)
	}
	os.WriteFile(source, []byte("module.exports=8;"), 0600)
	if e := run(); e == nil {
		t.Fatal("changed source did not invalidate Jest transform cache")
	}
	changed, _ := os.ReadFile(filepath.Join(workspace, "transform-count"))
	if len(changed) <= len(after) {
		t.Fatal("changed source was not transformed")
	}
	t.Logf("native Jest reused %d transforms, executed test again, and invalidated modified source", len(before))
}
