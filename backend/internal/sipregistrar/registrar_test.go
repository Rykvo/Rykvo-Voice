package sipregistrar

import (
	"crypto/md5"
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/icholy/digest"
)

func fixtureAccount(password string, version int64) Account {
	a := md5.Sum([]byte("1001:" + Realm + ":" + password))
	b := sha256.Sum256([]byte("1001:" + Realm + ":" + password))
	return Account{ID: "account-1", Username: "1001", Port: 20001, Revision: version, MD5: a[:], SHA256: b[:]}
}
func request(t *testing.T, device string, sequence int) *sip.Request {
	t.Helper()
	raw := fmt.Sprintf("REGISTER sip:127.0.0.1:20001 SIP/2.0\r\nVia: SIP/2.0/UDP 127.0.0.1:5090;branch=z9hG4bK-%s-%d;rport\r\nFrom: <sip:1001@127.0.0.1>;tag=%s\r\nTo: <sip:1001@127.0.0.1>\r\nCall-ID: %s\r\nCSeq: %d REGISTER\r\nContact: <sip:1001@127.0.0.1:5090>;expires=300;+sip.instance=\"%s\"\r\nMax-Forwards: 70\r\nContent-Length: 0\r\n\r\n", device, sequence, device, device, sequence, device)
	m, err := sip.ParseMessage([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	req := m.(*sip.Request)
	req.SetSource("127.0.0.1:5090")
	req.SetTransport("UDP")
	return req
}
func authorize(t *testing.T, r *Registrar, req *sip.Request, password, algorithm string) {
	t.Helper()
	res := r.Handle(req, 20001)
	if res.StatusCode != 401 {
		t.Fatalf("challenge=%d", res.StatusCode)
	}
	var challenge *digest.Challenge
	for _, h := range res.GetHeaders("WWW-Authenticate") {
		c, err := digest.ParseChallenge(h.Value())
		if err != nil {
			t.Fatal(err)
		}
		if c.Algorithm == algorithm {
			challenge = c
		}
	}
	if challenge == nil {
		t.Fatal("missing algorithm")
	}
	c, err := digest.Digest(challenge, digest.Options{Method: string(req.Method), URI: req.Recipient.String(), Username: "1001", Password: password, Count: 1, Cnonce: "test-cnonce"})
	if err != nil {
		t.Fatal(err)
	}
	req.AppendHeader(sip.NewHeader("Authorization", c.String()))
}
func TestRegisterAuthentication(t *testing.T) {
	for _, algorithm := range []string{"MD5", "SHA-256"} {
		t.Run(algorithm, func(t *testing.T) {
			r := New()
			r.Replace([]Account{fixtureAccount("test-secret", 1)})
			req := request(t, "phone-A", 1)
			authorize(t, r, req, "test-secret", algorithm)
			if res := r.Handle(req, 20001); res.StatusCode != 200 {
				t.Fatal(res.StatusCode)
			}
			if !r.Online("account-1") {
				t.Fatal("not online")
			}
			if res := r.Handle(req, 20001); res.StatusCode != 401 {
				t.Fatal("nonce replay accepted")
			}
		})
	}
}
func TestWrongPasswordAndPortNeverRegister(t *testing.T) {
	r := New()
	r.Replace([]Account{fixtureAccount("test-secret", 1)})
	req := request(t, "A", 1)
	authorize(t, r, req, "wrong", "MD5")
	if r.Handle(req, 20001).StatusCode != 401 || r.Online("account-1") {
		t.Fatal("invalid password accepted")
	}
	req = request(t, "A", 2)
	authorize(t, r, req, "test-secret", "MD5")
	if r.Handle(req, 20002).StatusCode != 401 || r.Online("account-1") {
		t.Fatal("wrong listener accepted")
	}
}
func TestReplacementRevokesOldRefresh(t *testing.T) {
	r := New()
	r.Replace([]Account{fixtureAccount("test-secret", 1)})
	for _, device := range []string{"A", "B"} {
		req := request(t, device, 1)
		authorize(t, r, req, "test-secret", "MD5")
		if r.Handle(req, 20001).StatusCode != 200 {
			t.Fatal("register failed")
		}
	}
	req := request(t, "A", 2)
	authorize(t, r, req, "test-secret", "MD5")
	if r.Handle(req, 20001).StatusCode != 403 {
		t.Fatal("old client reclaimed account")
	}
	req = request(t, "B", 2)
	authorize(t, r, req, "test-secret", "MD5")
	if r.Handle(req, 20001).StatusCode != 200 {
		t.Fatal("current refresh failed")
	}
}
func TestCredentialMutationAndExpiry(t *testing.T) {
	r := New()
	a := fixtureAccount("test-secret", 1)
	r.Replace([]Account{a})
	now := time.Now()
	r.now = func() time.Time { return now }
	register := func(password string, seq int) {
		req := request(t, "A", seq)
		authorize(t, r, req, password, "MD5")
		if r.Handle(req, 20001).StatusCode != 200 {
			t.Fatal("register failed")
		}
	}
	register("test-secret", 1)
	r.Replace([]Account{a})
	if !r.Online(a.ID) {
		t.Fatal("unchanged policy disconnected client")
	}
	r.Replace([]Account{fixtureAccount("new-secret", 2)})
	if r.Online(a.ID) {
		t.Fatal("old credentials still online")
	}
	req := request(t, "A", 2)
	authorize(t, r, req, "test-secret", "MD5")
	if r.Handle(req, 20001).StatusCode != 401 {
		t.Fatal("old password accepted")
	}
	register("new-secret", 3)
	now = now.Add(301 * time.Second)
	if r.Online(a.ID) {
		t.Fatal("expired registration online")
	}
	register("new-secret", 4)
	r.Replace(nil)
	if r.Online(a.ID) {
		t.Fatal("deleted account online")
	}
}
func TestUnregisterAndNoFakeCallAnswer(t *testing.T) {
	r := New()
	r.Replace([]Account{fixtureAccount("test-secret", 1)})
	req := request(t, "A", 1)
	authorize(t, r, req, "test-secret", "MD5")
	r.Handle(req, 20001)
	invite := request(t, "call", 2)
	invite.Method = sip.INVITE
	invite.CSeq().MethodName = sip.INVITE
	authorize(t, r, invite, "test-secret", "MD5")
	if r.Handle(invite, 20001).StatusCode != 503 {
		t.Fatal("unimplemented call was accepted")
	}
	req = request(t, "A", 2)
	req.Contact().Params.Add("expires", "0")
	authorize(t, r, req, "test-secret", "MD5")
	if r.Handle(req, 20001).StatusCode != 200 || r.Online("account-1") {
		t.Fatal("unregister failed")
	}
}
func TestNonceSourceBindingAndExpiry(t *testing.T) {
	r := New()
	r.Replace([]Account{fixtureAccount("test-secret", 1)})
	now := time.Now()
	r.now = func() time.Time { return now }
	req := request(t, "A", 1)
	authorize(t, r, req, "test-secret", "MD5")
	req.SetSource("127.0.0.2:5090")
	if r.Handle(req, 20001).StatusCode != 401 {
		t.Fatal("nonce stolen on other source")
	}
	req = request(t, "A", 2)
	authorize(t, r, req, "test-secret", "MD5")
	now = now.Add(61 * time.Second)
	if r.Handle(req, 20001).StatusCode != 401 {
		t.Fatal("expired challenge accepted")
	}
}
func TestBadContactsAndExpiry(t *testing.T) {
	for _, expiry := range []string{"-1", "abc", "86401", "1"} {
		r := New()
		r.Replace([]Account{fixtureAccount("test-secret", 1)})
		req := request(t, "A", 1)
		req.Contact().Params.Add("expires", expiry)
		authorize(t, r, req, "test-secret", "MD5")
		if r.Handle(req, 20001).StatusCode == 200 {
			t.Fatal("bad expiry accepted", expiry)
		}
	}
}
func FuzzRegistrar(f *testing.F) {
	f.Add("REGISTER sip:a SIP/2.0\r\nContent-Length: 0\r\n\r\n")
	f.Fuzz(func(t *testing.T, raw string) {
		if len(raw) > 8192 || strings.Contains(raw, "\x00") {
			return
		}
		message, err := sip.ParseMessage([]byte(raw))
		if err != nil {
			return
		}
		req, ok := message.(*sip.Request)
		if !ok {
			return
		}
		req.SetSource("127.0.0.1:5000")
		req.SetTransport("UDP")
		r := New()
		r.Replace([]Account{fixtureAccount("test-secret", 1)})
		r.Handle(req, 20001)
	})
}
