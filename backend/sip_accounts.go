package main

import (
	"context"
	"crypto/md5" // SIP Digest compatibility; never used for web passwords.
	"crypto/sha256"
	"errors"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const sipRealm = "rykvo"

type sipAccount struct {
	ID           string   `json:"id"`
	Username     string   `json:"username"`
	Port         int      `json:"port"`
	Allocation   string   `json:"allocation"`
	ModuleIDs    []string `json:"moduleIds"`
	ReceiveCalls bool     `json:"receiveCalls"`
	Revision     int64    `json:"revision"`
	Status       string   `json:"status"`
	IP           string   `json:"ip"`
}

type sipAccountInput struct {
	Username     string   `json:"username"`
	Password     string   `json:"password"`
	Port         int      `json:"port"`
	Allocation   string   `json:"allocation"`
	ModuleIDs    []string `json:"moduleIds"`
	ReceiveCalls bool     `json:"receiveCalls"`
	Revision     int64    `json:"revision"`
}

type sipAccountNetwork struct {
	Mode          string `json:"mode"`
	BindAddress   string `json:"-"`
	PublicAddress string `json:"-"`
	Host          string `json:"host"`
	Start         int    `json:"start"`
	End           int    `json:"end"`
	Default       int    `json:"default"`
}

func (s *server) sipAccountNetwork(ctx context.Context, r *http.Request) (sipAccountNetwork, error) {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if s.sipGateway != nil {
		addresses := localSIPAddresses()
		if len(addresses) > 0 && !slices.Contains(addresses, strings.Trim(host, "[]")) {
			host = addresses[0]
		}
	}
	network := sipAccountNetwork{Mode: "lan", Host: strings.Trim(host, "[]"), Start: 1024, End: 65535, Default: 5060}
	if s.sipNetwork == nil {
		return network, nil
	}
	lookup, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	status, err := s.sipNetwork.status(lookup)
	if err != nil {
		return network, err
	}
	if status.State == "disconnected" && !status.Enabled {
		return network, nil
	}
	// A transient tunnel failure is not an intentional switch to LAN.
	if status.State != "connected" || status.Network == nil {
		return network, errors.New("SIP_NETWORK_UNAVAILABLE")
	}
	if n := status.Network; n != nil {
		if n.Start < 1024 || n.End > 65535 || n.Start >= n.End {
			return network, errors.New("SIP_INVALID_CONFIG")
		}
		network.Start, network.End, network.Default = n.Start, n.End, n.Start
		network.Mode = "cloud"
		network.BindAddress = n.BindAddress
		network.PublicAddress = n.PublicAddress
		network.Host = n.Server
		if network.Host == "" {
			network.Host = n.PublicAddress
		}
	}
	return network, nil
}

var sipUsername = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
var sipAccountID = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

func validateSIPAccount(input *sipAccountInput, edit bool, network sipAccountNetwork) ([]int64, string) {
	input.Username = strings.TrimSpace(input.Username)
	if !sipUsername.MatchString(input.Username) {
		return nil, "SIP_INVALID_USERNAME"
	}
	if (!edit && input.Password == "") || len(input.Password) > 128 || !utf8.ValidString(input.Password) {
		return nil, "SIP_INVALID_PASSWORD"
	}
	for _, ch := range input.Password {
		if unicode.IsControl(ch) {
			return nil, "SIP_INVALID_PASSWORD"
		}
	}
	if input.Port < network.Start || input.Port > network.End || slices.Contains([]int{2019, 8080, 51820, 51821, 51822}, input.Port) {
		return nil, "SIP_INVALID_PORT"
	}
	if input.Allocation != "all" && input.Allocation != "fixed" {
		return nil, "SIP_INVALID_MODULES"
	}
	if len(input.ModuleIDs) > 256 {
		return nil, "SIP_INVALID_MODULES"
	}
	ids := make([]int64, 0, len(input.ModuleIDs))
	for _, value := range input.ModuleIDs {
		id, err := strconv.ParseInt(strings.TrimPrefix(value, "module-"), 10, 64)
		if err != nil || id < 1 || moduleID(id) != value {
			return nil, "SIP_INVALID_MODULES"
		}
		ids = append(ids, id)
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	if input.Allocation == "all" {
		ids = nil
	} else if len(ids) == 0 {
		return nil, "SIP_INVALID_MODULES"
	}
	if edit && input.Revision < 1 {
		return nil, "SIP_REVISION_REQUIRED"
	}
	return ids, ""
}

func sipDigests(username, password string) ([]byte, []byte) {
	material := username + ":" + sipRealm + ":" + password
	legacy, modern := md5.Sum([]byte(material)), sha256.Sum256([]byte(material))
	return legacy[:], modern[:]
}

const sipAccountSelect = `SELECT a.id,a.username,a.port,a.allocation,a.receive_calls,a.revision,
 COALESCE(array_agg(m.module_id ORDER BY m.module_id) FILTER (WHERE m.module_id IS NOT NULL),'{}'::bigint[])
 FROM sip_accounts a LEFT JOIN sip_account_modules m ON m.account_id=a.id`

func (s *server) sipAccountsAPI(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	tail := strings.TrimPrefix(r.URL.Path, "/api/sip/accounts")
	id := strings.TrimPrefix(tail, "/")
	if tail != "" && !sipAccountID.MatchString(id) {
		fail(w, 404, "NOT_FOUND")
		return
	}
	method := r.Method
	if method != "GET" && !(method == "POST" && tail == "") && !((method == "PATCH" || method == "DELETE") && id != "") {
		w.Header().Set("Allow", "GET, POST, PATCH, DELETE")
		fail(w, 405, "METHOD_NOT_ALLOWED")
		return
	}
	if method == "GET" {
		network, networkErr := s.sipAccountNetwork(ctx, r)
		rows, err := s.db.Query(ctx, sipAccountSelect+` WHERE ($1='' OR a.id=$1) GROUP BY a.id ORDER BY a.created_at,a.id`, id)
		if err != nil {
			fail(w, 503, "DATABASE_UNAVAILABLE")
			return
		}
		defer rows.Close()
		accounts := make([]sipAccount, 0)
		for rows.Next() {
			var a sipAccount
			var modules []int64
			if err := rows.Scan(&a.ID, &a.Username, &a.Port, &a.Allocation, &a.ReceiveCalls, &a.Revision, &modules); err != nil {
				fail(w, 503, "DATABASE_UNAVAILABLE")
				return
			}
			a.ModuleIDs = make([]string, 0, len(modules))
			for _, module := range modules {
				a.ModuleIDs = append(a.ModuleIDs, moduleID(module))
			}
			// Registration is independent of VPN connectivity and voice readiness.
			a.Status, a.IP = "offline", network.Host
			if s.sipGateway != nil && s.sipGateway.registrar.Online(a.ID) {
				a.Status = "online"
				if s.sipGateway.calls != nil && s.sipGateway.calls.router.Status(a.ID) == "busy" {
					a.Status = "busy"
				}
			}
			if networkErr != nil {
				a.IP = "—"
			}
			accounts = append(accounts, a)
		}
		if rows.Err() != nil {
			fail(w, 503, "DATABASE_UNAVAILABLE")
			return
		}
		if id != "" {
			if len(accounts) == 0 {
				fail(w, 404, "NOT_FOUND")
				return
			}
			reply(w, 200, map[string]any{"data": accounts[0]})
			return
		}
		outbound := networkErr == nil && s.sipGateway != nil && s.modules != nil && len(s.modules.moduleVoiceSamples()) > 0
		reply(w, 200, map[string]any{"data": map[string]any{"items": accounts, "network": network, "networkReady": networkErr == nil, "callsReady": outbound, "incomingReady": false}})
		return
	}
	var input sipAccountInput
	if !decodeBody(w, r, &input) {
		return
	}
	defer func() { input.Password = "" }()
	var ids []int64
	var network sipAccountNetwork
	if method != "DELETE" {
		var err error
		network, err = s.sipAccountNetwork(ctx, r)
		if err != nil {
			fail(w, 503, "SIP_NETWORK_UNAVAILABLE")
			return
		}
		var code string
		ids, code = validateSIPAccount(&input, method == "PATCH", network)
		if code != "" {
			fail(w, 400, code)
			return
		}
	} else if input.Revision < 1 {
		fail(w, 400, "SIP_REVISION_REQUIRED")
		return
	}
	s.sipAccountsMu.Lock()
	defer s.sipAccountsMu.Unlock()
	if method != "DELETE" && s.sipGateway != nil {
		if err := s.sipGateway.checkPort(input.Port, network); err != nil {
			fail(w, 409, "SIP_PORT_IN_USE")
			return
		}
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		fail(w, 503, "DATABASE_UNAVAILABLE")
		return
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(-734902)`); err != nil {
		fail(w, 503, "DATABASE_UNAVAILABLE")
		return
	}
	var oldUsername string
	var oldPort int
	var oldRevision int64
	if id != "" {
		err = tx.QueryRow(ctx, `SELECT username,port,revision FROM sip_accounts WHERE id=$1 FOR UPDATE`, id).Scan(&oldUsername, &oldPort, &oldRevision)
		if errors.Is(err, pgx.ErrNoRows) {
			fail(w, 404, "NOT_FOUND")
			return
		}
		if err != nil {
			fail(w, 503, "DATABASE_UNAVAILABLE")
			return
		}
		if oldRevision != input.Revision {
			fail(w, 409, "SIP_STALE_ACCOUNT")
			return
		}
	}
	if method == "DELETE" {
		_, err = tx.Exec(ctx, `DELETE FROM sip_accounts WHERE id=$1`, id)
	} else {
		if len(ids) > 0 {
			var count int
			if err = tx.QueryRow(ctx, `SELECT count(*) FROM modules WHERE id=ANY($1::bigint[])`, ids).Scan(&count); err != nil {
				fail(w, 503, "DATABASE_UNAVAILABLE")
				return
			}
			if count != len(ids) {
				fail(w, 400, "SIP_INVALID_MODULES")
				return
			}
		}
		if method == "POST" {
			var count int
			if err = tx.QueryRow(ctx, `SELECT count(*) FROM sip_accounts`).Scan(&count); err != nil {
				fail(w, 503, "DATABASE_UNAVAILABLE")
				return
			}
			if count >= 256 {
				fail(w, 409, "SIP_ACCOUNT_LIMIT")
				return
			}
			id = token()
			md5Hash, shaHash := sipDigests(input.Username, input.Password)
			_, err = tx.Exec(ctx, `INSERT INTO sip_accounts(id,username,port,digest_md5,digest_sha256,allocation,receive_calls) VALUES($1,$2,$3,$4,$5,$6,$7)`, id, input.Username, input.Port, md5Hash, shaHash, input.Allocation, input.ReceiveCalls)
		} else {
			if input.Username != oldUsername && input.Password == "" {
				fail(w, 400, "SIP_PASSWORD_REQUIRED")
				return
			}
			var md5Hash, shaHash []byte
			if input.Password != "" {
				md5Hash, shaHash = sipDigests(input.Username, input.Password)
			}
			revoke := input.Password != "" || input.Username != oldUsername || input.Port != oldPort
			_, err = tx.Exec(ctx, `UPDATE sip_accounts SET username=$2,port=$3,allocation=$4,receive_calls=$5,
 digest_md5=COALESCE($6,digest_md5),digest_sha256=COALESCE($7,digest_sha256),
 credential_revision=credential_revision+CASE WHEN $8 THEN 1 ELSE 0 END,revision=revision+1,updated_at=now() WHERE id=$1`, id, input.Username, input.Port, input.Allocation, input.ReceiveCalls, md5Hash, shaHash, revoke)
		}
		if err == nil {
			_, err = tx.Exec(ctx, `DELETE FROM sip_account_modules WHERE account_id=$1`, id)
			for _, module := range ids {
				if err != nil {
					break
				}
				_, err = tx.Exec(ctx, `INSERT INTO sip_account_modules(account_id,module_id) VALUES($1,$2)`, id, module)
			}
		}
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		fail(w, 409, "SIP_ACCOUNT_EXISTS")
		return
	}
	if err != nil || tx.Commit(ctx) != nil {
		fail(w, 503, "DATABASE_UNAVAILABLE")
		return
	}
	if s.sipGateway != nil {
		refresh, cancel := context.WithTimeout(ctx, 4*time.Second)
		s.sipGateway.refreshLocked(refresh)
		cancel()
	}
	if method == "DELETE" {
		w.WriteHeader(204)
		return
	}
	status := 200
	if method == "POST" {
		status = 201
	}
	reply(w, status, map[string]any{"data": map[string]any{"id": id, "revision": oldRevision + 1}})
}
