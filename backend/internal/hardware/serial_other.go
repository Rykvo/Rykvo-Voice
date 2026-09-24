//go:build !linux

package hardware

import "errors"

func openAT(string) (atPort, error) { return nil, errors.New("LINUX_REQUIRED") }
