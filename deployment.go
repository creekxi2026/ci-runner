package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"regexp"
)

// Set by the cloud build; prevents a mismatched controller/runner pair.
var imageRevision string
var digestReference = regexp.MustCompile(`^[a-zA-Z0-9_./:-]+@sha256:[a-f0-9]{64}$`)
var sourceRevision = regexp.MustCompile(`^[a-f0-9]{40}$`)

func deploymentImages(ctx context.Context) (string, string, error) {
	revision := os.Getenv("IMAGE_REVISION")
	if !sourceRevision.MatchString(revision) || revision != imageRevision {
		return "", "", fmt.Errorf("IMAGE_REVISION must match this cloud-built controller")
	}
	runner, controller := os.Getenv("RUNNER_IMAGE"), os.Getenv("CONTROLLER_IMAGE")
	for _, ref := range []string{runner, controller} {
		if !digestReference.MatchString(ref) {
			return "", "", fmt.Errorf("workload images must be digest pinned")
		}
		var image struct {
			Architecture, Os string
			Config           struct{ Labels map[string]string }
		}
		if err := dockerContext(ctx, "GET", "/images/"+url.PathEscape(ref)+"/json", nil, &image); err != nil {
			return "", "", fmt.Errorf("pre-pull workload image: %w", err)
		}
		if image.Architecture != "arm64" || image.Os != "linux" {
			return "", "", fmt.Errorf("workload image is not Linux ARM64")
		}
		if image.Config.Labels["org.opencontainers.image.revision"] != revision {
			return "", "", fmt.Errorf("image revision does not match controller")
		}
	}
	return runner, controller, nil
}

func verifyControllerImage(ctx context.Context, ref string) error {
	name := os.Getenv("CONTROLLER_CONTAINER")
	if name == "" {
		return fmt.Errorf("CONTROLLER_CONTAINER required")
	}
	var actual struct{ Image string }
	var selected struct{ ID string }
	if e := dockerContext(ctx, "GET", "/containers/"+url.PathEscape(name)+"/json", nil, &actual); e != nil {
		return e
	}
	if e := dockerContext(ctx, "GET", "/images/"+url.PathEscape(ref)+"/json", nil, &selected); e != nil {
		return e
	}
	if selected.ID == "" || actual.Image != selected.ID {
		return fmt.Errorf("controller fingerprint image does not match running image")
	}
	return nil
}
