package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
)

// Every allocation is generated in validJobName's namespace, before any
// resource or lease is created. Recovery enforces that same namespace. A name
// outside it cannot address an allocation, even if memory state is absent or
// the scheduler supplies a local runner ID. Never substitute that ID or derive
// a resource path from an unaddressable name. Owned resources still belong to
// the durable lease/recovery/janitor lifecycle, independent of this event.
func (f *fleet) recordUnaddressableEvent(name, handler string, event any) (bool, error) {
	if f.cleanupLog == nil || validJobName.MatchString(name) {
		return false, nil
	}
	if f.state(name) != nil {
		return true, fmt.Errorf("allocation outside runner namespace")
	}
	return true, f.cleanupLog.recordUnaddressable(handler, event)
}

// This is a terminal *routing disposition*, not a cleanup observation or a
// pending work queue. Retain only a canonical event fingerprint, never its raw
// name/body. A stable digest makes redelivery and restart idempotent. Distinct
// event payloads get distinct records. The .audit suffix keeps these out of
// the existing per-allocation .json recovery/collector contract.
func (c *cleanupJournal) recordUnaddressable(handler string, event any) error {
	if !oneOf(handler, "started", "completed") {
		return fmt.Errorf("invalid disposition handler")
	}
	h := sha256.New()
	if err := json.NewEncoder(h).Encode(struct {
		Owner, Handler string
		Event          any
	}{c.owner, handler, event}); err != nil {
		return err
	}
	digest := hex.EncodeToString(h.Sum(nil))
	data, err := json.Marshal(struct {
		Schema  string `json:"schema"`
		Owner   string `json:"owner_id"`
		Handler string `json:"handler"`
		Reason  string `json:"reason"`
		Digest  string `json:"event_sha256"`
	}{"ci-unaddressable-event/v1", c.owner, handler, "outside_runner_namespace", digest})
	if err != nil {
		return err
	}
	name := "unaddressable-" + digest + ".audit"
	c.mu.Lock()
	defer c.mu.Unlock()
	f, err := c.root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err == nil {
		defer f.Close()
		st, e := f.Stat()
		if e != nil || !protectedCleanup(st, c.uid, 0444, false) || st.Size() != int64(len(data)) {
			return fmt.Errorf("unsafe event disposition")
		}
		old, e := io.ReadAll(io.LimitReader(f, int64(len(data)+1)))
		if e != nil || !bytes.Equal(old, data) {
			return fmt.Errorf("invalid event disposition")
		}
		// The previous rename may have succeeded before directory fsync failed.
		return c.syncDir()
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	tmp := name + ".new"
	if err = c.root.Remove(tmp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err = c.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer c.root.Remove(tmp)
	_, err = f.Write(data)
	if err == nil {
		err = f.Chmod(0444)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = c.root.Rename(tmp, name); err != nil {
		return err
	}
	return c.syncDir()
}
