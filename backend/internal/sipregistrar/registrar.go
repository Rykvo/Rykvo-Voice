// Package sipregistrar authenticates SIP clients; it never infers call readiness.
package sipregistrar

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/icholy/digest"
)

const Realm = "rykvo"
const ttl = 5 * time.Minute

type Account struct {
	ID, Username string
	Port         int
	Revision     int64
	MD5, SHA256  []byte
}
type Registration struct {
	Account, CallID, Instance, Source, Transport string
	Sequence                                     uint32
	Expires                                      time.Time
	Contact                                      *sip.ContactHeader
}
type nonce struct {
	source, username string
	port             int
	expires          time.Time
	count            int
}
type rate struct {
	start time.Time
	count int
}
type Registrar struct {
	mu       sync.Mutex
	accounts map[string]Account
	current  map[string]Registration
	revoked  map[string]time.Time
	nonces   map[string]nonce
	rates    map[string]rate
	now      func() time.Time
}

func New() *Registrar {
	return &Registrar{accounts: map[string]Account{}, current: map[string]Registration{}, revoked: map[string]time.Time{}, nonces: map[string]nonce{}, rates: map[string]rate{}, now: time.Now}
}
func accountKey(username string, port int) string   { return fmt.Sprintf("%d/%s", port, username) }
func revokedKey(id, callID, instance string) string { return id + "\x00" + callID + "\x00" + instance }

// Replace is called under the same mutation gate as authenticated handlers.
func (r *Registrar) Replace(accounts []Account) {
	r.mu.Lock()
	defer r.mu.Unlock()
	next := make(map[string]Account, len(accounts))
	valid := map[string]bool{}
	for _, a := range accounts {
		key := accountKey(a.Username, a.Port)
		old, ok := r.accounts[key]
		if ok && old.ID == a.ID && old.Revision == a.Revision {
			valid[a.ID] = true
		}
		next[key] = a
	}
	for id := range r.current {
		if !valid[id] {
			delete(r.current, id)
		}
	}
	r.accounts = next
}
func (r *Registrar) OfflinePort(port int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, a := range r.accounts {
		if a.Port == port {
			delete(r.current, a.ID)
		}
	}
}
func (r *Registrar) Online(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.current[id].Expires.After(r.now())
}
func (r *Registrar) sweep(now time.Time) {
	for k, v := range r.current {
		if !v.Expires.After(now) {
			delete(r.current, k)
		}
	}
	for k, v := range r.nonces {
		if !v.expires.After(now) {
			delete(r.nonces, k)
		}
	}
	for k, v := range r.revoked {
		if !v.After(now) {
			delete(r.revoked, k)
		}
	}
	for k, v := range r.rates {
		if now.Sub(v.start) >= time.Minute {
			delete(r.rates, k)
		}
	}
}
func response(req *sip.Request, code int, reason string) *sip.Response {
	res := sip.NewResponseFromRequest(req, code, reason, nil)
	res.AppendHeader(sip.NewHeader("Server", "Rykvo Voice"))
	return res
}

// Handle supports registration and authenticated capability checks. Until a real
// media adapter is installed, INVITE is explicitly rejected, never answered.
func (r *Registrar) Handle(req *sip.Request, port int) *sip.Response {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	r.sweep(now)
	if req.From() == nil || req.To() == nil || req.CallID() == nil || req.CSeq() == nil || req.CSeq().SeqNo == 0 || req.CSeq().MethodName != req.Method || len(req.Body()) > 4096 {
		return response(req, 400, "Bad Request")
	}
	if req.Method != sip.REGISTER && req.Method != sip.OPTIONS && req.Method != sip.INVITE {
		return response(req, 405, "Method Not Allowed")
	}
	host, _, _ := net.SplitHostPort(req.Source())
	if host == "" {
		return response(req, 400, "Bad Request")
	}
	bucket := r.rates[host]
	if bucket.start.IsZero() {
		bucket.start = now
	}
	bucket.count++
	if len(r.rates) >= 2048 && bucket.count == 1 {
		return response(req, 503, "Service Unavailable")
	}
	r.rates[host] = bucket
	if bucket.count > 120 {
		return response(req, 429, "Too Many Requests")
	}
	username := req.From().Address.User
	if len(username) > 64 || len(req.CallID().Value()) > 256 {
		return response(req, 400, "Bad Request")
	}
	a, exists := r.accounts[accountKey(username, port)]
	if req.Method == sip.REGISTER && req.To().Address.User != username {
		return response(req, 403, "Forbidden")
	}
	authorized := false
	if h := req.GetHeader("Authorization"); h != nil && len(h.Value()) <= 2048 {
		c, err := digest.ParseCredentials(h.Value())
		if err == nil && exists && c.Username == username && c.Realm == Realm && c.URI == req.Recipient.String() && c.QOP == "auth" && c.Nc > 0 && len(c.Cnonce) > 0 && len(c.Cnonce) <= 256 && !c.Userhash {
			n, ok := r.nonces[c.Nonce]
			algorithm := strings.ToUpper(c.Algorithm)
			hash := a.MD5
			if algorithm == "SHA-256" {
				hash = a.SHA256
			}
			if ok && n.source == req.Source()+"/"+req.Transport() && n.port == port && n.username == username && c.Nc > n.count && (algorithm == "MD5" || algorithm == "SHA-256" || algorithm == "") {
				expected, e := digest.Digest(&digest.Challenge{Realm: Realm, Nonce: c.Nonce, Algorithm: algorithm, QOP: []string{"auth"}}, digest.Options{Method: string(req.Method), URI: c.URI, Username: username, A1: hex.EncodeToString(hash), Count: c.Nc, Cnonce: c.Cnonce})
				if e == nil && subtle.ConstantTimeCompare([]byte(strings.ToLower(c.Response)), []byte(expected.Response)) == 1 {
					authorized = true
					n.count = c.Nc
					r.nonces[c.Nonce] = n
				}
			}
		}
	}
	if !authorized {
		if len(r.nonces) >= 2048 {
			return response(req, 503, "Service Unavailable")
		}
		var seed [24]byte
		if _, err := rand.Read(seed[:]); err != nil {
			return response(req, 503, "Service Unavailable")
		}
		value := hex.EncodeToString(seed[:])
		r.nonces[value] = nonce{source: req.Source() + "/" + req.Transport(), username: username, port: port, expires: now.Add(time.Minute)}
		res := response(req, 401, "Unauthorized")
		for _, algorithm := range []string{"SHA-256", "MD5"} {
			res.AppendHeader(sip.NewHeader("WWW-Authenticate", fmt.Sprintf(`Digest realm="%s", nonce="%s", algorithm=%s, qop="auth"`, Realm, value, algorithm)))
		}
		return res
	}
	if req.Method != sip.REGISTER {
		reg, ok := r.current[a.ID]
		if !ok || reg.Source != req.Source() || reg.Transport != req.Transport() {
			return response(req, 403, "Forbidden")
		}
		if req.Method == sip.OPTIONS {
			res := response(req, 200, "OK")
			res.AppendHeader(sip.NewHeader("Allow", "REGISTER, OPTIONS"))
			return res
		}
		res := response(req, 503, "Voice Service Unavailable")
		res.AppendHeader(sip.NewHeader("Retry-After", "30"))
		return res
	}
	contacts := req.GetHeaders("Contact")
	if len(contacts) != 1 || req.Contact() == nil || len(contacts[0].Value()) > 1024 {
		return response(req, 400, "Bad Contact")
	}
	contact := req.Contact()
	seconds := int(ttl / time.Second)
	raw, has := contact.Params.Get("expires")
	if !has {
		if h := req.GetHeader("Expires"); h != nil {
			raw, has = h.Value(), true
		}
	}
	if has {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 || n > 86400 {
			return response(req, 400, "Bad Expires")
		}
		seconds = n
	}
	if seconds > int(ttl/time.Second) {
		seconds = int(ttl / time.Second)
	}
	if seconds > 0 && seconds < 60 {
		res := response(req, 423, "Interval Too Brief")
		res.AppendHeader(sip.NewHeader("Min-Expires", "60"))
		return res
	}
	callID := req.CallID().Value()
	instance := contact.Params.GetOr("+sip.instance", "")
	if instance == "" {
		instance = contact.Address.String()
	}
	if len(instance) > 512 {
		return response(req, 400, "Bad Contact")
	}
	old, registered := r.current[a.ID]
	if contact.Address.Wildcard && seconds == 0 && registered && old.CallID == callID {
		instance = old.Instance
	}
	key := revokedKey(a.ID, callID, instance)
	if _, ok := r.revoked[key]; ok {
		return response(req, 403, "Registration Replaced")
	}
	same := registered && old.CallID == callID && old.Instance == instance
	if same && req.CSeq().SeqNo <= old.Sequence {
		return response(req, 500, "CSeq Out of Order")
	}
	if seconds == 0 {
		if registered && !same {
			return response(req, 403, "Forbidden")
		}
		delete(r.current, a.ID)
	} else {
		if contact.Address.Wildcard || contact.Address.Host == "" {
			return response(req, 400, "Bad Contact")
		}
		if registered && !same {
			if len(r.revoked) >= 4096 {
				return response(req, 503, "Service Unavailable")
			}
			r.revoked[revokedKey(a.ID, old.CallID, old.Instance)] = now.Add(time.Hour)
		}
		r.current[a.ID] = Registration{Account: a.ID, CallID: callID, Instance: instance, Sequence: req.CSeq().SeqNo, Source: req.Source(), Transport: req.Transport(), Expires: now.Add(time.Duration(seconds) * time.Second), Contact: contact.Clone()}
	}
	res := response(req, 200, "OK")
	if seconds > 0 {
		h := contact.Clone()
		h.Params.Add("expires", strconv.Itoa(seconds))
		res.AppendHeader(h)
	}
	res.AppendHeader(sip.NewHeader("Expires", strconv.Itoa(seconds)))
	return res
}
