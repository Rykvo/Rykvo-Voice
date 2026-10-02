package ike

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

type sessionRelay struct {
	transport datagramTransport
	suite     negotiatedSuite
	keys      ikeKeys
	spii      [8]byte
	spir      [8]byte
	deleteID  uint32
	natt      bool
	keepalive time.Duration

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	esp    chan []byte

	mu      sync.Mutex
	lastErr error
}

func newSessionRelay(
	transport datagramTransport,
	suite negotiatedSuite,
	keys ikeKeys,
	initiatorSPI [8]byte,
	responderSPI [8]byte,
	deleteMessageID uint32,
	natt bool,
	keepalive time.Duration,
) *sessionRelay {
	if keepalive <= 0 {
		keepalive = 15 * time.Second
	}
	ctx, cancel := context.WithCancel(context.Background())
	relay := &sessionRelay{
		transport: transport,
		suite:     suite,
		keys:      keys,
		spii:      initiatorSPI,
		spir:      responderSPI,
		deleteID:  deleteMessageID,
		natt:      natt,
		keepalive: keepalive,
		ctx:       ctx,
		cancel:    cancel,
		done:      make(chan struct{}),
		esp:       make(chan []byte, 64),
	}
	go relay.run()
	return relay
}

func (relay *sessionRelay) run() {
	defer close(relay.done)
	defer close(relay.esp)
	buffer := make([]byte, 65535)
	lastKeepalive := time.Now()
	for {
		if err := relay.ctx.Err(); err != nil {
			return
		}
		n, isIKE, err := relay.transport.ReceiveSessionPacket(relay.ctx, buffer)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, net.ErrClosed) {
				return
			}
			if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
				if relay.natt && time.Since(lastKeepalive) >= relay.keepalive {
					if sendErr := relay.transport.SendSessionPacket(relay.ctx, []byte{0xff}, false); sendErr != nil {
						relay.fail(sendErr)
						return
					}
					lastKeepalive = time.Now()
				}
				continue
			}
			relay.fail(err)
			return
		}
		packet := append([]byte(nil), buffer[:n]...)
		if isIKE {
			if err := relay.handleIKE(packet); err != nil {
				if errors.Is(err, errMismatchedSessionSPIs) {
					// A reconnect can reuse the same NAT mapping while the ePDG still
					// has packets queued for the previous IKE SA. Those packets are
					// unrelated to this authenticated session and must be discarded;
					// treating one as fatal tears down the newly established CHILD_SA.
					continue
				}
				relay.fail(err)
				return
			}
			continue
		}
		if len(packet) == 1 && packet[0] == 0xff {
			// Peer NAT keepalive.
			continue
		}
		if len(packet) < 8 {
			// Unauthenticated network input must not tear down the session.
			continue
		}
		select {
		case relay.esp <- packet:
		default:
			// Keep the sole socket reader available for IKE/DPD if the
			// data-plane consumer falls behind.
		case <-relay.ctx.Done():
			return
		}
	}
}

var errMismatchedSessionSPIs = errors.New("ike: session packet has mismatched SPIs")

func (relay *sessionRelay) handleIKE(packet []byte) error {
	header, _, err := parseIKEPacket(packet)
	if err != nil {
		return err
	}
	if header.InitiatorSPI != relay.spii || header.ResponderSPI != relay.spir {
		return errMismatchedSessionSPIs
	}
	if header.Flags&flagResponse != 0 {
		return nil
	}
	if header.Exchange != exchangeInformational {
		return fmt.Errorf("ike: unsupported responder-initiated exchange %d", header.Exchange)
	}
	decryptedHeader, payloads, err := decryptPayloads(packet, relay.suite, relay.keys.SKer, relay.keys.SKar)
	if err != nil {
		return err
	}
	if len(payloads) != 0 {
		return errors.New("ike: responder INFORMATIONAL request is not an empty DPD probe")
	}
	response, err := encryptPayloads(ikeHeader{
		InitiatorSPI: relay.spii,
		ResponderSPI: relay.spir,
		Exchange:     exchangeInformational,
		Flags:        flagInitiator | flagResponse,
		MessageID:    decryptedHeader.MessageID,
	}, nil, relay.suite, relay.keys.SKei, relay.keys.SKai, nil)
	if err != nil {
		return err
	}
	return relay.transport.SendSessionPacket(relay.ctx, response, true)
}

func (relay *sessionRelay) fail(err error) {
	relay.mu.Lock()
	if relay.lastErr == nil {
		relay.lastErr = err
	}
	relay.mu.Unlock()
	relay.cancel()
}

func (relay *sessionRelay) SendESP(ctx context.Context, packet []byte) error {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-relay.done:
		return relay.terminalError()
	default:
	}
	return relay.transport.SendSessionPacket(ctx, packet, false)
}

func (relay *sessionRelay) ReceiveESP(ctx context.Context, buffer []byte) (int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case packet, ok := <-relay.esp:
		if !ok {
			return 0, relay.terminalError()
		}
		if len(packet) > len(buffer) {
			return 0, errors.New("ike: ESP receive buffer is too small")
		}
		copy(buffer, packet)
		return len(packet), nil
	}
}

func (relay *sessionRelay) terminalError() error {
	relay.mu.Lock()
	defer relay.mu.Unlock()
	if relay.lastErr != nil {
		return relay.lastErr
	}
	return net.ErrClosed
}

func (relay *sessionRelay) Close() error {
	relay.cancel()
	// Closing the socket also wakes reads that do not observe cancellation.
	transportErr := relay.transport.Close()
	<-relay.done
	// The data plane reports lastErr; cleanup reports only local close failures.
	if onlyAlreadyClosed(transportErr) {
		return nil
	}
	return transportErr
}

// Unlike errors.Is, keep real failures joined with net.ErrClosed.
func onlyAlreadyClosed(err error) bool {
	if err == net.ErrClosed {
		return true
	}
	if many, ok := err.(interface{ Unwrap() []error }); ok {
		children := many.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !onlyAlreadyClosed(child) {
				return false
			}
		}
		return true
	}
	if one, ok := err.(interface{ Unwrap() error }); ok {
		return onlyAlreadyClosed(one.Unwrap())
	}
	return false
}

func (relay *sessionRelay) CloseWithDelete(ctx context.Context, notify ...func()) error {
	deleteErr := relay.sendIKEDelete(ctx)
	localErr := relay.Close()
	if deleteErr != nil && localErr == nil && len(notify) > 0 && notify[0] != nil {
		// Local cleanup succeeded; retain the remote release failure as a warning.
		notify[0]()
		return nil
	}
	return errors.Join(deleteErr, localErr)
}

func (relay *sessionRelay) sendIKEDelete(ctx context.Context) error {
	return sendIKESADelete(ctx, relay.transport, relay.suite, relay.keys, relay.spii, relay.spir, relay.deleteID)
}

func sendIKESADelete(
	ctx context.Context,
	transport datagramTransport,
	suite negotiatedSuite,
	keys ikeKeys,
	initiatorSPI [8]byte,
	responderSPI [8]byte,
	messageID uint32,
) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		// Teardown is often called with the operation context already canceled.
		// Give the protocol-level release a short independent chance to leave.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), time.Second)
		defer cancel()
	}
	request, err := encryptPayloads(ikeHeader{
		InitiatorSPI: initiatorSPI,
		ResponderSPI: responderSPI,
		Exchange:     exchangeInformational,
		Flags:        flagInitiator,
		MessageID:    messageID,
	}, []payload{{
		Type: payloadDelete,
		Body: []byte{protocolIKE, 0, 0, 0},
	}}, suite, keys.SKei, keys.SKai, nil)
	if err != nil {
		return fmt.Errorf("ike: build IKE SA delete: %w", err)
	}
	if err := transport.SendSessionPacket(ctx, request, true); err != nil {
		return fmt.Errorf("ike: send IKE SA delete: %w", err)
	}
	return nil
}

var _ NATTPacketRelay = (*sessionRelay)(nil)
