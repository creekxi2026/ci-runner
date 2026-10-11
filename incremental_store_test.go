package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testCache(t *testing.T) *incrementalCache {
	root, _ := cacheTestRoot(t)
	return &incrementalCache{root: root, policy: cacheFixturePolicy(), key: strings.Repeat("c", 64), ownerUID: os.Getuid(), jobUID: os.Getuid()}
}
func TestIncrementalSnapshotsIsolationRetentionAndCompatibility(t *testing.T) {
	c := testCache(t)
	src, p := cacheTestRoot(t)
	writeCacheFixture(t, p)
	ctx := context.Background()
	sha := strings.Repeat("a", 40)
	if e := c.save(ctx, src, sha); e != nil {
		t.Fatal(e)
	}
	one, p1 := cacheTestRoot(t)
	two, p2 := cacheTestRoot(t)
	if e := c.restore(ctx, one); e != nil {
		t.Fatal(e)
	}
	if e := c.restore(ctx, two); e != nil {
		t.Fatal(e)
	}
	entry := filepath.Join(".cache", "compiler", "entry")
	if e := os.WriteFile(filepath.Join(p1, entry), []byte("poison"), 0600); e != nil {
		t.Fatal(e)
	}
	for _, root := range []string{p, p2} {
		b, e := os.ReadFile(filepath.Join(root, entry))
		if e != nil || string(b) != "cached transform" {
			t.Fatal("private copy changed another task", e)
		}
	}
	for range 3 {
		if e := c.save(ctx, src, sha); e != nil {
			t.Fatal(e)
		}
	}
	d, e := c.root.Open(".")
	if e != nil {
		t.Fatal(e)
	}
	defer d.Close()
	entries, e := d.ReadDir(-1)
	if e != nil {
		t.Fatal(e)
	}
	snapshots := 0
	for _, entry := range entries {
		if snapshotName(entry.Name()) {
			snapshots++
		}
	}
	if snapshots != 2 {
		t.Fatalf("unbounded snapshots: %d", snapshots)
	}
	c.key = strings.Repeat("d", 64)
	if e := c.restore(ctx, one); e == nil {
		t.Fatal("restored incompatible cache")
	}
	b, _ := os.ReadFile(filepath.Join(p1, entry))
	if string(b) != "poison" {
		t.Fatal("cache miss modified existing private seed")
	}
}
func TestIncrementalUnsafeInputNeverReplacesCurrentSnapshot(t *testing.T) {
	for _, kind := range []string{"symlink-file", "symlink-parent", "hardlink", "oversize", "many-files", "cancelled", "corrupt"} {
		t.Run(kind, func(t *testing.T) {
			c := testCache(t)
			src, p := cacheTestRoot(t)
			writeCacheFixture(t, p)
			ctx := context.Background()
			sha := strings.Repeat("a", 40)
			if e := c.save(ctx, src, sha); e != nil {
				t.Fatal(e)
			}
			before, _ := c.index()
			entry := filepath.Join(p, ".cache/compiler/entry")
			switch kind {
			case "symlink-file":
				os.Remove(entry)
				os.Symlink("../../secret", entry)
			case "symlink-parent":
				os.Rename(filepath.Join(p, ".cache/compiler"), filepath.Join(p, "moved"))
				os.Symlink("../moved", filepath.Join(p, ".cache/compiler"))
			case "hardlink":
				os.Link(entry, filepath.Join(p, ".cache/compiler/hard"))
			case "oversize":
				os.WriteFile(entry, make([]byte, c.policy.MaxBytes+1), 0600)
			case "many-files":
				c.policy.MaxEntries = 1
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "corrupt":
				f, e := c.root.OpenFile(before.Current+"/.cache/compiler/entry", os.O_WRONLY|os.O_TRUNC, 0)
				if e != nil {
					t.Fatal(e)
				}
				f.WriteString("corrupted")
				f.Close()
				home, h := cacheTestRoot(t)
				writeCacheFixture(t, h)
				if e = c.restore(ctx, home); e == nil {
					t.Fatal("corrupted cache restored")
				}
				b, _ := os.ReadFile(filepath.Join(h, ".cache/compiler/entry"))
				if string(b) != "cached transform" {
					t.Fatal("corrupt cache modified seed")
				}
				return
			}
			if e := c.save(ctx, src, sha); e == nil {
				t.Fatal("unsafe input published")
			}
			after, _ := c.index()
			if after != before {
				t.Fatal("failed save replaced last good snapshot")
			}
		})
	}
}
func TestIncrementalInterruptedPublicationCollected(t *testing.T) {
	c := testCache(t)
	src, p := cacheTestRoot(t)
	writeCacheFixture(t, p)
	orphan := "12345678-1234-1234-1234-123456789abc"
	if e := c.root.Mkdir(orphan, 0700); e != nil {
		t.Fatal(e)
	}
	if e := c.save(context.Background(), src, strings.Repeat("a", 40)); e != nil {
		t.Fatal(e)
	}
	if _, e := c.root.Stat(orphan); !os.IsNotExist(e) {
		t.Fatal("orphan snapshot retained", e)
	}
}

func TestIncrementalBusyAndCancelledNeverBlock(t *testing.T) {
	c := testCache(t)
	home, _ := cacheTestRoot(t)
	c.mu.Lock()
	if e := c.save(context.Background(), home, strings.Repeat("a", 40)); e == nil {
		t.Fatal("concurrent save waited or acquired busy lock")
	}
	if e := c.restore(context.Background(), home); e == nil {
		t.Fatal("concurrent restore waited or acquired busy lock")
	}
	c.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if e := c.save(ctx, home, strings.Repeat("a", 40)); e != context.Canceled {
		t.Fatal(e)
	}
}
