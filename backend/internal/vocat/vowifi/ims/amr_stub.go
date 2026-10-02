//go:build !linux || (!amd64 && !arm64)

package ims

func loadAMR(bool) *amrAPI { return nil }
