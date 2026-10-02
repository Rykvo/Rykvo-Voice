package main

import (
	"context"
	"testing"
	"time"

	"rykvo.local/auth/internal/telephony"
)

func TestSIPGatewayFinishedCallCleanup(t *testing.T) {
	for _, incoming := range []bool{false, true} {
		for _, binding := range []string{"online", "logout", "replacement"} {
			c := newSIPGateway(&server{}).calls
			c.router.PutPolicy(telephony.Policy{Account: "a", Revision: 1, All: true, Receive: true})
			reg, _, _ := c.router.Register("a", 1, time.Minute)
			c.registrations["a"] = reg
			c.router.SetModuleReady("module-03", true)
			var call telephony.Call
			if incoming {
				call, _, _ = c.router.Incoming("module-03")
			} else {
				call, _, _ = c.router.Dial(reg)
			}
			ctx, cancel := context.WithCancel(context.Background())
			v := &sipIncoming{owner: c, call: call, ctx: ctx, cancel: cancel, legs: map[telephony.ID]*sipIncomingLeg{}}
			l := &sipIncomingLeg{owner: v, reg: reg, ctx: ctx, cancel: cancel}
			v.legs[reg.ID] = l
			out := &sipOutgoing{owner: c, call: call, reg: reg, ctx: ctx, cancel: cancel}
			if incoming {
				c.incoming[call.ID] = v
			} else {
				c.active[call.ID] = out
			}
			checkHeld := func() {
				t.Helper()
				c.releaseFinishedLocked()
				if _, ok := c.router.Snapshot(call.ID); !ok {
					t.Fatal("released before cleanup evidence", incoming, binding)
				}
			}
			checkHeld()
			cancel()
			v.finished.Store(true)
			out.finished.Store(true)
			checkHeld()
			v.moduleClosed.Store(true)
			out.moduleClosed.Store(true)
			c.actionsLocked(c.router.ModuleClosed(call.ID))
			v.finished.Store(false)
			out.finished.Store(false)
			if binding != "online" {
				actions, _ := c.router.Logout(reg)
				c.actionsLocked(actions)
				delete(c.registrations, "a")
			}
			if binding == "replacement" {
				next, _, err := c.router.Register("a", 1, time.Minute)
				if err != nil {
					t.Fatal(err)
				}
				c.registrations["a"] = next
			}
			checkHeld() // Registration expiry alone cannot release a running audio/cleanup task.
			v.finished.Store(true)
			out.finished.Store(true)
			c.releaseFinishedLocked()
			c.wait.Wait()
			if _, ok := c.router.Snapshot(call.ID); ok || len(c.incoming)+len(c.active) != 0 {
				t.Fatal("finished call leaked", incoming, binding)
			}
			if binding != "logout" {
				if c.router.Status("a") != telephony.Online {
					t.Fatal("registered account still blocked")
				}
				next, _, err := c.router.Dial(c.registrations["a"])
				if err != nil {
					t.Fatal("module still blocked", err)
				}
				c.router.ClientClosed(call.ID, reg.ID)
				if snapshot, ok := c.router.Snapshot(next.ID); !ok || snapshot.Phase != telephony.Dialing {
					t.Fatal("old cleanup ended new call")
				}
			}
		}
	}
}
