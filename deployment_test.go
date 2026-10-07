package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestDeploymentRejectsMutableOrMismatchedImages(t *testing.T) {
	source := strings.Repeat("c", 40)
	t.Setenv("IMAGE_REVISION", source)
	t.Setenv("RUNNER_IMAGE", "ghcr.io/creekxi2026/ci-runner@sha256:"+strings.Repeat("a", 64))
	t.Setenv("CONTROLLER_IMAGE", "ghcr.io/creekxi2026/ci-runner-controller@sha256:"+strings.Repeat("b", 64))
	old := imageRevision
	imageRevision = source
	t.Cleanup(func() { imageRevision = old })
	withEngine(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(obj{"Architecture": "arm64", "Os": "linux", "Config": obj{"Labels": obj{"org.opencontainers.image.revision": strings.Repeat("d", 40)}}})
	})
	if _, _, err := deploymentImages(context.Background()); err == nil {
		t.Fatal("accepted a runner from a different revision")
	}
	t.Setenv("RUNNER_IMAGE", "ghcr.io/creekxi2026/ci-runner:runner")
	if _, _, err := deploymentImages(context.Background()); err == nil {
		t.Fatal("accepted floating image")
	}
	t.Setenv("IMAGE_REVISION", strings.Repeat("d", 40))
	if _, _, err := deploymentImages(context.Background()); err == nil {
		t.Fatal("accepted controller revision mismatch")
	}
}

func TestDeploymentAcceptsMatchingArm64Images(t *testing.T) {
	source := strings.Repeat("c", 40)
	old := imageRevision
	imageRevision = source
	t.Cleanup(func() { imageRevision = old })
	t.Setenv("IMAGE_REVISION", source)
	t.Setenv("RUNNER_IMAGE", "ghcr.io/creekxi2026/ci-runner@sha256:"+strings.Repeat("a", 64))
	t.Setenv("CONTROLLER_IMAGE", "ghcr.io/creekxi2026/ci-runner-controller@sha256:"+strings.Repeat("b", 64))
	calls := 0
	withEngine(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		json.NewEncoder(w).Encode(obj{"Architecture": "arm64", "Os": "linux", "Config": obj{"Labels": obj{"org.opencontainers.image.revision": source}}})
	})
	if _, _, err := deploymentImages(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatal("did not inspect both workload images")
	}
}
