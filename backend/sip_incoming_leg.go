package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	randv2 "math/rand/v2"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"rykvo.local/auth/internal/sipregistrar"
	"rykvo.local/auth/internal/telephony"
	"rykvo.local/auth/internal/vocat/vowifi/ims"
)

func sipToken() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(b[:])
}

type sipIncomingLeg struct {
	owner             *sipIncoming
	reg               telephony.Registration
	target            sipregistrar.Registration
	client            *sipgo.Client
	network           sipAccountNetwork
	bind, public      net.IP
	port              int
	number            string
	lowBandwidthAudio bool
	ctx               context.Context
	cancel            context.CancelFunc
	mu                sync.Mutex
	request           *sip.Request
	response          *sip.Response
	rtp               *ims.ClientRTP
	record, outcome   string
	ended             time.Time
	peerClosed        atomic.Bool
	finished          atomic.Bool
	selected          atomic.Bool
	answerReady       atomic.Bool
	remoteSeq         uint32
	refreshSeq        uint32
	refreshACK        chan struct{}
}

func newIncomingLeg(c *sipIncoming, reg telephony.Registration, target sipregistrar.Registration, client *sipgo.Client, network sipAccountNetwork, bind, public net.IP, port int, number string) *sipIncomingLeg {
	ctx, cancel := context.WithCancel(c.ctx)
	return &sipIncomingLeg{lowBandwidthAudio: c.owner.g.lowBandwidthAudioLocked(reg.Account), owner: c, reg: reg, target: target, client: client, network: network, bind: bind, public: public, port: port, number: number, ctx: ctx, cancel: cancel}
}
func (l *sipIncomingLeg) stop(reason string) {
	if reason == "no_answer" && l.selected.Load() {
		reason = "call_timeout" // APP answered, but the carrier never completed connection.
	}
	l.mu.Lock()
	if l.outcome == "" {
		l.outcome = reason
	}
	if l.ended.IsZero() {
		l.ended = time.Now()
	}
	l.mu.Unlock()
	l.cancel()
}
func (l *sipIncomingLeg) finish() {
	l.stop("no_answer")
	if l.rtp != nil {
		l.rtp.Close()
	}
	l.finished.Store(true)
	l.owner.finishAccountRecord(l.reg.Account)
	c := l.owner.owner
	c.g.server.sipAccountsMu.Lock()
	if l.peerClosed.Load() {
		c.actionsLocked(c.router.ClientClosed(l.owner.call.ID, l.reg.ID))
	}
	l.owner.forgetLocked()
	c.g.server.sipAccountsMu.Unlock()
}

func (l *sipIncomingLeg) run() {
	defer l.finish()
	c := l.owner.owner
	span := l.network.End - l.network.Start + 1
	if span <= 0 || l.network.Start < 1024 || l.network.End > 65535 {
		l.peerClosed.Store(true)
		l.stop("media_network_error")
		return
	}
	for i, offset := 0, randv2.IntN(span); i < min(span, 256); i++ {
		port := l.network.Start + (offset+i)%span
		if port == l.port || port == 2019 || port == 8080 || port >= 51820 && port <= 51822 {
			continue
		}
		rtp, err := ims.NewClientRTPOffer(l.bind, port, l.lowBandwidthAudio)
		if err == nil {
			l.rtp = rtp
			break
		}
	}
	if l.rtp == nil {
		l.peerClosed.Store(true)
		l.stop("media_network_error")
		return
	}
	req := sip.NewRequest(sip.INVITE, l.target.Contact.Address)
	from := &sip.FromHeader{Address: sip.Uri{Scheme: "sip", User: l.number, Host: l.public.String()}}
	if from.Address.User == "" {
		from.Address.User = "anonymous"
	}
	from.Params.Add("tag", sipToken())
	req.AppendHeader(from)
	req.AppendHeader(&sip.ContactHeader{Address: sip.Uri{Scheme: "sip", Host: l.public.String(), Port: l.port}})
	req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	req.SetBody(l.rtp.Offer(l.public))
	req.SetDestination(l.target.Source)
	req.SetTransport(l.target.Transport)
	// Build once before publishing to in-dialog request handlers.
	if sipgo.ClientRequestBuild(l.client, req) != nil {
		l.peerClosed.Store(true)
		return
	}
	c.g.server.sipAccountsMu.Lock()
	valid := c.router.ActionCurrent(telephony.Action{Kind: telephony.Ring, Call: l.owner.call.ID, Module: l.owner.call.Module, Registration: l.reg.ID}) && l.ctx.Err() == nil
	if valid {
		l.mu.Lock()
		l.request = req
		l.mu.Unlock()
	}
	c.g.server.sipAccountsMu.Unlock()
	if !valid {
		l.peerClosed.Store(true)
		return
	}
	sendCtx, sendCancel := context.WithTimeout(l.ctx, 3*time.Second)
	tx, err := l.client.TransactionRequest(sendCtx, req, func(*sipgo.Client, *sip.Request) error { return nil })
	sendCancel()
	if err != nil {
		l.peerClosed.Store(true)
		l.stop("peer_unavailable")
		return
	}
	defer func() {
		l.mu.Lock()
		accepted := l.response != nil
		l.mu.Unlock()
		// Let Timer M absorb accepted-INVITE retransmissions, even after BYE.
		if !accepted {
			tx.Terminate()
		}
	}()
	// One row per offered account, shared by that account's registered phones.
	recordCtx, cancel := context.WithTimeout(c.ctx, 3*time.Second)
	record, err := l.owner.recordOffer(recordCtx, l.reg.Account, l.number)
	cancel()
	l.mu.Lock()
	l.record = record
	l.mu.Unlock()
	if err != nil {
		l.stop("service_unavailable")
	}
	repeated := make(chan *sip.Response, 8)
	tx.OnRetransmission(func(r *sip.Response) {
		if !l.matchesResponse(r) {
			return
		}
		l.mu.Lock()
		accepted := l.response
		l.mu.Unlock()
		if accepted != nil && l.answerReady.Load() && r.To() != nil && r.To().Params.GetOr("tag", "") == accepted.To().Params.GetOr("tag", "") {
			_ = l.client.WriteRequest(l.dialogRequest(sip.ACK, accepted))
			return
		}
		select {
		case repeated <- r:
		default:
		}
	})
	done := l.ctx.Done()
	txDone := tx.Done()
	var deadline <-chan time.Time
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	ending, provisional, cancelled := false, false, false
	for {
		select {
		case <-done:
			done = nil
			ending = true
			if l.peerClosed.Load() {
				return
			}
			l.mu.Lock()
			accepted := l.response != nil
			l.mu.Unlock()
			if accepted {
				l.endDialog()
				return
			}
			timer = time.NewTimer(32 * time.Second)
			deadline = timer.C
		case <-deadline:
			return // End the local transaction; worker completion still waits for module cleanup.
		case <-txDone:
			txDone = nil
			l.mu.Lock()
			accepted := l.response != nil
			l.mu.Unlock()
			if !accepted {
				l.stop("peer_unavailable")
				return
			}
		case r := <-tx.Responses():
			if l.handleResponse(r) {
				return
			}
			provisional = provisional || r.IsProvisional()
		case r := <-repeated:
			if l.handleResponse(r) {
				return
			}
		}
		if ending && provisional && !cancelled {
			cancelled = true
			l.sendCancel()
		}
	}
}

func (l *sipIncomingLeg) handleResponse(res *sip.Response) bool {
	if !l.matchesResponse(res) {
		return false
	}
	if res.IsProvisional() {
		return false
	}
	if !res.IsSuccess() {
		l.peerClosed.Store(true)
		l.stop(incomingClientResult(res.StatusCode))
		return true
	}
	if res.To() == nil || res.To().Params.GetOr("tag", "") == "" || res.Contact() == nil || res.Contact().Address.Wildcard || res.Contact().Address.Host == "" || res.GetHeader("Record-Route") != nil {
		l.stop("peer_unavailable")
		return false
	}
	l.mu.Lock()
	previous := l.response
	if previous == nil {
		l.response = res.Clone()
	}
	l.mu.Unlock()
	if previous != nil {
		if l.client.WriteRequest(l.dialogRequest(sip.ACK, res)) != nil {
			l.stop("peer_unavailable")
		}
		if res.To().Params.GetOr("tag", "") != previous.To().Params.GetOr("tag", "") {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			_, _ = l.client.Do(ctx, l.dialogRequest(sip.BYE, res))
			cancel()
		}
		return false
	}
	peer, _, _ := net.SplitHostPort(l.target.Source)
	if l.rtp.AcceptAnswer(net.ParseIP(peer), res.Body()) != nil {
		l.stop("unsupported_audio")
	}
	c := l.owner.owner
	c.g.server.sipAccountsMu.Lock()
	accepted := false
	if l.ctx.Err() == nil {
		_, actions, err := c.router.Answer(l.reg, l.owner.call.ID)
		c.actionsLocked(actions)
		accepted = err == nil
	}
	if accepted {
		l.selected.Store(true)
	} else {
		l.stop("cancelled")
	}
	c.g.server.sipAccountsMu.Unlock()
	// Publish the selected dialog before ACK: an APP may immediately re-INVITE.
	l.answerReady.Store(true)
	if l.client.WriteRequest(l.dialogRequest(sip.ACK, res)) != nil {
		l.stop("peer_unavailable")
		if accepted {
			l.owner.stop("peer_unavailable")
		}
		accepted = false
	}
	if accepted {
		l.owner.winner <- l
	}
	if !accepted {
		l.endDialog()
		return true
	}
	return false
}

func (l *sipIncomingLeg) matchesResponse(res *sip.Response) bool {
	return res != nil && res.Source() == l.target.Source && strings.EqualFold(res.Transport(), l.target.Transport) &&
		res.CallID() != nil && res.CSeq() != nil && res.From() != nil && res.CSeq().MethodName == sip.INVITE &&
		res.CallID().Value() == l.request.CallID().Value() && res.CSeq().SeqNo == l.request.CSeq().SeqNo &&
		res.From().Params.GetOr("tag", "") == l.request.From().Params.GetOr("tag", "")
}

func (l *sipIncomingLeg) dialogRequest(method sip.RequestMethod, res *sip.Response) *sip.Request {
	r := sip.NewRequest(method, res.Contact().Address)
	r.AppendHeader(sip.HeaderClone(l.request.From()))
	r.AppendHeader(sip.HeaderClone(res.To()))
	r.AppendHeader(sip.HeaderClone(l.request.CallID()))
	seq := l.request.CSeq().SeqNo
	if method != sip.ACK {
		seq++
	}
	r.AppendHeader(&sip.CSeqHeader{SeqNo: seq, MethodName: method})
	r.SetDestination(l.target.Source)
	r.SetTransport(l.target.Transport)
	return r
}
func (l *sipIncomingLeg) sendCancel() {
	r := sip.NewRequest(sip.CANCEL, l.request.Recipient)
	for _, h := range []sip.Header{l.request.Via(), l.request.From(), l.request.To(), l.request.CallID()} {
		r.AppendHeader(sip.HeaderClone(h))
	}
	r.AppendHeader(&sip.CSeqHeader{SeqNo: l.request.CSeq().SeqNo, MethodName: sip.CANCEL})
	r.SetDestination(l.target.Source)
	r.SetTransport(l.target.Transport)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _ = l.client.Do(ctx, r) // CANCEL 200 alone does not end the INVITE.
}
func (l *sipIncomingLeg) endDialog() {
	if l.peerClosed.Load() {
		return
	}
	l.mu.Lock()
	res := l.response
	l.mu.Unlock()
	if res == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	r, e := l.client.Do(ctx, l.dialogRequest(sip.BYE, res))
	cancel()
	if sipByeConfirmed(l.record, r, e) {
		l.peerClosed.Store(true)
	}
}

func (c *sipCalls) incomingDialogLocked(req *sip.Request, tx sip.ServerTransaction) bool {
	if (req.Method != sip.BYE && req.Method != sip.ACK) || req.From() == nil || req.To() == nil || req.CallID() == nil || req.CSeq() == nil {
		return false
	}
	for _, call := range c.incoming {
		for _, l := range call.legs {
			l.mu.Lock()
			matches := l.matchesDialog(req)
			if req.Method == sip.ACK {
				if matches && l.refreshACK != nil && req.CSeq().SeqNo == l.refreshSeq {
					select {
					case l.refreshACK <- struct{}{}:
					default:
					}
				}
				l.mu.Unlock()
				if matches {
					return true
				}
				continue
			}
			matches = matches && req.CSeq().SeqNo > l.remoteSeq
			if matches {
				l.remoteSeq = req.CSeq().SeqNo
			}
			l.mu.Unlock()
			if !matches {
				continue
			}
			_ = tx.Respond(sip.NewResponseFromRequest(req, 200, "OK", nil))
			l.peerClosed.Store(true)
			l.stop("cancelled")
			c.actionsLocked(c.router.ClientClosed(call.call.ID, l.reg.ID))
			call.forgetLocked()
			return true
		}
	}
	return false
}
