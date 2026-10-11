package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"strings"
	"syscall"

	"github.com/google/uuid"
)

type cacheIndex struct{ Current, Previous string }
type cacheManifest struct {
	Key, Source string
	Files       map[string]string
}
type cacheBudget struct {
	bytes    int64
	entries  int
	metadata int
}

func (b *cacheBudget) addMetadata(name string) error {
	if len(name) > 1024 {
		return fmt.Errorf("cache path length limit")
	}
	// Count the actual JSON-escaped key, not six bytes for every ASCII byte.
	// Native caches commonly have thousands of long hexadecimal filenames.
	// The allowance covers the digest and JSON punctuation; directories count
	// too, conservatively bounding traversal even though they aren't in Files.
	encoded, err := json.Marshal(name)
	if err != nil {
		return err
	}
	b.metadata += len(encoded) + 80
	if b.metadata > 4<<20 {
		return fmt.Errorf("cache metadata limit (entries=%d, bytes=%d, max=%d)", b.entries, b.metadata, 4<<20)
	}
	return nil
}

func cacheGID(uid int) int {
	if uid == os.Getuid() {
		return os.Getgid()
	}
	return uid
}

func privateCacheDir(st os.FileInfo) bool {
	s, ok := st.Sys().(*syscall.Stat_t)
	return ok && st.IsDir() && st.Mode().Perm()&0077 == 0 && s.Uid == 0
}
func cacheDir(root *os.Root, rel string, create bool) error {
	for i, part := range strings.Split(rel, "/") {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("unsafe cache directory")
		}
		prefix := strings.Join(strings.Split(rel, "/")[:i+1], "/")
		if create {
			if e := root.Mkdir(prefix, 0700); e != nil && !os.IsExist(e) {
				return e
			}
		}
		st, e := root.Lstat(prefix)
		if e != nil {
			return e
		}
		if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("cache directory is a link or non-directory")
		}
	}
	return nil
}
func cacheWalk(ctx context.Context, src, dst *os.Root, rel string, b *cacheBudget, p incrementalPolicy, files map[string]string, uid int, depth int) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	if depth > 32 {
		return fmt.Errorf("cache directory depth limit")
	}
	if e := cacheDir(src, rel, false); e != nil {
		return e
	}
	if e := cacheDir(dst, rel, true); e != nil {
		return e
	}
	if e := dst.Chown(rel, uid, cacheGID(uid)); e != nil {
		return e
	}
	dir, e := src.Open(rel)
	if e != nil {
		return e
	}
	defer dir.Close()
	for {
		entries, e := dir.ReadDir(128)
		if e != nil && e != io.EOF {
			return e
		}
		for _, entry := range entries {
			if e := ctx.Err(); e != nil {
				return e
			}
			b.entries++
			if b.entries > p.MaxEntries {
				return fmt.Errorf("cache entry limit")
			}
			name := rel + "/" + entry.Name()
			if e := b.addMetadata(name); e != nil {
				return e
			}
			st, e := src.Lstat(name)
			if e != nil {
				return e
			}
			if st.IsDir() {
				if e = cacheWalk(ctx, src, dst, name, b, p, files, uid, depth+1); e != nil {
					return e
				}
				continue
			}
			sys, ok := st.Sys().(*syscall.Stat_t)
			if !st.Mode().IsRegular() || !ok || sys.Nlink != 1 || st.Size() > 256<<20 {
				return fmt.Errorf("unsupported cache file")
			}
			b.bytes += st.Size()
			if b.bytes > p.MaxBytes {
				return fmt.Errorf("cache byte limit")
			}
			in, e := src.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
			if e != nil {
				return e
			}
			actual, e := in.Stat()
			if e != nil || !os.SameFile(st, actual) {
				in.Close()
				return fmt.Errorf("cache source changed")
			}
			out, e := dst.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if e != nil {
				in.Close()
				return e
			}
			h := sha256.New()
			// Bounded copying also rejects changes in the source's size. Workers have
			// already been deleted and no shared writable mounts can reach this tree.
			cloned, copyErr := cloneCacheFile(in, out)
			var count int64
			if copyErr == nil {
				var target io.Writer = out
				if cloned {
					target = io.Discard
				}
				count, copyErr = io.Copy(target, io.TeeReader(io.LimitReader(in, st.Size()+1), h))
			}
			if copyErr == nil && count != st.Size() {
				copyErr = fmt.Errorf("cache size changed")
			}
			if copyErr == nil {
				copyErr = out.Chown(uid, cacheGID(uid))
			}
			in.Close()
			closeErr := out.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
			files[name] = hex.EncodeToString(h.Sum(nil))
		}
		if e == io.EOF {
			break
		}
	}
	// Cache payloads are disposable: avoid fsync per native cache entry. A host
	// crash may lose payloads; restore checks the manifest and rebuilds cold.
	return nil
}
func readCacheJSON(root *os.Root, name string, out any) error {
	st, e := root.Lstat(name)
	if e != nil {
		return e
	}
	if !st.Mode().IsRegular() || st.Size() > 8<<20 {
		return fmt.Errorf("invalid cache metadata")
	}
	f, e := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if e != nil {
		return e
	}
	defer f.Close()
	return json.NewDecoder(io.LimitReader(f, 8<<20)).Decode(out)
}
func writeCacheJSON(root *os.Root, name string, data any) error {
	f, e := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	e = json.NewEncoder(f).Encode(data)
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e != nil {
		return e
	}
	return ce
}
func syncCacheRoot(root *os.Root) error {
	f, e := root.Open(".")
	if e != nil {
		return e
	}
	defer f.Close()
	return f.Sync()
}
func snapshotName(s string) bool { _, e := uuid.Parse(s); return e == nil && len(s) == 36 }
func (c *incrementalCache) index() (cacheIndex, error) {
	var idx cacheIndex
	e := readCacheJSON(c.root, "index.json", &idx)
	if os.IsNotExist(e) {
		return idx, nil
	}
	if e == nil && (!snapshotName(idx.Current) || (idx.Previous != "" && !snapshotName(idx.Previous))) {
		e = fmt.Errorf("invalid cache index")
	}
	return idx, e
}

// root is not exposed to workers; each owner has an exclusive controller lease.
// At most two committed snapshots plus one bounded staging snapshot exist.
func (c *incrementalCache) save(ctx context.Context, home *os.Root, source string) error {
	if !c.mu.TryLock() {
		return fmt.Errorf("cache busy; skip this attempt")
	}
	defer c.mu.Unlock()
	if e := ctx.Err(); e != nil {
		return e
	}
	idx, e := c.index()
	if e != nil {
		return e
	}
	if e = c.collect(idx); e != nil {
		return e
	}
	id := uuid.NewString()
	if e = c.root.Mkdir(id, 0700); e != nil {
		return e
	}
	committed := false
	defer func() {
		if !committed {
			_ = c.root.RemoveAll(id)
		}
	}()
	dst, e := c.root.OpenRoot(id)
	if e != nil {
		return e
	}
	defer dst.Close()
	manifest := cacheManifest{Key: c.key, Source: source, Files: map[string]string{}}
	budget := cacheBudget{}
	for _, rel := range c.policy.Paths {
		if e = cacheWalk(ctx, home, dst, rel, &budget, c.policy, manifest.Files, c.ownerUID, 0); e != nil {
			return e
		}
	}
	if len(manifest.Files) == 0 {
		return fmt.Errorf("empty incremental cache")
	}
	encoded, e := json.Marshal(manifest)
	if e != nil {
		return e
	}
	if len(encoded) > 8<<20 {
		return fmt.Errorf("cache manifest limit")
	}
	if e = writeCacheJSON(dst, "manifest.json", manifest); e != nil {
		return e
	}
	if e = syncCacheRoot(dst); e != nil {
		return e
	}
	_ = c.root.Remove("index.new")
	next := cacheIndex{Current: id, Previous: idx.Current}
	if e = writeCacheJSON(c.root, "index.new", next); e != nil {
		return e
	}
	if e = c.root.Rename("index.new", "index.json"); e != nil {
		return e
	}
	committed = true
	if e = syncCacheRoot(c.root); e != nil {
		return e
	}
	return c.collect(next)
}
func (c *incrementalCache) collect(idx cacheIndex) error {
	d, e := c.root.Open(".")
	if e != nil {
		return e
	}
	defer d.Close()
	entries, e := d.ReadDir(-1)
	if e != nil {
		return e
	}
	for _, entry := range entries {
		n := entry.Name()
		if snapshotName(n) && n != idx.Current && n != idx.Previous {
			if e = c.root.RemoveAll(n); e != nil {
				return e
			}
		}
	}
	return nil
}
func (c *incrementalCache) restore(ctx context.Context, home *os.Root) error {
	if !c.mu.TryLock() {
		return fmt.Errorf("cache busy; skip this attempt")
	}
	defer c.mu.Unlock()
	if e := ctx.Err(); e != nil {
		return e
	}
	idx, e := c.index()
	if e != nil {
		return e
	}
	if idx.Current == "" {
		return fmt.Errorf("no incremental snapshot yet")
	}
	if e = cacheDir(c.root, idx.Current, false); e != nil {
		return e
	}
	src, e := c.root.OpenRoot(idx.Current)
	if e != nil {
		return e
	}
	defer src.Close()
	var manifest cacheManifest
	if e = readCacheJSON(src, "manifest.json", &manifest); e != nil {
		return e
	}
	if manifest.Key != c.key || !cacheSHA.MatchString(manifest.Source) {
		return fmt.Errorf("incompatible incremental snapshot")
	}
	// Copy and validate all directories before replacing any seed directory.
	staging := ".incremental-" + uuid.NewString()
	if e = home.Mkdir(staging, 0700); e != nil {
		return e
	}
	defer home.RemoveAll(staging)
	dst, e := home.OpenRoot(staging)
	if e != nil {
		return e
	}
	defer dst.Close()
	files := map[string]string{}
	budget := cacheBudget{}
	for _, rel := range c.policy.Paths {
		if e = cacheWalk(ctx, src, dst, rel, &budget, c.policy, files, c.jobUID, 0); e != nil {
			return e
		}
	}
	if len(files) != len(manifest.Files) {
		return fmt.Errorf("cache file manifest mismatch")
	}
	for k, v := range files {
		if manifest.Files[k] != v {
			return fmt.Errorf("cache content checksum mismatch")
		}
	}
	for _, rel := range c.policy.Paths {
		parent := path.Dir(rel)
		if e = cacheDir(home, parent, true); e != nil {
			return e
		}
		// Existing parents belong to the private seed; new parents also belong to
		// the job UID. No path can be a symlink, including every intermediate parent.
		for part := parent; part != "."; part = path.Dir(part) {
			if e = home.Chown(part, c.jobUID, cacheGID(c.jobUID)); e != nil {
				return e
			}
		}
		if e = home.RemoveAll(rel); e != nil {
			return e
		}
		if e = home.Rename(staging+"/"+rel, rel); e != nil {
			return e
		}
	}
	marker := obj{"snapshot": idx.Current, "source_sha": manifest.Source, "files": len(files), "bytes": budget.bytes}
	if e = writeCacheJSON(home, ".ci-incremental-cache.json", marker); e != nil {
		return e
	}
	if e = home.Chown(".ci-incremental-cache.json", c.jobUID, cacheGID(c.jobUID)); e != nil {
		return e
	}
	log.Printf("incremental cache: restored snapshot %s files=%d bytes=%d", idx.Current, len(files), budget.bytes)

	return nil
}
