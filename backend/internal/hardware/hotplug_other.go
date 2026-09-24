//go:build !linux

package hardware

import "context"

func Watch(context.Context) <-chan struct{} { return nil }
