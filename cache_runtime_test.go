package main

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

// Explicit opt-in: exercise the real controller cache provisioner against a local
// Docker daemon. It never registers with GitHub and never deletes the input volume.
func TestCacheRuntime(t *testing.T) {
	image := os.Getenv("CI_CACHE_INTEGRATION_IMAGE")
	if image == "" {
		t.Skip("set CI_CACHE_INTEGRATION_IMAGE for the local Docker fixture")
	}
	events, _ := allowedEvents("workflow_dispatch")
	c, err := dependencyCacheConfig("trusted-manual", os.Getenv("CI_CACHE_INTEGRATION_LANE"), os.Getenv("CI_CACHE_INTEGRATION_REPOSITORY"), os.Getenv("CI_CACHE_INTEGRATION_OWNER"), events, os.Getenv("CI_CACHE_INTEGRATION_VOLUME"))
	if err != nil {
		t.Fatal(err)
	}
	f := &fleet{cache: c, owner: fmt.Sprintf("cache-check-%d", time.Now().UnixNano()), image: image}
	for i := 0; i < 2; i++ {
		name := fmt.Sprintf("ci-cache-check-%d-%d", time.Now().UnixNano(), i)
		func() {
			defer func() {
				for _, n := range []string{name, name + "-fw"} {
					e := docker("DELETE", "/containers/"+n+"?force=true", nil, nil)
					if e != nil && e != errMissing {
						t.Errorf("fixture cleanup %s: %v", n, e)
					}
				}
				if e := f.removeJobDisk(name); e != nil {
					t.Error(e)
				}
			}()
			host := secure()
			host["ReadonlyRootfs"] = true
			host["Memory"] = int64(4 * 1024 * 1024 * 1024)
			host["MemorySwap"] = host["Memory"]
			if e := f.prepareJobDisk(context.Background(), name, host); e != nil {
				t.Fatal(e)
			}
			env, e := f.prepareDependencyCache(context.Background(), name, host, []string{"HOME=/home/runner"})
			if e != nil {
				t.Fatal(e)
			}
			mounts := host["Mounts"].([]obj)
			if len(mounts) != 4 || mounts[0]["Source"] != jobVolume(name) || mounts[2]["Source"] != c.volume || mounts[3]["ReadOnly"] != true {
				t.Fatal("incorrect mounts")
			}
			marker := "/opt/ci-cache/go-build/" + f.owner
			operation := "printf persistent > " + marker
			if i == 1 {
				operation = "test \"$(cat " + marker + ")\" = persistent; rm " + marker
			}
			script := `. /opt/ci/cache-env.sh
 test "$CI_DEPENDENCY_CACHE" = 1
 test -w "$GOMODCACHE"
 test -w "$GOCACHE"
 test ! -w /opt/ci-tools
 test ! -w /opt/ci-cache/tools
 test -f "$RUNNER_TOOL_CACHE/go/$(go env GOVERSION | sed s/^go//)/arm64.complete"
 node --version; go version
 ` + operation
			if e = f.createContext(context.Background(), name, image, "1001:1001", []string{"bash", "-ec", script}, env, host, "none"); e != nil {
				t.Fatal(e)
			}
			var result struct{ StatusCode *int }
			if e = docker("POST", "/containers/"+name+"/wait?condition=not-running", nil, &result); e != nil || result.StatusCode == nil || *result.StatusCode != 0 {
				t.Fatalf("job %d failed: %+v %v", i, result, e)
			}
			t.Logf("real controller provisioned fresh offline job %d against %s; tools read-only, cache retained", i+1, c.volume)
		}()
	}
}
