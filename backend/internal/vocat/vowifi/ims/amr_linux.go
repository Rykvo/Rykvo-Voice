//go:build linux && (amd64 || arm64)

package ims

import (
	"sync"

	"github.com/ebitengine/purego"
)

var amrNBLibrary = sync.OnceValue(func() *amrAPI { return openAMRLibrary(false) })
var amrWBLibrary = sync.OnceValue(func() *amrAPI { return openAMRLibrary(true) })

func loadAMR(wide bool) *amrAPI {
	if wide {
		return amrWBLibrary()
	}
	return amrNBLibrary()
}

func openAMRLibrary(wide bool) *amrAPI {
	decoderLib := "libopencore-amrnb.so.0"
	if wide {
		decoderLib = "libopencore-amrwb.so.0"
	}
	dh, err := purego.Dlopen(decoderLib, purego.RTLD_NOW|purego.RTLD_LOCAL)
	if err != nil {
		return nil
	}
	eh := dh
	success := false
	defer func() {
		if !success {
			purego.Dlclose(dh)
			if eh != dh && eh != 0 {
				purego.Dlclose(eh)
			}
		}
	}()
	if wide {
		eh, err = purego.Dlopen("libvo-amrwbenc.so.0", purego.RTLD_NOW|purego.RTLD_LOCAL)
		if err != nil {
			return nil
		}
	}
	a := &amrAPI{}
	bind := func(handle uintptr, name string, fn any) bool {
		addr, err := purego.Dlsym(handle, name)
		if err != nil {
			return false
		}
		purego.RegisterFunc(fn, addr)
		return true
	}
	if wide {
		if !bind(eh, "E_IF_init", &a.encoderCreate) || !bind(eh, "E_IF_exit", &a.encoderDestroy) || !bind(eh, "E_IF_encode", &a.encode) ||
			!bind(dh, "D_IF_init", &a.decoderCreate) || !bind(dh, "D_IF_exit", &a.decoderDestroy) || !bind(dh, "D_IF_decode", &a.decode) {
			return nil
		}
	} else {
		var create func(int32) uintptr
		if !bind(eh, "Encoder_Interface_init", &create) || !bind(eh, "Encoder_Interface_exit", &a.encoderDestroy) || !bind(eh, "Encoder_Interface_Encode", &a.encode) ||
			!bind(dh, "Decoder_Interface_init", &a.decoderCreate) || !bind(dh, "Decoder_Interface_exit", &a.decoderDestroy) || !bind(dh, "Decoder_Interface_Decode", &a.decode) {
			return nil
		}
		a.encoderCreate = func() uintptr { return create(0) }
	}
	success = true // Native states may outlive any individual call; retain handles.
	return a
}
