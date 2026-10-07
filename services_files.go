package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
	"unicode/utf8"
)

const serviceMount = "/run/ci-services"

type serviceIndex struct {
	Version     int                         `json:"version"`
	Lease       string                      `json:"lease_id"`
	Owner       string                      `json:"owner_id"`
	Job         string                      `json:"job_id"`
	Fingerprint string                      `json:"fingerprint"`
	Services    map[string]serviceIndexItem `json:"services"`
}
type serviceIndexItem struct {
	Image        string `json:"image"`
	Adapter      string `json:"adapter_image"`
	ConfigDigest string `json:"config_digest"`
	Ready        string `json:"ready_file"`
}
type serviceEndpoint struct {
	Host  string `json:"host"`
	Ports []int  `json:"ports"`
}
type serviceRecord struct {
	Phase    string          `json:"phase"`
	Deadline time.Time       `json:"deadline"`
	Endpoint serviceEndpoint `json:"endpoint"`
}

func rootOwned(st os.FileInfo) bool         { s, ok := st.Sys().(*syscall.Stat_t); return ok && s.Uid == 0 }
func serviceBase(n string) string           { return "jobs/" + n + "/service-private" }
func servicePublic(n string) string         { return "jobs/" + n + "/service-public" }
func serviceDir(n, id string) string        { return serviceBase(n) + "/" + id }
func serviceName(n, id, role string) string { return n + "-svc-" + id + "-" + role }
func (f *fleet) serviceMkdir(p string, mode os.FileMode) error {
	if e := f.work.root.Mkdir(p, mode); e != nil && !errors.Is(e, os.ErrExist) {
		return e
	}
	st, e := f.work.root.Lstat(p)
	if e != nil || !st.IsDir() || !rootOwned(st) || st.Mode().Perm() != mode {
		return fmt.Errorf("unsafe service directory")
	}
	return nil
}
func (f *fleet) serviceWrite(p string, b []byte, uid int, mode os.FileMode) error {
	// Private parent is controller-only. The temporary file is never visible in
	// the worker publication tree before its final owner/mode are installed.
	tmp := serviceBaseFromPath(p) + "/publish-" + serviceDigest(p) + ".new"
	out, e := f.work.root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if errors.Is(e, os.ErrExist) {
		if e = f.work.root.Remove(tmp); e != nil {
			return e
		}
		out, e = f.work.root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	}
	if e != nil {
		return e
	}
	defer f.work.root.Remove(tmp)
	if _, e = out.Write(b); e == nil {
		e = out.Chown(uid, uid)
	}
	if e == nil {
		e = out.Chmod(mode)
	}
	if e == nil {
		e = out.Sync()
	}
	ce := out.Close()
	if e != nil {
		return e
	}
	if ce != nil {
		return ce
	}
	if e = f.work.root.Rename(tmp, p); e != nil {
		return e
	}
	d, e := f.work.root.Open(filepath.Dir(p))
	if e != nil {
		return e
	}
	defer d.Close()
	return d.Sync()
}
func serviceBaseFromPath(p string) string {
	// Every call constructs a path below a valid jobs/<job> directory.
	for i := len("jobs/"); i < len(p); i++ {
		if p[i] == '/' {
			return p[:i] + "/service-private"
		}
	}
	panic("invalid service publication path")
}
func (f *fleet) serviceWriteJSON(p string, v any, mode os.FileMode) error {
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	return f.serviceWrite(p, b, 0, mode)
}
func (f *fleet) serviceRead(p string, max int64) ([]byte, error) {
	in, e := f.work.root.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if e != nil {
		return nil, e
	}
	defer in.Close()
	st, e := in.Stat()
	if e != nil || !st.Mode().IsRegular() || st.Size() < 1 || st.Size() > max {
		return nil, fmt.Errorf("invalid service file")
	}
	s, ok := st.Sys().(*syscall.Stat_t)
	if !ok || s.Nlink != 1 {
		return nil, fmt.Errorf("unsafe service file")
	}
	b, e := io.ReadAll(io.LimitReader(in, max+1))
	if int64(len(b)) > max {
		return nil, fmt.Errorf("oversized service file")
	}
	return b, e
}
func (f *fleet) prepareServices(n string, h obj) (*serviceIndex, error) {
	if f.services == nil {
		return nil, nil
	}
	if f.work == nil {
		return nil, fmt.Errorf("generic services require private shared-work subpaths")
	}
	if e := f.serviceMkdir(serviceBase(n), 0700); e != nil {
		return nil, e
	}
	for _, p := range []string{servicePublic(n), servicePublic(n) + "/services"} {
		if e := f.serviceMkdir(p, 0755); e != nil {
			return nil, e
		}
	}
	index := &serviceIndex{Version: 1, Owner: f.owner, Job: n, Fingerprint: f.services.Fingerprint, Services: map[string]serviceIndexItem{}}
	var random [32]byte
	if _, e := rand.Read(random[:]); e != nil {
		return nil, e
	}
	index.Lease = hex.EncodeToString(random[:])
	for id, s := range f.services.Catalog.Services {
		index.Services[id] = serviceIndexItem{s.Image, s.Adapter, serviceDigest(s.Config), serviceMount + "/services/" + id + "/ready.json"}
		for _, p := range []string{servicePublic(n) + "/services/" + id, servicePublic(n) + "/services/" + id + "/outputs"} {
			if e := f.serviceMkdir(p, 0755); e != nil {
				return nil, e
			}
		}
		if e := f.serviceMkdir(serviceDir(n, id), 0700); e != nil {
			return nil, e
		}
		for _, p := range []string{"secrets-init", "secrets-service", "init", "check", "outputs"} {
			if e := f.serviceMkdir(serviceDir(n, id)+"/"+p, 0700); e != nil {
				return nil, e
			}
		}
		for secret, recipients := range s.Secrets {
			if _, e := rand.Read(random[:]); e != nil {
				return nil, e
			}
			value := []byte(hex.EncodeToString(random[:]) + "\n")
			for _, recipient := range recipients {
				if e := f.serviceWrite(serviceDir(n, id)+"/secrets-"+recipient+"/"+secret, value, 0, 0400); e != nil {
					return nil, e
				}
			}
		}
	}
	// Persist identity and secrets before Docker can provision anything.
	if e := f.serviceWriteJSON(serviceBase(n)+"/index.json", index, 0400); e != nil {
		return nil, e
	}
	if e := f.serviceWriteJSON(servicePublic(n)+"/index.json", index, 0444); e != nil {
		return nil, e
	}
	mounts, _ := h["Mounts"].([]obj)
	mounts = append(mounts, f.serviceVolumeMount(servicePublic(n), serviceMount, true))
	h["Mounts"] = mounts
	return index, nil
}
func (f *fleet) serviceVolumeMount(sub, target string, ro bool) obj {
	return obj{"Type": "volume", "Source": f.work.volume, "Target": target, "ReadOnly": ro, "VolumeOptions": obj{"NoCopy": true, "Subpath": sub}}
}
func (f *fleet) readServiceIndex(n string) (*serviceIndex, error) {
	if f.services == nil || f.work == nil {
		return nil, fmt.Errorf("service configuration unavailable")
	}
	b, e := f.serviceRead(serviceBase(n)+"/index.json", 65536)
	if e != nil {
		return nil, e
	}
	var index serviceIndex
	if e = strictServiceDecode(b, &index); e != nil {
		return nil, e
	}
	if index.Version != 1 || index.Owner != f.owner || index.Job != n || !serviceHex.MatchString(index.Lease) || index.Fingerprint != f.services.Fingerprint || len(index.Services) != len(f.services.Catalog.Services) {
		return nil, fmt.Errorf("service lease mismatch")
	}
	for id, s := range f.services.Catalog.Services {
		if index.Services[id] != (serviceIndexItem{s.Image, s.Adapter, serviceDigest(s.Config), serviceMount + "/services/" + id + "/ready.json"}) {
			return nil, fmt.Errorf("service declaration mismatch")
		}
	}
	public, e := f.serviceRead(servicePublic(n)+"/index.json", 65536)
	if e != nil || string(public) != string(b) {
		return nil, fmt.Errorf("service publication mismatch")
	}
	return &index, nil
}
func (f *fleet) readServiceRecord(n, id string) (serviceRecord, error) {
	var r serviceRecord
	b, e := f.serviceRead(serviceDir(n, id)+"/state.json", 65536)
	if e != nil {
		return r, e
	}
	e = strictServiceDecode(b, &r)
	if e == nil && (r.Deadline.IsZero() || (r.Phase != "starting" && r.Phase != "ready" && r.Phase != "failed")) {
		e = fmt.Errorf("invalid service state")
	}
	return r, e
}
func (f *fleet) saveServiceRecord(n, id string, r serviceRecord) error {
	return f.serviceWriteJSON(serviceDir(n, id)+"/state.json", r, 0600)
}
func (f *fleet) publishService(index *serviceIndex, id string, endpoint serviceEndpoint) error {
	n := index.Job
	d, e := f.work.root.Open(serviceDir(n, id) + "/outputs")
	if e != nil {
		return e
	}
	entries, e := d.ReadDir(-1)
	d.Close()
	if e != nil || len(entries) != 1 || entries[0].Name() != "consumer.json" {
		return fmt.Errorf("invalid adapter output set")
	}
	b, e := f.serviceRead(serviceDir(n, id)+"/outputs/consumer.json", 65536)
	if e != nil || !utf8.Valid(b) || !json.Valid(b) {
		return fmt.Errorf("invalid adapter output")
	}
	if e = f.serviceWrite(servicePublic(n)+"/services/"+id+"/outputs/consumer.json", b, 1001, 0400); e != nil {
		return e
	}
	s := index.Services[id]
	ready := obj{"version": 1, "state": "ready", "lease_id": index.Lease, "owner_id": index.Owner, "job_id": n, "service_id": id, "fingerprint": index.Fingerprint, "image": s.Image, "adapter_image": s.Adapter, "config_digest": s.ConfigDigest, "endpoint": endpoint, "outputs": obj{"consumer": serviceMount + "/services/" + id + "/outputs/consumer.json"}}
	return f.serviceWriteJSON(servicePublic(n)+"/services/"+id+"/ready.json", ready, 0444)
}
