package hardware

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// A session owns its port and logical channel for the whole card transaction.
type cardChannel struct {
	ctx     context.Context
	send    func(context.Context, []byte) ([]byte, error)
	close   func() error
	channel byte
	bytes   int
}

func (c *cardChannel) Connect() error    { return c.ctx.Err() }
func (c *cardChannel) Disconnect() error { return c.close() }
func (c *cardChannel) Transmit(command []byte) ([]byte, error) {
	if err := c.ctx.Err(); err != nil {
		return nil, err
	}
	response, err := c.send(c.ctx, command)
	c.bytes += len(response)
	if len(response) < 2 && err == nil {
		err = errors.New("INVALID_RESPONSE")
	}
	if c.bytes > 8<<20 {
		err = errors.New("INVALID_RESPONSE")
	}
	return response, err
}
func (c *cardChannel) OpenLogicalChannel(aid []byte) (byte, error) {
	response, err := c.Transmit([]byte{0, 0x70, 0, 0, 1})
	if err != nil {
		return 0, err
	}
	if len(response) != 3 || !cardOK(response) || response[0] < 1 || response[0] > 19 {
		return 0, errors.New("EUICC_CHANNEL_UNAVAILABLE")
	}
	c.channel = response[0]
	cla := channelClass(0, c.channel)
	response, err = c.Transmit(append([]byte{cla, 0xa4, 4, 0, byte(len(aid))}, aid...))
	for n := 0; err == nil && len(response) >= 2 && response[len(response)-2] == 0x61 && n < 32; n++ {
		response, err = c.Transmit([]byte{channelClass(0x80, c.channel), 0xc0, 0, 0, response[len(response)-1]})
	}
	if err == nil && !cardOK(response) {
		err = errors.New("NO_EUICC")
	}
	if err != nil {
		_ = c.CloseLogicalChannel(c.channel)
		return 0, err
	}
	return c.channel, nil
}
func (c *cardChannel) CloseLogicalChannel(channel byte) error {
	if c.channel == 0 || c.channel != channel {
		return nil
	}
	c.channel = 0
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	response, err := c.send(ctx, []byte{0, 0x70, 0x80, channel, 0})
	if err == nil && !cardOK(response) {
		err = errors.New("EUICC_CHANNEL_UNAVAILABLE")
	}
	return err
}
func channelClass(cla, channel byte) byte {
	if channel < 4 {
		return cla&0x9c | channel
	}
	return cla&0xb0 | 0x40 | (channel - 4)
}
func cardOK(response []byte) bool {
	return len(response) >= 2 && response[len(response)-2] == 0x90 && response[len(response)-1] == 0
}
func parseCSIM(lines []string) ([]byte, error) {
	for _, line := range lines {
		if !strings.HasPrefix(line, "+CSIM:") {
			continue
		}
		values := fields(line)
		if len(values) != 2 {
			break
		}
		size, err := strconv.Atoi(values[0])
		data, decodeErr := hex.DecodeString(values[1])
		if err == nil && decodeErr == nil && size == len(values[1]) && len(data) >= 2 && len(data) <= 65536 {
			return data, nil
		}
	}
	return nil, errors.New("INVALID_RESPONSE")
}
func openATCard(ctx context.Context, c Candidate, expectedIMEI string) (*cardChannel, error) {
	last := errors.New("AT_PORT_MISSING")
	for _, port := range c.Ports {
		fd, err := openAT(port.Path)
		if err != nil {
			last = err
			continue
		}
		s := &atSession{port: fd}
		if _, err = s.query(ctx, "AT"); err != nil {
			fd.Close()
			last = err
			continue
		}
		if expectedIMEI != "" {
			lines, err := s.query(ctx, "AT+CGSN")
			if err != nil || digits(lines, 14, 17) != expectedIMEI {
				fd.Close()
				return nil, errors.New("DEVICE_CHANGED")
			}
		}
		return &cardChannel{ctx: ctx, close: fd.Close, send: func(ctx context.Context, apdu []byte) ([]byte, error) {
			command := fmt.Sprintf("AT+CSIM=%d,\"%X\"", len(apdu)*2, apdu)
			lines, err := s.exchange(ctx, command, 30*time.Second)
			if err != nil {
				return nil, err
			}
			return parseCSIM(lines)
		}}, nil
	}
	return nil, last
}
