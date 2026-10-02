//go:build linux && (amd64 || arm64)

package ims

import (
	"sync"

	"github.com/ebitengine/purego"
)

var opusLibrary = sync.OnceValue(func() *opusAPI {
	handle, err := purego.Dlopen("libopus.so.0", purego.RTLD_NOW|purego.RTLD_LOCAL)
	if err != nil {
		return nil
	}
	a := &opusAPI{}
	for _, symbol := range []struct {
		name string
		fn   any
	}{
		{"opus_encoder_create", &a.encoderCreate}, {"opus_decoder_create", &a.decoderCreate},
		{"opus_encoder_destroy", &a.encoderDestroy}, {"opus_decoder_destroy", &a.decoderDestroy},
		{"opus_encode", &a.encode}, {"opus_decode", &a.decode},
		{"opus_encoder_ctl", &a.encoderCTL}, {"opus_packet_get_nb_samples", &a.packetSamples},
	} {
		addr, err := purego.Dlsym(handle, symbol.name)
		if err != nil {
			_ = purego.Dlclose(handle)
			return nil
		}
		purego.RegisterFunc(symbol.fn, addr)
	}
	// Keep loaded while any call may still hold native state.
	return a
})

func loadOpus() *opusAPI { return opusLibrary() }
