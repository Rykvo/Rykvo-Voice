package ims

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	rtpClockRate     = 8000
	rtpPacketSamples = 160
)

type rtpMedia struct {
	conn       *net.UDPConn
	routeMu    sync.Mutex
	route      func(context.Context, *net.UDPAddr, *net.UDPAddr) (io.Closer, error)
	routeLease io.Closer

	mu           sync.RWMutex
	remote       *net.UDPAddr
	signaledPort int
	codec        string
	payloadType  byte
	allowOpus    bool
	opus         *opusCodec
	opusBitrate  int
	allowAMR     bool
	amr          *amrCodec
	offeredAMR   bool

	writeMu   sync.Mutex
	pending   []int16
	sequence  uint16
	timestamp uint32
	ssrc      uint32

	readMu    sync.Mutex
	readTimer *time.Timer
	jitter    rtpJitter
	wake      chan struct{}
	closed    chan struct{}
	close     sync.Once
}

func newRTPMedia(local net.IP) (*rtpMedia, error) {
	return newRTPMediaAt(local, 0)
}
func newRTPMediaAt(local net.IP, port int) (*rtpMedia, error) {
	address := &net.UDPAddr{IP: local, Port: port}
	connection, err := net.ListenUDP("udp", address)
	if err != nil {
		return nil, fmt.Errorf("ims: open RTP socket: %w", err)
	}
	seed := make([]byte, 10)
	if _, err := io.ReadFull(cryptorand.Reader, seed); err != nil {
		_ = connection.Close()
		return nil, fmt.Errorf("ims: initialize RTP state: %w", err)
	}
	media := &rtpMedia{
		conn: connection, sequence: binary.BigEndian.Uint16(seed[:2]),
		timestamp: binary.BigEndian.Uint32(seed[2:6]), ssrc: binary.BigEndian.Uint32(seed[6:]),
		wake: make(chan struct{}, 1), closed: make(chan struct{}),
	}
	go media.receive()
	return media, nil
}

func (media *rtpMedia) Codec() string {
	media.mu.RLock()
	defer media.mu.RUnlock()
	return media.codec
}

func (media *rtpMedia) ready() bool {
	media.mu.RLock()
	defer media.mu.RUnlock()
	return media.remote != nil && media.codec != ""
}

func (media *rtpMedia) offerSDP(local net.IP) []byte {
	// Advertise only codecs implemented by the PCM bridge.
	if media.allowOpus && opusAvailable() {
		return media.buildSDP(local, "111 8 0", append(opusAttributes(opusPayload),
			"a=rtpmap:8 PCMA/8000", "a=rtpmap:0 PCMU/8000"))
	}
	formats := "8 0"
	attributes := []string{
		"a=rtpmap:8 PCMA/8000",
		"a=rtpmap:0 PCMU/8000",
	}
	if media.allowAMR {
		for pt := byte(96); pt <= 99; pt++ {
			o, _ := amrOfferOptions(pt)
			if amrAvailable(o.wide) {
				formats += " " + strconv.Itoa(int(pt))
				attributes = append(attributes, o.attributes(pt)...)
			}
		}
		media.mu.Lock()
		media.offeredAMR = true
		media.mu.Unlock()
		attributes = append(attributes, "a=maxptime:200")
	}
	return media.buildSDP(local, formats, attributes)
}

func (media *rtpMedia) answerSDP(local net.IP) []byte {
	media.mu.RLock()
	codec, payload, amr := media.codec, media.payloadType, media.amr
	media.mu.RUnlock()
	if codec == "" {
		return media.offerSDP(local)
	}
	if codec == "OPUS" {
		return media.buildSDP(local, strconv.Itoa(int(payload)), opusAttributes(payload))
	}
	if amr != nil {
		return media.buildSDP(local, strconv.Itoa(int(payload)), append(amr.options.attributes(payload), "a=maxptime:200"))
	}
	rate := rtpClockRate
	return media.buildSDP(local, strconv.Itoa(int(payload)), []string{
		fmt.Sprintf("a=rtpmap:%d %s/%d", payload, codec, rate),
	})
}

func (media *rtpMedia) buildSDP(local net.IP, formats string, attributes []string) []byte {
	if local == nil || local.IsUnspecified() {
		if udp, ok := media.conn.LocalAddr().(*net.UDPAddr); ok {
			local = udp.IP
		}
	}
	if local == nil || local.IsUnspecified() {
		local = net.IPv4zero
	}
	family := "IP4"
	if local.To4() == nil {
		family = "IP6"
	}
	port := media.conn.LocalAddr().(*net.UDPAddr).Port
	sessionID := time.Now().UnixNano()
	lines := []string{
		"v=0",
		fmt.Sprintf("o=- %d %d IN %s %s", sessionID, sessionID, family, local.String()),
		"s=VoCat",
		fmt.Sprintf("c=IN %s %s", family, local.String()),
		"t=0 0",
		fmt.Sprintf("m=audio %d RTP/AVP %s", port, formats),
	}
	lines = append(lines, attributes...)
	lines = append(lines, "a=ptime:20", "a=sendrecv", "")
	return []byte(strings.Join(lines, "\r\n"))
}

func (media *rtpMedia) configureRemote(body []byte) error {
	address, port, formats, mappings, err := parseAudioSDP(body)
	if err != nil {
		return err
	}
	var codec string
	var payload byte
	bitrate := 12000
	var amrConfig amrOptions
	media.mu.RLock()
	offeredAMR := media.offeredAMR
	media.mu.RUnlock()
	if media.allowOpus && opusAvailable() {
		for _, value := range formats {
			pt, e := strconv.Atoi(value)
			if e == nil && pt >= 96 && pt <= 127 && strings.EqualFold(mappings[pt], "opus/48000/2") {
				if rate, ok := opusRemoteOptions(body, pt); ok {
					codec, payload, bitrate = "OPUS", byte(pt), rate
					break
				}
			}
		}
	}
	for _, value := range formats {
		if codec != "" {
			break
		}
		parsed, parseErr := strconv.Atoi(value)
		if parseErr != nil || parsed < 0 || parsed > 127 {
			continue
		}
		mapping := strings.ToUpper(mappings[parsed])
		if media.allowAMR && parsed >= 96 {
			if options, ok := amrRemoteOptions(body, parsed, mapping); ok && amrAvailable(options.wide) {
				if offeredAMR {
					expected, known := amrOfferOptions(byte(parsed))
					if !known || expected.wide != options.wide || expected.octet != options.octet {
						continue
					}
				}
				codec, payload, amrConfig = options.name(), byte(parsed), options
				break
			}
		}
		if mapping == "" {
			switch parsed {
			case 0:
				mapping = "PCMU/8000"
			case 8:
				mapping = "PCMA/8000"
			}
		}
		parts := strings.Split(mapping, "/")
		if len(parts) < 2 || len(parts) > 3 || parts[1] != "8000" || (len(parts) == 3 && parts[2] != "1") {
			continue
		}
		name := parts[0]
		if name != "PCMA" && name != "PCMU" {
			continue
		}
		if (parsed == 0 && name != "PCMU") || (parsed == 8 && name != "PCMA") || (parsed != 0 && parsed != 8 && parsed < 96) {
			continue
		}
		codec, payload = name, byte(parsed)
		break
	}

	if codec == "" {
		return errors.New("ims: remote SDP has no usable audio format")
	}
	media.routeMu.Lock()
	defer media.routeMu.Unlock()
	select {
	case <-media.closed:
		return io.EOF
	default:
	}
	media.mu.RLock()
	existing, oldCodec, oldPayload, oldBitrate, oldAMR := media.opus, media.codec, media.payloadType, media.opusBitrate, media.amr
	media.mu.RUnlock()
	if oldCodec != "" && (oldCodec == "OPUS" || codec == "OPUS") && (codec != oldCodec || payload != oldPayload || bitrate != oldBitrate) {
		return errors.New("ims: changed Opus negotiation")
	}
	isAMR := codec == "AMR" || codec == "AMR-WB"
	if oldCodec != "" && (oldAMR != nil || isAMR) && (oldAMR == nil || !isAMR || oldCodec != codec || oldPayload != payload || oldAMR.options != amrConfig) {
		return errors.New("ims: changed AMR negotiation")
	}
	var createdAMR *amrCodec
	if isAMR && oldAMR == nil {
		createdAMR, err = newAMRCodec(amrConfig)
		if err != nil {
			return err
		}
		defer func() {
			if createdAMR != nil {
				createdAMR.close()
			}
		}()
	}
	var created *opusCodec
	if codec == "OPUS" && existing == nil {
		created, err = newOpusCodec(bitrate)
		if err != nil {
			return err
		}
		defer func() {
			if created != nil {
				created.close()
			}
		}()
	}
	remote := &net.UDPAddr{IP: address, Port: port}
	media.mu.RLock()
	sameEndpoint := media.remote != nil && media.remote.IP.Equal(address) && media.signaledPort == port
	media.mu.RUnlock()
	var old io.Closer
	if media.route != nil && (media.routeLease == nil || !sameEndpoint) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		lease, err := media.route(ctx, media.conn.LocalAddr().(*net.UDPAddr), remote)
		cancel()
		if err != nil {
			return errors.New("ims: negotiated media route unavailable")
		}
		if lease == nil {
			return errors.New("ims: missing media route lease")
		}
		old, media.routeLease = media.routeLease, lease
	}
	media.mu.Lock()
	if created != nil {
		media.jitter.mu.Lock()
		media.jitter.compressed = &compressedPlayout{decode: created.decode, clockScale: 6, quantum: 20}
		media.jitter.mu.Unlock()
		media.opus, media.opusBitrate, created = created, bitrate, nil
	}
	if createdAMR != nil {
		media.jitter.mu.Lock()
		media.jitter.compressed = &compressedPlayout{decode: createdAMR.decode, clockScale: amrConfig.clockScale(), quantum: 160}
		media.jitter.mu.Unlock()
		media.amr, createdAMR = createdAMR, nil
	}
	if oldCodec != "" && (!sameEndpoint || oldCodec != codec || oldPayload != payload) {
		media.jitter.mu.Lock()
		media.jitter.initialized, media.jitter.playing = false, false
		clear(media.jitter.present[:])
		if media.jitter.compressed != nil {
			clear(media.jitter.compressed.packets[:])
		}
		media.jitter.stats.Resets++
		media.jitter.mu.Unlock()
	}
	if sameEndpoint {
		remote.Port = media.remote.Port
	}
	media.remote = remote
	media.signaledPort = port
	media.codec = codec
	media.payloadType = payload
	media.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	return nil
}

func parseAudioSDP(body []byte) (net.IP, int, []string, map[int]string, error) {
	var sessionIP, mediaIP net.IP
	var port int
	var formats []string
	mappings := make(map[int]string)
	inAudio, seenMedia := false, false
lines:
	for _, raw := range strings.Split(strings.ReplaceAll(string(body), "\r\n", "\n"), "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case strings.HasPrefix(line, "m="):
			if inAudio {
				break lines
			}
			seenMedia = true
			fields := strings.Fields(strings.TrimPrefix(line, "m="))
			inAudio = len(fields) >= 4 && strings.EqualFold(fields[0], "audio") && strings.EqualFold(fields[2], "RTP/AVP")
			if inAudio {
				port, _ = strconv.Atoi(strings.Split(fields[1], "/")[0])
				formats = append([]string(nil), fields[3:]...)
			}
		case strings.HasPrefix(line, "c="):
			fields := strings.Fields(strings.TrimPrefix(line, "c="))
			if len(fields) >= 3 {
				ip := net.ParseIP(strings.Split(fields[2], "/")[0])
				if inAudio {
					mediaIP = ip
				} else if !seenMedia {
					sessionIP = ip
				}
			}
		case inAudio && strings.HasPrefix(strings.ToLower(line), "a=rtpmap:"):
			fields := strings.Fields(line[len("a=rtpmap:"):])
			if len(fields) == 2 {
				pt, parseErr := strconv.Atoi(fields[0])
				if parseErr == nil {
					mappings[pt] = fields[1]
				}
			}
		}
	}
	if mediaIP == nil {
		mediaIP = sessionIP
	}
	if mediaIP == nil || port < 1 || port > 65535 || len(formats) == 0 {
		return nil, 0, nil, nil, errors.New("ims: remote SDP has no usable audio endpoint")
	}
	return mediaIP, port, formats, mappings, nil
}

func (media *rtpMedia) ReadPCM(ctx context.Context) ([]int16, error) {
	media.readMu.Lock()
	defer media.readMu.Unlock()
	if media.readTimer == nil {
		media.readTimer = time.NewTimer(time.Hour)
		media.readTimer.Stop()
	}
	timer := media.readTimer
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-media.closed:
			return nil, io.EOF
		default:
		}
		samples, wait := media.jitter.pop(time.Now())
		if samples != nil {
			return samples, nil
		}
		var tick <-chan time.Time
		wake := (<-chan struct{})(media.wake)
		if wait > 0 {
			timer.Reset(wait)
			tick = timer.C
			// New packets fill the window, not advance its playout deadline.
			wake = nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-media.closed:
			return nil, io.EOF
		case <-wake:
		case <-tick:
		}
		timer.Stop()
	}
}

func (media *rtpMedia) WritePCM(samples []int16) error {
	select {
	case <-media.closed:
		return io.EOF
	default:
	}
	media.mu.RLock()
	var remote *net.UDPAddr
	if media.remote != nil {
		copy := *media.remote
		remote = &copy
	}
	codec, payload, opus, amr := media.codec, media.payloadType, media.opus, media.amr
	media.mu.RUnlock()
	if remote == nil || (codec != "PCMA" && codec != "PCMU" && codec != "OPUS" && amr == nil) {
		return errors.New("ims: RTP media is not negotiated")
	}
	media.writeMu.Lock()
	defer media.writeMu.Unlock()
	media.pending = append(media.pending, samples...)
	for len(media.pending) >= rtpPacketSamples {
		var data []byte
		clockStep := uint32(rtpPacketSamples)
		if codec == "OPUS" {
			var err error
			data, err = opus.encode(media.pending[:rtpPacketSamples])
			if err != nil {
				return err
			}
			clockStep *= 6
		} else if amr != nil {
			var err error
			data, err = amr.encode(media.pending[:rtpPacketSamples])
			if err != nil {
				return err
			}
			clockStep *= amr.options.clockScale()
		} else {
			data = make([]byte, rtpPacketSamples)
			for index, sample := range media.pending[:rtpPacketSamples] {
				if codec == "PCMA" {
					data[index] = linearToALaw(sample)
				} else {
					data[index] = linearToMuLaw(sample)
				}
			}
		}
		packet := make([]byte, 12+len(data))
		packet[0], packet[1] = 0x80, payload
		binary.BigEndian.PutUint16(packet[2:4], media.sequence)
		binary.BigEndian.PutUint32(packet[4:8], media.timestamp)
		binary.BigEndian.PutUint32(packet[8:12], media.ssrc)
		copy(packet[12:], data)
		if _, err := media.conn.WriteToUDP(packet, remote); err != nil {
			return fmt.Errorf("ims: send RTP: %w", err)
		}
		media.pending = media.pending[rtpPacketSamples:]
		media.sequence++
		media.timestamp += clockStep
	}
	return nil
}

func (media *rtpMedia) receive() {
	packet := make([]byte, 2048)
	for {
		count, source, err := media.conn.ReadFromUDP(packet)
		if err != nil {
			return
		}
		if count < 12 || packet[0]>>6 != 2 {
			continue
		}
		header := 12 + int(packet[0]&0x0f)*4
		if packet[0]&0x10 != 0 {
			if count < header+4 {
				continue
			}
			header += 4 + int(binary.BigEndian.Uint16(packet[header+2:header+4]))*4
		}
		if header >= count {
			continue
		}
		if packet[0]&0x20 != 0 {
			padding := int(packet[count-1])
			if padding == 0 || padding >= count-header {
				continue
			}
			count -= padding
		}
		if count-header > 1600 {
			continue
		}
		sequence, timestamp, ssrc := binary.BigEndian.Uint16(packet[2:4]), binary.BigEndian.Uint32(packet[4:8]), binary.BigEndian.Uint32(packet[8:12])
		media.mu.Lock()
		remote, codec, payload, amr := media.remote, media.codec, media.payloadType, media.amr
		if remote == nil || !remote.IP.Equal(source.IP) || packet[1]&0x7f != payload {
			media.mu.Unlock()
			continue
		}
		accepted := false
		if codec == "OPUS" {
			accepted = media.jitter.pushOpus(sequence, timestamp, ssrc, packet[header:count], time.Now())
		} else if amr != nil {
			accepted = media.jitter.pushAMR(sequence, timestamp, ssrc, packet[header:count], amr, time.Now())
		} else {
			samples := make([]int16, count-header)
			for index, encoded := range packet[header:count] {
				if codec == "PCMA" {
					samples[index] = aLawToLinear(encoded)
				} else {
					samples[index] = muLawToLinear(encoded)
				}
			}
			accepted = media.jitter.push(sequence, timestamp, ssrc, samples, time.Now())
		}
		if accepted {
			media.remote.Port = source.Port // learn only from current-stream RTP
		}
		media.mu.Unlock()
		if accepted {
			select {
			case media.wake <- struct{}{}:
			default:
			}
		}
	}
}

func (media *rtpMedia) Close() error {
	media.close.Do(func() {
		close(media.closed)
		_ = media.conn.Close()
		media.routeMu.Lock()
		media.mu.RLock()
		opus, amr := media.opus, media.amr
		media.mu.RUnlock()
		opus.close()
		amr.close()
		if media.routeLease != nil {
			_ = media.routeLease.Close()
			media.routeLease = nil
		}
		media.routeMu.Unlock()
	})
	return nil
}

func linearToMuLaw(sample int16) byte {
	value := int(sample)
	sign := byte(0)
	if value < 0 {
		sign, value = 0x80, -value
		if value > 32767 {
			value = 32767
		}
	}
	value += 132
	if value > 32635 {
		value = 32635
	}
	exponent := 7
	for mask := 0x4000; exponent > 0 && value&mask == 0; mask >>= 1 {
		exponent--
	}
	mantissa := (value >> (exponent + 3)) & 0x0f
	return ^(sign | byte(exponent<<4) | byte(mantissa))
}

func muLawToLinear(value byte) int16 {
	value = ^value
	magnitude := ((int(value)&0x0f)<<3 + 132) << ((value & 0x70) >> 4)
	magnitude -= 132
	if value&0x80 != 0 {
		return int16(-magnitude)
	}
	return int16(magnitude)
}

func linearToALaw(sample int16) byte {
	value := int(sample)
	mask := byte(0xd5)
	if value < 0 {
		mask, value = 0x55, -value-1
	}
	if value > 32767 {
		value = 32767
	}
	var encoded byte
	if value < 256 {
		encoded = byte(value >> 4)
	} else {
		exponent := 1
		for threshold := 512; exponent < 7 && value >= threshold; threshold <<= 1 {
			exponent++
		}
		encoded = byte(exponent<<4) | byte((value>>(exponent+3))&0x0f)
	}
	return encoded ^ mask
}

func aLawToLinear(value byte) int16 {
	value ^= 0x55
	magnitude := int(value&0x0f)<<4 + 8
	exponent := int((value & 0x70) >> 4)
	if exponent != 0 {
		magnitude = (magnitude + 0x100) << (exponent - 1)
	}
	if value&0x80 == 0 {
		return int16(-magnitude)
	}
	return int16(magnitude)
}
