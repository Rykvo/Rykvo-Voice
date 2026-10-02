package uplink

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"strings"
)

var runtimePolicyPath = "/var/lib/rykvo-network-route/egress.json"

type runtimeNetwork struct {
	Enabled bool        `json:"enabled"`
	Ready   bool        `json:"ready"`
	Epoch   string      `json:"epoch"`
	Link    NetworkInfo `json:"link"`
}
type runtimePolicy struct {
	Primary  string                    `json:"primary"`
	Networks map[string]runtimeNetwork `json:"networks"`
}

func readRuntimePolicy() (runtimePolicy, error) {
	var p runtimePolicy
	b, err := os.ReadFile(runtimePolicyPath)
	if os.IsNotExist(err) {
		return p, nil
	}
	if err != nil || len(b) > 65536 {
		return p, errors.New("NETWORK_UNAVAILABLE")
	}
	if json.Unmarshal(b, &p) != nil || !ValidID(p.Primary) {
		return p, errors.New("NETWORK_UNAVAILABLE")
	}
	return p, nil
}

// An invalid active policy must not silently restore direct traffic.
func DefaultNetwork() string {
	p, err := readRuntimePolicy()
	if err != nil {
		return "unavailable"
	}
	return p.Primary
}

func runtimeSignature(id string) string {
	p, err := readRuntimePolicy()
	if err != nil {
		return "unavailable"
	}
	n := p.Networks[id]
	if !n.Enabled {
		return "direct"
	}
	if !n.Ready {
		return "blocked:" + n.Epoch
	}
	return "vpn:" + n.Epoch
}

func runtimeUplink(n NetworkInfo) (NetworkInfo, error) {
	p, err := readRuntimePolicy()
	if err != nil {
		return NetworkInfo{}, err
	}
	v := p.Networks[n.ID]
	if !v.Enabled {
		return n, nil
	}
	if !v.Ready || !strings.HasPrefix(v.Link.Name, "rvpn") || v.Link.ID != n.ID || v.Link.State != "configured" {
		return NetworkInfo{}, errors.New("NETWORK_UNAVAILABLE")
	}
	i, err := net.InterfaceByName(v.Link.Name)
	if err != nil || i.Flags&net.FlagUp == 0 {
		return NetworkInfo{}, errors.New("NETWORK_UNAVAILABLE")
	}
	v.Link.Index = i.Index
	return v.Link, nil
}
