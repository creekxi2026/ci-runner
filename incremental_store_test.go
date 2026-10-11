package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIncrementalLargeNativeDirectory(t *testing.T) {
	c := testCache(t)
	c.policy.Paths = []string{".cache/compiler"}
	c.policy.MaxEntries = 50000
	c.policy.MaxBytes = 1 << 30
	src, p := cacheTestRoot(t)
	if err := os.MkdirAll(filepath.Join(p, ".cache/compiler"), 0700); err != nil {
		t.Fatal(err)
	}
	// A native cache's small hashed entries must not consume a worst-case
	// Unicode escaping allowance for their ordinary ASCII paths.
	for i := range 12000 {
		name := fmt.Sprintf(".cache/compiler/%064x-d", i)
		if err := os.WriteFile(filepath.Join(p, name), []byte("native entry"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.save(context.Background(), src, strings.Repeat("a", 40)); err != nil {
		t.Fatal(err)
	}
	dst, _ := cacheTestRoot(t)
	if err := c.restore(context.Background(), dst); err != nil {
		t.Fatal(err)
	}
	for i := range 12000 {
		got, err := dst.ReadFile(fmt.Sprintf(".cache/compiler/%064x-d", i))
		if err != nil || string(got) != "native entry" {
			t.Fatalf("entry %d did not survive save/restore: %v", i, err)
		}
	}
}

func TestIncrementalMetadataStillBoundsEscapedNames(t *testing.T) {
	for _, part := range []string{strings.Repeat("a", 100), strings.Repeat("\x01", 100), strings.Repeat("<", 100), strings.Repeat("\xff", 100)} {
		b := cacheBudget{}
		files := map[string]string{}
		for i := 0; ; i++ {
			name := fmt.Sprintf(".cache/compiler/%s-%d", part, i)
			if err := b.addMetadata(name); err != nil {
				if !strings.Contains(err.Error(), "cache metadata limit") {
					t.Fatal(err)
				}
				break
			}
			files[name] = strings.Repeat("a", 64)
		}
		encoded, err := json.Marshal(cacheManifest{Key: strings.Repeat("b", 64), Source: strings.Repeat("c", 40), Files: files})
		if err != nil || len(encoded) > 4<<20 {
			t.Fatalf("accepted metadata exceeds encoded budget: bytes=%d err=%v", len(encoded), err)
		}
	}
	b := cacheBudget{}
	if err := b.addMetadata(strings.Repeat("x", 1025)); err == nil {
		t.Fatal("accepted overlong path")
	}
}

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
