package device

import (
	"encoding/csv"
	"encoding/hex"
	"io"
	"rykvo.local/auth/internal/vocat/modem"
	"strconv"
	"strings"
	"unicode/utf16"
)

func transparentSIMFileSize(payload []byte) int {
	// USIM FCP templates contain file size in tag 0x80. Skip the outer 0x62
	// template and walk its immediate TLVs.
	content := payload
	if len(content) >= 2 && content[0] == 0x62 {
		length, header, ok := berLength(content[1:])
		if !ok || 1+header+length > len(content) {
			return 0
		}
		content = content[1+header : 1+header+length]
	}
	for offset := 0; offset+2 <= len(content); {
		tag := content[offset]
		length, header, ok := berLength(content[offset+1:])
		start := offset + 1 + header
		end := start + length
		if !ok || end > len(content) {
			break
		}
		if tag == 0x80 && (length == 1 || length == 2) {
			size := 0
			for _, value := range content[start:end] {
				size = size<<8 | int(value)
			}
			return size
		}
		offset = end
	}
	// Legacy GSM GET RESPONSE data stores file size in bytes 2 and 3.
	if len(payload) >= 4 && payload[0] != 0x62 {
		return int(payload[2])<<8 | int(payload[3])
	}
	return 0
}

func berLength(value []byte) (length, header int, ok bool) {
	if len(value) == 0 {
		return 0, 0, false
	}
	if value[0] < 0x80 {
		return int(value[0]), 1, true
	}
	count := int(value[0] & 0x7f)
	if count == 0 || count > 2 || len(value) < count+1 {
		return 0, 0, false
	}
	for _, item := range value[1 : count+1] {
		length = length<<8 | int(item)
	}
	return length, count + 1, true
}

func encodeSIMGroupID(value []byte) string {
	if len(value) == 0 {
		return ""
	}
	allPadding := true
	for _, item := range value {
		if item != 0xff {
			allPadding = false
			break
		}
	}
	if allPadding {
		return ""
	}
	return strings.ToUpper(hex.EncodeToString(value))
}

func parseSPN(response modem.Response) string {
	value := valueAfterPrefix(response, "+CRSM:")
	fields := csvValues(value)
	if len(fields) < 3 {
		return ""
	}
	sw1, sw1Err := strconv.Atoi(strings.TrimSpace(fields[0]))
	sw2, sw2Err := strconv.Atoi(strings.TrimSpace(fields[1]))
	if sw1Err != nil || sw2Err != nil || (sw1 != 0x90 && sw1 != 0x91 && sw1 != 0x9f) || sw2 < 0 || sw2 > 255 {
		return ""
	}
	raw, err := hex.DecodeString(strings.Trim(strings.TrimSpace(fields[2]), `"`))
	if err != nil || len(raw) < 2 {
		return ""
	}
	alpha := raw[1:] // byte 0 is the display-condition bit field.
	for len(alpha) > 0 && (alpha[len(alpha)-1] == 0xff || alpha[len(alpha)-1] == 0x00) {
		alpha = alpha[:len(alpha)-1]
	}
	if len(alpha) == 0 {
		return ""
	}
	if alpha[0] == 0x80 {
		ucs2 := alpha[1:]
		if len(ucs2)%2 != 0 {
			ucs2 = ucs2[:len(ucs2)-1]
		}
		units := make([]uint16, 0, len(ucs2)/2)
		for index := 0; index+1 < len(ucs2); index += 2 {
			unit := uint16(ucs2[index])<<8 | uint16(ucs2[index+1])
			if unit != 0xffff && unit != 0 {
				units = append(units, unit)
			}
		}
		return strings.TrimSpace(string(utf16.Decode(units)))
	}
	// EF_SPN uses the unpacked GSM default alphabet. Its printable Latin subset
	// is byte-compatible with UTF-8/ASCII and covers operator brands in practice.
	printable := make([]byte, 0, len(alpha))
	for _, value := range alpha {
		if value >= 0x20 && value <= 0x7e {
			printable = append(printable, value)
		}
	}
	return strings.TrimSpace(string(printable))
}

func valueAfterPrefix(response modem.Response, prefix string) string {
	for _, line := range response.Lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToUpper(line), strings.ToUpper(prefix)) {
			return strings.TrimSpace(line[len(prefix):])
		}
	}
	return ""
}

func csvValues(value string) []string {
	reader := csv.NewReader(strings.NewReader(value))
	reader.TrimLeadingSpace = true
	reader.LazyQuotes = true
	record, err := reader.Read()
	if err != nil && err != io.EOF {
		return nil
	}
	for index := range record {
		record[index] = strings.TrimSpace(record[index])
	}
	return record
}

func crsmPayload(response modem.Response) []byte {
	value := valueAfterPrefix(response, "+CRSM:")
	values := csvValues(value)
	if len(values) < 3 {
		return nil
	}
	sw1, sw1Err := strconv.Atoi(values[0])
	sw2, sw2Err := strconv.Atoi(values[1])
	if sw1Err != nil || sw2Err != nil ||
		!((sw1 == 144 && sw2 == 0) || sw1 == 145) {
		return nil
	}
	payload := strings.Trim(values[2], `" `)
	decoded, err := hex.DecodeString(payload)
	if err != nil {
		return nil
	}
	return decoded
}
func SIMFileSize(p []byte) int           { return transparentSIMFileSize(p) }
func SIMGroupID(p []byte) string         { return encodeSIMGroupID(p) }
func SIMSPN(r modem.Response) string     { return parseSPN(r) }
func SIMPayload(r modem.Response) []byte { return crsmPayload(r) }
