package main

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func TestTeardownTransientServerInspectRetainsProofSource(t *testing.T) {
	f, n, _ := journalFixture(t)
	catalog, _ := catalogFixture()
	spec := catalog.Services["example"]
	spec.Teardown = &serviceTeardown{Checks: []string{"resource-absent"}, Admission: true}
	spec.Resources.Startup = 120
	index := &serviceIndex{Version: 1, Owner: f.owner, Job: n, Lease: strings.Repeat("a", 64), Fingerprint: strings.Repeat("b", 64), Services: map[string]serviceIndexItem{"example": {Image: spec.Image, Adapter: spec.Adapter, ConfigDigest: serviceDigest(spec.Config)}}}
	if e := f.work.root.MkdirAll(serviceDir(n, "example"), 0700); e != nil {
		t.Fatal(e)
	}
	write := func(path string, value any) {
		b, e := json.Marshal(value)
		if e != nil {
			t.Fatal(e)
		}
		out, e := f.work.root.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0400)
		if e != nil {
			t.Fatal(e)
		}
		_, e = out.Write(b)
		out.Close()
		if e != nil {
			t.Fatal(e)
		}
	}
	write(serviceDir(n, "example")+"/spec.json", spec)
	write(serviceBase(n)+"/index.json", index)
	write(serviceDir(n, "example")+"/state.json", serviceRecord{Phase: "ready", Deadline: time.Now().Add(time.Minute), Endpoint: serviceEndpoint{Host: "172.20.0.2", Ports: []int{8765}}})
	if e := f.cleanupLog.update(n, func(v *cleanupObservation) error {
		v.StartedAt = time.Now().UTC().Format(time.RFC3339Nano)
		v.Services = index
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	inspections, deletes := 0, 0
	unavailable := true
	withEngine(t, func(w http.ResponseWriter, r *http.Request) {
		server := "/v1.47/containers/" + serviceName(n, "example", "server")
		if r.Method == "GET" && r.URL.Path == server+"/json" {
			inspections++
			if unavailable {
				w.WriteHeader(503)
				return
			}
			json.NewEncoder(w).Encode(obj{"Config": obj{"Image": spec.Image, "Labels": f.serviceLabels(index, "example", "server")}, "State": obj{"Running": true}})
			return
		}
		if r.Method == "DELETE" && r.URL.Path == server {
			deletes++
			w.WriteHeader(204)
			return
		}
		w.WriteHeader(404)
	})
	if e := f.removeServices(n); e == nil {
		t.Fatal("unknown server observation allowed cleanup")
	}
	v, e := f.cleanupLog.read(n)
	if e != nil {
		t.Fatal(e)
	}
	if inspections != 1 || deletes != 0 || len(v.Proofs) != 0 {
		t.Fatalf("unexpected result inspections=%d deletes=%d proofs=%d", inspections, deletes, len(v.Proofs))
	}
	if _, e := f.work.root.Lstat(serviceDir(n, "example") + "/state.json"); e != nil {
		t.Fatal("retry state lost", e)
	}
	// Preserve the original deadline across a controller restart.
	restarted, e := openCleanupJournal(f.work, uint32(os.Getuid()))
	if e != nil {
		t.Fatal(e)
	}
	defer restarted.root.Close()
	f.cleanupLog = restarted
	after, e := restarted.read(n)
	if e != nil || after.StartedAt != v.StartedAt {
		t.Fatal("restart changed retry bound", e)
	}
	if e = f.removeServices(n); e == nil || inspections != 2 || deletes != 0 {
		t.Fatal("persistent unknown observation discarded the source")
	}
	// Once the original execution bound expires, an observable owned service
	// may be reclaimed without proof. Never extend the deadline or invent proof.
	if e = restarted.update(n, func(v *cleanupObservation) error {
		v.StartedAt = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	unavailable = false
	if e = f.removeServices(n); e != nil || deletes != 1 {
		t.Fatal("expired owned resource was not reclaimed", e)
	}
	after, e = restarted.read(n)
	if e != nil || len(after.Proofs) != 0 {
		t.Fatal("expired cleanup fabricated proof", e)
	}
	t.Log("transient/unknown inspect retains the source; restart preserves the deadline; expiry never fabricates proof")
}
