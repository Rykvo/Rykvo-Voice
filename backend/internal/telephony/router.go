// Package telephony schedules calls; protocol and hardware adapters execute actions.
package telephony

import (
	"errors"
	"math/rand/v2"
	"slices"
	"sync"
	"time"
)

var (
	ErrInvalid      = errors.New("invalid telephony operation")
	ErrUnauthorized = errors.New("registration is no longer valid")
	ErrBusy         = errors.New("account or module is occupied")
	ErrUnavailable  = errors.New("no eligible voice module")
	ErrState        = errors.New("call state has changed")
)

type ID uint64
type Status string

const (
	Offline Status = "offline"
	Online  Status = "online"
	Busy    Status = "busy"
)

type Kind string

const (
	Ring               Kind = "ring"
	DialModule         Kind = "dial-module"
	AnswerModule       Kind = "answer-module"
	StopMedia          Kind = "stop-media"
	TerminateClient    Kind = "terminate-client"
	HangupModule       Kind = "hangup-module"
	RevokeRegistration Kind = "revoke-registration"
)

type Action struct {
	Kind         Kind
	Call         ID
	Registration ID
	Module       string
	Reason       string
}

// Revision is the durable credential version, not a policy edit counter.
type Policy struct {
	Account  string
	Revision uint64
	All      bool
	Modules  []string
	Receive  bool
}

type Registration struct {
	ID      ID
	Account string
	Expires time.Time
}

type Phase string

const (
	Dialing    Phase = "dialing"
	Ringing    Phase = "ringing"
	Connecting Phase = "connecting"
	Connected  Phase = "connected"
	Ending     Phase = "ending"
)

type Call struct {
	ID       ID
	Module   string
	Phase    Phase
	Winner   ID
	WebOwner string
	Deadline time.Time
}

type leg struct {
	registration Registration
	closing      bool
}

type activeCall struct {
	Call
	legs         map[ID]*leg
	moduleClosed bool
	ownerAccount string
}

// Router owns reservations until adapters confirm cleanup. It holds no passwords.
// Adapters serialize action execution and check ActionCurrent before dispatch.
type Router struct {
	mu            sync.Mutex
	now           func() time.Time
	choose        func(int) int
	next          ID
	policies      map[string]Policy
	revisions     map[string]uint64
	registrations map[string]Registration
	ready         map[string]bool
	calls         map[ID]*activeCall
	moduleUse     map[string]ID
	accountUse    map[string]ID
	webUse        map[string]ID
}

func New() *Router {
	return &Router{
		now: time.Now, choose: rand.IntN,
		policies: make(map[string]Policy), revisions: make(map[string]uint64),
		registrations: make(map[string]Registration), ready: make(map[string]bool),
		calls: make(map[ID]*activeCall), moduleUse: make(map[string]ID),
		accountUse: make(map[string]ID), webUse: make(map[string]ID),
	}
}

func (r *Router) id() ID { r.next++; return r.next }

func (p Policy) allows(module string) bool { return p.All || slices.Contains(p.Modules, module) }

// PutPolicy revokes all old credentials on a version increase. Same-version edits
// only withdraw newly disallowed calls or incoming offers with Receive disabled.
func (r *Router) PutPolicy(p Policy) ([]Action, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p.Account == "" || p.Revision == 0 || (!p.All && len(p.Modules) == 0) || slices.Contains(p.Modules, "") {
		return nil, ErrInvalid
	}
	old, exists := r.policies[p.Account]
	if p.Revision < r.revisions[p.Account] || (!exists && p.Revision == r.revisions[p.Account]) {
		return nil, ErrUnauthorized
	}
	p.Modules = slices.Clone(p.Modules)
	if p.All {
		p.Modules = nil
	}
	r.policies[p.Account], r.revisions[p.Account] = p, p.Revision
	var actions []Action
	if exists && old.Revision != p.Revision {
		actions = r.revoke(p.Account, "credentials-changed")
	}
	if c := r.calls[r.accountUse[p.Account]]; c != nil {
		for id, l := range c.legs {
			if l.registration.Account == p.Account && (!p.allows(c.Module) || (c.Phase == Ringing && !p.Receive)) {
				actions = append(actions, r.withdraw(c, id, "permission-changed")...)
			}
		}
	}
	return actions, nil
}

func (r *Router) DeleteAccount(account string) []Action {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.policies, account)
	return r.revoke(account, "account-deleted")
}

// Register accepts only an authenticated grant for the current credential version.
// Routine REGISTER refreshes must use Refresh, never this replacement operation.
func (r *Router) Register(account string, revision uint64, ttl time.Duration) (Registration, []Action, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.policies[account]
	if !ok || p.Revision != revision {
		return Registration{}, nil, ErrUnauthorized
	}
	if ttl <= 0 || ttl > time.Hour {
		return Registration{}, nil, ErrInvalid
	}
	actions := r.revoke(account, "replaced")
	reg := Registration{ID: r.id(), Account: account, Expires: r.now().Add(ttl)}
	r.registrations[account] = reg
	return reg, actions, nil
}

func (r *Router) valid(reg Registration) bool {
	current, ok := r.registrations[reg.Account]
	return ok && current.ID == reg.ID && current.Expires.After(r.now())
}

func (r *Router) Refresh(reg Registration, ttl time.Duration) (Registration, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.valid(reg) {
		return Registration{}, ErrUnauthorized
	}
	if ttl <= 0 || ttl > time.Hour {
		return Registration{}, ErrInvalid
	}
	reg = r.registrations[reg.Account]
	reg.Expires = r.now().Add(ttl)
	r.registrations[reg.Account] = reg
	return reg, nil
}

func (r *Router) Logout(reg Registration) ([]Action, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if current := r.registrations[reg.Account]; reg.ID == 0 || current.ID != reg.ID {
		return nil, ErrUnauthorized
	}
	return r.revoke(reg.Account, "logout"), nil
}

func (r *Router) revoke(account, reason string) []Action {
	reg, ok := r.registrations[account]
	if !ok {
		return nil
	}
	delete(r.registrations, account)
	var actions []Action
	if c := r.calls[r.accountUse[account]]; c != nil {
		actions = append(actions, r.withdraw(c, reg.ID, reason)...)
	}
	return append(actions, Action{Kind: RevokeRegistration, Registration: reg.ID, Reason: reason})
}

func (r *Router) SetModuleReady(module string, ready bool) []Action {
	r.mu.Lock()
	defer r.mu.Unlock()
	if module == "" {
		return nil
	}
	r.ready[module] = ready
	if !ready {
		if c := r.calls[r.moduleUse[module]]; c != nil {
			return r.end(c, "module-unavailable")
		}
	}
	return nil
}

func (r *Router) newCall(module string, phase Phase, timeout time.Duration) *activeCall {
	c := &activeCall{Call: Call{ID: r.id(), Module: module, Phase: phase, Deadline: r.now().Add(timeout)}, legs: make(map[ID]*leg)}
	r.calls[c.ID], r.moduleUse[module] = c, c.ID
	return c
}

func (r *Router) addLeg(c *activeCall, reg Registration) {
	c.legs[reg.ID] = &leg{registration: reg}
	r.accountUse[reg.Account] = c.ID
}

func (r *Router) Dial(reg Registration) (Call, []Action, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.valid(reg) {
		return Call{}, nil, ErrUnauthorized
	}
	if r.accountUse[reg.Account] != 0 {
		return Call{}, nil, ErrBusy
	}
	var eligible []string
	for module, ready := range r.ready {
		if ready && r.moduleUse[module] == 0 && r.policies[reg.Account].allows(module) {
			eligible = append(eligible, module)
		}
	}
	if len(eligible) == 0 {
		return Call{}, nil, ErrUnavailable
	}
	slices.Sort(eligible)
	c := r.newCall(eligible[r.choose(len(eligible))], Dialing, 60*time.Second)
	c.Winner = reg.ID
	c.ownerAccount = reg.Account
	r.addLeg(c, reg)
	return c.Call, []Action{{Kind: DialModule, Call: c.ID, Module: c.Module, Registration: reg.ID}}, nil
}

// DialWeb is called after HTTP authorization. Web owners are never incoming legs.
func (r *Router) DialWeb(owner, module string) (Call, []Action, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if owner == "" {
		return Call{}, nil, ErrInvalid
	}
	if !r.ready[module] {
		return Call{}, nil, ErrUnavailable
	}
	if r.moduleUse[module] != 0 || r.webUse[owner] != 0 {
		return Call{}, nil, ErrBusy
	}
	c := r.newCall(module, Dialing, 60*time.Second)
	c.WebOwner, r.webUse[owner] = owner, c.ID
	return c.Call, []Action{{Kind: DialModule, Call: c.ID, Module: module}}, nil
}

// Incoming errors require the adapter to reject this new provider dialog, not
// terminate the module's existing call. Provider retransmissions must be deduped.
func (r *Router) Incoming(module string) (Call, []Action, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.ready[module] {
		return Call{}, nil, ErrUnavailable
	}
	if r.moduleUse[module] != 0 {
		return Call{}, nil, ErrBusy
	}
	var targets []Registration
	for account, reg := range r.registrations {
		p := r.policies[account]
		if r.valid(reg) && p.Receive && p.allows(module) && r.accountUse[account] == 0 {
			targets = append(targets, reg)
		}
	}
	if len(targets) == 0 {
		return Call{}, nil, ErrUnavailable
	}
	slices.SortFunc(targets, func(a, b Registration) int {
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	c := r.newCall(module, Ringing, 30*time.Second)
	var actions []Action
	for _, reg := range targets {
		r.addLeg(c, reg)
		actions = append(actions, Action{Kind: Ring, Call: c.ID, Module: module, Registration: reg.ID})
	}
	return c.Call, actions, nil
}

func (r *Router) Answer(reg Registration, id ID) (Call, []Action, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.valid(reg) {
		return Call{}, nil, ErrUnauthorized
	}
	c := r.calls[id]
	if c == nil {
		return Call{}, nil, ErrState
	}
	l := c.legs[reg.ID]
	if l == nil {
		return Call{}, nil, ErrUnauthorized
	}
	if c.Phase != Ringing || l.closing {
		return c.Call, nil, ErrState
	}
	if !c.Deadline.After(r.now()) {
		actions := r.end(c, "timeout")
		return c.Call, actions, ErrState
	}
	c.Phase, c.Winner, c.Deadline = Connecting, reg.ID, r.now().Add(30*time.Second)
	c.ownerAccount = reg.Account
	var actions []Action
	for other := range c.legs {
		if other != reg.ID {
			actions = append(actions, r.closeLeg(c, other, "answered-elsewhere")...)
		}
	}
	actions = append(actions, Action{Kind: AnswerModule, Call: c.ID, Module: c.Module, Registration: reg.ID})
	return c.Call, actions, nil
}

// Connected is internal evidence from the driver, never a browser timer.
func (r *Router) Connected(id ID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.calls[id]
	if c == nil || (c.Phase != Dialing && c.Phase != Connecting) || !c.Deadline.After(r.now()) {
		return ErrState
	}
	if c.WebOwner == "" && (c.legs[c.Winner] == nil || !r.valid(c.legs[c.Winner].registration)) {
		return ErrUnauthorized
	}
	c.Phase, c.Deadline = Connected, time.Time{}
	return nil
}

func (r *Router) Hangup(reg Registration, id ID) ([]Action, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.valid(reg) {
		return nil, ErrUnauthorized
	}
	c := r.calls[id]
	if c == nil {
		return nil, ErrState
	}
	if c.legs[reg.ID] == nil {
		return nil, ErrUnauthorized
	}
	return r.withdraw(c, reg.ID, "hangup"), nil
}

func (r *Router) HangupWeb(owner string, id ID) ([]Action, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.calls[id]
	if c == nil {
		return nil, ErrState
	}
	if owner == "" || c.WebOwner != owner {
		return nil, ErrUnauthorized
	}
	return r.end(c, "hangup"), nil
}

func (r *Router) closeLeg(c *activeCall, id ID, reason string) []Action {
	l := c.legs[id]
	if l == nil || l.closing {
		return nil
	}
	l.closing = true
	return []Action{{Kind: TerminateClient, Call: c.ID, Module: c.Module, Registration: id, Reason: reason}}
}

func (r *Router) withdraw(c *activeCall, id ID, reason string) []Action {
	if c.legs[id] == nil {
		return nil
	}
	if c.Winner == id {
		return r.end(c, reason)
	}
	actions := r.closeLeg(c, id, reason)
	if c.Phase == Ringing {
		for _, l := range c.legs {
			if !l.closing {
				return actions
			}
		}
		actions = append(actions, r.end(c, reason)...)
	}
	return actions
}

func (r *Router) end(c *activeCall, reason string) []Action {
	if c.Phase == Ending {
		return nil
	}
	c.Phase, c.Deadline = Ending, time.Time{}
	actions := []Action{{Kind: StopMedia, Call: c.ID, Module: c.Module, Reason: reason}}
	for id := range c.legs {
		actions = append(actions, r.closeLeg(c, id, reason)...)
	}
	if !c.moduleClosed {
		actions = append(actions, Action{Kind: HangupModule, Call: c.ID, Module: c.Module, Reason: reason})
	}
	return actions
}

// ModuleClosed also handles spontaneous remote hangup. It never frees a module
// while a forked client leg is still awaiting termination confirmation.
func (r *Router) ModuleClosed(id ID) []Action {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.calls[id]
	if c == nil {
		return nil
	}
	c.moduleClosed = true
	actions := r.end(c, "module-ended")
	r.finish(c)
	return actions
}

// ClientClosed is trusted transport evidence, including a revoked old client's
// final cleanup. Its call+registration binding cannot affect a replacement leg.
func (r *Router) ClientClosed(id ID, registration ID) []Action {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.calls[id]
	if c == nil || c.legs[registration] == nil {
		return nil
	}
	actions := r.withdraw(c, registration, "client-ended")
	account := c.legs[registration].registration.Account
	delete(c.legs, registration)
	if r.accountUse[account] == id && c.ownerAccount != account {
		delete(r.accountUse, account)
	}
	r.finish(c)
	return actions
}

func (r *Router) finish(c *activeCall) {
	if !c.moduleClosed || len(c.legs) != 0 {
		return
	}
	delete(r.calls, c.ID)
	if r.moduleUse[c.Module] == c.ID {
		delete(r.moduleUse, c.Module)
	}
	if c.WebOwner != "" && r.webUse[c.WebOwner] == c.ID {
		delete(r.webUse, c.WebOwner)
	}
	if c.ownerAccount != "" && r.accountUse[c.ownerAccount] == c.ID {
		delete(r.accountUse, c.ownerAccount)
	}
}

func (r *Router) Tick() []Action {
	r.mu.Lock()
	defer r.mu.Unlock()
	var actions []Action
	for account, reg := range r.registrations {
		if !r.valid(reg) {
			actions = append(actions, r.revoke(account, "registration-expired")...)
		}
	}
	for _, c := range r.calls {
		if !c.Deadline.IsZero() && !c.Deadline.After(r.now()) {
			actions = append(actions, r.end(c, "timeout")...)
		}
	}
	return actions
}

func (r *Router) Status(account string) Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.valid(r.registrations[account]) {
		return Offline
	}
	if r.accountUse[account] != 0 {
		return Busy
	}
	return Online
}

func (r *Router) Snapshot(id ID) (Call, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.calls[id]
	if !ok {
		return Call{}, false
	}
	return c.Call, true
}

// ActionCurrent fences stale work. A late answered fork still needs protocol
// ACK+BYE cleanup even after removal; that belongs to the SIP transaction driver.
func (r *Router) ActionCurrent(a Action) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if a.Kind == RevokeRegistration {
		for _, reg := range r.registrations {
			if reg.ID == a.Registration {
				return false
			}
		}
		return a.Registration != 0
	}
	if a.Kind == StopMedia {
		return a.Call != 0
	}
	c := r.calls[a.Call]
	if c == nil || c.Module != a.Module {
		return false
	}
	l := c.legs[a.Registration]
	switch a.Kind {
	case Ring:
		return c.Phase == Ringing && c.Deadline.After(r.now()) && l != nil && !l.closing && r.valid(l.registration)
	case DialModule, AnswerModule:
		phase := Dialing
		if a.Kind == AnswerModule {
			phase = Connecting
		}
		return c.Phase == phase && !c.moduleClosed && c.Deadline.After(r.now()) && c.Winner == a.Registration &&
			((c.WebOwner != "" && a.Kind == DialModule) || (l != nil && !l.closing && r.valid(l.registration)))
	case TerminateClient:
		return l != nil && l.closing
	case HangupModule:
		return c.Phase == Ending && !c.moduleClosed
	}
	return false
}
