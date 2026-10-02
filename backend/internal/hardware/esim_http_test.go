package hardware

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sgp22 "github.com/damonto/euicc-go/v2"
)

func TestESIMHTTPPreservesServerCodesWithoutMessage(t *testing.T) {
	for _, body := range []string{
		`{"header":{"functionExecutionStatus":{"status":"Failed","statusCodeData":{"subjectCode":"8.2.6","reasonCode":"3.8","message":"LPA:1$carrier.example$SECRET"}}}}`,
		`{"header":{"functionExecutionStatus":{"status":"Executed-Success"}},"transactionId":"fixture"}`,
		`not json`,
	} {
		t.Run(body[:8], func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, body) }))
			defer server.Close()
			transport := &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // Local TLS fixture only.
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "tcp", server.Listener.Addr().String())
				},
			}
			defer transport.CloseIdleConnections()
			client := &http.Client{Transport: &esimTransport{context.Background(), transport}}
			response, err := client.Post("https://carrier.example/gsma/rsp2/es9plus/authenticateClient", "application/json", strings.NewReader(`{}`))
			if strings.Contains(body, "Failed") {
				var remote *sgp22.StatusCodeData
				if !errors.As(err, &remote) || remote.SubjectCode != "8.2.6" || remote.ReasonCode != "3.8" || remote.Message != "" || strings.Contains(err.Error(), "SECRET") {
					t.Fatalf("server code lost or secret exposed: %v", err)
				}
				if esimError(err) != "ESIM_SERVER_REJECTED" || esimDiagnostic(err) != "server:8.2.6/3.8" {
					t.Fatal("HTTP wrapping lost diagnostic")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			data, err := io.ReadAll(response.Body)
			if err != nil || string(data) != body {
				t.Fatal("normal response changed")
			}
		})
	}
}
