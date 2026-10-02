//go:build !linux || (!amd64 && !arm64)

package ims

func loadOpus() *opusAPI { return nil }
