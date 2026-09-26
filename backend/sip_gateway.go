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
}

func newSIPGateway(s *server) *sipGateway {
	g := &sipGateway{server: s, registrar: sipregistrar.New(), listeners: map[string]*sipListener{}, done: make(chan struct{})}
	g.calls = newSIPCalls(g)
	return g
}
func localSIPAddresses() []string {
	var result []string
	interfaces, _ := net.Interfaces()
	for _, device := range interfaces {
		if device.Flags&net.FlagUp == 0 || device.Flags&net.FlagLoopback != 0 {
			continue
		}
		skip := false
		for _, prefix := range []string{"sip", "wg", "tun", "tap", "rv-", "vocat", "docker", "veth", "br-"} {
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
	g.calls.ctx = ctx
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		g.server.sipAccountsMu.Lock()
		refresh, cancel := context.WithTimeout(ctx, 4*time.Second)
		g.refreshLocked(refresh)
		cancel()
		g.server.sipAccountsMu.Unlock()
		select {
		case <-ctx.Done():
			g.server.sipAccountsMu.Lock()
			for _, call := range g.calls.active {
				call.stop()
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
		}
	}
}
func (g *sipGateway) refreshLocked(ctx context.Context) {
	addresses := localSIPAddresses()
	host := ""
	if len(addresses) > 0 {
		host = addresses[0]
	}
	network, err := g.server.sipAccountNetwork(ctx, &http.Request{Host: host})
	if err != nil {
		addresses = nil
	}
	if network.Mode == "cloud" {
		if ip := net.ParseIP(network.BindAddress); err == nil && ip != nil && !ip.IsUnspecified() && !ip.IsLoopback() {
			addresses = []string{ip.String()}
		} else {
			addresses = nil
		}
	}
	accounts, err := g.readAccounts(ctx)
	if err != nil {
		addresses = nil
		accounts = nil
	}
	desired := map[string]int{}
	for _, a := range accounts {
		if a.Port < network.Start || a.Port > network.End {
			continue
		}
		for _, host := range addresses {
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
	for _, a := range accounts {
		for _, l := range g.listeners {
			if l.port == a.Port {
				enabled = append(enabled, a)
				break
			}
		}
	}
	g.registrar.Replace(enabled)
	g.calls.syncLocked(ctx, enabled)
}
func (g *sipGateway) readAccounts(ctx context.Context) ([]sipregistrar.Account, error) {
	rows, err := g.server.db.Query(ctx, `SELECT id,username,port,credential_revision,digest_md5,digest_sha256 FROM sip_accounts ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var accounts []sipregistrar.Account
	for rows.Next() {
		var a sipregistrar.Account
		if err := rows.Scan(&a.ID, &a.Username, &a.Port, &a.Revision, &a.MD5, &a.SHA256); err != nil {
			return nil, err
		}
		accounts = append(accounts, a)
	}
	return accounts, rows.Err()
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
		res := g.registrar.Handle(req, port)
		_ = tx.Respond(res)
		if req.Method == sip.REGISTER && res.StatusCode == 200 && g.server.db != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if accounts, err := g.readAccounts(ctx); err == nil {
				g.calls.syncLocked(ctx, accounts)
			}
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
	srv.OnInvite(func(req *sip.Request, tx sip.ServerTransaction) {
		g.server.sipAccountsMu.Lock()
		defer g.server.sipAccountsMu.Unlock()
		g.calls.inviteLocked(req, tx, port, address, ua)
	})
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
