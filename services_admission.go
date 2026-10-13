package main

import (
	"fmt"
	"io"
	"os"
	"syscall"
)

// Only a trusted host issuer writes this store. Neither candidates nor service
// adapters receive its mount. The project owns payload meaning and provenance.
type serviceAdmission struct {
	Version     int            `json:"version"`
	Lease       string         `json:"lease_id"`
	Owner       string         `json:"owner_id"`
	Job         string         `json:"job_id"`
	Service     string         `json:"service_id"`
	Fingerprint string         `json:"fingerprint"`
	Payload     map[string]any `json:"payload"`
}

func (f *fleet) admitService(index *serviceIndex, id string) error {
	r := f.work.root
	for _, p := range []string{"admissions-v1", "admissions-v1/" + index.Owner, "admissions-v1/" + index.Owner + "/" + index.Job} {
		st, e := r.Lstat(p)
		if e != nil {
			return e
		}
		if !protectedCleanup(st, 0, 0700, true) {
			return fmt.Errorf("unsafe service admission directory")
		}
	}
	in, e := r.OpenFile("admissions-v1/"+index.Owner+"/"+index.Job+"/"+id+".json", os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if e != nil {
		return e
	}
	defer in.Close()
	st, e := in.Stat()
	if e != nil || !protectedCleanup(st, 0, 0444, false) || st.Size() < 1 || st.Size() > 65536 {
		return fmt.Errorf("unsafe service admission")
	}
	b, e := io.ReadAll(io.LimitReader(in, 65537))
	if e != nil || len(b) > 65536 {
		return fmt.Errorf("invalid service admission length")
	}
	var a serviceAdmission
	if strictServiceDecode(b, &a) != nil || a.Version != 1 || a.Lease != index.Lease || a.Owner != index.Owner || a.Job != index.Job || a.Service != id || a.Fingerprint != index.Fingerprint || a.Payload == nil {
		return fmt.Errorf("foreign service admission")
	}
	digest := serviceDigest(a)
	return f.cleanupLog.update(index.Job, func(v *cleanupObservation) error {
		if v.StartedAt != "" {
			return fmt.Errorf("service admission after cleanup")
		}
		if old := v.Admissions[id]; old != "" && old != digest {
			return fmt.Errorf("service admission changed")
		}
		if v.Admissions == nil {
			v.Admissions = map[string]string{}
		}
		v.Admissions[id] = digest
		return nil
	})
}
