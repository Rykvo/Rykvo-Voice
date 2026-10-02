package uplink

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Address struct {
	Family     string `json:"family"`
	Local      string `json:"local"`
	Prefix     int    `json:"prefixlen"`
	Scope      string `json:"scope"`
	Tentative  bool   `json:"tentative,omitempty"`
	Deprecated bool   `json:"deprecated,omitempty"`
	DADFailed  bool   `json:"dadfailed,omitempty"`
}
type NetworkInfo struct {
	HostDefault []string  `json:"hostDefault,omitempty"`
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Label       string    `json:"label"`
	State       string    `json:"state"`
	Index       int       `json:"-"`
	Addresses   []Address `json:"addresses"`
	Gateways    []string  `json:"gateways"`
	DNS         []string  `json:"dns"`
}
type linkInfo struct {
	Index        int       `json:"ifindex"`
	Name         string    `json:"ifname"`
	MAC          string    `json:"address"`
	PermanentMAC string    `json:"permaddr"`
	Flags        []string  `json:"flags"`
	Addresses    []Address `json:"addr_info"`
}
type routeInfo struct {
	Family      string     `json:"-"`
	Type        string     `json:"type"`
	Flags       []string   `json:"flags"`
	NextHops    []routeHop `json:"nexthops"`
	Destination string     `json:"dst"`
	Device      string     `json:"dev"`
	Gateway     string     `json:"gateway"`
	Metric      int        `json:"metric"`
}

type routeHop struct {
	Device string   `json:"dev"`
	Flags  []string `json:"flags"`
}

// Main-table defaults only; VPN/policy-specific traffic keeps its own routing.
func hostDefaults(routes []routeInfo) map[string][]string {
	out := map[string][]string{}
	for _, family := range []string{"IPv4", "IPv6"} {
		best, found := 0, false
		candidates := map[string]bool{}
		for _, r := range routes {
			if r.Family != family || r.Destination != "default" || (r.Type != "" && r.Type != "unicast") || has(r.Flags, "linkdown") || has(r.Flags, "dead") {
				continue
			}
			hops := r.NextHops
			if len(hops) == 0 {
				hops = []routeHop{{Device: r.Device}}
			}
			var devices []string
			for _, h := range hops {
				if h.Device != "" && !has(h.Flags, "dead") && !has(h.Flags, "linkdown") {
					devices = append(devices, h.Device)
				}
			}
			if len(devices) == 0 || found && r.Metric > best {
				continue
			}
			if !found || r.Metric < best {
				candidates = map[string]bool{}
				best, found = r.Metric, true
			}
			for _, name := range devices {
				candidates[name] = true
			}
		}
		for name := range candidates {
			out[name] = append(out[name], family)
		}
	}
	return out
}

func command(ctx context.Context, name string, args ...string) ([]byte, error) {
	call, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(call, name, args...)
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C"}
	return cmd.Output()
}
func read(path string) string { b, _ := os.ReadFile(path); return strings.TrimSpace(string(b)) }
func has(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}
func stableID(device, mac string) string {
	function := read(filepath.Join(device, "bInterfaceNumber"))
	for p := device; p != "/" && p != "."; p = filepath.Dir(p) {
		vendor := read(filepath.Join(p, "idVendor"))
		if vendor == "" {
			continue
		}
		// Modem data bearers are not host uplinks.
		if vendor == "2c7c" || vendor == "05c6" {
			return ""
		}
		if serial := read(filepath.Join(p, "serial")); serial != "" {
			return digest("usb:" + vendor + ":" + read(filepath.Join(p, "idProduct")) + ":" + serial + ":" + function)
		}
		break
	}
	if mac == "" {
		return ""
	}
	return digest(device + ":" + mac)
}
func digest(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:16]) }

// Inspect is read-only. Link readiness does not claim internet/carrier reachability.
func Inspect(ctx context.Context) ([]NetworkInfo, error) {
	return inspect(ctx, "")
}
func inspect(ctx context.Context, selected string) ([]NetworkInfo, error) {
	b, err := command(ctx, "ip", "-j", "address", "show")
	if err != nil {
		return nil, errors.New("NETWORK_INVENTORY_UNAVAILABLE")
	}
	var links []linkInfo
	if json.Unmarshal(b, &links) != nil {
		return nil, errors.New("NETWORK_INVENTORY_UNAVAILABLE")
	}
	var routes []routeInfo
	for _, family := range []string{"-4", "-6"} {
		b, err = command(ctx, "ip", "-j", family, "route", "show", "table", "main")
		if err != nil {
			return nil, errors.New("NETWORK_INVENTORY_UNAVAILABLE")
		}
		var values []routeInfo
		if json.Unmarshal(b, &values) != nil {
			return nil, errors.New("NETWORK_INVENTORY_UNAVAILABLE")
		}
		for i := range values {
			if family == "-4" {
				values[i].Family = "IPv4"
			} else {
				values[i].Family = "IPv6"
			}
		}
		routes = append(routes, values...)
	}
	defaults := hostDefaults(routes)
	var result = []NetworkInfo{}
	for _, l := range links {
		root := filepath.Join("/sys/class/net", l.Name)
		device, err := filepath.EvalSymlinks(filepath.Join(root, "device"))
		if err != nil || strings.Contains(device, "/virtual/") {
			continue
		}
		driver, _ := filepath.EvalSymlinks(filepath.Join(root, "device/driver"))
		if has([]string{"qmi_wwan", "cdc_mbim", "mhi_net", "iosm", "t7xx"}, filepath.Base(driver)) {
			continue
		}
		mac := l.PermanentMAC
		if mac == "" {
			mac = l.MAC
		}
		id := stableID(device, mac)
		if id == "" || selected != "" && id != selected {
			continue
		}
		n := NetworkInfo{HostDefault: defaults[l.Name], ID: id, Name: l.Name, Label: l.Name, Index: l.Index, State: "unavailable", Addresses: []Address{}, Gateways: []string{}, DNS: []string{}}
		for _, a := range l.Addresses {
			ip, e := netip.ParseAddr(a.Local)
			if e == nil && !a.Tentative && !a.Deprecated && !a.DADFailed && a.Scope == "global" && ip.IsGlobalUnicast() && ((ip.Is4() && a.Prefix >= 0 && a.Prefix <= 32) || (ip.Is6() && a.Prefix >= 0 && a.Prefix <= 128)) {
				n.Addresses = append(n.Addresses, a)
			}
		}
		sort.SliceStable(routes, func(i, j int) bool { return routes[i].Metric < routes[j].Metric })
		for _, r := range routes {
			if r.Device == l.Name && r.Destination == "default" {
				if ip, e := netip.ParseAddr(r.Gateway); e == nil && !ip.IsUnspecified() {
					n.Gateways = append(n.Gateways, r.Gateway)
				}
			}
		}
		if len(n.Gateways) == 0 {
			n.Gateways = linkGateways(ctx, l.Name, l.Index)
		}
		n.DNS = linkDNS(ctx, l.Name, l.Index)
		v4, v6 := n.routedFamilies()
		dns := n.DNS[:0]
		for _, value := range n.DNS {
			ip, _ := netip.ParseAddr(value)
			if ip.Is4() && v4 || ip.Is6() && v6 {
				dns = append(dns, value)
			}
		}
		n.DNS = dns
		n.State = linkState(l, n)
		if has(l.Flags, "UP") && has(l.Flags, "LOWER_UP") && (v4 || v6) {
			n.State = "configured"
			if len(n.DNS) == 0 {
				n.State = "dns-missing"
			}
			all, _ := strconv.Atoi(read("/proc/sys/net/ipv4/conf/all/rp_filter"))
			own, _ := strconv.Atoi(read(filepath.Join("/proc/sys/net/ipv4/conf", l.Name, "rp_filter")))
			if v4 && max(all, own) == 1 {
				n.State = "strict-rpf"
			}
		}
		result = append(result, n)
	}
	counts := map[string]int{}
	for _, n := range result {
		counts[n.ID]++
	}
	for i := range result {
		if counts[result[i].ID] > 1 {
			result[i].State = "ambiguous"
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

func linkState(l linkInfo, n NetworkInfo) string {
	if !has(l.Flags, "UP") {
		return "unconfigured"
	}
	if !has(l.Flags, "LOWER_UP") {
		return "no-carrier"
	}
	if len(n.Addresses) == 0 {
		return "address-pending"
	}
	return "gateway-missing"
}

func nmLease(ctx context.Context, name, option string) []string {
	b, err := command(ctx, "nmcli", "-g", "DHCP4.OPTION", "device", "show", name)
	if err != nil {
		return nil
	}
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if ok && strings.TrimSpace(k) == option {
			return validDNS(strings.Fields(v))
		}
	}
	return nil
}

func linkDNS(ctx context.Context, name string, index int) []string {
	var tokens []string
	if b, e := command(ctx, "resolvectl", "dns", name); e == nil {
		if _, v, ok := strings.Cut(string(b), ":"); ok {
			tokens = append(tokens, strings.Fields(v)...)
		}
	}
	tokens = validDNS(tokens)
	if len(tokens) == 0 {
		for _, line := range strings.Split(read(fmt.Sprintf("/run/systemd/netif/leases/%d", index)), "\n") {
			if v, ok := strings.CutPrefix(line, "DNS="); ok {
				tokens = append(tokens, strings.Fields(v)...)
			}
		}
	}
	tokens = validDNS(tokens)
	if len(tokens) == 0 {
		if b, e := command(ctx, "nmcli", "-g", "IP4.DNS,IP6.DNS", "device", "show", name); e == nil {
			tokens = strings.Fields(strings.ReplaceAll(string(b), "\\:", ":"))
		}
	}
	if len(validDNS(tokens)) == 0 {
		tokens = nmLease(ctx, name, "domain_name_servers")
	}
	return validDNS(tokens)
}

func linkGateways(ctx context.Context, name string, index int) []string {
	var values []string
	for _, line := range strings.Split(read(fmt.Sprintf("/run/systemd/netif/leases/%d", index)), "\n") {
		if v, ok := strings.CutPrefix(line, "ROUTER="); ok {
			values = append(values, strings.Fields(v)...)
		}
	}
	if len(values) == 0 {
		if b, e := command(ctx, "nmcli", "-g", "IP4.GATEWAY,IP6.GATEWAY", "device", "show", name); e == nil {
			values = strings.Fields(strings.ReplaceAll(string(b), "\\:", ":"))
		}
	}
	if len(validDNS(values)) == 0 {
		values = nmLease(ctx, name, "routers")
	}
	return validDNS(values)
}

func (n NetworkInfo) routedFamilies() (v4, v6 bool) {
	for _, gateway := range n.Gateways {
		ip, e := netip.ParseAddr(gateway)
		if e != nil {
			continue
		}
		if ip.Is4() && n.source(true) != nil {
			v4 = true
		}
		if ip.Is6() && n.source(false) != nil {
			v6 = true
		}
	}
	return
}
func validDNS(tokens []string) []string {
	result := []string{}
	for _, s := range tokens {
		ip, e := netip.ParseAddr(s)
		if e != nil || ip.IsLoopback() || ip.IsUnspecified() || ip.IsMulticast() || ip.Zone() != "" {
			continue
		}
		if !has(result, ip.String()) {
			result = append(result, ip.String())
		}
	}
	return result
}
func Find(ctx context.Context, id string) (NetworkInfo, error) {
	if !ValidID(id) {
		return NetworkInfo{}, errors.New("NETWORK_UNAVAILABLE")
	}
	list, e := inspect(ctx, id)
	if e != nil {
		return NetworkInfo{}, e
	}
	for _, n := range list {
		if n.ID == id && n.State == "configured" {
			return runtimeUplink(n)
		}
	}
	return NetworkInfo{}, errors.New("NETWORK_UNAVAILABLE")
}
func (n NetworkInfo) Fingerprint() string {
	b, _ := json.Marshal(struct {
		Index         int
		Name          string
		Addresses     []Address
		Gateways, DNS []string
	}{n.Index, n.Name, n.Addresses, n.Gateways, n.DNS})
	return digest(string(b) + runtimeSignature(n.ID))
}
func (n NetworkInfo) source(v4 bool) net.IP {
	for _, a := range n.Addresses {
		ip := net.ParseIP(a.Local)
		if ip != nil && (ip.To4() != nil) == v4 {
			return ip
		}
	}
	return nil
}
