package hardware

import (
	"encoding/binary"
)

func (s *wifiIMS) receiveDatagram(wire []byte) ([]byte, error) {
	if s.protection == nil {
		return s.child.receiveUDP(wire, s.local, s.peer, 49160, s.target, nil)
	}
	raw, kind, e := s.child.open(wire)
	if e != nil {
		return nil, e
	}
	defer clear(raw)
	src, dst, proto, esp, e := wifiParseIP(raw, kind)
	if e != nil || proto != 50 || !src.Equal(s.peer) || !dst.Equal(s.local) || len(esp) < 8 {
		return nil, errWiFiESP
	}
	protection := s.protection
	port := uint16(49160)
	if s.serverProtection != nil && binary.BigEndian.Uint32(esp) == s.serverProtection.spiIn {
		protection = s.serverProtection
		port = 49162
	}
	data, proto, e := protection.open(esp)
	if e != nil {
		return nil, e
	}
	defer clear(data)
	if proto != 17 {
		return nil, errWiFiESP
	}
	ip, kind, e := wifiIP(src, dst, 17, data)
	if e != nil {
		return nil, e
	}
	defer clear(ip)
	result, e := wifiReadUDP(ip, kind, s.local, s.peer, port, 0)
	return result, e
}
