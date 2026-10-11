package main

import (
	"context"
	"testing"
)

func TestGitCacheModeCompatibility(t *testing.T) {
	for _, mode := range []string{"", "auto", "off"} {
		f := &fleet{cache: &dependencyCache{}}
		if err := f.openGitObjectCache(context.Background(), mode, "", "https://github.com/example/project", ""); err != nil || f.gitCache != nil {
			t.Fatalf("legacy shared cache mode=%q: %v", mode, err)
		}
	}
	f := &fleet{cache: &dependencyCache{}}
	if err := f.openGitObjectCache(context.Background(), "on", "", "https://github.com/example/project", ""); err == nil {
		t.Fatal("explicit shared writable cache conflict accepted")
	}
	if err := f.openGitObjectCache(context.Background(), "unknown", "", "https://github.com/example/project", ""); err == nil {
		t.Fatal("unknown cache mode accepted")
	}
}
