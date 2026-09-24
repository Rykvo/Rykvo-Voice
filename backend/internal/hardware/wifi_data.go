package hardware

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

func wifiHostData(network string) error {
	if network == "" {
		return nil
	}
	iface, err := net.InterfaceByName(network)
	if err != nil {
		return errors.New("WIFI_DATA_STATE_UNKNOWN")
	}
	if iface.Flags&net.FlagUp != 0 {
		return errors.New("WIFI_DATA_ACTIVE")
	}
	return nil
}

func wifiActiveData(lines []string) ([]int, error) {
	var active []int
	seen := map[int]bool{}
	for _, line := range lines {
		if !strings.HasPrefix(line, "+CGACT:") {
			continue
		}
		v := fields(line)
		if len(v) != 2 {
			return nil, errors.New("WIFI_DATA_STATE_UNKNOWN")
		}
		cid, state := integer(v[0]), integer(v[1])
		if cid == nil || *cid < 1 || *cid > 16 || state == nil || *state < 0 || *state > 1 || seen[*cid] {
			return nil, errors.New("WIFI_DATA_STATE_UNKNOWN")
		}
		seen[*cid] = true
		if *state == 1 {
			active = append(active, *cid)
		}
	}
	return active, nil
}

// Only modem PDP contexts; host interfaces and routes are never changed.
func wifiStopData(ctx context.Context, session *atSession) error {
	read := func() ([]int, error) {
		lines, err := session.exchange(ctx, "AT+CGACT?", 5*time.Second)
		if err != nil {
			return nil, errors.New("WIFI_DATA_STATE_UNKNOWN")
		}
		return wifiActiveData(lines)
	}
	active, err := read()
	if err != nil || len(active) == 0 {
		return err
	}
	call, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for _, cid := range active {
		if _, err := session.exchange(call, fmt.Sprintf("AT+CGACT=0,%d", cid), 10*time.Second); err != nil {
			return errors.New("WIFI_DATA_STATE_UNKNOWN")
		}
	}
	active, err = read()
	if err != nil || len(active) != 0 {
		return errors.New("WIFI_DATA_STATE_UNKNOWN")
	}
	return nil
}

func wifiRFOff(ctx context.Context, session *atSession, before int) error {
	if before != 4 {
		if _, err := session.exchange(ctx, "AT+CFUN=4", 15*time.Second); err != nil {
			return errors.New("WIFI_RADIO_UNCONFIRMED")
		}
	}
	mode, err := wifiRadioMode(ctx, session)
	if err != nil || mode != 4 {
		return errors.New("WIFI_RADIO_UNCONFIRMED")
	}
	return wifiStopData(ctx, session)
}
