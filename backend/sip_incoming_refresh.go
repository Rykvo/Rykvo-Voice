package main

import (
	"strings"
	"time"

	"github.com/emiago/sipgo/sip"
)

// Caller holds l.mu. A dialog belongs to its authenticated endpoint and tags.
func (l *sipIncomingLeg) matchesDialog(req *sip.Request) bool {
	r, res := l.request, l.response
	return r != nil && res != nil && req.From() != nil && req.To() != nil && req.CallID() != nil && req.CSeq() != nil &&
		req.Source() == l.target.Source && strings.EqualFold(req.Transport(), l.target.Transport) &&
		req.CallID().Value() == r.CallID().Value() && req.From().Params.GetOr("tag", "") == res.To().Params.GetOr("tag", "") &&
		req.To().Params.GetOr("tag", "") == r.From().Params.GetOr("tag", "")
}

// A receiving APP may refresh its SDP immediately after answering; this is not a new call.
func (c *sipCalls) incomingRefreshLocked(req *sip.Request, tx sip.ServerTransaction) <-chan struct{} {
	for _, call := range c.incoming {
		for _, l := range call.legs {
			l.mu.Lock()
			matches := l.matchesDialog(req) && l.selected.Load() && l.ctx.Err() == nil && !l.peerClosed.Load()
			if !matches {
				l.mu.Unlock()
				continue
			}
			code, reason := 200, "OK"
			if l.refreshACK != nil {
				code, reason = 491, "Request Pending"
			} else if req.CSeq().SeqNo <= l.remoteSeq {
				code, reason = 500, "CSeq Out of Order"
			} else {
				l.remoteSeq = req.CSeq().SeqNo
				if l.rtp == nil || req.ContentType() == nil || !strings.EqualFold(strings.TrimSpace(strings.Split(req.ContentType().Value(), ";")[0]), "application/sdp") ||
					req.Contact() == nil || req.Contact().Address.Wildcard || req.Contact().Address.Host == "" ||
					req.GetHeader("Record-Route") != nil || l.rtp.RefreshOffer(req.Body()) != nil {
					code, reason = 488, "Not Acceptable Here"
				}
			}
			if code != 200 {
				l.mu.Unlock()
				_ = tx.Respond(sip.NewResponseFromRequest(req, code, reason, nil))
				return nil
			}
			ack := make(chan struct{}, 1)
			l.refreshSeq, l.refreshACK = req.CSeq().SeqNo, ack
			res := sip.NewResponseFromRequest(req, 200, "OK", l.rtp.Answer(l.public))
			res.AppendHeader(sip.HeaderClone(l.request.Contact()))
			res.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
			l.mu.Unlock()
			done := make(chan struct{})
			c.wait.Add(1)
			go func() {
				defer c.wait.Done()
				defer close(done)
				defer func() { l.mu.Lock(); l.refreshACK = nil; l.mu.Unlock() }()
				deadline := time.NewTimer(32 * time.Second)
				defer deadline.Stop()
				repeat := time.NewTicker(time.Second)
				defer repeat.Stop()
				if tx.Respond(res) != nil {
					l.stop("peer_unavailable")
					return
				}
				acks := tx.Acks()
				for {
					select {
					case <-l.ctx.Done():
						return
					case <-ack:
						return
					case r, ok := <-acks:
						if !ok {
							acks = nil
							continue
						}
						if r != nil {
							c.g.server.sipAccountsMu.Lock()
							c.incomingDialogLocked(r, nil)
							c.g.server.sipAccountsMu.Unlock()
						}
					case <-deadline.C:
						l.stop("call_timeout")
						return
					case <-repeat.C:
						if tx.Respond(res) != nil {
							l.stop("peer_unavailable")
							return
						}
					}
				}
			}()
			return done
		}
	}
	_ = tx.Respond(sip.NewResponseFromRequest(req, 481, "Call Does Not Exist", nil))
	return nil
}
