//go:build !linux

package ike

import (
	"context"
	"errors"
	"net"
)

func (*Session) DialData(context.Context, string, string) (net.Conn, error) {
	return nil, errors.New("MMS_NETWORK_REQUIRED")
}
