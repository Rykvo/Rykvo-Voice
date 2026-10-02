package main

import (
	"context"
	"errors"
	"log"
	"net"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"rykvo.local/auth/internal/hardware"
	"rykvo.local/auth/internal/telephony"
	"rykvo.local/auth/internal/vocat/vowifi/ims"
)

type incomingVoice interface {
	moduleVoice
	Answer(context.Context) error
}
type wifiIncomingAdapter interface {
	WiFiCalls(context.Context, hardware.Candidate, string, string) ([]hardware.VoiceCall, error)
	OpenWiFiIncoming(context.Context, hardware.Candidate, string, string) (*hardware.WiFiCall, error)
}
type sipIncoming struct {
	owner        *sipCalls
	call         telephony.Call
	sample       sipVoiceSample
	device       incomingVoice
	ctx          context.Context
	cancel       context.CancelFunc
	legs         map[telephony.ID]*sipIncomingLeg
	winner       chan *sipIncomingLeg
	media        atomic.Pointer[ims.ClientRTP]
	connected    atomic.Bool
	finished     atomic.Bool
	moduleClosed atomic.Bool
	recordsMu    sync.Mutex
	records      map[string]*incomingAccountRecord
	mu           sync.Mutex
	reason       string
}

// Slow hardware/IPC work never runs under the account gate or a UI request.
func (c *sipCalls) scanIncomingLocked(network sipAccountNetwork) {
	if c.ctx.Err() != nil || c.g.server.modules == nil {
		return
	}
	samples := c.g.server.modules.moduleVoiceSamples()
	ids := make([]string, 0, len(samples))
	now := time.Now()
	for id := range c.probeAfter {
		if _, ok := samples[id]; !ok {
			delete(c.probeAfter, id)
		}
	}
	for id := range samples {
		if !now.Before(c.probeAfter[id]) {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := c.probeAfter[ids[i]], c.probeAfter[ids[j]]
		if a.Equal(b) {
			return ids[i] < ids[j]
		}
		return a.Before(b)
	})
	for _, id := range ids {
		sample := samples[id]
		if c.probing[id] {
			continue
		}
		busy := false
		for _, call := range c.active {
			if call.call.Module == id {
				busy = true
			}
		}
		for _, call := range c.incoming {
			if call.call.Module == id {
				busy = true
			}
		}
		if busy {
			continue
		}
		select {
		case c.probeSlots <- struct{}{}:
		default:
			continue
		}
		c.probing[id] = true
		c.probeAfter[id] = now.Add(2 * time.Second)
		c.wait.Add(1)
		go func() {
			defer c.wait.Done()
			defer func() { c.g.server.sipAccountsMu.Lock(); delete(c.probing, id); c.g.server.sipAccountsMu.Unlock() }()
			c.probeIncoming(id, sample, network)
		}()
	}
}

func (c *sipCalls) releaseProbeSlot() {
	<-c.probeSlots
	select {
	case c.probeWake <- struct{}{}:
	default:
	}
}

func (c *sipCalls) probeIncoming(id string, sample sipVoiceSample, network sipAccountNetwork) {
	releaseWork := c.g.server.modules.reserveModuleWork(sample.moduleSample)
	if releaseWork == nil {
		c.releaseProbeSlot()
		return
	}
	defer releaseWork()
	var device incomingVoice
	var number string
	ctx, cancel := context.WithTimeout(c.ctx, 20*time.Second)
	defer cancel()
	var err error
	if sample.wifi {
		if a, ok := c.g.server.modules.wifiEngine.(wifiIncomingAdapter); ok {
			var calls []hardware.VoiceCall
			calls, err = a.WiFiCalls(ctx, sample.Candidate, sample.Reading.ICCID, sipToken())
			for _, call := range calls {
				if err != nil || call.Direction != "incoming" || call.State != "ringing" {
					continue
				}
				v, e := a.OpenWiFiIncoming(ctx, sample.Candidate, sample.Reading.ICCID, call.ID)
				err = e
				if v != nil {
					device, number = v, call.Number
				}
				break
			}
		}
	} else if system, ok := c.g.server.modules.source.(*hardware.System); ok {
		gate := c.g.server.modules.gate(sample.Candidate.Key)
		select {
		case gate <- struct{}{}:
			defer func() { <-gate }()
			v, e := system.OpenCellularIncoming(ctx, sample.Candidate, sample.Candidate.Identity(sample.Reading), sample.Reading.ICCID)
			err = e
			if v != nil {
				device, number = v, v.Caller()
			}
		default:
		}
	}
	c.releaseProbeSlot()
	if device == nil {
		return
	}
	defer device.Close()
	if err == nil && c.ctx.Err() == nil {
		c.g.server.sipAccountsMu.Lock()
		call := c.startIncomingLocked(id, sample, device, number, network)
		c.g.server.sipAccountsMu.Unlock()
		if call != nil {
			call.run()
			return
		}
	}
	// No eligible receiver: end only this bound new call, without a call record.
	c.cleanIncoming(device)
}

func (c *sipCalls) cleanIncoming(device incomingVoice) bool {
	lastError := ""
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		err := device.Hangup(ctx)
		cancel()
		if err == nil {
			return true
		}
		if err.Error() != lastError {
			log.Printf("SIP incoming: module cleanup pending: %v", err)
			lastError = err.Error()
		}
		select {
		case <-c.ctx.Done():
			return false
		case <-time.After(3 * time.Second):
		}
	}
}

func (c *sipCalls) startIncomingLocked(id string, sample sipVoiceSample, device incomingVoice, number string, network sipAccountNetwork) *sipIncoming {
	if c.ctx.Err() != nil {
		return nil
	}
	c.router.SetModuleReady(id, true)
	call, actions, err := c.router.Incoming(id)
	if err != nil {
		c.actionsLocked(actions)
		return nil
	}
	ctx, cancel := context.WithCancel(c.ctx)
	v := &sipIncoming{owner: c, call: call, sample: sample, device: device, ctx: ctx, cancel: cancel, legs: map[telephony.ID]*sipIncomingLeg{}, winner: make(chan *sipIncomingLeg, 1)}
	c.incoming[call.ID] = v
	current := c.g.registrar.Registrations()
	for _, action := range actions {
		if action.Kind != telephony.Ring {
			continue
		}
		var reg telephony.Registration
		binding := ""
		for id, r := range c.registrations {
			if r.ID == action.Registration {
				reg, binding = r, id
				break
			}
		}
		target := current[binding]
		listener := c.g.listeners[target.Listener]
		if listener == nil || target.Contact == nil || !target.Expires.After(time.Now()) {
			c.actionsLocked(c.router.ClientClosed(call.ID, action.Registration))
			continue
		}
		bind, _, _ := net.SplitHostPort(target.Listener)
		public := bind
		if network.Mode == "cloud" {
			public = network.PublicAddress
		}
		client, err := newSIPPeerClient(listener.ua, public, listener.port, target.Listener, target.Transport)
		if err != nil || net.ParseIP(public) == nil {
			c.actionsLocked(c.router.ClientClosed(call.ID, action.Registration))
			continue
		}
		leg := newIncomingLeg(v, reg, target, client, network, net.ParseIP(bind), net.ParseIP(public), listener.port, number)
		v.legs[reg.ID] = leg
	}
	return v
}

func (c *sipIncoming) stop(reason string) {
	c.mu.Lock()
	if c.reason == "" {
		c.reason = reason
	}
	c.mu.Unlock()
	for _, l := range c.legs {
		l.stop(reason)
	}
	c.cancel()
}
func (c *sipIncoming) mediaFailed(stage string, err error) {
	if !errors.Is(err, hardware.ErrVoiceEnded) && c.ctx.Err() == nil {
		log.Printf("SIP incoming %d %s: %s failed: %v", c.call.ID, c.call.Module, stage, err)
		c.stop(mediaCallResult(err))
	}
}
func (c *sipIncoming) actionLocked(a telephony.Action) {
	switch a.Kind {
	case telephony.StopMedia, telephony.HangupModule:
		c.stop(incomingActionResult(a.Reason))
	case telephony.TerminateClient:
		if l := c.legs[a.Registration]; l != nil {
			l.stop(incomingActionResult(a.Reason))
		}
	}
}
func (c *sipIncoming) forgetLocked() {
	if _, exists := c.owner.router.Snapshot(c.call.ID); !exists && c.owner.incoming[c.call.ID] == c {
		delete(c.owner.incoming, c.call.ID)
		c.owner.wait.Add(1)
		go func() {
			defer c.owner.wait.Done()
			c.finalizeRecords()
		}()
	}
}

func (c *sipIncoming) run() {
	defer c.owner.workerFinished(&c.finished)
	var audio sync.WaitGroup
	var legs sync.WaitGroup
	var winner *sipIncomingLeg
	var playback cellularPCMStats
	defer func() {
		c.stop("failed")
		c.mu.Lock()
		reason := c.reason
		c.mu.Unlock()
		log.Printf("SIP incoming %d %s: ending reason=%s connected=%t", c.call.ID, c.call.Module, reason, c.connected.Load())
		for _, l := range c.legs {
			l.stop(reason)
		}
		closed := c.owner.cleanIncoming(c.device)
		audio.Wait()
		if winner != nil && winner.rtp != nil {
			log.Printf("SIP incoming %d %s audio: %s", c.call.ID, c.call.Module, callAudioSummary(c.sample.wifi, winner.rtp.ReceiveStats(), c.device, &playback))
		}
		c.owner.g.server.sipAccountsMu.Lock()
		if closed {
			c.moduleClosed.Store(true)
			c.owner.actionsLocked(c.owner.router.ModuleClosed(c.call.ID))
		}
		c.forgetLocked()
		c.owner.g.server.sipAccountsMu.Unlock()
		legs.Wait()
	}()
	if len(c.legs) == 0 {
		return
	}
	for _, l := range c.legs {
		legs.Add(1)
		go func() { defer legs.Done(); l.run() }()
	}
	// Drain pre-answer PCM; only the winning, connected leg gets audio.
	audio.Add(1)
	go func() {
		defer audio.Done()
		for c.ctx.Err() == nil {
			p, err := c.device.ReadPCM(c.ctx)
			if err != nil {
				c.mediaFailed("module read", err)
				return
			}
			if r := c.media.Load(); r != nil && c.ctx.Err() == nil {
				if err := r.WritePCM(p); err != nil {
					c.mediaFailed("RTP write", err)
					return
				}
			}
		}
	}()
	poll := time.NewTicker(500 * time.Millisecond)
	defer poll.Stop()
	stateChanges := callStateChanges(c.device)
	lastState := ""
	for {
		select {
		case <-c.ctx.Done():
			return
		case winner = <-c.winner:
			c.owner.g.server.sipAccountsMu.Lock()
			valid := c.owner.router.ActionCurrent(telephony.Action{Kind: telephony.AnswerModule, Call: c.call.ID, Module: c.call.Module, Registration: winner.reg.ID})
			c.owner.g.server.sipAccountsMu.Unlock()
			if !valid || c.ctx.Err() != nil {
				return
			}
			ctx, cancel := context.WithTimeout(c.ctx, 8*time.Second)
			err := c.device.Answer(ctx)
			cancel()
			if err != nil {
				c.stop(moduleCallResult(err))
				return
			}
			log.Printf("SIP incoming %d %s: client answered, waiting for carrier media", c.call.ID, c.call.Module)
			continue
		case <-poll.C:
		case <-stateChanges:
		}
		state, err := readCallState(c.ctx, c.device.State)
		if c.ctx.Err() != nil {
			return
		}
		if err != nil {
			log.Printf("SIP incoming %d %s: module state unavailable: %v", c.call.ID, c.call.Module, err)
			c.stop("module_error")
			return
		}
		if state != lastState {
			log.Printf("SIP incoming %d %s: module=%s", c.call.ID, c.call.Module, state)
			lastState = state
		}
		if state == "idle" {
			c.stop("remote_cancelled")
			return
		}
		if state != "active" || winner == nil || c.connected.Load() {
			continue
		}
		c.owner.g.server.sipAccountsMu.Lock()
		err = c.owner.router.Connected(c.call.ID)
		c.owner.g.server.sipAccountsMu.Unlock()
		if err != nil || c.ctx.Err() != nil {
			return
		}
		c.connected.Store(true)
		log.Printf("SIP incoming %d %s: connected", c.call.ID, c.call.Module)
		winner.recordConnected()
		c.media.Store(winner.rtp)
		audio.Add(1)
		go func(l *sipIncomingLeg) {
			defer audio.Done()
			var err error
			if c.sample.wifi {
				err = forwardCallPCM(c.ctx, &c.connected, l.rtp.ReadPCM, c.device.WritePCM)
			} else {
				err = playCellularPCM(c.ctx, l.rtp.ReadPCM, c.device.WritePCM, &playback)
			}
			if err != nil && c.ctx.Err() == nil {
				c.mediaFailed("uplink", err)
			}
		}(winner)
	}
}
