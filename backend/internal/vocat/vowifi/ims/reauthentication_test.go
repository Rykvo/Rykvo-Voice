package ims

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"

	"rykvo.local/auth/internal/vocat/vowifi"
)

func TestProtectedRefreshChallengeRequestsFreshAuthenticatedSession(t *testing.T) {
	nonce := base64.StdEncoding.EncodeToString(make([]byte, 32))
	for _, tc := range []struct {
		name          string
		status        int
		header, nonce string
		registered    bool
		expires       int
		want          bool
	}{
		{"renew401", 401, "www-authenticate", nonce, true, 3600, true},
		{"renew407", 407, "proxy-authenticate", nonce, true, 3600, true},
		{"initial401", 401, "www-authenticate", nonce, false, 3600, false},
		{"deregister401", 401, "www-authenticate", nonce, true, 0, false},
		{"malformed", 401, "www-authenticate", "invalid", true, 3600, false},
		{"missing", 401, "missing", nonce, true, 3600, false},
		{"forbidden", 403, "www-authenticate", nonce, true, 3600, false},
		{"throttled", 503, "www-authenticate", nonce, true, 3600, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStableInstanceTestSession(t, vowifi.IMSRequest{Identity: vowifi.SIMIdentity{IMSI: "001010123456789", HomeMCC: "001", HomeMNC: "01"}})
			s.securityActive = true
			s.runtimeStarted = true
			s.evidence.Registered = tc.registered
			writes := 0
			s.conn.(*fakeConn).onWrite = func([]byte) {
				writes++
				response := &sipResponse{StatusCode: tc.status, Headers: map[string][]string{tc.header: {`Digest realm="ims.example", nonce="` + tc.nonce + `", algorithm=AKAv1-MD5, qop="auth"`}}}
				for _, ch := range s.transactions {
					ch <- response
				}
			}
			var err error
			if tc.registered && tc.expires > 0 {
				err = s.refreshOnce(context.Background())
			} else {
				_, err = s.register(context.Background(), tc.expires)
			}
			if errors.Is(err, vowifi.ErrIMSReauthenticationRequired) != tc.want {
				t.Fatalf("wrong recovery decision: %v", err)
			}
			if writes != 1 {
				t.Fatalf("replayed challenge on old association: %d writes", writes)
			}
			if tc.registered && tc.expires > 0 && s.evidence.Registered {
				t.Fatal("stale registration survived failed renewal")
			}
		})
	}
}
