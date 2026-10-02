//go:build linux

package hardware

import (
	"context"
	"errors"
	"github.com/damonto/euicc-go/driver"
	"github.com/damonto/wwan-go/qcom"
	"github.com/damonto/wwan-go/qcom/qmi"
	"time"
)

type qmiCard struct {
	ctx      context.Context
	client   *qcom.Client
	channel  byte
	received int
}

func openQMICard(ctx context.Context, device string) (driver.SmartCardChannel, error) {
	t, err := qmi.Open(ctx, qmi.WithProxy(device))
	if err != nil {
		return nil, errors.New("QMI_UNAVAILABLE")
	}
	c, err := qcom.NewClient(t, qcom.WithSlot(1))
	if err != nil {
		t.Close()
		return nil, err
	}
	return &qmiCard{ctx: ctx, client: c}, nil
}
func (c *qmiCard) Connect() error    { return c.ctx.Err() }
func (c *qmiCard) Disconnect() error { return c.client.Close() }
func (c *qmiCard) OpenLogicalChannel(aid []byte) (byte, error) {
	ctx, cancel := context.WithTimeout(c.ctx, 30*time.Second)
	defer cancel()
	n, err := c.client.OpenLogicalChannel(ctx, aid)
	c.channel = n
	return n, err
}
func (c *qmiCard) CloseLogicalChannel(n byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return c.client.CloseLogicalChannel(ctx, n)
}
func (c *qmiCard) Transmit(command []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(c.ctx, 30*time.Second)
	defer cancel()
	v, err := c.client.SendAPDU(ctx, c.channel, command)
	c.received += len(v)
	if c.received > 8<<20 {
		return nil, errors.New("INVALID_RESPONSE")
	}
	return v, err
}
