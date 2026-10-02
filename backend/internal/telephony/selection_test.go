package telephony

import (
	"errors"
	"testing"
	"time"
)

// Only scheduler actions are exercised; no SIP transport or hardware is opened.
func finishSelection(t *testing.T, r *Router, reg Registration, call Call) {
	t.Helper()
	if _, err := r.Hangup(reg, call.ID); err != nil {
		t.Fatal(err)
	}
	r.ModuleClosed(call.ID)
	r.ClientClosed(call.ID, reg.ID)
}

func TestSelectionAlternatesTwoUsableModules(t *testing.T) {
	f := setup(t)
	a := f.account(t, "a", false, "01", "02")
	previous := ""
	for i := 0; i < 100; i++ {
		call, _ := outbound(t, f.r, a)
		if call.Module == previous {
			t.Fatalf("consecutive selection %q", call.Module)
		}
		previous = call.Module
		finishSelection(t, f.r, a, call)
	}
}

func TestSelectionRemainsRandomAmongOtherModules(t *testing.T) {
	for choice, want := range []string{"01", "02"} {
		f := setup(t)
		a := f.account(t, "a", false)
		f.r.choose = func(n int) int { return n - 1 }
		first, _ := outbound(t, f.r, a)
		finishSelection(t, f.r, a, first)
		f.r.choose = func(n int) int {
			if n != 2 {
				t.Fatalf("wanted two random candidates, got %d", n)
			}
			return choice
		}
		next, _ := outbound(t, f.r, a)
		if first.Module != "03" || next.Module != want {
			t.Fatal(first, next)
		}
	}
}

func TestSelectionHistoryIsIndependentPerAccount(t *testing.T) {
	f := setup(t)
	a := f.account(t, "a", false, "01", "02")
	b := f.account(t, "b", false, "01", "02")
	for _, item := range []struct {
		reg  Registration
		want string
	}{{a, "01"}, {b, "01"}, {a, "02"}, {b, "02"}} {
		call, _ := outbound(t, f.r, item.reg)
		if call.Module != item.want {
			t.Fatalf("account %s: got %s, want %s", item.reg.Account, call.Module, item.want)
		}
		finishSelection(t, f.r, item.reg, call)
	}
}

func TestFiveModuleSelectionDoesNotRequireAFullCycle(t *testing.T) {
	f := setup(t)
	f.r.SetModuleReady("04", true)
	f.r.SetModuleReady("05", true)
	a := f.account(t, "a", false)
	for i, item := range []struct {
		pick int
		want string
	}{{0, "01"}, {1, "03"}, {0, "01"}, {3, "05"}, {1, "02"}} {
		f.r.choose = func(n int) int {
			want := 4
			if i == 0 {
				want = 5
			}
			if n != want {
				t.Fatalf("wrong candidate count: %d, want %d", n, want)
			}
			return item.pick
		}
		call, _ := outbound(t, f.r, a)
		if call.Module != item.want {
			t.Fatalf("selection %d: got %s, want %s", i, call.Module, item.want)
		}
		finishSelection(t, f.r, a, call)
	}
}

func TestSelectionUsesOnlyRemainingAvailableModule(t *testing.T) {
	for _, state := range []string{"offline", "busy"} {
		t.Run(state, func(t *testing.T) {
			f := setup(t)
			a := f.account(t, "a", false, "01", "02")
			call, _ := outbound(t, f.r, a)
			finishSelection(t, f.r, a, call)
			if state == "offline" {
				f.r.SetModuleReady("02", false)
			} else {
				b := f.account(t, "b", false, "02")
				outbound(t, f.r, b)
			}
			repeated, _ := outbound(t, f.r, a)
			if repeated.Module != "01" {
				t.Fatal("only available authorized module was discarded", repeated)
			}
		})
	}
}

func TestSelectionSkipsUnavailablePreviousModule(t *testing.T) {
	f := setup(t)
	a := f.account(t, "a", false)
	first, _ := outbound(t, f.r, a)
	finishSelection(t, f.r, a, first)
	f.r.SetModuleReady("01", false)
	f.r.choose = func(n int) int {
		if n != 2 {
			t.Fatal("removed an extra available module", n)
		}
		return 1
	}
	next, _ := outbound(t, f.r, a)
	if next.Module != "03" {
		t.Fatal(next)
	}
}

func TestSelectionHistorySurvivesRegistrationAndPolicyRefresh(t *testing.T) {
	f := setup(t)
	a := f.account(t, "a", false, "01", "02")
	first, _ := outbound(t, f.r, a)
	finishSelection(t, f.r, a, first)
	f.r.Logout(a)
	_, err := f.r.PutPolicy(Policy{Account: "a", Revision: 1, Modules: []string{"01", "02"}})
	if err != nil {
		t.Fatal(err)
	}
	a, _, err = f.r.Register("a", 1, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	a, err = f.r.Refresh(a, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	next, _ := outbound(t, f.r, a)
	if next.Module != "02" {
		t.Fatal("registration or policy refresh reset selection", next)
	}
	finishSelection(t, f.r, a, next)
	f.r.DeleteAccount("a")
	if _, exists := f.r.lastDial["a"]; exists {
		t.Fatal("deleted account retained selection history")
	}
}

func TestFailedSelectionDoesNotAdvanceHistory(t *testing.T) {
	f := setup(t)
	a := f.account(t, "a", false, "01", "02")
	first, _ := outbound(t, f.r, a)
	if _, _, err := f.r.Dial(a); !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}
	finishSelection(t, f.r, a, first)
	f.r.SetModuleReady("01", false)
	f.r.SetModuleReady("02", false)
	if _, _, err := f.r.Dial(a); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	f.r.SetModuleReady("01", true)
	f.r.SetModuleReady("02", true)
	next, _ := outbound(t, f.r, a)
	if next.Module != "02" {
		t.Fatal("failed selection advanced history", next)
	}
}
