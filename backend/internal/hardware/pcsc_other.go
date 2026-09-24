//go:build !linux

package hardware

import "errors"

func nativeCard(string) (any, error) { return nil, errors.New("LINUX_REQUIRED") }

func withPCSC(string, func(*cardChannel, string) (any, error)) (any, error) {
	return nil, errors.New("LINUX_REQUIRED")
}
