package ims

import (
	"io"
	"net"
	"sync"
	"time"
)

func (session *Session) writeRuntime(data []byte) error {
	return writeSIPRuntime(session.conn, &session.writeMu, data)
}

// Keep responses, calls and keepalives serialized without an unbounded write.
// Initial REGISTER uses its existing transaction/cancellation deadline instead.
func writeSIPRuntime(conn net.Conn, mu *sync.Mutex, data []byte) error {
	mu.Lock()
	defer mu.Unlock()
	if err := conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	defer conn.SetWriteDeadline(time.Time{})
	n, err := conn.Write(data)
	if err == nil && n != len(data) {
		return io.ErrShortWrite
	}
	return err
}
