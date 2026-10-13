package main

import (
	"encoding/json"
	"net/http"
	"os"
	"testing"
)

func TestTeardownCatalogAndProofAreClosed(t *testing.T) {
	c, p := catalogFixture()
	baseline := serviceDigest(c)
	spec := c.Services["example"]
	spec.Teardown = &serviceTeardown{Checks: []string{"resource-absent", "credential-disabled"}, Admission: true}
	c.Services["example"] = spec
	if e := validateServices(&c, p); e != nil {
		t.Fatal(e)
	}
	if serviceDigest(c) == baseline {
		t.Fatal("fingerprint ignores teardown")
	}
	b, _ := json.Marshal(c)
	var decoded serviceCatalog
	if e := strictServiceDecode(b, &decoded); e != nil {
		t.Fatal(e)
	}
	for _, checks := range [][]string{nil, {"duplicated", "duplicated"}, {"../escape"}, {"password=value"}} {
		spec.Teardown = &serviceTeardown{Checks: checks, Admission: true}
		c.Services["example"] = spec
		if validateServices(&c, p) == nil {
			t.Fatal("accepted unsafe proof whitelist")
		}
	}
	for _, raw := range []string{`{"version":1,"checks":{"ok":"true"}}`, `{"version":1,"checks":{"ok":true},"password":"secret"}`, `{"version":1,"checks":{"ok":true,"ok":false}}`} {
		var proof serviceTerminalOutput
		if strictServiceDecode([]byte(raw), &proof) == nil {
			t.Fatal("accepted invalid proof")
		}
	}
}

func TestTeardownUnknownCompletionRetainsEvidenceAfterDeadline(t *testing.T) {
	f, n, _ := journalFixture(t)
	catalog, _ := catalogFixture()
	spec := catalog.Services["example"]
	spec.Teardown = &serviceTeardown{Checks: []string{"resource-absent"}, Admission: true}
	if e := f.work.root.MkdirAll(serviceDir(n, "example"), 0700); e != nil {
		t.Fatal(e)
	}
	b, _ := json.Marshal(spec)
	file, e := f.work.root.OpenFile(serviceDir(n, "example")+"/spec.json", os.O_CREATE|os.O_WRONLY, 0400)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = file.Write(b); e != nil {
		t.Fatal(e)
	}
	file.Close()
	if e = f.cleanupLog.update(n, func(v *cleanupObservation) error { v.StartedAt = "2000-01-01T00:00:00Z"; return nil }); e != nil {
		t.Fatal(e)
	}
	index := &serviceIndex{Owner: f.owner, Job: n, Lease: "fixture", Services: map[string]serviceIndexItem{"example": {Image: spec.Image, Adapter: spec.Adapter, ConfigDigest: serviceDigest(spec.Config)}}}
	withEngine(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1.47/containers/"+n+"/json" {
			w.WriteHeader(404)
		} else {
			w.WriteHeader(503)
		}
	})
	if e = f.collectServiceTeardown(index, "example"); e == nil {
		t.Fatal("unknown completed phase was treated as disposable")
	}
}
