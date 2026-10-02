package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log"
	randv2 "math/rand/v2"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"rykvo.local/auth/internal/hardware"
	"rykvo.local/auth/internal/sipregistrar"
	"rykvo.local/auth/internal/telephony"
	"rykvo.local/auth/internal/vocat/vowifi/ims"
)

// Scheduler and credential mutations share a short gate; SQL runs outside it.
type sipCalls struct {
	g             *sipGateway
	router        *telephony.Router
	registrations map[string]telephony.Registration
	policies      map[string]bool
	modules       map[string]bool
	active        map[telephony.ID]*sipOutgoing
	incoming      map[telephony.ID]*sipIncoming
	probing       map[string]bool
	admitting     map[string]bool
	probeSlots    chan struct{}
	probeWake     chan struct{}
	probeAfter    map[string]time.Time
	ctx           context.Context
	open          func(context.Context, sipVoiceSample) (moduleVoice, error)
	wait          sync.WaitGroup
}
type sipOutgoing struct {
	owner             *sipCalls
	call              telephony.Call
	reg               telephony.Registration
	request           *sip.Request
	tx                sip.ServerTransaction
	client            *sipgo.Client
	contact           sip.ContactHeader
	ctx               context.Context
	cancel            context.CancelFunc
	ack               chan struct{}
	ackOnce           sync.Once
	peerClosed        atomic.Bool
	finished          atomic.Bool
	moduleClosed      atomic.Bool
	mu                sync.Mutex
	rtp               *ims.ClientRTP
	answered          bool
	accepted          atomic.Bool
	alertRinging      atomic.Bool
	acceptedAt        time.Time
	final             atomic.Bool
	record            string
	outcome           string
	endedAt           time.Time
	releaseWork       func()
	lowBandwidthAudio bool
}

type moduleVoice interface {
	Dial(context.Context, string) error
	State(context.Context) (string, error)
	Hangup(context.Context) error
	ReadPCM(context.Context) ([]int16, error)
	WritePCM([]int16) error
	Close()
}

func callStateChanges(device moduleVoice) <-chan struct{} {
	if source, ok := device.(interface{ StateChanges() <-chan struct{} }); ok {
		return source.StateChanges()
	}
	return nil
}

// A slow AT status reply is not a hangup. Retry only timeouts, with fresh evidence.
func readCallState(ctx context.Context, read func(context.Context) (string, error)) (string, error) {
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		check, cancel := context.WithTimeout(ctx, 3*time.Second)
		state, err := read(check)
		cancel()
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if err == nil || err.Error() != "READ_TIMEOUT" || attempt == 2 {
			return state, err
		}
		timer := time.NewTimer(200 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", ctx.Err()
		case <-timer.C:
		}
	}
}

// Called under the dialog lock; cleanup still owns the account and module.
func (c *sipCalls) endingModuleLocked(account string) string {
	for _, call := range c.active {
		if call.reg.Account == account && call.ctx.Err() != nil {
			return call.call.Module
		}
	}
	for _, call := range c.incoming {
		if call.ctx.Err() == nil {
			continue
		}
		for _, leg := range call.legs {
			if leg.reg.Account == account {
				return call.call.Module
			}
		}
	}
	return ""
}

func newSIPCalls(g *sipGateway) *sipCalls {
	c := &sipCalls{g: g, router: telephony.New(), registrations: map[string]telephony.Registration{}, policies: map[string]bool{}, modules: map[string]bool{}, active: map[telephony.ID]*sipOutgoing{}, ctx: context.Background()}
	c.incoming = map[telephony.ID]*sipIncoming{}
	c.probing = map[string]bool{}
	c.admitting = map[string]bool{}
	c.probeSlots = make(chan struct{}, 4)
	c.probeWake = make(chan struct{}, 1)
	c.probeAfter = map[string]time.Time{}
	c.open = func(ctx context.Context, sample sipVoiceSample) (moduleVoice, error) {
		if sample.wifi {
			adapter, ok := g.server.modules.wifiEngine.(wifiVoiceAdapter)
			if !ok {
				return nil, errors.New("Wi-Fi voice adapter unavailable")
			}
			device, err := adapter.OpenWiFiCall(ctx, sample.Candidate, sample.Reading.ICCID)
			if device == nil {
				return nil, err
			}
			return device, err
		}
		system, ok := g.server.modules.source.(*hardware.System)
		if !ok {
			return nil, errors.New("voice adapter unavailable")
		}
		device, err := system.OpenCellularCall(ctx, sample.Candidate, sample.Candidate.Identity(sample.Reading), sample.Reading.ICCID)
		if device == nil {
			return nil, err
		}
		return device, err
	}
	return c
}
func (c *sipOutgoing) stop() {
	c.cancel()
	c.mu.Lock()
	if c.rtp != nil {
		c.rtp.Close()
	}
	c.mu.Unlock()
}
func (c *sipCalls) actionsLocked(actions []telephony.Action) {
	for _, a := range actions {
		if a.Kind == telephony.RevokeRegistration {
			log.Printf("SIP registration %d removed: reason=%s", a.Registration, a.Reason)
		}
		if a.Kind == telephony.StopMedia {
			log.Printf("SIP dialog %d module=%s ending: reason=%s", a.Call, a.Module, a.Reason)
		}
		if call := c.incoming[a.Call]; call != nil {
			call.actionLocked(a)
		}
		if a.Kind == telephony.StopMedia || a.Kind == telephony.TerminateClient || a.Kind == telephony.HangupModule {
			if call := c.active[a.Call]; call != nil {
				if reason := actionCallResult(a.Reason); reason != "" {
					call.result(reason)
				}
				call.stop()
			}
		}
	}
}
func (c *sipCalls) syncLocked(accounts []sipregistrar.Account, policies []telephony.Policy) {
	versions := make(map[string]int64, len(accounts))
	for _, a := range accounts {
		versions[a.ID] = a.Revision
	}
	next := map[string]bool{}
	for _, p := range policies {
		actions, err := c.router.PutPolicy(p)
		c.actionsLocked(actions)
		if err == nil {
			next[p.Account] = true
			if versions[p.Account] == 0 {
				c.actionsLocked(c.router.SuspendAccount(p.Account))
			}
		}
	}

	for id := range c.policies {
		if !next[id] {
			c.actionsLocked(c.router.DeleteAccount(id))
		}
	}
	c.policies = next
	current := c.g.registrar.Registrations()
	for id, reg := range c.registrations {
		if _, ok := current[id]; !ok || !next[reg.Account] {
			a, _ := c.router.Logout(reg)
			c.actionsLocked(a)
			delete(c.registrations, id)
		}
	}
	for id, r := range current {
		if !next[r.Account] {
			continue
		}
		ttl := time.Until(r.Expires)
		if ttl <= 0 {
			continue
		}
		if old, ok := c.registrations[id]; ok {
			if reg, e := c.router.Refresh(old, ttl); e == nil {
				c.registrations[id] = reg
				continue
			}
		}
		reg, actions, e := c.router.Register(r.Account, uint64(versions[r.Account]), ttl)
		c.actionsLocked(actions)
		if e == nil {
			c.registrations[id] = reg
		}
	}
	c.actionsLocked(c.router.Tick())
	c.releaseFinishedLocked()
}

// Module idle + stopped media + terminal SIP work release a call, not a registration.
// Missing client replies never pin an already stopped call to REGISTER renewal.
func (c *sipCalls) releaseFinishedLocked() {
	for _, call := range c.incoming {
		if !call.finished.Load() || !call.moduleClosed.Load() || call.ctx.Err() == nil {
			continue
		}
		for _, l := range call.legs {
			if !l.peerClosed.Swap(true) {
				log.Printf("SIP incoming %d: client cleanup completed without reply", call.call.ID)
			}
			c.actionsLocked(c.router.ClientClosed(call.call.ID, l.reg.ID))
		}
		call.forgetLocked()
	}
	for id, call := range c.active {
		if call.finished.Load() && call.moduleClosed.Load() && call.ctx.Err() != nil {
			if !call.peerClosed.Swap(true) {
				log.Printf("SIP call %s: client cleanup completed without reply", call.record)
			}
			c.actionsLocked(c.router.ClientClosed(id, call.reg.ID))
			if _, exists := c.router.Snapshot(id); !exists {
				delete(c.active, id)
			}
		}
	}
}

func (c *sipCalls) workerFinished(finished *atomic.Bool) {
	finished.Store(true)
	c.g.server.sipAccountsMu.Lock()
	c.releaseFinishedLocked()
	c.g.server.sipAccountsMu.Unlock()
}

type sipVoiceSample struct {
	moduleSample
	wifi bool
}
type wifiVoiceAdapter interface {
	WiFiVoiceReady(hardware.Candidate, string) bool
	OpenWiFiCall(context.Context, hardware.Candidate, string) (*hardware.WiFiCall, error)
}

func (m *moduleManager) moduleVoiceSamples() map[string]sipVoiceSample {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := map[string]sipVoiceSample{}
	if !m.ready || m.ctx == nil || m.ctx.Err() != nil || time.Since(m.lastScan) > 20*time.Second {
		return out
	}
	for id, sample := range m.values {
		current, ok := m.seen[sample.Candidate.Key]
		w := m.wifi[id]
		if !ok || !sameEndpoint(current, sample.Candidate) || m.jobs[id].active() {
			continue
		}
		r := sample.Reading
		if w != nil && (w.Enabled || w.running) {
			adapter, supported := m.wifiEngine.(wifiVoiceAdapter)
			if supported && w.Enabled && w.running && w.Registered && w.State == "connected" && w.RadioOff && w.Issue == "" && w.ICCID == r.ICCID && sameEndpoint(w.candidate, current) && r.Responsive && r.SIM == "READY" && r.Issue == "" && adapter.WiFiVoiceReady(current, r.ICCID) {
				out[moduleID(id)] = sipVoiceSample{moduleSample: sample, wifi: true}
			}
			continue
		}
		if hardware.CellularVoiceSupported(current) && r.Responsive && r.SIM == "READY" && r.Issue == "" && (r.Registration == "home" || r.Registration == "roaming") {
			out[moduleID(id)] = sipVoiceSample{moduleSample: sample}
		}
	}
	return out
}

// Only short admission decisions hold the dialog mutex; shutdown waits for the I/O.
func (c *sipCalls) outsideDialogLock(work func()) {
	c.g.server.sipAccountsMu.Unlock()
	defer c.g.server.sipAccountsMu.Lock()
	work()
}

func (c *sipCalls) inviteBindingCurrent(a sipregistrar.Account, binding sipregistrar.Registration) bool {
	if c.ctx.Err() != nil || !c.g.snapshotReady() {
		return false
	}
	valid := false
	for _, current := range c.g.accounts {
		if current.ID == a.ID && current.Revision == a.Revision && current.Username == a.Username && current.Port == a.Port {
			valid = true
			break
		}
	}
	current, ok := c.g.registrar.Registrations()[binding.ID]
	return valid && ok && current.Account == a.ID && current.Source == binding.Source && current.Transport == binding.Transport && current.Listener == binding.Listener
}

func (c *sipCalls) inviteLocked(req *sip.Request, tx sip.ServerTransaction, port int, address string, ua *sipgo.UserAgent) <-chan struct{} {
	if req.To() != nil && req.To().Params.GetOr("tag", "") != "" {
		return c.incomingRefreshLocked(req, tx)
	}
	a, binding, res := c.g.registrar.AuthenticateInvite(req, port)
	if res != nil {
		_ = tx.Respond(res)
		return nil
	}
	if c.ctx.Err() != nil || c.g.server.modules == nil || c.g.server.modules.isDraining() || !c.g.snapshotReady() {
		_ = tx.Respond(sip.NewResponseFromRequest(req, 503, "Voice Unavailable", nil))
		return nil
	}
	if req.Contact() == nil || req.Contact().Address.Wildcard || req.Contact().Address.Host == "" || req.GetHeader("Record-Route") != nil || req.To().Params.Has("tag") || !validDialNumber(req.Recipient.User) {
		_ = tx.Respond(sip.NewResponseFromRequest(req, 400, "Bad Call Request", nil))
		return nil
	}
	if c.admitting[a.ID] || len(c.admitting) >= 64 {
		_ = tx.Respond(sip.NewResponseFromRequest(req, 486, "Busy Here", nil))
		return nil
	}
	c.admitting[a.ID] = true
	c.wait.Add(1)
	defer c.wait.Done()
	defer delete(c.admitting, a.ID)
	var record string
	var recordErr error
	c.outsideDialogLock(func() {
		ctx, cancel := context.WithTimeout(c.ctx, 3*time.Second)
		defer cancel()
		record, recordErr = c.g.server.startCallRecord(ctx, req, a.ID)
	})
	if recordErr != nil {
		_ = tx.Respond(sip.NewResponseFromRequest(req, 503, "Call Record Unavailable", nil))
		return nil
	}
	recordHanded := false
	earlyModule, earlyReason := "", "service_unavailable"
	defer func() {
		if !recordHanded {
			c.outsideDialogLock(func() { c.g.server.endCallRecord(record, earlyModule, "ended", earlyReason, time.Now()) })
		}
	}()
	if !c.inviteBindingCurrent(a, binding) || tx.Err() != nil {
		earlyReason = "account_revoked"
		if errors.Is(tx.Err(), sip.ErrTransactionCanceled) {
			earlyReason = "cancelled"
		}
		_ = tx.Respond(sip.NewResponseFromRequest(req, 503, "Service Unavailable", nil))
		return nil
	}
	c.syncLocked(c.g.accounts, c.g.policies)
	network := c.g.network
	if !c.g.snapshotReady() {
		_ = tx.Respond(sip.NewResponseFromRequest(req, 503, "Service Unavailable", nil))
		return nil
	}

	samples := c.g.server.modules.moduleVoiceSamples()
	for id := range c.modules {
		if _, ok := samples[id]; !ok {
			c.actionsLocked(c.router.SetModuleReady(id, false))
		}
	}
	c.modules = map[string]bool{}
	for id := range samples {
		c.router.SetModuleReady(id, true)
		c.modules[id] = true
	}
	reg := c.registrations[binding.ID]
	call, actions, err := c.router.Dial(reg)
	c.actionsLocked(actions)
	if err != nil {
		endingModule := ""
		if errors.Is(err, telephony.ErrBusy) {
			endingModule = c.endingModuleLocked(a.ID)
		}
		c.outsideDialogLock(func() {
			reasonCtx, done := context.WithTimeout(c.ctx, 3*time.Second)
			defer done()
			earlyModule, earlyReason = c.unavailableResult(reasonCtx, a.ID, err)
		})
		if endingModule != "" {
			earlyModule, earlyReason = endingModule, "call_ending"
		}
		code, reason := 503, "No Available Voice Module"
		if errors.Is(err, telephony.ErrBusy) || errors.Is(err, telephony.ErrModuleBusy) {
			code, reason = 486, "Busy Here"
		}
		_ = tx.Respond(sip.NewResponseFromRequest(req, code, reason, nil))
		return nil
	}
	earlyModule = call.Module
	releaseWork := c.g.server.modules.reserveModuleWork(samples[call.Module].moduleSample)
	if releaseWork == nil {
		c.router.ModuleClosed(call.ID)
		c.router.ClientClosed(call.ID, reg.ID)
		earlyReason = "module_busy"
		_ = tx.Respond(sip.NewResponseFromRequest(req, 503, "Module Busy", nil))
		return nil
	}
	defer func() {
		if !recordHanded {
			releaseWork()
		}
	}()
	callCtx, stop := context.WithCancel(c.ctx)
	copy := req.Clone()
	seed := make([]byte, 16)
	if _, err = rand.Read(seed); err != nil {
		stop()
		c.router.ModuleClosed(call.ID)
		c.router.ClientClosed(call.ID, reg.ID)
		_ = tx.Respond(sip.NewResponseFromRequest(req, 500, "Server Error", nil))
		return nil
	}
	copy.To().Params.Add("tag", hex.EncodeToString(seed))
	bind, _, _ := net.SplitHostPort(address)
	public := bind
	if network.Mode == "cloud" {
		public = network.PublicAddress
	}
	client, err := newSIPPeerClient(ua, public, port, address, req.Transport())
	if err != nil || net.ParseIP(public) == nil {
		stop()
		c.router.ModuleClosed(call.ID)
		c.router.ClientClosed(call.ID, reg.ID)
		_ = tx.Respond(sip.NewResponseFromRequest(req, 503, "Media Network Unavailable", nil))
		return nil
	}
	leg := &sipOutgoing{lowBandwidthAudio: c.g.lowBandwidthAudioLocked(a.ID), releaseWork: releaseWork, record: record, owner: c, call: call, reg: reg, request: copy, tx: tx, client: client, ctx: callCtx, cancel: stop, ack: make(chan struct{}), contact: sip.ContactHeader{Address: sip.Uri{Scheme: "sip", Host: public, Port: port}}}
	recordHanded = true
	c.active[call.ID] = leg
	leg.bindCancel()
	_ = tx.Respond(sip.NewResponseFromRequest(copy, 100, "Trying", nil))
	done := make(chan struct{})
	c.wait.Add(1)
	go func() {
		defer close(done)
		defer c.wait.Done()
		leg.run(samples[call.Module], network, net.ParseIP(bind), net.ParseIP(public))
	}()
	return done
}
func (c *sipOutgoing) bindCancel() {
	if c.tx.OnCancel(func(r *sip.Request) {
		if r.Source() == c.request.Source() && r.Transport() == c.request.Transport() {
			c.result("cancelled")
			c.peerClosed.Store(true)
			c.stop()
		}
	}) {
		return
	}
	// CANCEL may finish the transaction while policy/database checks run.
	if errors.Is(c.tx.Err(), sip.ErrTransactionCanceled) {
		c.result("cancelled")
		c.peerClosed.Store(true)
	} else {
		c.result("service_unavailable")
	}
	c.stop()
}
func validDialNumber(v string) bool {
	if len(v) < 3 || len(v) > 16 {
		return false
	}
	for i, r := range v {
		if r == '+' && i == 0 {
			continue
		}
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
func (c *sipCalls) dialogLocked(req *sip.Request, tx sip.ServerTransaction) {
	if c.incomingDialogLocked(req, tx) {
		return
	}
	for _, leg := range c.active {
		r := leg.request
		if req.CallID() == nil || req.From() == nil || req.To() == nil || req.CSeq() == nil || req.CallID().Value() != r.CallID().Value() || req.Source() != r.Source() || req.Transport() != r.Transport() || req.From().Params.GetOr("tag", "") != r.From().Params.GetOr("tag", "") || req.To().Params.GetOr("tag", "") != r.To().Params.GetOr("tag", "") {
			continue
		}
		if req.Method == sip.ACK && leg.accepted.Load() && req.CSeq().SeqNo == r.CSeq().SeqNo {
			leg.ackOnce.Do(func() { close(leg.ack) })
			return
		}
		if req.Method == sip.BYE && req.CSeq().SeqNo > r.CSeq().SeqNo {
			_ = tx.Respond(sip.NewResponseFromRequest(req, 200, "OK", nil))
			if leg.accepted.Load() {
				leg.result("completed")
			} else {
				leg.result("cancelled")
			}
			leg.peerClosed.Store(true)
			leg.stop()
			c.actionsLocked(c.router.ClientClosed(leg.call.ID, leg.reg.ID))
			if _, exists := c.router.Snapshot(leg.call.ID); !exists {
				delete(c.active, leg.call.ID)
			}
			return
		}
	}
	if req.Method != sip.ACK {
		_ = tx.Respond(sip.NewResponseFromRequest(req, 481, "Call Does Not Exist", nil))
	}
}
func (c *sipOutgoing) respond(code int, reason string, body []byte) error {
	if code >= 200 {
		c.final.Store(true)
	}
	if code == 200 {
		if !c.accepted.Load() {
			c.acceptedAt = time.Now()
			c.accepted.Store(true)
		}
	}
	r := sip.NewResponseFromRequest(c.request, code, reason, body)
	r.AppendHeader(&c.contact)
	if len(body) > 0 {
		r.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	}
	return c.tx.Respond(r)
}
func (c *sipOutgoing) run(sample sipVoiceSample, network sipAccountNetwork, bind, public net.IP) {
	defer c.owner.workerFinished(&c.finished)
	if c.releaseWork != nil {
		defer c.releaseWork()
	}
	g := c.owner.g
	m := g.server.modules
	if c.ctx.Err() != nil {
		c.endPeer()
		c.finish(true)
		return
	}
	c.recordState("dialing")
	// The Wi-Fi worker owns the hardware gate for its entire registration.
	if !sample.wifi {
		gate := m.gate(sample.Candidate.Key)
		gateTimer := time.NewTimer(8 * time.Second)
		defer gateTimer.Stop()
		select {
		case gate <- struct{}{}:
		case <-c.ctx.Done():
			c.endPeer()
			c.finish(true)
			return
		case <-gateTimer.C:
			c.result("module_busy")
			_ = c.respond(503, "Module Busy", nil)
			c.finish(true)
			return
		}
		defer func() { <-gate }()
	}
	var device moduleVoice
	var audio sync.WaitGroup
	var playback cellularPCMStats
	defer func() {
		c.stop()
		c.endRecord("cleanup_pending")
		peerDone := make(chan struct{})
		go func() { defer close(peerDone); c.endPeer() }()
		defer func() { <-peerDone }()
		if device != nil {
			lastError := ""
			for {
				clean, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				err := device.Hangup(clean)
				cancel()
				if err == nil {
					break
				}
				if err.Error() != lastError {
					log.Printf("SIP call %s: module cleanup pending: %v", c.record, err)
					lastError = err.Error()
				}
				select {
				case <-c.owner.ctx.Done():
					device.Close()
					audio.Wait()
					c.finish(false)
					return
				case <-time.After(3 * time.Second):
				}
			}
			device.Close()
		}
		audio.Wait()
		if c.rtp != nil && c.record != "" {
			log.Printf("SIP call %s audio: %s", c.record, callAudioSummary(sample.wifi, c.rtp.ReceiveStats(), device, &playback))
		}
		c.finish(true)
	}()
	peer, _, _ := net.SplitHostPort(c.request.Source())
	start := network.Start
	span := network.End - start + 1
	if start < 1024 || network.End > 65535 || span <= 0 {
		c.result("media_network_error")
		_ = c.respond(503, "Media Network Unavailable", nil)
		return
	}
	for n, offset := 0, randv2.IntN(span); n < min(span, 256); n++ {
		port := start + (offset+n)%span
		if port == c.request.Recipient.Port || port == 2019 || port == 8080 || port >= 51820 && port <= 51822 {
			continue
		}
		rtp, err := ims.OpenClientRTP(bind, port, net.ParseIP(peer), c.request.Body(), c.lowBandwidthAudio)
		if err == nil {
			c.mu.Lock()
			c.rtp = rtp
			c.mu.Unlock()
			break
		}
		if !strings.Contains(err.Error(), "bind") && !strings.Contains(err.Error(), "address already") {
			c.result("unsupported_audio")
			_ = c.respond(488, "Unsupported Audio", nil)
			return
		}
	}
	if c.rtp == nil {
		c.result("media_network_error")
		_ = c.respond(503, "No Media Port", nil)
		return
	}
	op, opCancel := context.WithTimeout(c.ctx, 20*time.Second)
	var err error
	device, err = c.owner.open(op, sample)
	opCancel()
	if err != nil {
		c.result(moduleCallResult(err))
		_ = c.respond(503, "Module Voice Unavailable", nil)
		return
	}
	g.server.sipAccountsMu.Lock()
	current := c.owner.router.ActionCurrent(telephony.Action{Kind: telephony.DialModule, Call: c.call.ID, Module: c.call.Module, Registration: c.reg.ID})
	g.server.sipAccountsMu.Unlock()
	if !current || c.ctx.Err() != nil {
		return
	}
	var mediaActive, uplinkActive atomic.Bool
	audio.Add(1)
	go func() {
		defer audio.Done()
		if err := forwardCallPCM(c.ctx, &mediaActive, device.ReadPCM, c.rtp.WritePCM); err != nil {
			c.mediaFailed("downlink", err)
		}
	}()
	if sample.wifi {
		audio.Add(1)
		go func() {
			defer audio.Done()
			if err := forwardCallPCM(c.ctx, &uplinkActive, c.rtp.ReadPCM, device.WritePCM); err != nil {
				c.mediaFailed("uplink", err)
			}
		}()
	}
	op, opCancel = context.WithTimeout(c.ctx, 8*time.Second)
	err = device.Dial(op, c.request.Recipient.User)
	opCancel()
	if err != nil {
		if c.ctx.Err() != nil {
			return
		}
		c.result(dialCallResult(device, err))
		_ = c.respond(503, "Dial Failed", nil)
		return
	}
	timer := time.NewTimer(55 * time.Second)
	defer timer.Stop()
	poll := time.NewTicker(500 * time.Millisecond)
	defer poll.Stop()
	ringing, early := false, false
	var earlyAt time.Time
	body := c.rtp.Answer(public)
	stateChanges := callStateChanges(device)
	for !c.answered {
		select {
		case <-c.ctx.Done():
			return
		case <-timer.C:
			if ringing {
				c.result("no_answer")
			} else {
				c.result("call_timeout")
			}
			_ = c.respond(480, "No Answer", nil)
			return
		case <-poll.C:
		case <-stateChanges:
		}
		state, e := readCallState(c.ctx, device.State)
		if c.ctx.Err() != nil {
			return
		}
		if e != nil {
			c.result("module_error")
			log.Printf("SIP call %s: module state unavailable: %v", c.record, e)
			_ = c.respond(503, "Module Lost", nil)
			return
		}
		switch state {
		case "idle":
			code := 480
			reason := "failed"
			if d, ok := device.(interface{ SIPCode() int }); ok {
				carrier := d.SIPCode()
				reason = carrierCallResult(carrier)
				if carrier >= 400 && carrier <= 699 {
					code = carrier
				}
			}
			if d, ok := device.(interface{ FailureReason() string }); ok && d.FailureReason() != "" {
				reason = d.FailureReason()
			}
			c.result(reason)
			_ = c.respond(code, "Call Ended", nil)
			return
		case "early_media":
			if !early || time.Since(earlyAt) >= time.Second {
				if err := c.respond(183, "Session Progress", body); err != nil {
					return
				}
				early, earlyAt = true, time.Now()
			}
			mediaActive.Store(true)
		case "ringing":
			trusted := !sample.wifi
			if carrier, ok := device.(interface{ SIPCode() int }); ok && sample.wifi {
				trusted = carrier.SIPCode() == 180
			}
			newEvidence := trusted && !c.alertRinging.Swap(true)
			mediaActive.Store(false)
			if !ringing || early {
				early = false
				_ = c.respond(180, "Ringing", nil)
			}
			if !ringing || newEvidence {
				ringing = true
				c.recordState("ringing")
			}
		case "active":
			c.answered = true
			c.recordState("connected")
		}
	}
	ackTimer := time.NewTimer(32 * time.Second)
	defer ackTimer.Stop()
	repeat := time.NewTicker(time.Second)
	defer repeat.Stop()
	if err := c.respond(200, "OK", body); err != nil {
		log.Printf("SIP call %s: answer transaction failed: %v", c.record, err)
		return
	}
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ackTimer.C:
			return
		case <-repeat.C:
			if c.respond(200, "OK", body) != nil {
				return
			}
		case ack := <-c.tx.Acks():
			if ack != nil {
				g.server.sipAccountsMu.Lock()
				c.owner.dialogLocked(ack, nil)
				g.server.sipAccountsMu.Unlock()
			}
		case <-c.ack:
			goto media
		}
	}
media:
	g.server.sipAccountsMu.Lock()
	err = c.owner.router.Connected(c.call.ID)
	g.server.sipAccountsMu.Unlock()
	if err != nil {
		return
	}
	mediaActive.Store(true)
	uplinkActive.Store(true)
	if !sample.wifi {
		audio.Add(1)
		go func() {
			defer audio.Done()
			if err := playCellularPCM(c.ctx, c.rtp.ReadPCM, device.WritePCM, &playback); err != nil {
				c.mediaFailed("uplink", err)
			}
		}()
	}
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-c.tx.Acks():
			// Drain retransmitted same-branch ACKs while media is active.
			continue
		case <-poll.C:
		case <-stateChanges:
		}
		state, e := readCallState(c.ctx, device.State)
		if c.ctx.Err() != nil {
			return
		}
		if e != nil || state != "active" {
			if e != nil || state != "idle" {
				c.result("module_error")
				log.Printf("SIP call %s: module state=%s error=%v", c.record, state, e)
			} else {
				c.result("completed")
			}
			return
		}
	}
}
func (c *sipOutgoing) mediaFailed(stage string, err error) {
	if errors.Is(err, hardware.ErrVoiceEnded) {
		return // State polling relays the confirmed carrier termination.
	}
	if c.ctx.Err() == nil {
		c.result(mediaCallResult(err))
		log.Printf("SIP call %s: %s failed: %v", c.record, stage, err)
	}
	c.stop()
}
func (c *sipOutgoing) endPeer() {
	if !c.accepted.Load() {
		if !c.final.Load() && !c.peerClosed.Load() {
			_ = c.respond(487, "Request Terminated", nil)
		}
		c.peerClosed.Store(true)
	} else if !c.peerClosed.Load() {
		// End the modem/media immediately; SIP BYE waits for ACK or its deadline.
		select {
		case <-c.ack:
		case <-time.After(max(0, time.Until(c.acceptedAt.Add(32*time.Second)))):
		}

		bye := sip.NewRequest(sip.BYE, c.request.Contact().Address)
		from := c.request.To().AsFrom()
		to := c.request.From().AsTo()
		bye.AppendHeader(&from)
		bye.AppendHeader(&to)
		bye.AppendHeader(sip.HeaderClone(c.request.CallID()))
		bye.AppendHeader(&sip.CSeqHeader{SeqNo: c.request.CSeq().SeqNo + 1, MethodName: sip.BYE})
		bye.SetDestination(c.request.Source())
		bye.SetTransport(c.request.Transport())
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		res, err := c.client.Do(ctx, bye)
		cancel()
		if sipByeConfirmed(c.record, res, err) {
			c.peerClosed.Store(true)
		}
	}
	c.owner.g.server.sipAccountsMu.Lock()
	if c.peerClosed.Load() {
		c.owner.actionsLocked(c.owner.router.ClientClosed(c.call.ID, c.reg.ID))
	}
	if _, exists := c.owner.router.Snapshot(c.call.ID); !exists {
		delete(c.owner.active, c.call.ID)
	}
	// Worker completion releases a locally terminated dialog after confirmed module cleanup.
	c.owner.g.server.sipAccountsMu.Unlock()
}
func (c *sipOutgoing) finish(moduleClosed bool) {
	c.moduleClosed.Store(moduleClosed)
	if moduleClosed {
		c.endRecord("ended")
	} else {
		c.endRecord("cleanup_pending")
	}

	c.stop()
	g := c.owner.g
	g.server.sipAccountsMu.Lock()
	defer g.server.sipAccountsMu.Unlock()
	if moduleClosed {
		c.owner.actionsLocked(c.owner.router.ModuleClosed(c.call.ID))
	}
	if !c.accepted.Load() || c.peerClosed.Load() {
		c.owner.actionsLocked(c.owner.router.ClientClosed(c.call.ID, c.reg.ID))
	}
	if _, exists := c.owner.router.Snapshot(c.call.ID); !exists {
		delete(c.owner.active, c.call.ID)
	}
}
