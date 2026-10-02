package durable

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestReplayBoundsPayloadReadsAndMovesPastCorruptBacklog(t *testing.T) {
	j := &Journal{Dir: t.TempDir()}
	keys := map[string]string{}
	var names []string
	for i := 0; i < 300; i++ {
		key := fmt.Sprintf("record-%04d", i)
		name := filename(key)
		keys[name] = key
		names = append(names, name)
	}
	sort.Strings(names)
	for i, name := range names {
		data := []byte("broken")
		if i >= 128 {
			data, _ = json.Marshal(record{Key: keys[name], Data: json.RawMessage(`{}`)})
		}
		if err := os.WriteFile(filepath.Join(j.Dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	count := 0
	apply := func(context.Context, []byte) error { count++; return nil }
	err := j.Replay(context.Background(), apply)
	if !errors.Is(err, ErrPending) || !errors.Is(err, ErrCorrupt) || count != 0 {
		t.Fatal("replay read beyond bounded first batch", err, count)
	}
	for i := 0; i < 5 && count < 172; i++ {
		before := count
		err = j.Replay(context.Background(), apply)
		if count-before > 128 {
			t.Fatal("unbounded replay")
		}
		if err != nil && !errors.Is(err, ErrPending) && !errors.Is(err, ErrCorrupt) {
			t.Fatal(err)
		}
	}
	if count != 172 {
		t.Fatal("corrupt files starved healthy outcomes", count)
	}
	if err = j.Replay(context.Background(), apply); err != ErrCorrupt {
		t.Fatal("damaged results lost their diagnostic", err)
	}
	files, _ := os.ReadDir(j.Dir)
	if len(files) != 128 {
		t.Fatal("damaged files discarded", len(files))
	}
}

func TestReplayRotatesPastDatabaseFailure(t *testing.T) {
	j := &Journal{Dir: t.TempDir()}
	var names []string
	keys := map[string]string{}
	for i := 0; i < 10; i++ {
		key := fmt.Sprintf("retry-%d", i)
		name := filename(key)
		keys[name] = key
		names = append(names, name)
		data, _ := json.Marshal(record{Key: key, Data: json.RawMessage(fmt.Sprintf(`{"id":%q}`, key))})
		if err := os.WriteFile(filepath.Join(j.Dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	sort.Strings(names)
	failed := keys[names[0]]
	count := 0
	apply := func(_ context.Context, data []byte) error {
		var v struct{ ID string }
		if err := json.Unmarshal(data, &v); err != nil {
			return err
		}
		if v.ID == failed {
			return errors.New("record temporarily blocked")
		}
		count++
		return nil
	}
	if j.Replay(context.Background(), apply) == nil {
		t.Fatal("lost database failure")
	}
	_ = j.Replay(context.Background(), apply)
	if count != 9 || !j.Pending(failed) {
		t.Fatal("one blocked result starved others", count)
	}
}

func TestFailedDatabaseOutcomeSurvivesRestart(t *testing.T) {
	j := &Journal{Dir: t.TempDir()}
	if err := j.Prepare(); err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"id":"test","state":"accepted"}`)
	if err := j.Apply(context.Background(), "test", data, func(context.Context, []byte) error { return errors.New("database offline") }); err == nil {
		t.Fatal("database error lost")
	}
	if _, err := os.Stat(filepath.Join(j.Dir, filename("test"))); err != nil {
		t.Fatal("durable outcome missing", err)
	}
	fresh := &Journal{Dir: j.Dir}
	calls := 0
	apply := func(_ context.Context, b []byte) error {
		calls++
		if string(b) != string(data) {
			t.Fatal("changed outcome")
		}
		return nil
	}
	if err := fresh.Replay(context.Background(), apply); err != nil {
		t.Fatal(err)
	}
	if err := fresh.Replay(context.Background(), apply); err != nil || calls != 1 {
		t.Fatal("completed outcome replayed", calls, err)
	}
	files, _ := os.ReadDir(j.Dir)
	if len(files) != 0 {
		t.Fatal("completed journal retained")
	}
}

func TestMemoryFallbackAndCancelledRequest(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	j := &Journal{Dir: file}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := j.Apply(ctx, "test", []byte(`{"state":"accepted"}`), func(ctx context.Context, _ []byte) error { return ctx.Err() })
	if err == nil || len(j.pending) != 1 {
		t.Fatal("outcome lost when disk and database failed")
	}
	j.Dir = dir
	if err := j.Replay(context.Background(), func(context.Context, []byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if len(j.pending) != 0 {
		t.Fatal("in-memory state leaked")
	}
}

func TestJournalKeepsCorruptFileAndBoundsInput(t *testing.T) {
	j := &Journal{Dir: t.TempDir()}
	path := filepath.Join(j.Dir, filename("test"))
	if err := os.WriteFile(path, []byte(`{"key":"another","data":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	called := false
	apply := func(context.Context, []byte) error { called = true; return nil }
	if err := j.Replay(context.Background(), apply); err == nil || called {
		t.Fatal("invalid recovery file accepted")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("corrupt file discarded")
	}
	for _, data := range []string{"broken", `{"x":"` + strings.Repeat("x", maxRecord) + `"}`} {
		if j.Apply(context.Background(), "other", []byte(data), apply) == nil {
			t.Fatal("invalid payload accepted")
		}
	}
}

func TestUnrelatedResultsDoNotShareSlowDatabaseLock(t *testing.T) {
	j := &Journal{Dir: t.TempDir()}
	blocked, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- j.Apply(context.Background(), "a", []byte(`{}`), func(context.Context, []byte) error { close(blocked); <-release; return nil })
	}()
	<-blocked
	defer func() {
		close(release)
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	other := make(chan error, 1)
	go func() {
		other <- j.Apply(context.Background(), "b", []byte(`{}`), func(context.Context, []byte) error { return nil })
	}()
	select {
	case err := <-other:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("slow result blocked another result")
	}
}

func TestJournalConcurrentReplayAndWrite(t *testing.T) {
	j := &Journal{Dir: t.TempDir()}
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			_ = j.Apply(context.Background(), "same", []byte(`{}`), func(context.Context, []byte) error { return errors.New("offline") })
		})
		wg.Go(func() { _ = j.Replay(context.Background(), func(context.Context, []byte) error { return nil }) })
	}
	wg.Wait()
	if err := j.Replay(context.Background(), func(context.Context, []byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if len(j.pending) != 0 {
		t.Fatal("journal bookkeeping leaked")
	}
}

func TestDamagedResultDoesNotBlockOtherKeys(t *testing.T) {
	j := &Journal{Dir: t.TempDir()}
	if err := j.Prepare(); err != nil {
		t.Fatal(err)
	}
	fail := func(context.Context, []byte) error { return errors.New("offline") }
	if j.Apply(context.Background(), "good", []byte(`{"ok":true}`), fail) == nil {
		t.Fatal("expected deferred result")
	}
	if err := os.WriteFile(filepath.Join(j.Dir, filename("bad")), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	count := 0
	err := j.Replay(context.Background(), func(context.Context, []byte) error { count++; return nil })
	if err != ErrCorrupt || count != 1 || !j.Pending("bad") || j.Pending("good") {
		t.Fatal("unrelated recovery stalled", err, count)
	}
	if !j.Pending("bad") {
		t.Fatal("damaged journal discarded")
	}
}
