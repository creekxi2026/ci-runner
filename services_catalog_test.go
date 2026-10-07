package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func catalogFixture() (serviceCatalog, servicePolicy) {
	s := serviceSpec{Image: "registry/service@sha256:" + strings.Repeat("a", 64), Adapter: "registry/adapter@sha256:" + strings.Repeat("b", 64), Config: map[string]any{"schema": 1, "layout": "project-owned"}, Ports: []int{8765}, Resources: serviceResources{512 << 20, 1000, 128, 1 << 30, 120}, Storage: []string{"/data"}, Env: map[string]string{"PASSWORD_FILE": "/run/ci-service-secrets/bootstrap"}, Secrets: map[string][]string{"bootstrap": {"init", "service"}}}
	return serviceCatalog{1, map[string]serviceSpec{"example": s}}, servicePolicy{1, 2, serviceResources{1 << 30, 2000, 256, 2 << 30, 180}, []string{"/data"}, "reviewed-v1"}
}
func TestServiceCanonicalInteroperability(t *testing.T) {
	input := []byte("{\"z\":\"<>&/\\u007f\\n\\t\",\"a\":[9007199254740991,-2,true,null]}")
	v, e := serviceJSON(input)
	if e != nil {
		t.Fatal(e)
	}
	b, e := serviceCanonical(v)
	if e != nil {
		t.Fatal(e)
	}
	// Produced independently with the consumer's Python canonical encoder.
	want := `{"a":[9007199254740991,-2,true,null],"z":"<>&/\u007f\n\t"}`
	if string(b) != want {
		t.Fatalf("canonical mismatch: %q", b)
	}
	for _, bad := range []string{`{"x":1,"x":2}`, `1.0`, `1e2`, `9007199254740992`, `"é"`, `{} {}`, `{"x":{"y":1,"y":2}}`} {
		if _, e := serviceJSON([]byte(bad)); e == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}
func TestServiceCatalogRejectsUntrustedInputs(t *testing.T) {
	cases := map[string]func(*serviceSpec){
		"tag":                  func(s *serviceSpec) { s.Image = "registry/service:latest" },
		"adapter tag":          func(s *serviceSpec) { s.Adapter = "registry/adapter:latest" },
		"host mount":           func(s *serviceSpec) { s.Storage = []string{"/var/run/docker.sock"} },
		"traversal":            func(s *serviceSpec) { s.Storage = []string{"/data/../etc"} },
		"worker secret":        func(s *serviceSpec) { s.Secrets["bootstrap"] = []string{"worker"} },
		"duplicate recipients": func(s *serviceSpec) { s.Secrets["bootstrap"] = []string{"init", "init"} },
		"duplicate ports":      func(s *serviceSpec) { s.Ports = []int{8765, 8765} },
		"no ports":             func(s *serviceSpec) { s.Ports = []int{} },
		"over memory":          func(s *serviceSpec) { s.Resources.Memory = 4 << 30 },
		"over deadline":        func(s *serviceSpec) { s.Resources.Startup = 301 },
		"bad env":              func(s *serviceSpec) { s.Env["BAD=KEY"] = "value" },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			c, p := catalogFixture()
			s := c.Services["example"]
			change(&s)
			c.Services["example"] = s
			if e := validateServices(&c, p); e == nil {
				t.Fatal("accepted unsafe catalog")
			}
		})
	}
	c, p := catalogFixture()
	if e := validateServices(&c, p); e != nil {
		t.Fatal(e)
	}
	encoded, _ := json.Marshal(c)
	for _, bad := range [][]byte{
		bytes.Replace(encoded, []byte(`"version":1`), []byte(`"version":1,"version":1`), 1),
		bytes.Replace(encoded, []byte(`"storage":["/data"]`), []byte(`"storage":["/data"],"host_script":"/admin"`), 1),
		bytes.Replace(encoded, []byte(`"service_env":{"PASSWORD_FILE":"/run/ci-service-secrets/bootstrap"}`), []byte(`"service_env":{"PASSWORD_FILE":null}`), 1),
		bytes.Replace(encoded, []byte(`"secret_files":{"bootstrap":["init","service"]}`), []byte(`"secret_files":null`), 1),
	} {
		var got serviceCatalog
		e := strictServiceDecode(bad, &got)
		if e == nil {
			e = validateServices(&got, p)
		}
		if e == nil {
			t.Fatalf("accepted malformed catalog %s", bad)
		}
	}
}
func TestServiceFingerprintCoversStaticInputs(t *testing.T) {
	c, p := catalogFixture()
	base := obj{"version": 1, "adapter_abi": 1, "platform": "linux/arm64", "controller_image": "registry/controller@sha256:" + strings.Repeat("c", 64), "runner_image": "registry/runner@sha256:" + strings.Repeat("d", 64), "tools_seed": "", "dependency_seed": "", "policy_digest": serviceDigest(p), "catalog": c}
	before := serviceDigest(base)
	for _, key := range []string{"controller_image", "runner_image", "tools_seed", "dependency_seed", "policy_digest", "platform"} {
		old := base[key]
		base[key] = "changed"
		if serviceDigest(base) == before {
			t.Errorf("fingerprint ignores %s", key)
		}
		base[key] = old
	}
	for _, field := range []string{"image", "adapter", "config", "ports", "memory", "storage", "secret"} {
		changed, _ := catalogFixture()
		s := changed.Services["example"]
		switch field {
		case "image":
			s.Image = "different"
		case "adapter":
			s.Adapter = "different"
		case "config":
			s.Config["layout"] = "changed"
		case "ports":
			s.Ports = []int{8766}
		case "memory":
			s.Resources.Memory++
		case "storage":
			s.Storage = []string{"/other"}
		case "secret":
			s.Secrets["bootstrap"] = []string{"init"}
		}
		changed.Services["example"] = s
		base["catalog"] = changed
		if serviceDigest(base) == before {
			t.Errorf("fingerprint ignores %s", field)
		}
	}
	base["catalog"] = c
	if serviceDigest(base) != before {
		t.Fatal("determinism lost")
	}
}
func TestServiceImageAdmission(t *testing.T) {
	ref := "registry/service@sha256:" + strings.Repeat("a", 64)
	for _, arch := range []string{"amd64", "arm64"} {
		t.Run(arch, func(t *testing.T) {
			withEngine(t, func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(obj{"Architecture": arch, "Os": "linux"})
			})
			e := verifyServiceImage(context.Background(), ref)
			if (e == nil) != (arch == "arm64") {
				t.Fatal("architecture gate incorrect")
			}
		})
	}
}

func TestServiceCleanupRejectsForeignLease(t *testing.T) {
	index := &serviceIndex{Owner: "ours", Job: "ci-job-12345678-001", Lease: strings.Repeat("a", 64), Fingerprint: strings.Repeat("b", 64)}
	f := &fleet{owner: index.Owner}
	deletes := 0
	withEngine(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" {
			deletes++
			return
		}
		labels := f.serviceLabels(index, "example", "server")
		labels["ci-runner.lease"] = strings.Repeat("c", 64)
		json.NewEncoder(w).Encode(obj{"Config": obj{"Labels": labels}})
	})
	if e := f.removeServiceContainer(context.Background(), index, "example", "server"); e == nil || deletes != 0 {
		t.Fatal("foreign lease was deleted")
	}
}
