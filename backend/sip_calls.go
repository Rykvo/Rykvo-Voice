package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log"
	randv2 "math/rand/v2"
	"net"
	"net/http"
	"strconv"
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

// All scheduler mutations share the account credential/CRUD gate.
type sipCalls struct {
	g             *sipGateway
	router        *telephony.Router
	registrations map[string]telephony.Registration
	keys          map[string]string
	policies      map[string]bool
	modules       map[string]bool
	active        map[telephony.ID]*sipOutgoing
	ctx           context.Context
	open          func(context.Context, sipVoiceSample) (moduleVoice, error)
	wait          sync.WaitGroup
}
type sipOutgoing struct {
	owner      *sipCalls
	call       telephony.Call
	reg        telephony.Registration
	request    *sip.Request
	tx         sip.ServerTransaction
	client     *sipgo.Client
	contact    sip.ContactHeader
	ctx        context.Context
	cancel     context.CancelFunc
	ack        chan struct{}
	ackOnce    sync.Once
	peerClosed atomic.Bool
	mu         sync.Mutex
	rtp        *ims.ClientRTP
	answered   bool
	accepted   atomic.Bool
	acceptedAt time.Time
	final      atomic.Bool
	record     string
}

type moduleVoice interface {
	Dial(context.Context, string) error
	State(context.Context) (string, error)
	Hangup(context.Context) error
	ReadPCM(context.Context) ([]int16, error)
	WritePCM([]int16) error
	Close()
}

func newSIPCalls(g *sipGateway) *sipCalls {
	c := &sipCalls{g: g, router: telephony.New(), registrations: map[string]telephony.Registration{}, keys: map[string]string{}, policies: map[string]bool{}, modules: map[string]bool{}, active: map[telephony.ID]*sipOutgoing{}, ctx: context.Background()}
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
		if a.Kind == telephony.StopMedia || a.Kind == telephony.TerminateClient || a.Kind == telephony.HangupModule {
			if call := c.active[a.Call]; call != nil {
				call.stop()
			}
		}
	}
}
func (c *sipCalls) syncLocked(ctx context.Context, accounts []sipregistrar.Account) bool {
	if c.g.server.db == nil {
		return false
	}
	rows, err := c.g.server.db.Query(ctx, `SELECT a.id,a.credential_revision,a.allocation,a.receive_calls,COALESCE(array_agg(m.module_id) FILTER(WHERE m.module_id IS NOT NULL),'{}'::bigint[]) FROM sip_accounts a LEFT JOIN sip_account_modules m ON m.account_id=a.id GROUP BY a.id`)
	next := map[string]bool{}
	if err == nil {
		for rows.Next() {
			var id, allocation string
			var revision uint64
			var receive bool
			var modules []int64
			if rows.Scan(&id, &revision, &allocation, &receive, &modules) != nil {
				err = errors.New("policy read")
				break
			}
			p := telephony.Policy{Account: id, Revision: revision, All: allocation == "all", Receive: receive}
			for _, id := range modules {
				p.Modules = append(p.Modules, moduleID(id))
			}
			a, e := c.router.PutPolicy(p)
			c.actionsLocked(a)
			if e == nil {
				next[id] = true
			}
		}
		if rows.Err() != nil {
			err = rows.Err()
		}
		rows.Close()
	}
	if err != nil {
		for _, call := range c.active {
			call.stop()
		}
		return false
	}
	for id := range c.policies {
		if !next[id] {
			c.actionsLocked(c.router.DeleteAccount(id))
		}
	}
	c.policies = next
	current := c.g.registrar.Registrations()
	for id, reg := range c.registrations {
		if _, ok := current[id]; !ok || !next[id] {
			a, _ := c.router.Logout(reg)
			c.actionsLocked(a)
			delete(c.registrations, id)
			delete(c.keys, id)
		}
	}
	for _, a := range accounts {
		r, ok := current[a.ID]
		if !ok || !next[a.ID] {
			continue
		}
		key := r.CallID + "\x00" + r.Instance + "\x00" + r.Source + "/" + r.Transport + "/" + strconv.FormatInt(a.Revision, 10)
		ttl := time.Until(r.Expires)
		if ttl <= 0 {
			continue
		}
		if c.keys[a.ID] == key {
			reg, e := c.router.Refresh(c.registrations[a.ID], ttl)
			if e == nil {
				c.registrations[a.ID] = reg
				continue
			}
		}
		reg, actions, e := c.router.Register(a.ID, uint64(a.Revision), ttl)
		c.actionsLocked(actions)
		if e == nil {
			c.registrations[a.ID] = reg
			c.keys[a.ID] = key
		}
	}
	c.actionsLocked(c.router.Tick())
	return true
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
	if m.ctx == nil || m.ctx.Err() != nil || time.Since(m.lastScan) > 20*time.Second {
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
func (c *sipCalls) inviteLocked(req *sip.Request, tx sip.ServerTransaction, port int, address string, ua *sipgo.UserAgent) <-chan struct{} {
	a, _, res := c.g.registrar.AuthenticateInvite(req, port)
	if res != nil {
		_ = tx.Respond(res)
		return nil
	}
	if c.ctx.Err() != nil || c.g.server.modules == nil {
		_ = tx.Respond(sip.NewResponseFromRequest(req, 503, "Voice Unavailable", nil))
		return nil
	}
	if req.Contact() == nil || req.Contact().Address.Wildcard || req.Contact().Address.Host == "" || req.GetHeader("Record-Route") != nil || req.To().Params.Has("tag") || !validDialNumber(req.Recipient.User) {
		_ = tx.Respond(sip.NewResponseFromRequest(req, 400, "Bad Call Request", nil))
		return nil
	}
	ctx, cancel := context.WithTimeout(c.ctx, 3*time.Second)
	accounts, err := c.g.readAccounts(ctx)
	if err == nil && !c.syncLocked(ctx, accounts) {
		err = errors.New("voice policy unavailable")
	}
	network, netErr := c.g.server.sipAccountNetwork(ctx, &http.Request{Host: address})
	cancel()
	if err != nil || netErr != nil {
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
	reg := c.registrations[a.ID]
	call, actions, err := c.router.Dial(reg)
	c.actionsLocked(actions)
	if err != nil {
		_ = tx.Respond(sip.NewResponseFromRequest(req, 503, "No Available Voice Module", nil))
		return nil
	}
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
	client, err := sipgo.NewClient(ua, sipgo.WithClientHostname(public), sipgo.WithClientPort(port), sipgo.WithClientConnectionAddr(address))
	if err != nil || net.ParseIP(public) == nil {
		stop()
		c.router.ModuleClosed(call.ID)
		c.router.ClientClosed(call.ID, reg.ID)
		_ = tx.Respond(sip.NewResponseFromRequest(req, 503, "Media Network Unavailable", nil))
		return nil
	}
	leg := &sipOutgoing{owner: c, call: call, reg: reg, request: copy, tx: tx, client: client, ctx: callCtx, cancel: stop, ack: make(chan struct{}), contact: sip.ContactHeader{Address: sip.Uri{Scheme: "sip", Host: public, Port: port}}}
	c.active[call.ID] = leg
	tx.OnCancel(func(r *sip.Request) {
		if r.Source() == req.Source() {
			leg.peerClosed.Store(true)
			leg.stop()
		}
	})
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
	g := c.owner.g
	m := g.server.modules
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
		audio.Wait()
		if c.rtp != nil && c.record != "" {
			var flowDropped uint64
			if d, ok := device.(interface{ DroppedPCMSamples() uint64 }); ok {
				flowDropped = d.DroppedPCMSamples()
			}
			if sample.wifi {
				code := 0
				if d, ok := device.(interface{ SIPCode() int }); ok {
					code = d.SIPCode()
				}
				log.Printf("SIP call %s Wi-Fi audio: RTP=%+v CarrierCode=%d", c.record, c.rtp.ReceiveStats(), code)
			} else {
				log.Printf("SIP call %s audio: RTP=%+v USB={Warmup:%d Missing:%d Dropped:%d LateWrites:%d FlowDropped:%d}",
					c.record, c.rtp.ReceiveStats(), playback.warmup.Load(), playback.missing.Load(), playback.dropped.Load(), playback.lateWrites.Load(), flowDropped)
			}
		}
		peerDone := make(chan struct{})
		go func() { defer close(peerDone); c.endPeer() }()
		defer func() { <-peerDone }()
		if device != nil {
			for {
				clean, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				err := device.Hangup(clean)
				cancel()
				if err == nil {
					break
				}
				select {
				case <-c.owner.ctx.Done():
					device.Close()
					c.finish(false)
					return
				case <-time.After(3 * time.Second):
				}
			}
			device.Close()
		}
		c.finish(true)
	}()
	peer, _, _ := net.SplitHostPort(c.request.Source())
	start := network.Start
	span := network.End - start + 1
	if start < 1024 || network.End > 65535 || span <= 0 {
		_ = c.respond(503, "Media Network Unavailable", nil)
		return
	}
	for n, offset := 0, randv2.IntN(span); n < min(span, 256); n++ {
		port := start + (offset+n)%span
		if port == c.request.Recipient.Port || port == 2019 || port == 8080 || port >= 51820 && port <= 51822 {
			continue
		}
		rtp, err := ims.OpenClientRTP(bind, port, net.ParseIP(peer), c.request.Body())
		if err == nil {
			c.mu.Lock()
			c.rtp = rtp
			c.mu.Unlock()
			break
		}
		if !strings.Contains(err.Error(), "bind") && !strings.Contains(err.Error(), "address already") {
			_ = c.respond(488, "Unsupported Audio", nil)
			return
		}
	}
	if c.rtp == nil {
		_ = c.respond(503, "No Media Port", nil)
		return
	}
	op, opCancel := context.WithTimeout(c.ctx, 20*time.Second)
	var err error
	device, err = c.owner.open(op, sample)
	opCancel()
	if err != nil {
		_ = c.respond(503, "Module Voice Unavailable", nil)
		return
	}
	g.server.sipAccountsMu.Lock()
	current := c.owner.router.ActionCurrent(telephony.Action{Kind: telephony.DialModule, Call: c.call.ID, Module: c.call.Module, Registration: c.reg.ID})
	g.server.sipAccountsMu.Unlock()
	if !current || c.ctx.Err() != nil {
		return
	}
	c.record = c.request.To().Params.GetOr("tag", "")
	if g.server.db != nil {
		recordCtx, done := context.WithTimeout(c.ctx, 3*time.Second)
		_, err = g.server.db.Exec(recordCtx, `INSERT INTO sip_call_records(id,account_id,module_id,peer) VALUES($1,$2,$3,$4)`, c.record, c.reg.Account, c.call.Module, c.request.Recipient.User)
		done()
		if err != nil {
			_ = c.respond(503, "Call Record Unavailable", nil)
			return
		}
	}
	var mediaActive atomic.Bool
	audio.Add(1)
	go func() {
		defer audio.Done()
		if err := forwardCallPCM(c.ctx, &mediaActive, device.ReadPCM, c.rtp.WritePCM); err != nil {
			if errors.Is(err, hardware.ErrVoiceEnded) {
				return
			} // State polling relays the carrier response.
			c.mediaFailed("downlink", err)
		}
	}()
	op, opCancel = context.WithTimeout(c.ctx, 8*time.Second)
	err = device.Dial(op, c.request.Recipient.User)
	opCancel()
	if err != nil {
		_ = c.respond(503, "Dial Failed", nil)
		return
	}
	timer := time.NewTimer(55 * time.Second)
	defer timer.Stop()
	poll := time.NewTicker(500 * time.Millisecond)
	defer poll.Stop()
	ringing := false
	for !c.answered {
		select {
		case <-c.ctx.Done():
			return
		case <-timer.C:
			_ = c.respond(480, "No Answer", nil)
			return
		case <-poll.C:
			check, done := context.WithTimeout(c.ctx, 3*time.Second)
			state, e := device.State(check)
			done()
			if e != nil {
				_ = c.respond(503, "Module Lost", nil)
				return
			}
			switch state {
			case "idle":
				code, reason := 486, "Call Ended"
				if d, ok := device.(interface{ SIPCode() int }); ok {
					switch d.SIPCode() {
					case 403:
						code, reason = 403, "Carrier Rejected"
					case 404:
						code, reason = 404, "Number Not Found"
					case 480:
						code, reason = 480, "Temporarily Unavailable"
					case 488:
						code, reason = 488, "Unsupported Carrier Audio"
					case 503:
						code, reason = 503, "Carrier Unavailable"
					case 603:
						code, reason = 603, "Decline"
					}
				}
				_ = c.respond(code, reason, nil)
				return
			case "ringing":
				if !ringing {
					_ = c.respond(180, "Ringing", nil)
					ringing = true
				}
			case "active":
				c.answered = true
				c.recordState("connected")
			}
		}
	}
	body := c.rtp.Answer(public)
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
	audio.Add(1)
	go func() {
		defer audio.Done()
		var err error
		if sample.wifi {
			err = forwardCallPCM(c.ctx, &mediaActive, c.rtp.ReadPCM, device.WritePCM)
		} else {
			err = playCellularPCM(c.ctx, c.rtp.ReadPCM, device.WritePCM, &playback)
		}
		if err != nil {
			c.mediaFailed("uplink", err)
		}
	}()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-c.tx.Acks():
			// Drain retransmitted same-branch ACKs while media is active.
		case <-poll.C:
			check, done := context.WithTimeout(c.ctx, 3*time.Second)
			state, e := device.State(check)
			done()
			if e != nil || state != "active" {
				return
			}
		}
	}
}
func (c *sipOutgoing) mediaFailed(stage string, err error) {
	if c.ctx.Err() == nil {
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
		if err == nil && (res.StatusCode == 200 || res.StatusCode == 481) {
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
	// An unanswered BYE remains reserved until a real peer BYE arrives.
	c.owner.g.server.sipAccountsMu.Unlock()
}
func (c *sipOutgoing) recordState(state string) {
	if c.record == "" || c.owner.g.server.db == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := c.owner.g.server.db.Exec(ctx, `UPDATE sip_call_records SET state=$2,answered_at=CASE WHEN $2='connected' THEN COALESCE(answered_at,now()) ELSE answered_at END,ended_at=CASE WHEN $2='ended' THEN now() ELSE ended_at END WHERE id=$1`, c.record, state)
	if err != nil {
		log.Print("SIP call record update failed")
	}
}
func (c *sipOutgoing) finish(moduleClosed bool) {
	if moduleClosed {
		c.recordState("ended")
	} else {
		c.recordState("cleanup_pending")
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
