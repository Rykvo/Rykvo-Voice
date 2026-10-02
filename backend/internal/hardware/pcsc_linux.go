//go:build linux

package hardware

import (
	"context"
	"errors"
	"github.com/ebitengine/purego"
	"strings"
	"time"
	"unsafe"
)

func withPCSC(reader string, use func(*cardChannel, string) (any, error)) (any, error) {
	lib, err := purego.Dlopen("libpcsclite.so.1", purego.RTLD_NOW|purego.RTLD_LOCAL)
	if err != nil {
		return nil, err
	}
	defer purego.Dlclose(lib)
	var establish func(uintptr, uintptr, uintptr, *uintptr) uintptr
	var release func(uintptr) uintptr
	var list func(uintptr, uintptr, *byte, *uintptr) uintptr
	var connect func(uintptr, string, uintptr, uintptr, *uintptr, *uintptr) uintptr
	var disconnect func(uintptr, uintptr) uintptr
	var transmit func(uintptr, *[2]uintptr, *byte, uintptr, uintptr, *byte, *uintptr) uintptr
	var begin func(uintptr) uintptr
	var end func(uintptr, uintptr) uintptr
	var attribute func(uintptr, uintptr, *byte, *uintptr) uintptr
	for _, item := range []struct {
		name string
		fn   any
	}{{"SCardEstablishContext", &establish}, {"SCardReleaseContext", &release}, {"SCardListReaders", &list}, {"SCardConnect", &connect}, {"SCardDisconnect", &disconnect}, {"SCardTransmit", &transmit}, {"SCardBeginTransaction", &begin}, {"SCardEndTransaction", &end}} {
		fn, e := purego.Dlsym(lib, item.name)
		if e != nil {
			return nil, e
		}
		purego.RegisterFunc(item.fn, fn)
	}
	var handle uintptr
	if fn, e := purego.Dlsym(lib, "SCardGetAttrib"); e == nil {
		purego.RegisterFunc(&attribute, fn)
	}
	if establish(2, 0, 0, &handle) != 0 {
		return nil, errors.New("PCSC_UNAVAILABLE")
	}
	defer release(handle)
	if reader == "" {
		var size uintptr
		code := list(handle, 0, nil, &size)
		if code == 0x8010002e {
			return []string{}, nil
		}
		if code != 0 || size > 65536 {
			return nil, errors.New("PCSC_UNAVAILABLE")
		}
		if size == 0 {
			return []string{}, nil
		}
		data := make([]byte, size)
		if list(handle, 0, &data[0], &size) != 0 {
			return nil, errors.New("PCSC_UNAVAILABLE")
		}
		return strings.Split(strings.TrimRight(string(data), "\x00"), "\x00"), nil
	}
	r := Reading{Model: reader, SIM: "unknown", UpdatedAt: time.Now().UTC()}
	var card, protocol uintptr
	code := connect(handle, reader, 1, 3, &card, &protocol)
	if code == 0x8010000c {
		r.Responsive = true
		r.SIM = "absent"
		return nil, errors.New("NO_SIM")
	}
	if code != 0 {
		return nil, errors.New("DEVICE_BUSY")
	}
	defer disconnect(card, 0)
	if attribute != nil {
		var serial [256]byte
		size := uintptr(len(serial))
		if attribute(card, 0x00010103, &serial[0], &size) == 0 && size <= uintptr(len(serial)) {
			r.ReaderSerial = strings.TrimSpace(strings.TrimRight(string(serial[:size]), "\x00"))
		}
	}
	if begin(card) != 0 {
		return nil, errors.New("DEVICE_BUSY")
	}
	defer end(card, 0)
	r.Responsive = true
	pci := [2]uintptr{protocol, unsafe.Sizeof([2]uintptr{})}
	send := func(apdu []byte) ([]byte, error) {
		var out [65538]byte
		size := uintptr(len(out))
		if transmit(card, &pci, &apdu[0], uintptr(len(apdu)), 0, &out[0], &size) != 0 || size < 2 || size > uintptr(len(out)) {
			return nil, errors.New("CARD_READ_FAILED")
		}
		return append([]byte(nil), out[:size]...), nil
	}
	return use(&cardChannel{ctx: context.Background(), close: func() error { return nil }, send: func(_ context.Context, apdu []byte) ([]byte, error) { return send(apdu) }}, r.ReaderSerial)
}

func nativeCard(reader string) (any, error) {
	result, err := withPCSC(reader, func(channel *cardChannel, serial string) (any, error) {
		r := Reading{Model: reader, ReaderSerial: serial, SIM: "unknown", Responsive: true, UpdatedAt: time.Now().UTC()}
		send := func(apdu []byte) ([]byte, error) { return channel.Transmit(apdu) }
		var payload []byte
		for _, apdu := range [][]byte{{0, 0xa4, 0, 0, 2, 0x3f, 0}, {0, 0xa4, 0, 0, 2, 0x2f, 0xe2}, {0, 0xb0, 0, 0, 10}} {
			data, e := send(apdu)
			if e != nil {
				r.Issue = "CARD_READ_FAILED"
				return r, nil
			}
			for tries := 0; tries < 3 && data[len(data)-2] == 0x61; tries++ {
				data, e = send([]byte{0, 0xc0, 0, 0, data[len(data)-1]})
				if e != nil {
					r.Issue = "CARD_READ_FAILED"
					return r, nil
				}
			}
			if data[len(data)-2] != 0x90 || data[len(data)-1] != 0 {
				r.Issue = "CARD_UNSUPPORTED"
				return r, nil
			}
			payload = data[:len(data)-2]
		}
		r.ICCID = decodeICCID(payload)
		if r.ICCID == "" {
			r.Issue = "CARD_UNSUPPORTED"
		} else {
			r.SIM = "READY"
		}
		return r, nil
	})
	if err != nil && reader != "" {
		r := Reading{Model: reader, SIM: "unknown", Issue: errorCode(err), UpdatedAt: time.Now().UTC()}
		if err.Error() == "NO_SIM" {
			r.SIM = "absent"
			r.Responsive = true
			r.Issue = ""
		}
		return r, nil
	}
	return result, err
}
