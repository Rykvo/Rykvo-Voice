package telephony

import (
	"errors"
	"testing"
	"time"
)

func detachedDialog(t *testing.T, incoming, expired bool) (*fixture, Registration, Call) {
	t.Helper()
	f := setup(t)
	a := f.account(t, "a", true, "01")
	var c Call
	if incoming {
		c, _ = inbound(t, f.r, "01")
		if _, _, err := f.r.Answer(a, c.ID); err != nil {
			t.Fatal(err)
		}
	} else {
		c, _ = outbound(t, f.r, a)
	}
	if err := f.r.Connected(c.ID); err != nil {
		t.Fatal(err)
	}
	var actions []Action
	if expired {
		f.now = a.Expires
		actions = f.r.Tick()
	} else {
		var err error
		actions, err = f.r.Logout(a)
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(actions) != 1 || !has(actions, RevokeRegistration, a.ID) {
		t.Fatalf("registration removal interrupted established dialog: %v", actions)
	}
	if got, ok := f.r.Snapshot(c.ID); !ok || got.Phase != Connected || f.r.Status("a") != Busy {
		t.Fatalf("lost connected call: %+v", got)
	}
	if _, _, err := f.r.Dial(a); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("removed registration admitted a new call", err)
	}
	return f, a, c
}

func TestEstablishedDialogSurvivesRegistrationRemoval(t *testing.T) {
	for _, incoming := range []bool{false, true} {
		for _, expired := range []bool{false, true} {
			name := map[bool]string{false: "outgoing", true: "incoming"}[incoming] + "/" + map[bool]string{false: "logout", true: "expiry"}[expired]
			t.Run(name, func(t *testing.T) {
				f, old, c := detachedDialog(t, incoming, expired)
				if actions := f.r.Tick(); len(actions) != 0 {
					t.Fatal("duplicate cleanup", actions)
				}
				reg, actions, err := f.r.Register("a", 1, time.Hour)
				if err != nil || len(actions) != 0 {
					t.Fatal(reg, actions, err)
				}
				if _, _, err := f.r.Dial(reg); !errors.Is(err, ErrBusy) {
					t.Fatal("replacement bypassed occupied account", err)
				}
				if _, err := f.r.Hangup(reg, c.ID); !errors.Is(err, ErrUnauthorized) {
					t.Fatal("replacement took ownership of old dialog", err)
				}
				b := f.account(t, "b", true, "01")
				if _, _, err := f.r.Dial(b); !errors.Is(err, ErrModuleBusy) {
					t.Fatal("lost module reservation", err)
				}
				// An authenticated in-dialog BYE is independent of REGISTER.
				if actions := f.r.ClientClosed(c.ID, old.ID); !has(actions, StopMedia, 0) || !has(actions, HangupModule, 0) {
					t.Fatal("old dialog could not terminate", actions)
				}
				if _, _, err := f.r.Dial(reg); !errors.Is(err, ErrBusy) {
					t.Fatal("released before hardware cleanup", err)
				}
				f.r.ModuleClosed(c.ID)
				outbound(t, f.r, reg)
			})
		}
	}
}

func TestAccountRevocationEndsDetachedDialog(t *testing.T) {
	for _, incoming := range []bool{false, true} {
		for _, change := range []string{"password", "delete", "permission", "suspend"} {
			t.Run(map[bool]string{false: "outgoing", true: "incoming"}[incoming]+"/"+change, func(t *testing.T) {
				f, _, c := detachedDialog(t, incoming, true)
				var actions []Action
				switch change {
				case "password":
					actions, _ = f.r.PutPolicy(Policy{Account: "a", Revision: 2, All: true, Receive: true})
				case "delete":
					actions = f.r.DeleteAccount("a")
				case "permission":
					actions, _ = f.r.PutPolicy(Policy{Account: "a", Revision: 1, Modules: []string{"02"}, Receive: true})
				case "suspend":
					actions = f.r.SuspendAccount("a")
				}
				if !has(actions, StopMedia, 0) || !has(actions, HangupModule, 0) || !has(actions, TerminateClient, c.Winner) {
					t.Fatal("detached dialog escaped account revocation", actions)
				}
			})
		}
	}
}

func TestUnansweredCallsStillEndOnRegistrationRemoval(t *testing.T) {
	for _, incoming := range []bool{false, true} {
		t.Run(map[bool]string{false: "dialing", true: "ringing"}[incoming], func(t *testing.T) {
			f := setup(t)
			a := f.account(t, "a", true, "01")
			if incoming {
				inbound(t, f.r, "01")
			} else {
				outbound(t, f.r, a)
			}
			actions, err := f.r.Logout(a)
			if err != nil || !has(actions, StopMedia, 0) || !has(actions, HangupModule, 0) {
				t.Fatal(actions, err)
			}
		})
	}
}
