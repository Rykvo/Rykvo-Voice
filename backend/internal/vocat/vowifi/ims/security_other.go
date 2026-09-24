//go:build !linux

package ims

import (
	"context"
	"errors"
)

type unsupportedIPSecInstaller struct{}

func defaultIPSecInstaller() IPSecSAInstaller {
	return unsupportedIPSecInstaller{}
}

func (unsupportedIPSecInstaller) Install(context.Context, IPSecSAConfig) (IPSecSAHandle, error) {
	return nil, errors.New("ims: ipsec-3gpp XFRM installation is supported only on Linux")
}
