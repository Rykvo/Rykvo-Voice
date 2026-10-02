//go:build !linux

package ike

import (
	"context"
	"errors"
)

type unsupportedInstaller struct{}

func defaultChildSAInstaller() ChildSAInstaller {
	return unsupportedInstaller{}
}

func (unsupportedInstaller) Install(context.Context, ChildSAConfig) (ChildSAHandle, error) {
	return nil, errors.New("kernel CHILD_SA installation is supported only on Linux")
}
