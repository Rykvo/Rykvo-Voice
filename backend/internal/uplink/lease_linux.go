package uplink

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

var leaseDirectory = "/run/rykvo-voice"

// A lease owns only marked socket routes, never the host's main table or DNS.
type Lease struct {
	Dialer          *net.Dialer
	Resolver        *net.Resolver
	network         NetworkInfo
	table           string
	rules           []string
	close           sync.Once
	closed          atomic.Bool
	stop            chan struct{}
	record          string
	policyID        string
	policySignature string
	routeBaseline   []string
}

func Open(ctx context.Context, id string) (*Lease, error) {
	n, err := Find(ctx, id)
	if err != nil {
		return nil, err
	}
	l, err := openNetwork(ctx, n)
	if err == nil {
		l.policyID, l.policySignature = id, runtimeSignature(id)
	}
	return l, err
}

func openNetwork(ctx context.Context, n NetworkInfo) (_ *Lease, err error) {
	if n.State != "configured" || len(n.DNS) == 0 {
		return nil, errors.New("NETWORK_UNAVAILABLE")
	}
	// Serialize allocation across socket-activated workers. No caller chooses a table.
	f, err := os.OpenFile(filepath.Join(leaseDirectory, "uplink.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, errors.New("NETWORK_UNAVAILABLE")
	}
	defer f.Close()
	if unix.Flock(int(f.Fd()), unix.LOCK_EX) != nil {
		return nil, errors.New("NETWORK_UNAVAILABLE")
	}
	defer unix.Flock(int(f.Fd()), unix.LOCK_UN)
	cleanupAbandoned(ctx)
	l := &Lease{network: n, stop: make(chan struct{})}
	defer func() {
		if err != nil {
			l.Close()
		}
	}()
	var mark uint32
	for attempt := 0; attempt < 10; attempt++ {
		var b [4]byte
		if _, err = rand.Read(b[:]); err != nil {
			return nil, err
		}
		mark = 0x52000000 | (binary.BigEndian.Uint32(b[:]) & 0xffffff)
		table := strconv.FormatUint(uint64(mark), 10)
		free := true
		for _, family := range []string{"-4", "-6"} {
			rules, e := command(ctx, "ip", family, "rule", "show")
			if e != nil {
				return nil, errors.New("NETWORK_UNAVAILABLE")
			}
			if !compatibleRules(string(rules)) {
				return nil, errors.New("NETWORK_UNAVAILABLE")
			}
			if strings.Contains(string(rules), table) || strings.Contains(string(rules), fmt.Sprintf("0x%x", mark)) {
				free = false
			}
			routes, _ := command(ctx, "ip", family, "route", "show", "table", table)
			if len(routes) > 0 {
				free = false
			}
		}
		if free {
			l.table = table
			break
		}
	}
	if l.table == "" {
		return nil, errors.New("NETWORK_UNAVAILABLE")
	}
	l.record = filepath.Join(leaseDirectory, "uplink-"+l.table+".json")
	owner := routeOwner{Table: l.table, PID: os.Getpid(), Start: processStart(os.Getpid()), Namespace: networkNamespace()}
	b, _ := json.Marshal(owner)
	file, e := os.OpenFile(l.record, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0600)
	if e != nil {
		l.record = ""
		l.table = ""
		return nil, errors.New("NETWORK_UNAVAILABLE")
	}
	_, e = file.Write(b)
	closeErr := file.Close()
	if e != nil || closeErr != nil {
		return nil, errors.New("NETWORK_UNAVAILABLE")
	}
	expectedRoutes := 2
	for _, family := range []string{"-4", "-6"} {
		v4 := family == "-4"
		// A terminal route prevents fall-through to a different uplink on link loss.
		if _, err = command(ctx, "ip", family, "route", "add", "unreachable", "default", "metric", "32767", "table", l.table); err != nil {
			return nil, errors.New("NETWORK_UNAVAILABLE")
		}
		seen := map[string]bool{}
		for _, a := range n.Addresses {
			ip, e := netip.ParseAddr(a.Local)
			if e != nil || ip.Is4() != v4 {
				continue
			}
			prefix := netip.PrefixFrom(ip, a.Prefix).Masked().String()
			if seen[prefix] {
				continue
			}
			seen[prefix] = true
			expectedRoutes++
			if _, err = command(ctx, "ip", family, "route", "add", prefix, "dev", n.Name, "src", a.Local, "table", l.table); err != nil {
				return nil, errors.New("NETWORK_UNAVAILABLE")
			}
		}
		for _, g := range n.Gateways {
			ip, e := netip.ParseAddr(g)
			if e != nil || ip.Is4() != v4 || n.source(v4) == nil {
				continue
			}
			if _, err = command(ctx, "ip", family, "route", "add", "default", "via", g, "dev", n.Name, "onlink", "src", n.source(v4).String(), "metric", "100", "table", l.table); err != nil {
				return nil, errors.New("NETWORK_UNAVAILABLE")
			}
			expectedRoutes++
			break
		}
		if _, err = command(ctx, "ip", family, "rule", "add", "priority", "1", "fwmark", l.table, "lookup", l.table); err != nil {
			return nil, errors.New("NETWORK_UNAVAILABLE")
		}
		l.rules = append(l.rules, family)
	}
	state, rules, routes, e := l.routeState()
	if e != nil || rules != 2 || routes != expectedRoutes {
		return nil, errors.New("NETWORK_UNAVAILABLE")
	}
	l.routeBaseline = state
	control := func(_, _ string, c syscall.RawConn) error {
		if l.closed.Load() {
			return errors.New("NETWORK_UNAVAILABLE")
		}
		current, e := net.InterfaceByName(n.Name)
		if e != nil || current.Index != n.Index || current.Flags&net.FlagUp == 0 {
			return errors.New("NETWORK_UNAVAILABLE")
		}
		var socketErr error
		e = c.Control(func(fd uintptr) {
			socketErr = unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, n.Name)
			if socketErr == nil {
				socketErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_MARK, int(mark))
			}
		})
		if e != nil {
			return e
		}
		return socketErr
	}
	var next atomic.Uint32
	resolver := &net.Resolver{PreferGo: true, StrictErrors: true}
	dnsDialer := &net.Dialer{Timeout: 5 * time.Second, Control: control}
	resolver.Dial = func(call context.Context, network, _ string) (net.Conn, error) {
		ip := netip.MustParseAddr(n.DNS[int(next.Add(1)-1)%len(n.DNS)])
		host := ip.String()
		if ip.IsLinkLocalUnicast() {
			host += "%" + n.Name
		}
		return dnsDialer.DialContext(call, network, net.JoinHostPort(host, "53"))
	}
	l.Resolver = resolver
	l.Dialer = &net.Dialer{Timeout: 10 * time.Second, Control: control, Resolver: resolver}
	return l, nil
}

// Watch cancels the session when its exact link configuration disappears/changes.
// The caller keeps routes alive until IMS/MMS cleanup finishes, then closes them.
func (l *Lease) Watch(ctx context.Context, cancel context.CancelFunc) {
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	var nextRoutes time.Time
	routeErrors := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-l.stop:
			return
		case <-t.C:
			if l.policyID != "" && runtimeSignature(l.policyID) != l.policySignature {
				cancel()
				return
			}
			// Global discovery/DNS is sampled once by the manager, not per module.
			// Here only guard this lease's actual kernel interface generation/address.
			n, e := net.InterfaceByName(l.network.Name)
			if e != nil || n.Index != l.network.Index || n.Flags&net.FlagUp == 0 || !sameAddresses(n, l.network.Addresses) {
				cancel()
				return
			}
			if time.Now().Before(nextRoutes) {
				continue
			}
			nextRoutes = time.Now().Add(15 * time.Second)
			state, _, _, err := l.routeState()
			if err != nil {
				routeErrors++
				if routeErrors < 3 {
					continue
				}
			} else {
				routeErrors = 0
				if slices.Equal(state, l.routeBaseline) {
					continue
				}
			}
			// DHCP/network managers may remove rules without changing the link.
			cancel()
			return
		}
	}
}

func sameAddresses(n *net.Interface, expected []Address) bool {
	addresses, e := n.Addrs()
	if e != nil {
		return false
	}
	for _, want := range expected {
		found := false
		for _, got := range addresses {
			p, e := netip.ParsePrefix(got.String())
			if e == nil && p.Addr().String() == want.Local && p.Bits() == want.Prefix {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
func (l *Lease) Close() {
	if l == nil {
		return
	}
	l.close.Do(func() {
		l.closed.Store(true)
		close(l.stop)
		if l.table == "" {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		// Remove only exact rules/tables created by this lease.
		for _, family := range l.rules {
			_, _ = command(ctx, "ip", family, "rule", "del", "priority", "1", "fwmark", l.table, "lookup", l.table)
		}
		for _, family := range []string{"-4", "-6"} {
			_, _ = command(ctx, "ip", family, "route", "flush", "table", l.table)
		}
		if l.record != "" {
			_ = os.Remove(l.record)
		}
	})
}

type routeOwner struct {
	Table            string
	PID              int
	Start, Namespace string
}

func networkNamespace() string { v, _ := os.Readlink("/proc/self/ns/net"); return v }
func processStart(pid int) string {
	v := read(fmt.Sprintf("/proc/%d/stat", pid))
	end := strings.LastIndex(v, ")")
	if end < 0 {
		return ""
	}
	fields := strings.Fields(v[end+1:])
	if len(fields) < 20 {
		return ""
	}
	return fields[19]
}
func compatibleRules(rules string) bool {
	for _, line := range strings.Split(rules, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		priority, e := strconv.Atoi(strings.TrimSuffix(fields[0], ":"))
		if e != nil {
			return false
		}
		if priority > 1 {
			continue
		}
		// ip spacing varies; only the standard local rule or our marked rule is allowed first.
		if priority == 0 && strings.Join(fields[1:], " ") == "from all lookup local" {
			continue
		}
		if priority == 1 && len(fields) == 7 && fields[1] == "from" && fields[2] == "all" && fields[3] == "fwmark" && fields[5] == "lookup" {
			mark, e1 := strconv.ParseUint(fields[4], 0, 32)
			table, e2 := strconv.ParseUint(fields[6], 10, 32)
			if e1 == nil && e2 == nil && mark == table && mark>>24 == 0x52 {
				continue
			}
		}
		return false
	}
	return true
}
func cleanupAbandoned(ctx context.Context) {
	paths, _ := filepath.Glob(filepath.Join(leaseDirectory, "uplink-*.json"))
	for _, p := range paths {
		info, e := os.Lstat(p)
		if e != nil || !info.Mode().IsRegular() || info.Size() > 1024 {
			continue
		}
		b, e := os.ReadFile(p)
		if e != nil {
			continue
		}
		var owner routeOwner
		if json.Unmarshal(b, &owner) != nil || owner.Namespace != networkNamespace() || owner.PID < 1 || owner.Start == "" {
			continue
		}
		table, e := strconv.ParseUint(owner.Table, 10, 32)
		if e != nil || table>>24 != 0x52 || filepath.Base(p) != "uplink-"+owner.Table+".json" {
			continue
		}
		if processStart(owner.PID) == owner.Start {
			continue
		}
		for _, family := range []string{"-4", "-6"} {
			_, _ = command(ctx, "ip", family, "rule", "del", "priority", "1", "fwmark", owner.Table, "lookup", owner.Table)
			_, _ = command(ctx, "ip", family, "route", "flush", "table", owner.Table)
		}
		_ = os.Remove(p)
	}
}
