// Package mms implements the bounded OMA MMS 1.2 MM1 wire format.
package mms

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

const MaxSize = 2 * 1024 * 1024

var ErrPDU = errors.New("MMS_INVALID_PDU")

type Part struct {
	Type string
	Data []byte
}
type PDU struct {
	Type                                       byte
	Transaction, MessageID, From, To, Location string
	Status                                     byte
	Parts                                      []Part
}

func uv(n int) []byte {
	if n < 0 {
		return nil
	}
	b := []byte{byte(n & 127)}
	for n >>= 7; n > 0; n >>= 7 {
		b = append([]byte{byte(n&127) | 128}, b...)
	}
	return b
}
func txt(s string) []byte { return append([]byte(s), 0) }
func val(b []byte) []byte {
	if len(b) < 31 {
		return append([]byte{byte(len(b))}, b...)
	}
	return append(append([]byte{31}, uv(len(b))...), b...)
}
func SendRequest(id, to, text string, image *Part) ([]byte, error) {
	if len(id) < 8 || len(id) > 80 || strings.ContainsAny(id, "\x00\r\n") || !utf8.ValidString(text) || strings.ContainsRune(text, 0) || len(text) > 4096 || len(to) < 3 || len(to) > 16 {
		return nil, ErrPDU
	}
	for i, c := range to {
		if c == '+' && i == 0 {
			continue
		}
		if c < '0' || c > '9' {
			return nil, ErrPDU
		}
	}
	parts := []Part{}
	if text != "" {
		parts = append(parts, Part{"text/plain", []byte(text)})
	}
	if image != nil {
		if !imageType(image.Type) || len(image.Data) == 0 {
			return nil, ErrPDU
		}
		parts = append(parts, *image)
	}
	if len(parts) == 0 {
		return nil, ErrPDU
	}
	b := []byte{0x8c, 0x80, 0x98}
	b = append(b, txt(id)...)
	b = append(b, 0x8d, 0x92, 0x89, 1, 0x81, 0x97)
	b = append(b, txt(to+"/TYPE=PLMN")...)
	b = append(b, 0x8a, 0x80, 0x86, 0x80, 0x84, 0xa3) // Request an actual delivery report.
	b = append(b, uv(len(parts))...)
	for i, p := range parts {
		h := txt(p.Type)
		if p.Type == "text/plain" {
			h = val(append(h, 0x81, 0xea))
		}
		h = append(h, 0x8e)
		h = append(h, txt(fmt.Sprintf("part%d", i))...)
		b = append(b, uv(len(h))...)
		b = append(b, uv(len(p.Data))...)
		b = append(b, h...)
		b = append(b, p.Data...)
	}
	if len(b) > MaxSize {
		return nil, errors.New("MMS_TOO_LARGE")
	}
	return b, nil
}
func NotifyResponse(id string, status byte) []byte {
	b := []byte{0x8c, 0x83, 0x98}
	b = append(b, txt(id)...)
	return append(b, 0x8d, 0x92, 0x95, status)
}
func imageType(s string) bool {
	return s == "image/jpeg" || s == "image/png" || s == "image/gif"
}

type cursor struct {
	b []byte
	i int
}

func (c *cursor) take(n int) ([]byte, error) {
	if n < 0 || n > len(c.b)-c.i {
		return nil, ErrPDU
	}
	b := c.b[c.i : c.i+n]
	c.i += n
	return b, nil
}
func (c *cursor) oct() (byte, error) {
	b, e := c.take(1)
	if e != nil {
		return 0, e
	}
	return b[0], nil
}
func (c *cursor) uintvar() (int, error) {
	n := 0
	for i := 0; i < 5; i++ {
		v, e := c.oct()
		if e != nil {
			return 0, e
		}
		n = n<<7 | int(v&127)
		if n > MaxSize {
			return 0, ErrPDU
		}
		if v&128 == 0 {
			return n, nil
		}
	}
	return 0, ErrPDU
}
func (c *cursor) length() (int, error) {
	v, e := c.oct()
	if e != nil {
		return 0, e
	}
	if v < 31 {
		return int(v), nil
	}
	if v == 31 {
		return c.uintvar()
	}
	return 0, ErrPDU
}
func (c *cursor) text() (string, error) {
	start := c.i
	for c.i < len(c.b) {
		v, _ := c.oct()
		if v == 0 {
			b := c.b[start : c.i-1]
			if len(b) > 0 && b[0] == 127 {
				b = b[1:]
			}
			if len(b) > 4096 || !utf8.Valid(b) {
				return "", ErrPDU
			}
			return string(b), nil
		}
	}
	return "", ErrPDU
}
func (c *cursor) encoded() (string, error) {
	if c.i >= len(c.b) {
		return "", ErrPDU
	}
	if c.b[c.i] > 31 {
		return c.text()
	}
	n, e := c.length()
	if e != nil {
		return "", e
	}
	b, e := c.take(n)
	if e != nil {
		return "", e
	}
	d := cursor{b: b}
	v, e := d.oct()
	if e != nil {
		return "", e
	}
	charset := int(v & 127)
	if v < 128 {
		x, e := d.take(int(v))
		if e != nil {
			return "", e
		}
		charset = 0
		for _, a := range x {
			charset = charset<<8 | int(a)
		}
	}
	if charset != 106 && charset != 3 {
		return "", ErrPDU
	}
	return d.text()
}
func (c *cursor) skip() error {
	if c.i >= len(c.b) {
		return ErrPDU
	}
	v := c.b[c.i]
	if v <= 31 {
		n, e := c.length()
		if e != nil {
			return e
		}
		_, e = c.take(n)
		return e
	}
	if v >= 128 {
		_, e := c.oct()
		return e
	}
	_, e := c.text()
	return e
}
func (c *cursor) contentType() (string, error) {
	if c.i >= len(c.b) {
		return "", ErrPDU
	}
	if c.b[c.i] <= 31 {
		n, e := c.length()
		if e != nil {
			return "", e
		}
		b, e := c.take(n)
		if e != nil {
			return "", e
		}
		if len(b) == 0 || b[0] <= 31 {
			return "", ErrPDU
		}
		d := cursor{b: b}
		return d.contentType()
	}
	if c.b[c.i] < 128 {
		return c.text()
	}
	v, _ := c.oct()
	switch v & 127 {
	case 3:
		return "text/plain", nil
	case 0x1c:
		return "image/gif", nil
	case 0x1e:
		return "image/jpeg", nil
	case 0x20:
		return "image/png", nil
	case 0x23:
		return "application/vnd.wap.multipart.mixed", nil
	case 0x33:
		return "application/vnd.wap.multipart.related", nil
	case 0x3e:
		return "application/vnd.wap.mms-message", nil
	}
	return "application/octet-stream", nil
}
func Parse(b []byte) (PDU, error) {
	p := PDU{}
	if len(b) < 2 || len(b) > MaxSize || b[0] != 0x8c {
		return p, ErrPDU
	}
	c := cursor{b: b}
	seen := map[byte]bool{}
	for c.i < len(c.b) {
		h, e := c.oct()
		if e != nil {
			return p, e
		}
		if seen[h] && h != 0x97 {
			return p, ErrPDU
		}
		seen[h] = true
		switch h {
		case 0x8c:
			p.Type, e = c.oct()
		case 0x8d:
			var v byte
			v, e = c.oct()
			if v < 0x90 || v > 0x93 {
				return p, ErrPDU
			}
		case 0x98:
			p.Transaction, e = c.text()
		case 0x8b:
			p.MessageID, e = c.text()
		case 0x92, 0x95, 0x99:
			p.Status, e = c.oct()
		case 0x83:
			p.Location, e = c.text()
		case 0x89:
			var n int
			n, e = c.length()
			if e == nil {
				var b []byte
				b, e = c.take(n)
				if e == nil {
					d := cursor{b: b}
					var t byte
					t, e = d.oct()
					if t == 0x80 {
						p.From, e = d.encoded()
					} else if t != 0x81 {
						e = ErrPDU
					}
				}
			}
		case 0x97:
			p.To, e = c.encoded()
		case 0x96, 0x93:
			_, e = c.encoded()
		case 0x84:
			var ct string
			ct, e = c.contentType()
			if e != nil {
				return p, e
			}
			if ct != "application/vnd.wap.multipart.mixed" && ct != "application/vnd.wap.multipart.related" {
				return p, ErrPDU
			}
			p.Parts, e = c.parts()
			if e != nil || c.i != len(c.b) {
				return p, ErrPDU
			}
			if !seen[0x8d] {
				return p, ErrPDU
			}
			return p, nil
		default:
			if h < 128 {
				return p, ErrPDU
			}
			e = c.skip()
		}
		if e != nil {
			return p, e
		}
	}
	if !seen[0x8d] {
		return p, ErrPDU
	}
	return p, nil
}
func (c *cursor) parts() ([]Part, error) {
	n, e := c.uintvar()
	if e != nil || n < 1 || n > 32 {
		return nil, ErrPDU
	}
	parts := []Part{}
	for i := 0; i < n; i++ {
		hl, e := c.uintvar()
		if e != nil {
			return nil, e
		}
		dl, e := c.uintvar()
		if e != nil {
			return nil, e
		}
		h, e := c.take(hl)
		if e != nil {
			return nil, e
		}
		data, e := c.take(dl)
		if e != nil {
			return nil, e
		}
		d := cursor{b: h}
		kind, e := d.contentType()
		if e != nil {
			return nil, e
		}
		if imageType(kind) || kind == "text/plain" {
			parts = append(parts, Part{kind, append([]byte(nil), data...)})
		}
	}
	return parts, nil
}

// WAP Push uses a transaction octet, Push/ConfirmedPush PDU, and uintvar headers.
func Push(b []byte) (PDU, error) {
	if len(b) < 4 || (b[1] != 6 && b[1] != 7) {
		return PDU{}, ErrPDU
	}
	c := cursor{b: b, i: 2}
	n, e := c.uintvar()
	if e != nil {
		return PDU{}, e
	}
	h, e := c.take(n)
	if e != nil {
		return PDU{}, e
	}
	d := cursor{b: h}
	kind, e := d.contentType()
	if e != nil || kind != "application/vnd.wap.mms-message" {
		return PDU{}, ErrPDU
	}
	return Parse(b[c.i:])
}
