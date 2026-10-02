// Package durable preserves completed work while its database is unavailable.
package durable

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"rykvo.local/auth/internal/dispatch"
)

const maxRecord = 2 * 1024 * 1024

var ErrCorrupt = errors.New("INVALID_OUTCOME_JOURNAL")
var ErrPending = errors.New("OUTCOME_REPLAY_PENDING")

type record struct {
	Key  string          `json:"key"`
	Data json.RawMessage `json:"data"`
}
type Journal struct {
	Dir         string
	mu          sync.Mutex
	pending     map[string]record
	locks       dispatch.Locks[string]
	replayMu    sync.Mutex
	replayAfter string
	damaged     map[string]bool
}

func filename(key string) string {
	digest := sha256.Sum256([]byte(key))
	return hex.EncodeToString(digest[:]) + ".json"
}

func syncDir(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func (j *Journal) Prepare() error {
	if j.Dir == "" {
		return nil
	}
	if err := os.MkdirAll(j.Dir, 0700); err != nil {
		return err
	}
	if err := syncDir(filepath.Dir(j.Dir)); err != nil {
		return err
	}
	// Probe the real state directory before accepting work, including read-only mounts.
	f, err := os.CreateTemp(j.Dir, ".probe-")
	if err != nil {
		return err
	}
	name := f.Name()
	err = f.Sync()
	closeErr := f.Close()
	removeErr := os.Remove(name)
	return errors.Join(err, closeErr, removeErr, syncDir(j.Dir))
}

func (j *Journal) save(r record) error {
	if j.Dir == "" {
		return nil
	}
	f, err := os.CreateTemp(j.Dir, ".pending-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	err = json.NewEncoder(f).Encode(r)
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), filepath.Join(j.Dir, filename(r.Key))); err != nil {
		return err
	}
	return syncDir(j.Dir)
}

func (j *Journal) remove(key string) error {
	if j.Dir != "" {
		err := os.Remove(filepath.Join(j.Dir, filename(key)))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err = syncDir(j.Dir); err != nil {
			return err
		}
	}
	j.mu.Lock()
	delete(j.pending, key)
	j.mu.Unlock()
	return nil
}

// Apply never repeats a network operation. If both stores fail, retain memory too.
func (j *Journal) Apply(ctx context.Context, key string, data []byte, apply func(context.Context, []byte) error) error {
	if key == "" || len(key) > 128 || len(data) > maxRecord || !json.Valid(data) {
		return errors.New("INVALID_OUTCOME")
	}
	// A cancelled request still needs its already-completed outcome persisted.
	lockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	unlock, err := j.locks.Lock(lockCtx, key)
	if err != nil {
		return err
	}
	defer unlock()
	r := record{Key: key, Data: append(json.RawMessage(nil), data...)}
	j.mu.Lock()
	if j.pending == nil {
		j.pending = make(map[string]record)
	}
	j.pending[key] = r
	j.mu.Unlock()
	saveErr := j.save(r)
	if err = apply(ctx, data); err != nil {
		return errors.Join(err, saveErr)
	}
	return j.remove(key)
}

func readRecord(path string) (record, error) {
	var r record
	f, err := os.Open(path)
	if err != nil {
		return r, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxRecord+1025))
	if err != nil {
		return r, err
	}
	if len(b) > maxRecord+1024 || json.Unmarshal(b, &r) != nil || r.Key == "" || len(r.Key) > 128 || !json.Valid(r.Data) || filename(r.Key) != filepath.Base(path) {
		return r, ErrCorrupt
	}
	return r, nil
}

// Replay handles at most 128 records per pass; only selected payloads are opened.
func (j *Journal) Replay(ctx context.Context, apply func(context.Context, []byte) error) error {
	j.replayMu.Lock()
	defer j.replayMu.Unlock()
	names := make(map[string]string)
	j.mu.Lock()
	for key := range j.pending {
		names[filename(key)] = key
	}
	j.mu.Unlock()
	if j.Dir != "" {
		entries, err := os.ReadDir(j.Dir)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
				if _, ok := names[entry.Name()]; !ok {
					names[entry.Name()] = ""
				}
			}
		}
	}
	if j.damaged == nil {
		j.damaged = make(map[string]bool)
	}
	for name := range j.damaged {
		if _, exists := names[name]; !exists {
			delete(j.damaged, name)
		}
	}
	ordered := make([]string, 0, len(names))
	for name := range names {
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)
	if len(ordered) == 0 {
		j.replayAfter = ""
		return nil
	}
	start := sort.Search(len(ordered), func(i int) bool { return ordered[i] > j.replayAfter })
	processed := make(map[string]bool)
	for i := 0; i < min(128, len(ordered)); i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := ordered[(start+i)%len(ordered)]
		j.replayAfter = name
		processed[name] = true
		key := names[name]
		if key == "" {
			record, err := readRecord(filepath.Join(j.Dir, name))
			if errors.Is(err, os.ErrNotExist) {
				delete(j.damaged, name)
				continue
			}
			if errors.Is(err, ErrCorrupt) {
				j.damaged[name] = true
				continue
			}
			if err != nil {
				return err
			}
			key = record.Key
		}
		err := j.replayOne(ctx, key, apply)
		if errors.Is(err, ErrCorrupt) {
			j.damaged[name] = true
			continue
		}
		if err != nil {
			return err
		}
		delete(j.damaged, name)
	}
	pending := false
	for name := range names {
		pending = pending || !processed[name] && !j.damaged[name]
	}
	if pending && len(j.damaged) > 0 {
		return errors.Join(ErrPending, ErrCorrupt)
	}
	if pending {
		return ErrPending
	}
	if len(j.damaged) > 0 {
		return ErrCorrupt
	}
	return nil
}

// Pending results, including damaged files, must never trigger a network retry.
func (j *Journal) Pending(key string) bool {
	j.mu.Lock()
	_, ok := j.pending[key]
	j.mu.Unlock()
	if ok || j.Dir == "" {
		return ok
	}
	_, err := os.Stat(filepath.Join(j.Dir, filename(key)))
	return !errors.Is(err, os.ErrNotExist)
}

func (j *Journal) replayOne(ctx context.Context, key string, apply func(context.Context, []byte) error) error {
	unlock, err := j.locks.Lock(ctx, key)
	if err != nil {
		return err
	}
	defer unlock()
	j.mu.Lock()
	r, ok := j.pending[key]
	j.mu.Unlock()
	if !ok {
		if j.Dir == "" {
			return nil
		}
		r, err = readRecord(filepath.Join(j.Dir, filename(key)))
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
	}
	if err = apply(ctx, r.Data); err != nil {
		return err
	}
	return j.remove(key)
}
