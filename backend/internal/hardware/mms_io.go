package hardware

import (
	"context"
	"errors"
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

// Short USB packets flush EC20 socket data before waiting for SEND OK.
func mmsDataWrite(parent context.Context, at *atSession, data []byte) error {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	for len(data) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		n := len(data)
		if n > 511 {
			n = 511
		}
		written, err := at.port.Write(data[:n])
		if err != nil {
			return err
		}
		if written < 0 || written > n {
			return errors.New("MMS_INVALID_WRITE")
		}
		data = data[written:]
		if written == 0 {
			// The nonblocking serial port returns zero when its poll times out.
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(5 * time.Millisecond):
			}
		}
	}
	return nil
}
