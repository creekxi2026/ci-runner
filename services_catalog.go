package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// The catalog is operator input, never input from a job checkout. Config and
// environment keys are opaque to the controller.
type serviceResources struct {
	Memory  int64 `json:"memory_bytes"`
	CPU     int64 `json:"cpu_millis"`
	Pids    int64 `json:"pids"`
	Data    int64 `json:"data_bytes"`
	Startup int64 `json:"startup_seconds"`
}
type serviceSpec struct {
	Image     string              `json:"image"`
	Adapter   string              `json:"adapter_image"`
	Config    map[string]any      `json:"config"`
	Ports     []int               `json:"ports"`
	Resources serviceResources    `json:"resources"`
	Storage   []string            `json:"storage"`
	Env       map[string]string   `json:"service_env"`
	Secrets   map[string][]string `json:"secret_files"`
}
type serviceCatalog struct {
	Version  int                    `json:"version"`
	Services map[string]serviceSpec `json:"services"`
}
type servicePolicy struct {
	Version        int              `json:"version"`
	MaxServices    int              `json:"max_services"`
	MaxResources   serviceResources `json:"max_resources"`
	StorageTargets []string         `json:"storage_targets"`
	TrustRevision  string           `json:"trust_revision"`
}
type serviceManager struct {
	Catalog     serviceCatalog
	Fingerprint string
}

var serviceID = regexp.MustCompile(`^[a-z][a-z0-9-]{0,47}$`)
var serviceHex = regexp.MustCompile(`^[a-f0-9]{64}$`)
var envKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Decode token-by-token: encoding/json's usual struct decoder accepts duplicate
// keys. Reject them, floats, non-ASCII and unsafe integers before typed decode.
func serviceJSON(b []byte) (any, error) {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var read func() (any, error)
	read = func() (any, error) {
		t, e := d.Token()
		if e != nil {
			return nil, e
		}
		switch v := t.(type) {
		case json.Delim:
			switch v {
			case '{':
				m := map[string]any{}
				for d.More() {
					k, e := d.Token()
					if e != nil {
						return nil, e
					}
					s, ok := k.(string)
					if !ok || !ascii(s) {
						return nil, fmt.Errorf("invalid key")
					}
					if _, ok = m[s]; ok {
						return nil, fmt.Errorf("duplicate key")
					}
					m[s], e = read()
					if e != nil {
						return nil, e
					}
				}
				_, e = d.Token()
				return m, e
			case '[':
				a := []any{}
				for d.More() {
					x, e := read()
					if e != nil {
						return nil, e
					}
					a = append(a, x)
				}
				_, e = d.Token()
				return a, e
			}
		case string:
			if ascii(v) {
				return v, nil
			}
		case json.Number:
			n, e := strconv.ParseInt(string(v), 10, 64)
			if e == nil && n >= -9007199254740991 && n <= 9007199254740991 {
				return n, nil
			}
		case bool:
			return v, nil
		case nil:
			return nil, nil
		}
		return nil, fmt.Errorf("unsupported JSON value")
	}
	v, e := read()
	if e != nil {
		return nil, e
	}
	if _, e = d.Token(); e != io.EOF {
		return nil, fmt.Errorf("trailing JSON")
	}
	return v, nil
}
func ascii(s string) bool {
	for _, c := range s {
		if c > 127 {
			return false
		}
	}
	return true
}

// Match Python json.dumps(sort_keys=True, separators=(',', ':'), ensure_ascii=True).
// Go's JSON encoder otherwise HTML-escapes '<', '>' and '&'.
func serviceCanonical(v any) ([]byte, error) {
	b, e := json.Marshal(v)
	if e != nil {
		return nil, e
	}
	v, e = serviceJSON(b)
	if e != nil {
		return nil, e
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if e = enc.Encode(v); e != nil {
		return nil, e
	}
	return bytes.ReplaceAll(bytes.TrimSuffix(out.Bytes(), []byte{'\n'}), []byte{127}, []byte(`\u007f`)), nil
}
func serviceDigest(v any) string {
	b, e := serviceCanonical(v)
	if e != nil {
		panic(e)
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func strictServiceDecode(b []byte, v any) error {
	raw, e := serviceJSON(b)
	if e != nil {
		return e
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	d.DisallowUnknownFields()
	if e = d.Decode(v); e != nil {
		return e
	}
	before, e := serviceCanonical(raw)
	if e != nil {
		return e
	}
	after, e := serviceCanonical(v)
	if e != nil || !bytes.Equal(before, after) {
		return fmt.Errorf("missing or incorrectly typed service fields")
	}
	return nil
}
func loadServices(ctx context.Context, catalogPath, policyPath, controller, runner, tools, seed string) (*serviceManager, error) {
	c := serviceCatalog{Version: 1, Services: map[string]serviceSpec{}}
	p := servicePolicy{Version: 1, MaxServices: 0, StorageTargets: []string{}, TrustRevision: "empty-v1"}
	if catalogPath != "" {
		b, e := trustedServiceFile(catalogPath)
		if e != nil {
			return nil, e
		}
		if e = strictServiceDecode(b, &c); e != nil {
			return nil, fmt.Errorf("invalid service catalog")
		}
		b, e = trustedServiceFile(policyPath)
		if e != nil {
			return nil, e
		}
		if e = strictServiceDecode(b, &p); e != nil {
			return nil, fmt.Errorf("invalid service policy")
		}
	}
	if e := validateServices(&c, p); e != nil {
		return nil, e
	}
	if !digestReference.MatchString(controller) || !digestReference.MatchString(runner) {
		return nil, fmt.Errorf("immutable controller/runner required")
	}
	for _, s := range []string{tools, seed} {
		if s != "" && !serviceHex.MatchString(s) {
			return nil, fmt.Errorf("invalid service seed")
		}
	}
	for _, s := range c.Services {
		for _, ref := range []string{s.Image, s.Adapter} {
			if e := verifyServiceImage(ctx, ref); e != nil {
				return nil, e
			}
		}
		if e := verifyServiceVolumes(ctx, s.Image, s.Storage); e != nil {
			return nil, e
		}
		if e := verifyServiceVolumes(ctx, s.Adapter, nil); e != nil {
			return nil, e
		}
	}
	policy := obj{"version": 1, "operator": p, "network": "private-per-service-namespace-v1", "output": "root-readonly-uid1001-64k-v1", "storage": "bounded-tmpfs-no-restart-v1", "adapter": "isolated-init-check-no-logs-v1", "budget": "server12-adapter2-namespace1-firewall1-of16-v1"}
	fp := obj{"version": 1, "adapter_abi": 1, "platform": "linux/arm64", "controller_image": controller, "runner_image": runner, "tools_seed": tools, "dependency_seed": seed, "policy_digest": serviceDigest(policy), "catalog": c}
	return &serviceManager{c, serviceDigest(fp)}, nil
}
func trustedServiceFile(p string) ([]byte, error) {
	f, e := os.OpenFile(p, os.O_RDONLY, 0)
	if e != nil {
		return nil, fmt.Errorf("service configuration unavailable")
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0022 != 0 || st.Size() > 1024*1024 {
		return nil, fmt.Errorf("unsafe service configuration")
	}
	// Deployment mounts these controller-only files read-only. Check identity as
	// well as mode, including the path (reject a symlink installed by mistake).
	lst, e := os.Lstat(p)
	if e != nil || lst.Mode()&os.ModeSymlink != 0 || !rootOwned(st) {
		return nil, fmt.Errorf("untrusted service configuration")
	}
	return io.ReadAll(io.LimitReader(f, 1024*1024+1))
}
func validateServices(c *serviceCatalog, p servicePolicy) error {
	bad := func() error { return fmt.Errorf("service catalog/policy rejected") }
	if c.Version != 1 || c.Services == nil || p.Version != 1 || p.MaxServices < 0 || p.MaxServices > 4 || len(c.Services) > p.MaxServices || p.TrustRevision == "" {
		return bad()
	}
	targets := map[string]bool{}
	for _, t := range p.StorageTargets {
		if !safeServiceTarget(t) || targets[t] {
			return bad()
		}
		targets[t] = true
	}
	for id, s := range c.Services {
		if !serviceID.MatchString(id) || !digestReference.MatchString(s.Image) || !digestReference.MatchString(s.Adapter) || s.Config == nil || s.Env == nil || s.Secrets == nil || s.Storage == nil || len(s.Ports) == 0 || len(s.Ports) > 32 {
			return bad()
		}
		r, m := s.Resources, p.MaxResources
		for _, pair := range [][2]int64{{r.Memory, m.Memory}, {r.CPU, m.CPU}, {r.Pids, m.Pids}, {r.Data, m.Data}, {r.Startup, m.Startup}} {
			if pair[0] < 1 || pair[0] > pair[1] {
				return bad()
			}
		}
		if r.Memory < 256<<20 || r.CPU < 160 || r.Pids < 64 || r.Memory > 8<<30 || r.CPU > 4000 || r.Pids > 1024 || r.Data < 4096 || r.Data > 8<<30 || r.Startup > 300 {
			return bad()
		}
		sort.Ints(s.Ports)
		for i, port := range s.Ports {
			if port < 1 || port > 65535 || (i > 0 && port == s.Ports[i-1]) {
				return bad()
			}
		}
		seen := map[string]bool{}
		for _, t := range s.Storage {
			if !targets[t] || seen[t] {
				return bad()
			}
			for old := range seen {
				if strings.HasPrefix(t, old+"/") || strings.HasPrefix(old, t+"/") {
					return bad()
				}
			}
			seen[t] = true
		}
		for k, v := range s.Env {
			if !envKey.MatchString(k) || strings.ContainsRune(v, 0) || len(v) > 4096 {
				return bad()
			}
		}
		if len(s.Secrets) > 16 {
			return bad()
		}
		for k, v := range s.Secrets {
			if !serviceID.MatchString(k) || len(v) == 0 || len(v) > 2 {
				return bad()
			}
			for i, x := range v {
				if (x != "init" && x != "service") || (i > 0 && v[i-1] >= x) {
					return bad()
				}
			}
		}
		c.Services[id] = s
	}
	return nil
}
func safeServiceTarget(t string) bool {
	if !strings.HasPrefix(t, "/") || path.Clean(t) != t || t == "/" || strings.ContainsAny(t, "\x00,:") {
		return false
	}
	for _, p := range []string{"/proc", "/sys", "/dev", "/run", "/tmp", "/outputs", "/etc", "/bin", "/usr", "/lib", "/sbin"} {
		if t == p || strings.HasPrefix(t, p+"/") {
			return false
		}
	}
	return true
}
func verifyServiceImage(ctx context.Context, ref string) error {
	var v struct{ Architecture, Os string }
	if !digestReference.MatchString(ref) {
		return fmt.Errorf("service image must be immutable")
	}
	if e := dockerContext(ctx, "GET", "/images/"+url.PathEscape(ref)+"/json", nil, &v); e != nil {
		return fmt.Errorf("pre-pull service image")
	}
	if v.Architecture != "arm64" || v.Os != "linux" {
		return fmt.Errorf("service image must be Linux ARM64")
	}
	return nil
}

// Image-declared VOLUMEs would otherwise create anonymous, unbounded storage
// outside the explicit service data budget. Admit only covered data targets.
func verifyServiceVolumes(ctx context.Context, ref string, storage []string) error {
	var v struct {
		Config struct{ Volumes map[string]any }
	}
	if e := dockerContext(ctx, "GET", "/images/"+url.PathEscape(ref)+"/json", nil, &v); e != nil {
		return fmt.Errorf("service image configuration unavailable")
	}
	for target := range v.Config.Volumes {
		covered := false
		for _, allowed := range storage {
			if target == allowed {
				covered = true
			}
		}
		if !covered {
			return fmt.Errorf("image contains undeclared storage")
		}
	}
	return nil
}
