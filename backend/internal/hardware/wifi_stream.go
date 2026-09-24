package hardware

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

type wifiStreamFrame struct {
	data []byte
	conn net.Conn
	err  error
}
type wifiSIPReader struct{ r *bufio.Reader }

func newWiFiSIPReader(r io.Reader) *wifiSIPReader {
	return &wifiSIPReader{bufio.NewReaderSize(r, 8192)}
}
func (r *wifiSIPReader) read() ([]byte, error) {
	var data []byte
	length := -1
	for {
		line, e := r.r.ReadSlice('\n')
		if e != nil {
			return nil, e
		}
		if len(data)+len(line) > 16384 || !bytes.HasSuffix(line, []byte("\r\n")) {
			return nil, errors.New("WIFI_IMS_INVALID")
		}
		if len(data) == 0 && bytes.Equal(line, []byte("\r\n")) {
			continue
		}
		data = append(data, line...)
		if bytes.Equal(line, []byte("\r\n")) {
			break
		}
		k, v, ok := strings.Cut(string(line), ":")
		if ok && (strings.EqualFold(k, "Content-Length") || strings.EqualFold(k, "l")) {
			if length >= 0 {
				return nil, errors.New("WIFI_IMS_INVALID")
			}
			n, e := strconv.Atoi(strings.TrimSpace(v))
			if e != nil || n < 0 || n > 65536 {
				return nil, errors.New("WIFI_IMS_INVALID")
			}
			length = n
		}
	}
	if length < 0 || len(data)+length > 65536 {
		return nil, errors.New("WIFI_IMS_INVALID")
	}
	header := len(data)
	data = append(data, make([]byte, length)...)
	_, e := io.ReadFull(r.r, data[header:])
	return data, e
}
func (s *wifiIMS) tcpExchange(ctx context.Context, body []byte, match func([]byte) bool) ([]byte, error) {
	if s.tcp == nil {
		var e error
		s.tcp, e = openWiFiTCP(ctx, s)
		if e != nil {
			return nil, e
		}
	}
	if e := s.tcp.write(ctx, s.tcp.client, body); e != nil {
		return nil, e
	}
	call, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	for {
		select {
		case f := <-s.tcp.frames:
			if f.err != nil {
				if f.conn == s.tcp.client {
					return nil, errors.New("WIFI_TCP_CLOSED")
				}
				continue
			}
			s.replyConn = f.conn
			yes := match(f.data)
			s.replyConn = nil
			if yes {
				return f.data, nil
			}
			clear(f.data)
		default:
			if e := s.tcp.pump(call, 20*time.Millisecond); e != nil {
				if call.Err() != nil && ctx.Err() == nil {
					return nil, errors.New("WIFI_IMS_TIMEOUT")
				}
				return nil, e
			}
		}
	}
}
