package telephony

import (
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fixture struct {
	r   *Router
	now time.Time
}

func setup(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{r: New(), now: time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)}
	f.r.now = func() time.Time { return f.now }
	f.r.choose = func(int) int { return 0 }
	for _, module := range []string{"01", "02", "03"} {
		f.r.SetModuleReady(module, true)
	}
	t.Cleanup(func() { invariant(t, f.r) })
	return f
}

func (f *fixture) account(t *testing.T, name string, receive bool, modules ...string) Registration {
	t.Helper()
	_, err := f.r.PutPolicy(Policy{Account: name, Revision: 1, All: len(modules) == 0, Modules: modules, Receive: receive})
	if err != nil {
		t.Fatal(err)
	}
	reg, _, err := f.r.Register(name, 1, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

func has(actions []Action, kind Kind, reg ID) bool {
	return slices.ContainsFunc(actions, func(a Action) bool { return a.Kind == kind && (reg == 0 || a.Registration == reg) })
}

func inbound(t *testing.T, r *Router, module string) (Call, []Action) {
	t.Helper()
	c, actions, err := r.Incoming(module)
	if err != nil {
		t.Fatal(err)
	}
	return c, actions
}

func outbound(t *testing.T, r *Router, reg Registration) (Call, []Action) {
	t.Helper()
	c, actions, err := r.Dial(reg)
	if err != nil {
		t.Fatal(err)
	}
	return c, actions
}

func invariant(t testing.TB, r *Router) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	for module, id := range r.moduleUse {
		if c := r.calls[id]; c == nil || c.Module != module {
			t.Fatalf("orphan module reservation %s", module)
		}
	}
	for account, id := range r.accountUse {
		c := r.calls[id]
		if c == nil {
			t.Fatalf("orphan account %s", account)
		}
		found := c.ownerAccount == account
		for _, l := range c.legs {
			found = found || l.registration.Account == account
		}
		if !found {
			t.Fatalf("unowned account %s", account)
		}
	}
	for owner, id := range r.webUse {
		if c := r.calls[id]; c == nil || c.WebOwner != owner {
			t.Fatal("orphan web reservation")
		}
	}
	for id, c := range r.calls {
		if r.moduleUse[c.Module] != id {
			t.Fatal("module double allocation")
		}
		if c.WebOwner != "" && r.webUse[c.WebOwner] != id {
			t.Fatal("web double allocation")
		}
		if c.ownerAccount != "" && r.accountUse[c.ownerAccount] != id {
			t.Fatal("owner reservation lost")
		}
		for reg, l := range c.legs {
			if r.accountUse[l.registration.Account] != id {
				t.Fatal("account double allocation")
			}
			if c.Phase == Ending && !l.closing {
				t.Fatal("live leg on ended call")
			}
			if c.Winner != 0 && c.Winner != reg && !l.closing {
				t.Fatal("two winning legs")
			}
		}
		if c.moduleClosed && c.Phase != Ending {
			t.Fatal("closed module still live")
		}
	}
}

func TestBothIdleRingFirstAnswerWins(t *testing.T) {
	f := setup(t)
	a, b := f.account(t, "a", true, "01"), f.account(t, "b", true, "01")
	c, actions := inbound(t, f.r, "01")
	if len(actions) != 2 || !has(actions, Ring, a.ID) || !has(actions, Ring, b.ID) {
		t.Fatal(actions)
	}
	if f.r.Status("a") != Busy || f.r.Status("b") != Busy {
		t.Fatal("offers must reserve both users")
	}
	winner, actions, err := f.r.Answer(b, c.ID)
	if err != nil || winner.Winner != b.ID || winner.Phase != Connecting {
		t.Fatal(winner, err)
	}
	if !has(actions, TerminateClient, a.ID) || !has(actions, AnswerModule, b.ID) {
		t.Fatal(actions)
	}
	if _, _, err := f.r.Answer(a, c.ID); !errors.Is(err, ErrState) {
		t.Fatal("late answer stole call", err)
	}
	if err := f.r.Connected(c.ID); err != nil {
		t.Fatal(err)
	}
	f.r.ClientClosed(c.ID, a.ID)
	if f.r.Status("a") != Online || f.r.Status("b") != Busy {
		t.Fatal("loser reservation not released")
	}
}

func TestBusyUserSkippedOnOtherModule(t *testing.T) {
	f := setup(t)
	a := f.account(t, "a", true, "01", "02")
	b := f.account(t, "b", true, "02")
	first, _ := outbound(t, f.r, a)
	if first.Module != "01" {
		t.Fatal(first)
	}
	_, actions := inbound(t, f.r, "02")
	if len(actions) != 1 || !has(actions, Ring, b.ID) {
		t.Fatal(actions)
	}
	if _, _, err := f.r.Incoming("01"); !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}
	if c, _ := f.r.Snapshot(first.ID); c.Phase != Dialing {
		t.Fatal("existing call interrupted")
	}
}

func TestIncomingEligibility(t *testing.T) {
	for _, mode := range []string{"disabled", "offline", "expired", "wrong-module", "not-ready"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t)
			modules := []string{"01"}
			if mode == "wrong-module" {
				modules = []string{"02"}
			}
			a := f.account(t, "a", mode != "disabled", modules...)
			switch mode {
			case "offline":
				f.r.Logout(a)
			case "expired":
				f.now = f.now.Add(time.Hour)
			case "not-ready":
				f.r.SetModuleReady("01", false)
			}
			if _, actions, err := f.r.Incoming("01"); !errors.Is(err, ErrUnavailable) || len(actions) != 0 {
				t.Fatal(actions, err)
			}
			if len(f.r.calls) != 0 {
				t.Fatal("unanswerable incoming occupied module")
			}
		})
	}
}

func TestDisabledReceiveStillAllowsDial(t *testing.T) {
	f := setup(t)
	outbound(t, f.r, f.account(t, "a", false))
}

func TestFixedAllocationNeverEscapesAndPoliciesAreCopied(t *testing.T) {
	f := setup(t)
	modules := []string{"02"}
	a := f.account(t, "a", true, modules...)
	modules[0] = "01"
	c, _ := outbound(t, f.r, a)
	if c.Module != "02" {
		t.Fatal(c)
	}
	b := f.account(t, "b", true, "02")
	if _, _, err := f.r.Dial(b); !errors.Is(err, ErrUnavailable) {
		t.Fatal("escaped fixed allocation", err)
	}
}

func TestAllAllocationChoosesOnlyFreeReadyModules(t *testing.T) {
	f := setup(t)
	a, b := f.account(t, "a", false), f.account(t, "b", false)
	f.r.SetModuleReady("02", false)
	first, _ := outbound(t, f.r, a)
	second, _ := outbound(t, f.r, b)
	if first.Module != "01" || second.Module != "03" {
		t.Fatal(first, second)
	}
}

func TestSelectionUsesRandomChooser(t *testing.T) {
	f := setup(t)
	f.r.choose = func(n int) int {
		if n != 3 {
			t.Fatal(n)
		}
		return n - 1
	}
	a := f.account(t, "a", false)
	c, _ := outbound(t, f.r, a)
	if c.Module != "03" {
		t.Fatal(c)
	}
}

func TestSingleAccountCannotDialOrRingTwice(t *testing.T) {
	f := setup(t)
	a := f.account(t, "a", true)
	inbound(t, f.r, "01")
	if _, _, err := f.r.Dial(a); !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}
	if _, _, err := f.r.Incoming("02"); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
}

func TestOneDeclineDoesNotHangupOtherUser(t *testing.T) {
	f := setup(t)
	a, b := f.account(t, "a", true), f.account(t, "b", true)
	c, _ := inbound(t, f.r, "01")
	actions, err := f.r.Hangup(a, c.ID)
	if err != nil || has(actions, HangupModule, 0) {
		t.Fatal(actions, err)
	}
	f.r.ClientClosed(c.ID, a.ID)
	if _, _, err := f.r.Answer(b, c.ID); err != nil {
		t.Fatal(err)
	}
}

func TestAllDeclineEndsNewCall(t *testing.T) {
	f := setup(t)
	a, b := f.account(t, "a", true), f.account(t, "b", true)
	c, _ := inbound(t, f.r, "01")
	f.r.Hangup(a, c.ID)
	actions, err := f.r.Hangup(b, c.ID)
	if err != nil || !has(actions, HangupModule, 0) {
		t.Fatal(actions, err)
	}
}

func TestConcurrentAnswersHaveExactlyOneWinner(t *testing.T) {
	f := setup(t)
	a, b := f.account(t, "a", true), f.account(t, "b", true)
	c, _ := inbound(t, f.r, "01")
	var wins atomic.Int32
	var wg sync.WaitGroup
	for _, reg := range []Registration{a, b} {
		wg.Add(1)
		go func(reg Registration) {
			defer wg.Done()
			if _, _, err := f.r.Answer(reg, c.ID); err == nil {
				wins.Add(1)
			}
		}(reg)
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatal(wins.Load())
	}
}

func TestConcurrentDialsReserveModuleOnce(t *testing.T) {
	f := setup(t)
	a, b := f.account(t, "a", true, "01"), f.account(t, "b", true, "01")
	var wins atomic.Int32
	var wg sync.WaitGroup
	for _, reg := range []Registration{a, b} {
		wg.Add(1)
		go func(reg Registration) {
			defer wg.Done()
			if _, _, err := f.r.Dial(reg); err == nil {
				wins.Add(1)
			}
		}(reg)
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatal(wins.Load())
	}
}

func TestSingleRegistrationReplacementAndOldRefresh(t *testing.T) {
	f := setup(t)
	a := f.account(t, "a", true)
	c, dial := outbound(t, f.r, a)
	if err := f.r.Connected(c.ID); err != nil {
		t.Fatal(err)
	}
	b, actions, err := f.r.Register("a", 1, time.Hour)
	if err != nil || b.ID == a.ID {
		t.Fatal(b, err)
	}
	if actions[0].Kind != StopMedia || !has(actions, TerminateClient, a.ID) || !has(actions, RevokeRegistration, a.ID) || !has(actions, HangupModule, 0) {
		t.Fatal(actions)
	}
	if _, err := f.r.Refresh(a, time.Hour); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("old registration returned", err)
	}
	if _, _, err := f.r.Dial(a); !errors.Is(err, ErrUnauthorized) {
		t.Fatal(err)
	}
	if _, err := f.r.Logout(a); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("old logout removed replacement", err)
	}
	if f.r.ActionCurrent(dial[0]) {
		t.Fatal("stale dial still executable")
	}
	if _, _, err := f.r.Dial(b); !errors.Is(err, ErrBusy) {
		t.Fatal("cleanup not finished", err)
	}
	f.r.ClientClosed(c.ID, a.ID)
	if _, _, err := f.r.Dial(b); !errors.Is(err, ErrBusy) {
		t.Fatal("account freed before module ended", err)
	}
	f.r.ModuleClosed(c.ID)
	outbound(t, f.r, b)
}

func TestPasswordChangeAndDeleteHangupImmediately(t *testing.T) {
	for _, change := range []string{"password", "delete"} {
		for _, phase := range []string{"dialing", "connecting", "connected"} {
			t.Run(change+"/"+phase, func(t *testing.T) {
				f := setup(t)
				a := f.account(t, "a", true)
				var c Call
				if phase == "connecting" {
					c, _ = inbound(t, f.r, "01")
					f.r.Answer(a, c.ID)
				} else {
					c, _ = outbound(t, f.r, a)
				}
				if phase == "connected" {
					if err := f.r.Connected(c.ID); err != nil {
						t.Fatal(err)
					}
				}
				var actions []Action
				if change == "password" {
					var err error
					actions, err = f.r.PutPolicy(Policy{Account: "a", Revision: 2, All: true, Receive: true})
					if err != nil {
						t.Fatal(err)
					}
				} else {
					actions = f.r.DeleteAccount("a")
				}
				if len(actions) == 0 || actions[0].Kind != StopMedia || !has(actions, HangupModule, 0) || !has(actions, RevokeRegistration, a.ID) {
					t.Fatal(actions)
				}
				if f.r.Status("a") != Offline {
					t.Fatal("still online")
				}
				if _, _, err := f.r.Register("a", 1, time.Hour); !errors.Is(err, ErrUnauthorized) {
					t.Fatal("old credential accepted", err)
				}
				if err := f.r.Connected(c.ID); !errors.Is(err, ErrState) {
					t.Fatal("late connect revived call", err)
				}
				if change == "password" {
					if _, _, err := f.r.Register("a", 2, time.Hour); err != nil {
						t.Fatal(err)
					}
				}
			})
		}
	}
}

func TestDeleteOneForkPreservesOtherAccount(t *testing.T) {
	f := setup(t)
	a, b := f.account(t, "a", true), f.account(t, "b", true)
	c, _ := inbound(t, f.r, "01")
	actions := f.r.DeleteAccount("a")
	if !has(actions, TerminateClient, a.ID) || has(actions, HangupModule, 0) {
		t.Fatal(actions)
	}
	if _, _, err := f.r.Answer(b, c.ID); err != nil {
		t.Fatal(err)
	}
}

func TestNewLoginDuringForkDoesNotInheritOffer(t *testing.T) {
	f := setup(t)
	a := f.account(t, "a", true)
	b := f.account(t, "b", true)
	c, _ := inbound(t, f.r, "01")
	replacement, actions, err := f.r.Register("a", 1, time.Hour)
	if err != nil || !has(actions, TerminateClient, a.ID) || has(actions, HangupModule, 0) {
		t.Fatal(actions, err)
	}
	if _, _, err := f.r.Answer(replacement, c.ID); !errors.Is(err, ErrUnauthorized) {
		t.Fatal(err)
	}
	if _, _, err := f.r.Answer(b, c.ID); err != nil {
		t.Fatal(err)
	}
}

func TestWrongAccountCannotAnswerOrHangup(t *testing.T) {
	f := setup(t)
	a := f.account(t, "a", true, "01")
	b := f.account(t, "b", true, "02")
	c, _ := inbound(t, f.r, "01")
	if _, _, err := f.r.Answer(b, c.ID); !errors.Is(err, ErrUnauthorized) {
		t.Fatal(err)
	}
	if _, err := f.r.Hangup(b, c.ID); !errors.Is(err, ErrUnauthorized) {
		t.Fatal(err)
	}
	forged := a
	forged.Account = "b"
	if _, _, err := f.r.Answer(forged, c.ID); !errors.Is(err, ErrUnauthorized) {
		t.Fatal(err)
	}
}

func TestRefreshPreservesCallAndRegistration(t *testing.T) {
	f := setup(t)
	a := f.account(t, "a", true)
	c, _ := outbound(t, f.r, a)
	f.now = f.now.Add(time.Minute)
	updated, err := f.r.Refresh(a, time.Hour)
	if err != nil || updated.ID != a.ID || !updated.Expires.After(a.Expires) {
		t.Fatal(updated, err)
	}
	if got, _ := f.r.Snapshot(c.ID); got.Phase != Dialing {
		t.Fatal(got)
	}
}

func TestExpiryIsOfflineAndTerminatesCall(t *testing.T) {
	f := setup(t)
	a := f.account(t, "a", true)
	c, _ := outbound(t, f.r, a)
	if err := f.r.Connected(c.ID); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(time.Hour)
	if f.r.Status("a") != Offline {
		t.Fatal("expired registration online")
	}
	if _, err := f.r.Refresh(a, time.Hour); !errors.Is(err, ErrUnauthorized) {
		t.Fatal(err)
	}
	actions := f.r.Tick()
	if !has(actions, StopMedia, 0) || !has(actions, RevokeRegistration, a.ID) {
		t.Fatal(actions)
	}
	if len(f.r.Tick()) != 0 {
		t.Fatal("duplicate timeout actions")
	}
}

func TestDialAndRingTimeoutKeepModuleQuarantined(t *testing.T) {
	for _, incoming := range []bool{false, true} {
		t.Run(map[bool]string{false: "dial", true: "ring"}[incoming], func(t *testing.T) {
			f := setup(t)
			a := f.account(t, "a", true, "01")
			var c Call
			if incoming {
				c, _ = inbound(t, f.r, "01")
			} else {
				c, _ = outbound(t, f.r, a)
			}
			f.now = c.Deadline
			if !has(f.r.Tick(), HangupModule, 0) {
				t.Fatal("timeout did not hangup")
			}
			b := f.account(t, "b", true, "01")
			if _, _, err := f.r.Dial(b); !errors.Is(err, ErrUnavailable) {
				t.Fatal("timeout freed module without evidence", err)
			}
			f.r.ModuleClosed(c.ID)
			if _, _, err := f.r.Dial(b); !errors.Is(err, ErrUnavailable) {
				t.Fatal("client still not cleaned", err)
			}
			f.r.ClientClosed(c.ID, a.ID)
			outbound(t, f.r, b)
		})
	}
}

func TestAnswerAtDeadlineRejectedWithoutTick(t *testing.T) {
	f := setup(t)
	a := f.account(t, "a", true)
	c, _ := inbound(t, f.r, "01")
	f.now = c.Deadline
	result, actions, err := f.r.Answer(a, c.ID)
	if !errors.Is(err, ErrState) || result.Phase != Ending || !has(actions, HangupModule, 0) {
		t.Fatal(result, actions, err)
	}
}

func TestCloseEventsIdempotentAndOldCallbacksCannotTouchNewCall(t *testing.T) {
	f := setup(t)
	a := f.account(t, "a", true, "01")
	c, _ := outbound(t, f.r, a)
	f.r.Hangup(a, c.ID)
	f.r.ModuleClosed(c.ID)
	f.r.ClientClosed(c.ID, a.ID)
	if _, ok := f.r.Snapshot(c.ID); ok {
		t.Fatal("call retained")
	}
	next, _ := outbound(t, f.r, a)
	if len(f.r.ModuleClosed(c.ID)) != 0 || len(f.r.ClientClosed(c.ID, a.ID)) != 0 {
		t.Fatal("duplicate cleanup")
	}
	if got, ok := f.r.Snapshot(next.ID); !ok || got.Phase != Dialing {
		t.Fatal("stale callback damaged new call")
	}
}

func TestSpontaneousHangupCleansBothSides(t *testing.T) {
	for _, side := range []string{"client", "module"} {
		t.Run(side, func(t *testing.T) {
			f := setup(t)
			a := f.account(t, "a", true)
			c, _ := outbound(t, f.r, a)
			var actions []Action
			if side == "client" {
				actions = f.r.ClientClosed(c.ID, a.ID)
			} else {
				actions = f.r.ModuleClosed(c.ID)
			}
			if !has(actions, StopMedia, 0) {
				t.Fatal(actions)
			}
			if side == "client" && !has(actions, HangupModule, 0) {
				t.Fatal(actions)
			}
			if side == "module" && (!has(actions, TerminateClient, a.ID) || has(actions, HangupModule, 0)) {
				t.Fatal(actions)
			}
		})
	}
}

func TestPolicyDisableReceiveCancelsOnlyPendingOffer(t *testing.T) {
	f := setup(t)
	a, b := f.account(t, "a", true), f.account(t, "b", true)
	c, _ := inbound(t, f.r, "01")
	actions, err := f.r.PutPolicy(Policy{Account: "a", Revision: 1, All: true, Receive: false})
	if err != nil || !has(actions, TerminateClient, a.ID) || has(actions, HangupModule, 0) || has(actions, RevokeRegistration, a.ID) {
		t.Fatal(actions, err)
	}
	if _, _, err := f.r.Answer(b, c.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.r.Connected(c.ID); err != nil {
		t.Fatal(err)
	}
	actions, err = f.r.PutPolicy(Policy{Account: "b", Revision: 1, All: true, Receive: false})
	if err != nil || len(actions) != 0 {
		t.Fatal("disable receive interrupted established call", actions, err)
	}
}

func TestRemovingAssignedModuleEndsDisallowedCall(t *testing.T) {
	f := setup(t)
	a := f.account(t, "a", true, "01")
	outbound(t, f.r, a)
	actions, err := f.r.PutPolicy(Policy{Account: "a", Revision: 1, Modules: []string{"02"}, Receive: true})
	if err != nil || !has(actions, HangupModule, 0) {
		t.Fatal(actions, err)
	}
}

func TestWebAndSIPUseSameModuleLocks(t *testing.T) {
	f := setup(t)
	a := f.account(t, "a", true, "01")
	c, actions, err := f.r.DialWeb("web-session", "01")
	if err != nil || !has(actions, DialModule, 0) || !f.r.ActionCurrent(actions[0]) {
		t.Fatal(actions, err)
	}
	if _, _, err := f.r.Dial(a); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	if _, _, err := f.r.Incoming("01"); !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}
	if _, _, err := f.r.DialWeb("web-session", "02"); !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}
	if _, err := f.r.HangupWeb("other", c.ID); !errors.Is(err, ErrUnauthorized) {
		t.Fatal(err)
	}
	if err := f.r.Connected(c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.r.HangupWeb("web-session", c.ID); err != nil {
		t.Fatal(err)
	}
	f.r.ModuleClosed(c.ID)
	outbound(t, f.r, a)
}

func TestWebNeverReceivesIncoming(t *testing.T) {
	f := setup(t)
	c, _, err := f.r.DialWeb("web-session", "01")
	if err != nil {
		t.Fatal(err)
	}
	f.r.ModuleClosed(c.ID)
	if _, _, err := f.r.Incoming("02"); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
}

func TestModuleLostStopsMediaWithoutFalseRelease(t *testing.T) {
	f := setup(t)
	a := f.account(t, "a", true, "01")
	c, _ := outbound(t, f.r, a)
	actions := f.r.SetModuleReady("01", false)
	if !has(actions, StopMedia, 0) || !has(actions, HangupModule, 0) {
		t.Fatal(actions)
	}
	f.r.SetModuleReady("01", true)
	if _, _, err := f.r.DialWeb("web", "01"); !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}
	f.r.ModuleClosed(c.ID)
	f.r.ClientClosed(c.ID, a.ID)
}

func TestStaleActionsFencedAfterWinnerAndRevocation(t *testing.T) {
	f := setup(t)
	a, b := f.account(t, "a", true), f.account(t, "b", true)
	c, ring := inbound(t, f.r, "01")
	for _, action := range ring {
		if !f.r.ActionCurrent(action) {
			t.Fatal(action)
		}
	}
	_, answer, err := f.r.Answer(b, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range ring {
		if f.r.ActionCurrent(action) {
			t.Fatal("stale ring", action)
		}
	}
	for _, action := range answer {
		if !f.r.ActionCurrent(action) {
			t.Fatal(action)
		}
	}
	f.r.DeleteAccount("b")
	for _, action := range answer {
		if action.Kind == AnswerModule && f.r.ActionCurrent(action) {
			t.Fatal("answer after deletion")
		}
	}
	f.r.ClientClosed(c.ID, a.ID)
	if f.r.ActionCurrent(Action{Kind: TerminateClient, Call: c.ID, Module: "01", Registration: a.ID}) {
		t.Fatal("already cleaned client")
	}
}

func TestInvalidPoliciesGrantsAndDeletedRevision(t *testing.T) {
	f := setup(t)
	for _, p := range []Policy{{}, {Account: "a", Revision: 1}, {Account: "a", All: true}, {Account: "a", Revision: 1, Modules: []string{""}}} {
		if _, err := f.r.PutPolicy(p); !errors.Is(err, ErrInvalid) {
			t.Fatal(p, err)
		}
	}
	a := f.account(t, "a", true)
	for _, ttl := range []time.Duration{0, -1, 2 * time.Hour} {
		if _, _, err := f.r.Register("a", 1, ttl); !errors.Is(err, ErrInvalid) {
			t.Fatal(err)
		}
		if _, err := f.r.Refresh(a, ttl); !errors.Is(err, ErrInvalid) {
			t.Fatal(err)
		}
	}
	if f.r.Status("a") != Online {
		t.Fatal("invalid requests revoked valid session")
	}
	f.r.DeleteAccount("a")
	if _, err := f.r.PutPolicy(Policy{Account: "a", Revision: 1, All: true}); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("deleted generation resurrected", err)
	}
	if _, err := f.r.PutPolicy(Policy{Account: "a", Revision: 2, All: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.r.PutPolicy(Policy{Account: "a", Revision: 1, All: true}); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("version went backward", err)
	}
	if f.r.Status("unknown") != Offline {
		t.Fatal("unknown account online")
	}
}

func TestSnapshotsCannotMutateState(t *testing.T) {
	f := setup(t)
	a := f.account(t, "a", true)
	c, _ := outbound(t, f.r, a)
	snapshot, _ := f.r.Snapshot(c.ID)
	snapshot.Phase = Connected
	if actual, _ := f.r.Snapshot(c.ID); actual.Phase != Dialing {
		t.Fatal(actual)
	}
}

func FuzzReservations(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 8, 9, 5, 6, 11, 7, 4})
	f.Add([]byte{2, 2, 2, 10, 12, 8, 3, 7, 9})
	f.Fuzz(func(t *testing.T, ops []byte) {
		if len(ops) > 256 {
			ops = ops[:256]
		}
		r := New()
		now := time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC)
		r.now = func() time.Time { return now }
		r.choose = func(int) int { return 0 }
		names, modules := []string{"a", "b", "c"}, []string{"01", "02", "03"}
		regs := make([]Registration, 3)
		versions := []uint64{1, 1, 1}
		for i, name := range names {
			r.PutPolicy(Policy{Account: name, Revision: 1, All: true, Receive: true})
			regs[i], _, _ = r.Register(name, 1, time.Hour)
			r.SetModuleReady(modules[i], true)
		}
		for i, op := range ops {
			index := i % 3
			var id ID
			for candidate := range r.calls {
				if id == 0 || candidate < id {
					id = candidate
				}
			}
			switch op % 14 {
			case 0:
				r.Dial(regs[index])
			case 1:
				r.Incoming(modules[index])
			case 2:
				r.Answer(regs[index], id)
			case 3:
				r.Hangup(regs[index], id)
			case 4:
				r.ClientClosed(id, regs[index].ID)
			case 5:
				r.ModuleClosed(id)
			case 6:
				regs[index], _, _ = r.Register(names[index], versions[index], time.Hour)
			case 7:
				r.Connected(id)
			case 8:
				now = now.Add(31 * time.Second)
				r.Tick()
			case 9:
				r.DialWeb("web", modules[index])
			case 10:
				r.SetModuleReady(modules[index], op&16 == 0)
			case 11:
				versions[index]++
				r.PutPolicy(Policy{Account: names[index], Revision: versions[index], All: true, Receive: op&16 == 0})
			case 12:
				r.DeleteAccount(names[index])
			case 13:
				r.HangupWeb("web", id)
			}
			invariant(t, r)
		}
	})
}
