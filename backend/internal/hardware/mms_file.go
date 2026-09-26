package hardware

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// EC20 R08 can emit CONNECT before its USB data handler is ready.
func mmsDataReady(ctx context.Context) error {
	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Short USB packets flush EC20 file data before waiting for the 1 KiB ACK.
func mmsDataWrite(ctx context.Context, at *atSession, data []byte) error {
	for len(data) > 0 {
		n := len(data)
		if n > 511 {
			n = 511
		}
		if e := smsATWrite(ctx, at, data[:n]); e != nil {
			return e
		}
		data = data[n:]
	}
	return nil
}

func mmsChunk(data []byte) int {
	n := len(data)
	if n > 1024 {
		n = 1024
	}
	if i := bytes.Index(data[:n], []byte("+++")); i >= 0 {
		n = i + 2
	}
	return n
}

func (b *mmsBearer) fileAvailable(ctx context.Context, name string) error {
	lines, e := b.command(ctx, `AT+QFLST="RAM:*"`, 3*time.Second)
	if e != nil {
		return errors.New("MMS_STORAGE_UNAVAILABLE")
	}
	for _, line := range lines {
		if strings.HasPrefix(line, "+QFLST:") {
			f := fields(line)
			if len(f) != 2 {
				return errors.New("MMS_STORAGE_UNAVAILABLE")
			}
			if strings.EqualFold(f[0], name) {
				return errors.New("MMS_STORAGE_BUSY")
			}
		}
	}
	return nil
}

// QFUPL treats +++ as an escape. Split those bytes across separate QFWRITE
// commands, then read back every byte: no image recompression or content changes.
func (b *mmsBearer) uploadChunks(parent context.Context, name string, data []byte) error {
	ctx, cancel := context.WithTimeout(parent, 90*time.Second)
	defer cancel()
	lines, e := b.command(ctx, fmt.Sprintf("AT+QFOPEN=%q,0", name), 2*time.Second)
	if e != nil {
		return errors.New("MMS_UPLOAD_FAILED")
	}
	handle := -1
	for _, line := range lines {
		if strings.HasPrefix(line, "+QFOPEN:") {
			handle, e = strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "+QFOPEN:")))
			if e != nil {
				return errors.New("MMS_UPLOAD_FAILED")
			}
		}
	}
	if handle < 0 || handle > 65535 {
		return errors.New("MMS_UPLOAD_FAILED")
	}
	defer func() {
		if b.healthy {
			clean, stop := context.WithTimeout(context.Background(), 3*time.Second)
			defer stop()
			_, _ = b.command(clean, fmt.Sprintf("AT+QFCLOSE=%d", handle), 3*time.Second)
		}
	}()
	for offset := 0; offset < len(data); {
		n := mmsChunk(data[offset:])
		e = smsATWrite(ctx, b.at, []byte(fmt.Sprintf("AT+QFWRITE=%d,%d,10\r", handle, n)))
		if e == nil {
			_, e = mmsWait(ctx, b.at, "CONNECT")
		}
		if e == nil {
			e = mmsDataReady(ctx)
		}
		if e == nil {
			e = mmsDataWrite(ctx, b.at, data[offset:offset+n])
		}
		var line string
		if e == nil {
			line, e = mmsWait(ctx, b.at, "+QFWRITE")
		}
		if e == nil {
			_, e = mmsWait(ctx, b.at, "OK")
		}
		if e != nil {
			b.healthy = false
			return errors.New("MMS_UPLOAD_FAILED")
		}
		f := fields(line)
		if len(f) != 2 || f[0] != strconv.Itoa(n) || f[1] != strconv.Itoa(offset+n) {
			return errors.New("MMS_UPLOAD_CHECKSUM")
		}
		offset += n
	}
	if _, e = b.command(ctx, fmt.Sprintf("AT+QFSEEK=%d,0,0", handle), 2*time.Second); e != nil {
		return errors.New("MMS_UPLOAD_FAILED")
	}
	for offset := 0; offset < len(data); {
		n := len(data) - offset
		if n > 1024 {
			n = 1024
		}
		e = smsATWrite(ctx, b.at, []byte(fmt.Sprintf("AT+QFREAD=%d,%d\r", handle, n)))
		if e != nil {
			b.healthy = false
			return errors.New("MMS_UPLOAD_FAILED")
		}
		for {
			line, err := mmsLine(ctx, b.at)
			if err != nil {
				b.healthy = false
				return errors.New("MMS_UPLOAD_FAILED")
			}
			if line == "CONNECT" || line == fmt.Sprintf("CONNECT %d", n) {
				break
			}
			if strings.HasPrefix(line, "CONNECT") || strings.Contains(line, "ERROR") {
				b.healthy = false
				return errors.New("MMS_UPLOAD_FAILED")
			}
		}
		got, e := mmsReadBytes(ctx, b.at, n)
		if e == nil {
			_, e = mmsWait(ctx, b.at, "OK")
		}
		if e != nil {
			b.healthy = false
			return errors.New("MMS_UPLOAD_FAILED")
		}
		if !bytes.Equal(got, data[offset:offset+n]) {
			return errors.New("MMS_UPLOAD_CHECKSUM")
		}
		offset += n
	}
	return nil
}
