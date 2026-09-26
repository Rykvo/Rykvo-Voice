package hardware

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"rykvo.local/auth/internal/mms"
)

func TestCellularMM1SubmissionEvidence(t *testing.T) {
	const id = "transaction-12345"
	confirmation := func(transaction, message string, status byte) []byte {
		v := append([]byte{0x8c, 0x81, 0x98}, []byte(transaction)...)
		v = append(v, 0, 0x8d, 0x92, 0x92, status)
		if message != "" {
			v = append(v, 0x8b)
			v = append(v, []byte(message)...)
			v = append(v, 0)
		}
		return v
	}
	for _, tc := range []struct {
		name                string
		code                int
		mime                string
		pdu                 []byte
		fault               string
		accepted, attempted bool
		issue               string
	}{
		{"accepted", 200, "application/vnd.wap.mms-message", confirmation(id, "server-id-123", 0x80), "", true, true, ""},
		{"no-id", 200, "application/vnd.wap.mms-message", confirmation(id, "", 0x80), "", false, true, "MMS_OUTCOME_UNKNOWN"},
		{"wrong-transaction", 200, "application/vnd.wap.mms-message", confirmation("another-id", "server-id", 0x80), "", false, true, "MMS_OUTCOME_UNKNOWN"},
		{"rejected", 200, "application/vnd.wap.mms-message", confirmation(id, "", 0x83), "", false, true, "MMS_REJECTED"},
		{"malformed", 200, "application/vnd.wap.mms-message", []byte("invalid"), "", false, true, "MMS_OUTCOME_UNKNOWN"},
		{"html", 200, "text/html", confirmation(id, "server-id", 0x80), "", false, true, "MMS_HTTP_CONTENT_TYPE"},
		{"redirect", 302, "application/vnd.wap.mms-message", nil, "", false, true, "MMS_HTTP_302"},
		{"empty", 204, "application/vnd.wap.mms-message", nil, "", false, true, "MMS_HTTP_204"},
		{"connect", 200, "application/vnd.wap.mms-message", nil, "connect", false, false, "MMS_SOCKET_FAILED"},
		{"partial-write", 200, "application/vnd.wap.mms-message", nil, "write", false, true, "MMS_SOCKET_FAILED"},
		{"lost-response", 200, "application/vnd.wap.mms-message", nil, "response", false, true, "MMS_HTTP_INVALID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload, err := mms.SendRequest(id, "+8613800000000", "test", &mms.Part{Type: "image/png", Data: bytes.Repeat([]byte{0, 0xff, '+', '+', '+', 0x1a, 13, 10}, 1200)})
			if err != nil {
				t.Fatal(err)
			}
			response := append([]byte(fmt.Sprintf("HTTP/1.1 %d Status\r\nContent-Type: %s\r\nContent-Length: %d\r\n\r\n", tc.code, tc.mime, len(tc.pdu))), tc.pdu...)
			if tc.fault == "response" {
				response = nil
			}
			port := &mmsTestPort{}
			var sent bytes.Buffer
			pending, opens := 0, 0
			port.onWrite = func(data []byte) string {
				if pending > 0 {
					if tc.fault == "write" {
						pending = 0
						return "ERROR\r\n"
					}
					if len(data) > pending {
						t.Fatal("exceeded declared write")
					}
					sent.Write(data)
					pending -= len(data)
					if pending == 0 {
						return "SEND OK\r\n"
					}
					return ""
				}
				cmd := string(data)
				switch {
				case strings.HasPrefix(cmd, "AT+QIOPEN="):
					opens++
					if !strings.Contains(cmd, `AT+QIOPEN=4,11,"TCP","10.0.0.172",80,0,0`) {
						t.Fatal(cmd)
					}
					if tc.fault == "connect" {
						return "OK\r\n+QIOPEN: 11,566\r\n"
					}
					return "OK\r\n+QIOPEN: 11,0\r\n"
				case strings.HasPrefix(cmd, "AT+QISEND="):
					fmt.Sscanf(cmd, "AT+QISEND=11,%d", &pending)
					if pending < 1 || pending > 1460 {
						t.Fatal(pending)
					}
					return "\r\n> "
				case strings.HasPrefix(cmd, "AT+QIRD="):
					n := len(response)
					if n > 41 {
						n = 41
					}
					chunk := response[:n]
					response = response[n:]
					return fmt.Sprintf("+QIRD: %d\r\n%s\r\nOK\r\n", n, chunk)
				default:
					return "OK\r\n"
				}
			}
			b := &mmsBearer{at: &atSession{port: port}, cid: 4, healthy: true}
			result, err := sendCellularMM1(context.Background(), b, testMMSProfile(), id, payload)
			issue := ""
			if err != nil {
				issue = err.Error()
			}
			if result.Accepted != tc.accepted || result.Attempted != tc.attempted || issue != tc.issue || opens != 1 {
				t.Fatal(result, issue, opens)
			}
			if result.Accepted && result.MessageID != "server-id-123" {
				t.Fatal(result)
			}
			if tc.fault == "" {
				request, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(sent.Bytes())))
				if err != nil {
					t.Fatal(err)
				}
				body, err := io.ReadAll(request.Body)
				request.Body.Close()
				if err != nil || !bytes.Equal(body, payload) || request.Method != "POST" || request.RequestURI != "http://mmsc.example.test/" {
					t.Fatal("corrupt or wrong submission", request.RequestURI, err)
				}
			}
		})
	}
}

func TestCellularMM1OversizeNeverConnects(t *testing.T) {
	b := &mmsBearer{}
	_, err := b.httpExchange(context.Background(), testMMSProfile(), "POST", testMMSProfile().MMSC, make([]byte, mms.MaxSize+1), &MMSSubmitResult{})
	if err == nil || err.Error() != "MMS_TOO_LARGE" {
		t.Fatal(err)
	}
}
