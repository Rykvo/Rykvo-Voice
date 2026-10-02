package main

import (
	"context"
	"errors"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"rykvo.local/auth/internal/sipregistrar"
	"rykvo.local/auth/internal/telephony"
)

type sipListener struct {
	ua   *sipgo.UserAgent
	udp  net.PacketConn
	tcp  net.Listener
	port int
}

func (l *sipListener) close() { l.udp.Close(); l.tcp.Close(); l.ua.Close() }

type sipGateway struct {
	server    *server
	registrar *sipregistrar.Registrar
	listeners map[string]*sipListener // guarded by sipAccountsMu
	done      chan struct{}
	calls     *sipCalls
	epoch     uint64
	ready     bool
	checkedAt time.Time
	accounts  []sipregistrar.Account
	policies  []telephony.Policy
	network   sipAccountNetwork
	status    workerStatus
}

func newSIPGateway(s *server) *sipGateway {
	g := &sipGateway{server: s, registrar: sipregistrar.New(), listeners: map[string]*sipListener{}, done: make(chan struct{})}
	g.calls = newSIPCalls(g)
	return g
}

func newSIPPeerClient(ua *sipgo.UserAgent, public string, port int, listener, transport string) (*sipgo.Client, error) {
	bind := listener
	if strings.EqualFold(transport, "TCP") {
		// TCP flows share a local listener; reuse by remote endpoint, not local port.
		host, _, err := net.SplitHostPort(listener)
		if err != nil {
			return nil, err
		}
		bind = net.JoinHostPort(host, "0")
	}
	return sipgo.NewClient(ua, sipgo.WithClientHostname(public), sipgo.WithClientPort(port), sipgo.WithClientConnectionAddr(bind))
}
func localSIPAddresses() []string {
	var result []string
	interfaces, _ := net.Interfaces()
	for _, device := range interfaces {
		if device.Flags&net.FlagUp == 0 || device.Flags&net.FlagLoopback != 0 {
			continue
		}
		skip := false
		for _, prefix := range []string{"sip", "wg", "tun", "tap", "rvpn", "rv-", "vocat", "docker", "veth", "br-"} {
			if strings.HasPrefix(device.Name, prefix) {
				skip = true
			}
		}
		if skip {
			continue
		}
		addresses, _ := device.Addrs()
		for _, address := range addresses {
			ip, _, err := net.ParseCIDR(address.String())
			if err == nil && ip.To4() != nil && ip.IsPrivate() {
				result = append(result, ip.String())
			}
		}
	}
	slices.Sort(result)
	return slices.Compact(result)
}
func (g *sipGateway) run(ctx context.Context) {
	defer close(g.done)
	defer g.status.update("sip", "WORKER_STOPPED")
	g.calls.ctx = ctx
	for g.server.db != nil {
		recovery, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := g.server.recoverCallRecords(recovery)
		cancel()
		if err == nil {
			break
		}
		g.status.update("sip", "CALL_RECORD_RECOVERY_FAILED")
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
	}
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	refresh := func() {
		call, cancel := context.WithTimeout(ctx, 4*time.Second)
		defer cancel()
		g.refresh(call)
	}
	refresh()
	for {
		select {
		case <-ctx.Done():
			g.server.sipAccountsMu.Lock()
			for _, call := range g.calls.active {
				call.stop()
			}
			for _, call := range g.calls.incoming {
				call.stop("interrupted")
			}
			g.server.sipAccountsMu.Unlock()
			g.calls.wait.Wait()
			g.server.sipAccountsMu.Lock()
			for key, l := range g.listeners {
				l.close()
				delete(g.listeners, key)
			}
			g.registrar.Replace(nil)
			g.server.sipAccountsMu.Unlock()
			return
		case <-ticker.C:
			refresh()
		case <-g.calls.probeWake:
			g.server.sipAccountsMu.Lock()
			if g.snapshotReady() {
				g.calls.scanIncomingLocked(g.network)
			}
			g.server.sipAccountsMu.Unlock()
		}
	}
}

type sipSnapshot struct {
	addresses []string
	network   sipAccountNetwork
	accounts  []sipregistrar.Account
	policies  []telephony.Policy
}

func (g *sipGateway) snapshotReady() bool {
	return g.ready && time.Since(g.checkedAt) < 15*time.Second
}

// The periodic database/network read never holds the SIP dialog mutex.
func (g *sipGateway) refresh(ctx context.Context) {
	g.server.sipAccountsMu.Lock()
	epoch := g.epoch
	g.server.sipAccountsMu.Unlock()
	snapshot, err := g.readSnapshot(ctx)
	g.server.sipAccountsMu.Lock()
	defer g.server.sipAccountsMu.Unlock()
	if epoch != g.epoch {
		return
	}
	if err != nil {
		g.ready = false
		g.status.update("sip", "SIP_REFRESH_FAILED")
		// Existing calls keep their media; new work fails closed until a fresh snapshot.
		g.calls.actionsLocked(g.calls.router.Tick())
		g.calls.releaseFinishedLocked()
		return
	}
	g.applySnapshotLocked(snapshot)
}

// Fence stale snapshots and update only the affected account before any slow read.
func (g *sipGateway) applyAccountLocked(id string, account *sipregistrar.Account, policy *telephony.Policy) {
	g.epoch++
	accounts := make([]sipregistrar.Account, 0, len(g.accounts)+1)
	policies := make([]telephony.Policy, 0, len(g.policies)+1)
	for _, a := range g.accounts {
		if a.ID != id {
			accounts = append(accounts, a)
		}
	}
	for _, p := range g.policies {
		if p.Account != id {
			policies = append(policies, p)
		}
	}
	if account != nil {
		accounts = append(accounts, *account)
	}
	if policy != nil {
		policies = append(policies, *policy)
	}
	g.accounts, g.policies = accounts, policies
	g.registrar.Replace(accounts)
	g.calls.syncLocked(accounts, policies)
}

// Preserve the policy version, but require fresh authentication after reconciliation.
func (g *sipGateway) suspendAccountLocked(id string) {
	var policy *telephony.Policy
	for _, p := range g.policies {
		if p.Account == id {
			policy = &p
			break
		}
	}
	g.applyAccountLocked(id, nil, policy)
}

func (g *sipGateway) readSnapshot(ctx context.Context) (sipSnapshot, error) {
	snapshot := sipSnapshot{addresses: localSIPAddresses()}
	host := ""
	if len(snapshot.addresses) > 0 {
		host = snapshot.addresses[0]
	}
	network, err := g.server.sipAccountNetwork(ctx, &http.Request{Host: host})
	if err != nil {
		return snapshot, err
	}
	snapshot.network = network
	if network.Mode == "cloud" {
		ip := net.ParseIP(network.BindAddress)
		if ip == nil || ip.IsUnspecified() || ip.IsLoopback() {
			return snapshot, errors.New("SIP_NETWORK_UNAVAILABLE")
		}
		snapshot.addresses = []string{ip.String()}
	}
	rows, err := g.server.db.Query(ctx, `SELECT a.id,a.username,a.port,a.credential_revision,a.digest_md5,a.digest_sha256,a.allocation,a.receive_calls,a.low_bandwidth_audio,
  COALESCE(array_agg(m.module_id) FILTER(WHERE m.module_id IS NOT NULL),'{}'::bigint[]) FROM sip_accounts a LEFT JOIN sip_account_modules m ON m.account_id=a.id GROUP BY a.id ORDER BY a.id`)
	if err != nil {
		return snapshot, err
	}
	defer rows.Close()
	for rows.Next() {
		var a sipregistrar.Account
		var allocation string
		var receive bool
		var modules []int64
		if err = rows.Scan(&a.ID, &a.Username, &a.Port, &a.Revision, &a.MD5, &a.SHA256, &allocation, &receive, &a.LowBandwidthAudio, &modules); err != nil {
			return snapshot, err
		}
		p := telephony.Policy{Account: a.ID, Revision: uint64(a.Revision), All: allocation == "all", Receive: receive}
		for _, id := range modules {
			p.Modules = append(p.Modules, moduleID(id))
		}
		snapshot.accounts = append(snapshot.accounts, a)
		snapshot.policies = append(snapshot.policies, p)
	}
	return snapshot, rows.Err()
}

// Snapshot audio preferences once per new call; edits never renegotiate active media.
func (g *sipGateway) lowBandwidthAudioLocked(id string) bool {
	for _, a := range g.accounts {
		if a.ID == id {
			return a.LowBandwidthAudio
		}
	}
	return false
}

func (g *sipGateway) applySnapshotLocked(snapshot sipSnapshot) {
	desired := map[string]int{}
	for _, a := range snapshot.accounts {
		if a.Port < snapshot.network.Start || a.Port > snapshot.network.End {
			continue
		}
		for _, host := range snapshot.addresses {
			desired[net.JoinHostPort(host, strconv.Itoa(a.Port))] = a.Port
		}
	}
	for key, l := range g.listeners {
		if _, ok := desired[key]; !ok {
			l.close()
			delete(g.listeners, key)
			g.registrar.OfflinePort(l.port)
		}
	}
	for address, port := range desired {
		if _, ok := g.listeners[address]; ok {
			continue
		}
		l, err := g.listen(address, port)
		if err != nil {
			continue
		}
		g.listeners[address] = l
	}
	var enabled []sipregistrar.Account
	for _, a := range snapshot.accounts {
		for _, l := range g.listeners {
			if l.port == a.Port {
				enabled = append(enabled, a)
				break
			}
		}
	}
	g.registrar.Replace(enabled)
	g.accounts = enabled
	g.policies = snapshot.policies
	g.network = snapshot.network
	g.ready = true
	g.checkedAt = time.Now()
	issue := ""
	if len(enabled) != len(snapshot.accounts) {
		issue = "SIP_LISTENER_UNAVAILABLE"
	}
	g.status.update("sip", issue)
	g.calls.syncLocked(enabled, snapshot.policies)
	g.calls.scanIncomingLocked(snapshot.network)
}
func (g *sipGateway) listen(address string, port int) (*sipListener, error) {
	udp, err := net.ListenPacket("udp", address)
	if err != nil {
		return nil, err
	}
	tcp, err := net.Listen("tcp", address)
	if err != nil {
		udp.Close()
		return nil, err
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ua, err := sipgo.NewUA(sipgo.WithUserAgent("Rykvo Voice"), sipgo.WithUserAgentTransactionLayerOptions(sip.WithTransactionLayerLogger(logger)), sipgo.WithUserAgentTransportLayerOptions(sip.WithTransportLayerLogger(logger)))
	if err != nil {
		udp.Close()
		tcp.Close()
		return nil, err
	}
	srv, err := sipgo.NewServer(ua, sipgo.WithServerLogger(logger))
	if err != nil {
		udp.Close()
		tcp.Close()
		ua.Close()
		return nil, err
	}
	handle := func(req *sip.Request, tx sip.ServerTransaction) {
		g.server.sipAccountsMu.Lock()
		defer g.server.sipAccountsMu.Unlock()
		req.SetDestination(address)
		if req.Method == sip.REGISTER && g.server.db != nil && !g.snapshotReady() {
			_ = tx.Respond(sip.NewResponseFromRequest(req, 503, "Service Unavailable", nil))
			return
		}
		res := g.registrar.Handle(req, port)
		_ = tx.Respond(res)
		if req.Method == sip.REGISTER && res.StatusCode == 200 && g.server.db != nil {
			g.calls.syncLocked(g.accounts, g.policies)
		}
	}
	dialog := func(req *sip.Request, tx sip.ServerTransaction) {
		g.server.sipAccountsMu.Lock()
		defer g.server.sipAccountsMu.Unlock()
		g.calls.dialogLocked(req, tx)
	}
	srv.OnAck(dialog)
	srv.OnBye(dialog)
	srv.OnRegister(handle)
	srv.OnOptions(handle)
	srv.OnInvite(g.callHandler(func(req *sip.Request, tx sip.ServerTransaction) <-chan struct{} {
		return g.calls.inviteLocked(req, tx, port, address, ua)
	}))
	srv.OnNoRoute(func(req *sip.Request, tx sip.ServerTransaction) {
		_ = tx.Respond(sip.NewResponseFromRequest(req, 405, "Method Not Allowed", nil))
	})
	listener := &sipListener{ua: ua, udp: udp, tcp: tcp, port: port}
	go func() {
		if err := srv.ServeUDP(udp); err != nil && !errors.Is(err, net.ErrClosed) {
			log.Print("SIP UDP listener stopped")
		}
	}()
	go func() {
		if err := srv.ServeTCP(tcp); err != nil && !errors.Is(err, net.ErrClosed) {
			log.Print("SIP TCP listener stopped")
		}
	}()
	return listener, nil
}

// Probe new bindings without taking over another service's socket.
func (g *sipGateway) checkPort(port int, network sipAccountNetwork) error {
	addresses := localSIPAddresses()
	if network.Mode == "cloud" {
		addresses = []string{network.BindAddress}
	}
	if len(addresses) == 0 {
		return errors.New("no local SIP address")
	}
	for _, host := range addresses {
		if net.ParseIP(host) == nil {
			return errors.New("invalid SIP bind address")
		}
		address := net.JoinHostPort(host, strconv.Itoa(port))
		if _, owned := g.listeners[address]; owned {
			continue
		}
		udp, err := net.ListenPacket("udp", address)
		if err != nil {
			return err
		}
		tcp, err := net.Listen("tcp", address)
		udp.Close()
		if err != nil {
			return err
		}
		tcp.Close()
	}
	return nil
}

// Keep the INVITE transaction alive without blocking account changes or ACK/BYE.
func (g *sipGateway) callHandler(start func(*sip.Request, sip.ServerTransaction) <-chan struct{}) sipgo.RequestHandler {
	return func(req *sip.Request, tx sip.ServerTransaction) {
		g.server.sipAccountsMu.Lock()
		done := start(req, tx)
		g.server.sipAccountsMu.Unlock()
		if done != nil {
			<-done
		}
	}
}
