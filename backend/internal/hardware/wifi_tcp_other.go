//go:build !linux

package hardware

import (
	"context"
	"errors"
	"net"
	"time"
)

type wifiTCP struct {
	frames chan wifiStreamFrame
	client net.Conn
}

func openWiFiTCP(context.Context, *wifiIMS) (*wifiTCP, error) {
	return nil, errors.New("WIFI_TCP_PLATFORM_UNSUPPORTED")
}
func (t *wifiTCP) close() {}
func (t *wifiTCP) write(context.Context, net.Conn, []byte) error {
	return errors.New("WIFI_TCP_PLATFORM_UNSUPPORTED")
}
func (t *wifiTCP) pump(context.Context, time.Duration) error {
	return errors.New("WIFI_TCP_PLATFORM_UNSUPPORTED")
}
