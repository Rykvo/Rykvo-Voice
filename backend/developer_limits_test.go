package main

import (
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDeveloperBudgetSmoothing(t *testing.T) {
	var a developerAdmission
	now := time.Unix(1000, 0)
	budget := apiBudget{"total", 3000, 64}
	for range 64 {
		if a.take(now, budget) != 0 {
			t.Fatal("burst rejected")
		}
	}
	if delay := a.take(now, budget); delay != 20*time.Millisecond {
		t.Fatal(delay)
	}
	if a.take(now.Add(19*time.Millisecond), budget) == 0 {
		t.Fatal("early refill")
	}
	if a.take(now.Add(20*time.Millisecond), budget) != 0 {
		t.Fatal("refill missing")
	}
	if a.take(now.Add(time.Minute), budget) != 0 {
		t.Fatal("recovery")
	}
	for range 63 {
		if a.take(now.Add(time.Minute), budget) != 0 {
			t.Fatal("burst cap")
		}
	}
	if a.take(now.Add(time.Minute), budget) == 0 {
		t.Fatal("idle accumulated a whole minute of burst")
	}
}

func TestDeveloperBudgetCategoryIsolation(t *testing.T) {
	var a developerAdmission
	now := time.Unix(1000, 0)
	total := apiBudget{"total", 3000, 64}
	upload := developerCategory(http.MethodPost, "/attachments")
	for range 4 {
		if a.take(now, total, upload) != 0 {
			t.Fatal("upload rejected")
		}
	}
	for range 100 {
		if a.take(now, total, upload) == 0 {
			t.Fatal("upload not limited")
		}
	}
	if a.buckets["total"].tokens != 60 {
		t.Fatal("rejected uploads drained total budget")
	}
	if a.take(now, total, developerCategory(http.MethodPost, "/messages")) != 0 {
		t.Fatal("upload blocked send")
	}
	if a.take(now, total, developerCategory(http.MethodGet, "/events")) != 0 {
		t.Fatal("upload blocked read")
	}
}

func TestDeveloperBudgetConcurrentAndBounded(t *testing.T) {
	var a developerAdmission
	now := time.Unix(1000, 0)
	var accepted atomic.Int64
	var wg sync.WaitGroup
	for range 500 {
		wg.Go(func() {
			if a.take(now, apiBudget{"total", 3000, 64}) == 0 {
				accepted.Add(1)
			}
		})
	}
	wg.Wait()
	if accepted.Load() != 64 {
		t.Fatal(accepted.Load())
	}
	for i := range 4095 {
		a.buckets[time.Unix(int64(i), 0).String()] = apiBucket{tokens: 1, at: now}
	}
	if a.take(now, apiBudget{"new-peer", 6000, 100}) == 0 {
		t.Fatal("unbounded peer map")
	}
	if a.take(now.Add(3*time.Minute), apiBudget{"new-peer", 6000, 100}) != 0 || len(a.buckets) != 1 {
		t.Fatal("stale peer cleanup")
	}
}

func TestDeveloperWorkCapacity(t *testing.T) {
	var a developerAdmission
	var releases []func()
	for range 4 {
		release := a.enter(true)
		if release == nil {
			t.Fatal("upload capacity")
		}
		releases = append(releases, release)
	}
	if a.enter(true) != nil || a.image() != nil {
		t.Fatal("image path bypassed upload capacity")
	}
	for range 12 {
		release := a.enter(false)
		if release == nil {
			t.Fatal("ordinary request blocked by uploads")
		}
		releases = append(releases, release)
	}
	if a.enter(false) != nil {
		t.Fatal("unbounded request admission")
	}
	for _, release := range releases {
		release()
		release()
	}
	if a.active != 0 || a.uploads != 0 {
		t.Fatal("release leak or duplicate release")
	}
	inline := a.image()
	if inline == nil {
		t.Fatal("inline image not recovered")
	}
	inline()
	inline()
	if a.uploads != 0 {
		t.Fatal("inline leak")
	}
}

func TestDeveloper120ModuleRequestLoad(t *testing.T) {
	var a developerAdmission
	start := time.Unix(1000, 0)
	// 120 modules x 10/minute, evenly submitted; one incremental read/second.
	for i := range 1200 {
		now := start.Add(time.Duration(i) * 50 * time.Millisecond)
		if delay := a.take(now, apiBudget{"total", 3000, 64}, developerCategory("POST", "/messages")); delay != 0 {
			t.Fatalf("send %d: %s", i, delay)
		}
		if i%20 == 0 {
			if a.take(now, apiBudget{"total", 3000, 64}, developerCategory("GET", "/events")) != 0 {
				t.Fatal("incremental read starved")
			}
		}
	}
}

func TestDeveloperInlineUploadSharesBudget(t *testing.T) {
	var a developerAdmission
	now := time.Unix(1000, 0)
	upload := developerCategory("POST", "/attachments")
	for range 4 {
		if a.take(now, upload) != 0 {
			t.Fatal("upload capacity")
		}
	}
	if a.take(now, developerCategory("POST", "/attachments")) == 0 {
		t.Fatal("inline image bypassed upload budget")
	}
	if a.take(now, developerCategory("POST", "/messages")) != 0 {
		t.Fatal("image limit blocked plain SMS")
	}
}

func TestDeveloperStatusReadCapacityDoesNotRaiseSending(t *testing.T) {
	var a developerAdmission
	start := time.Unix(1000, 0)
	// 25 accepted sends/s may produce several reads each. Leave ingress headroom.
	for tick := range 6000 {
		now := start.Add(time.Duration(tick) * 10 * time.Millisecond)
		if tick%4 == 0 && a.take(now, developerTotalBudget(), developerCategory("POST", "/messages")) != 0 {
			t.Fatal("status reads blocked sending")
		}
		if tick%2 == 0 && a.take(now, developerTotalBudget(), developerCategory("GET", "/messages/id")) != 0 {
			t.Fatal("50 status confirmations/s rejected")
		}
	}
	if got := developerCategory("POST", "/messages"); got.perMinute != 1500 || got.burst != 32 {
		t.Fatal("carrier send allowance changed", got)
	}
	var read developerAdmission
	budget := developerCategory("GET", "/messages/id")
	for range 64 {
		if read.take(start, developerTotalBudget(), budget) != 0 {
			t.Fatal("read burst rejected")
		}
	}
	if read.take(start, developerTotalBudget(), budget) == 0 {
		t.Fatal("unbounded reads")
	}
	if read.take(start.Add(17*time.Millisecond), developerTotalBudget(), budget) != 0 {
		t.Fatal("read recovery")
	}
}

func TestDeveloperBulkLeavesPriorityHeadroom(t *testing.T) {
	var a developerAdmission
	now := time.Now()
	for range 25 {
		if a.take(now, developerCategory("POST", "/messages"), developerBulkBudget()) != 0 {
			t.Fatal("bulk burst rejected")
		}
	}
	if a.take(now, developerCategory("POST", "/messages"), developerBulkBudget()) == 0 {
		t.Fatal("batch bypassed item budget")
	}
	if a.take(now, developerCategory("POST", "/messages")) != 0 {
		t.Fatal("bulk starved reply")
	}
	var releases []func()
	for range 4 {
		releases = append(releases, a.bulk())
		if releases[len(releases)-1] == nil {
			t.Fatal("bulk admission")
		}
	}
	if a.bulk() != nil {
		t.Fatal("unbounded bulk requests")
	}
	priority := a.enter(false)
	if priority == nil {
		t.Fatal("no priority slot")
	}
	priority()
	for _, release := range releases {
		release()
		release()
	}
	if a.batches != 0 {
		t.Fatal("batch release leak")
	}
}
