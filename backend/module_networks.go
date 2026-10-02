package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"rykvo.local/auth/internal/uplink"
)

type networkChange struct {
	ID       string    `json:"id"`
	Label    *string   `json:"label,omitempty"`
	Modules  *[]string `json:"modules,omitempty"`
	Revision int64     `json:"revision"`
}
type networkModule struct {
	Number  string `json:"number"`
	ID      string `json:"id"`
	Label   string `json:"label"`
	Network string `json:"network"`
	Busy    bool   `json:"busy"`
}

func (m *moduleManager) loadNetworks(ctx context.Context) error {
	rows, err := m.db.Query(ctx, "SELECT module_id,network_id FROM module_network_bindings")
	if err != nil {
		return err
	}
	defer rows.Close()
	values := map[int64]string{}
	for rows.Next() {
		var id int64
		var n string
		if err = rows.Scan(&id, &n); err != nil {
			return err
		}
		values[id] = n
	}
	if rows.Err() != nil {
		return rows.Err()
	}
	m.mu.Lock()
	m.networks = values
	m.mu.Unlock()
	return nil
}

func (m *moduleManager) discoverNetworks(ctx context.Context) ([]uplink.NetworkInfo, error) {
	list, err := uplink.Inspect(ctx)
	if err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	blocked := map[string]bool{}
	for _, c := range m.seen {
		if c.Network != "" {
			blocked[c.Network] = true
		}
	}
	out := list[:0]
	for _, n := range list {
		if !blocked[n.Name] {
			out = append(out, n)
		}
	}
	return out, nil
}

func (m *moduleManager) networkInventory(context.Context) ([]uplink.NetworkInfo, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if !m.networkReadOK {
		return nil, errors.New("NETWORK_INVENTORY_UNAVAILABLE")
	}
	return append([]uplink.NetworkInfo{}, m.networkList...), nil
}

func prepareModuleNetworks(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", "/run/rykvo-voice-host.sock")
	if err != nil {
		return errors.New("NETWORK_AUTO_UNAVAILABLE")
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if err = json.NewEncoder(conn).Encode(map[string]string{"action": "prepare-networks"}); err != nil {
		return err
	}
	var result struct {
		Data *struct {
			Prepared string `json:"prepared"`
		} `json:"data"`
		Error string `json:"error"`
	}
	if err = json.NewDecoder(io.LimitReader(conn, 4096)).Decode(&result); err != nil {
		return errors.New("NETWORK_AUTO_UNAVAILABLE")
	}
	if result.Error != "" || result.Data == nil {
		return errors.New("NETWORK_AUTO_UNAVAILABLE")
	}
	return nil
}

func (m *moduleManager) watchNetworks(ctx context.Context) {
	defer m.operations.Done()
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	var lastPrepare time.Time
	var prepareFailed bool
	for {
		if time.Since(lastPrepare) >= 15*time.Second {
			err := prepareModuleNetworks(ctx)
			if err != nil && !prepareFailed && ctx.Err() == nil {
				log.Print("Automatic uplink preparation unavailable; existing networks unchanged")
			}
			prepareFailed = err != nil
			lastPrepare = time.Now()
		}
		call, cancel := context.WithTimeout(ctx, 8*time.Second)
		list, err := m.discoverNetworks(call)
		cancel()
		next := map[string]string{}
		if err == nil {
			for _, n := range list {
				if n.State == "configured" {
					next[n.ID] = n.Fingerprint()
				}
			}
		}
		m.mu.Lock()
		m.networkList, m.networkReadOK = list, err == nil
		previous := m.networkLinks
		m.networkLinks = next
		for id := range m.wifi {
			network := m.networks[id]
			if network == "" {
				network = uplink.DefaultNetwork()
			}
			if network == "" {
				continue
			}
			w := m.wifi[id]
			if w == nil || !w.Enabled {
				continue
			}
			if w.running && previous[network] != next[network] {
				w.Registered, w.SMSReady = false, false
				w.rebinding = true
				w.cancel()
			}
			if !w.running && next[network] != "" && previous[network] != next[network] && w.Issue == "NETWORK_UNAVAILABLE" {
				w.State, w.Issue = "waiting", ""
			}
			if sample, ok := m.values[id]; ok {
				m.startWiFiLocked(id, sample)
			}
		}
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (m *moduleManager) networkBusyLocked(v moduleRecord) bool {
	// Saving desired routing is independent of registration/route teardown.
	// Hardware work still waits on the module gate and rebinding guard.
	return m.networkChanging[v.ID] || m.controlPending[v.Endpoint] || m.work[v.Endpoint] > 0 || m.jobs[v.ID].active()
}

func (s *server) networksAPI(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	if s.modules == nil {
		fail(w, 503, "DEVICE_UNAVAILABLE")
		return
	}
	if r.Method == http.MethodPut {
		var input networkChange
		if !decodeBody(w, r, &input) {
			return
		}
		if err := s.modules.changeNetwork(ctx, input); err != nil {
			status := 503
			switch err.Error() {
			case "INVALID_NETWORK":
				status = 400
			case "NETWORK_CONFLICT", "NETWORK_HOST_DEFAULT", "DEVICE_BUSY":
				status = 409
			}
			fail(w, status, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET, PUT")
		fail(w, 405, "METHOD_NOT_ALLOWED")
		return
	}
	list, err := s.modules.networkInventory(ctx)
	if err != nil {
		fail(w, 503, "NETWORK_INVENTORY_UNAVAILABLE")
		return
	}
	// Keep a single consistent revision, names and bindings snapshot.
	release, err := s.modules.networkWrites.Lock(ctx, "networks")
	if err != nil {
		fail(w, 503, "DEVICE_UNAVAILABLE")
		return
	}
	defer release()
	var revision int64
	if s.db.QueryRow(ctx, "SELECT revision FROM module_network_settings WHERE singleton").Scan(&revision) != nil {
		fail(w, 503, "DATABASE_UNAVAILABLE")
		return
	}
	rows, err := s.db.Query(ctx, "SELECT id,label,snapshot FROM module_network_catalog ORDER BY id")
	if err != nil {
		fail(w, 503, "DATABASE_UNAVAILABLE")
		return
	}
	known := map[string]int{}
	for i, n := range list {
		known[n.ID] = i
	}
	for rows.Next() {
		var id, label string
		var raw []byte
		if rows.Scan(&id, &label, &raw) != nil {
			rows.Close()
			fail(w, 503, "DATABASE_UNAVAILABLE")
			return
		}
		if i, ok := known[id]; ok {
			list[i].Label = label
		} else {
			var n uplink.NetworkInfo
			_ = json.Unmarshal(raw, &n)
			n.ID, n.Label, n.State = id, label, "unavailable"
			n.HostDefault = nil
			n.Addresses = []uplink.Address{}
			n.Gateways = []string{}
			n.DNS = []string{}
			list = append(list, n)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		fail(w, 503, "DATABASE_UNAVAILABLE")
		return
	}
	records, err := listModuleRecords(ctx, s.db)
	if err != nil {
		fail(w, 503, "DATABASE_UNAVAILABLE")
		return
	}
	modules := []networkModule{}
	for _, v := range records {
		reading, present, _ := s.modules.state(v)
		number := ""
		if present && reading.SIM == "READY" {
			number = reading.Number
		}
		s.modules.mu.RLock()
		modules = append(modules, networkModule{ID: moduleID(v.ID), Label: moduleDisplayName(v.Label), Number: number, Network: s.modules.networks[v.ID], Busy: s.modules.networkBusyLocked(v)})
		s.modules.mu.RUnlock()
	}
	reply(w, 200, map[string]any{"data": map[string]any{"networks": list, "modules": modules, "revision": revision}})
}

func (m *moduleManager) changeNetwork(ctx context.Context, input networkChange) error {
	if !uplink.ValidID(input.ID) || input.Revision < 1 || (input.Label == nil) == (input.Modules == nil) {
		return errors.New("INVALID_NETWORK")
	}
	if input.Label != nil {
		*input.Label = strings.TrimSpace(*input.Label)
		if !validModuleLabel(*input.Label) {
			return errors.New("INVALID_NETWORK")
		}
	}
	if input.Modules != nil && len(*input.Modules) > 4096 {
		return errors.New("INVALID_NETWORK")
	}
	release, err := m.networkWrites.Lock(ctx, "networks")
	if err != nil {
		return err
	}
	defer release()
	list, err := m.networkInventory(ctx)
	if err != nil {
		return errors.New("NETWORK_INVENTORY_UNAVAILABLE")
	}
	var target uplink.NetworkInfo
	present := false
	for _, n := range list {
		if n.ID == input.ID {
			target = n
			present = true
			break
		}
	}
	if !present {
		var raw []byte
		if m.db.QueryRow(ctx, "SELECT snapshot FROM module_network_catalog WHERE id=$1", input.ID).Scan(&raw) != nil {
			return errors.New("INVALID_NETWORK")
		}
		if json.Unmarshal(raw, &target) != nil {
			return errors.New("INVALID_NETWORK")
		}
		target.State = "unavailable"
	}
	records, err := listModuleRecords(ctx, m.db)
	if err != nil {
		return errors.New("DATABASE_UNAVAILABLE")
	}
	wanted := map[int64]bool{}
	if input.Modules != nil {
		for _, v := range *input.Modules {
			id, e := parseModuleID(v)
			if e != nil || wanted[id] {
				return errors.New("INVALID_NETWORK")
			}
			wanted[id] = true
		}
	}
	m.mu.RLock()
	changes := map[int64]string{}
	affected := []moduleRecord{}
	for _, v := range records {
		if input.Modules == nil {
			break
		}
		old := m.networks[v.ID]
		next := old
		if wanted[v.ID] {
			next = input.ID
			delete(wanted, v.ID)
		} else if old == input.ID {
			next = ""
		}
		if old != next {
			changes[v.ID] = next
			affected = append(affected, v)
		}
	}
	m.mu.RUnlock()
	if len(wanted) > 0 {
		return errors.New("INVALID_NETWORK")
	}
	for _, next := range changes {
		if next != "" && target.State != "configured" {
			return errors.New("NETWORK_UNAVAILABLE")
		}
	}
	// Take the same control locks as Wi-Fi/eSIM/restart, then reserve admission.
	sort.Slice(affected, func(i, j int) bool { return affected[i].ID < affected[j].ID })
	for _, v := range affected {
		unlock, e := m.controlWrites.Lock(ctx, v.ID)
		if e != nil {
			return e
		}
		defer unlock()
	}
	m.mu.Lock()
	if m.draining || !m.ready || m.ctx == nil || m.ctx.Err() != nil {
		m.mu.Unlock()
		return errors.New("DEVICE_UNAVAILABLE")
	}
	for _, v := range affected {
		if m.networkBusyLocked(v) {
			m.mu.Unlock()
			return errors.New("DEVICE_BUSY")
		}
	}
	for _, v := range affected {
		m.controlPending[v.Endpoint] = true
		m.networkChanging[v.ID] = true
	}
	m.operations.Add(1)
	m.mu.Unlock()
	committed := false
	defer func() {
		m.mu.Lock()
		for _, v := range affected {
			delete(m.controlPending, v.Endpoint)
			delete(m.networkChanging, v.ID)
			if committed {
				m.resumeNetworkLocked(v.ID)
			}
		}
		m.mu.Unlock()
		m.operations.Done()
	}()
	tx, err := m.db.Begin(ctx)
	if err != nil {
		return errors.New("DATABASE_UNAVAILABLE")
	}
	defer tx.Rollback(ctx)
	var revision int64
	if tx.QueryRow(ctx, "SELECT revision FROM module_network_settings WHERE singleton FOR UPDATE").Scan(&revision) != nil {
		return errors.New("DATABASE_UNAVAILABLE")
	}
	if revision != input.Revision {
		return errors.New("NETWORK_CONFLICT")
	}
	raw, _ := json.Marshal(target)
	label := target.Label
	if input.Label != nil {
		label = *input.Label
	}
	if _, err = tx.Exec(ctx, `INSERT INTO module_network_catalog(id,label,snapshot) VALUES($1,$2,$3) ON CONFLICT(id) DO UPDATE SET snapshot=$3,label=CASE WHEN $4 THEN $2 ELSE module_network_catalog.label END`, input.ID, label, raw, input.Label != nil); err != nil {
		return errors.New("DATABASE_UNAVAILABLE")
	}
	for id, next := range changes {
		if next == "" {
			_, err = tx.Exec(ctx, "DELETE FROM module_network_bindings WHERE module_id=$1", id)
		} else {
			_, err = tx.Exec(ctx, `INSERT INTO module_network_bindings(module_id,network_id) VALUES($1,$2) ON CONFLICT(module_id) DO UPDATE SET network_id=$2`, id, next)
		}
		if err != nil {
			return errors.New("DATABASE_UNAVAILABLE")
		}
	}
	if _, err = tx.Exec(ctx, "UPDATE module_network_settings SET revision=revision+1 WHERE singleton"); err != nil {
		return errors.New("DATABASE_UNAVAILABLE")
	}
	if tx.Commit(ctx) != nil {
		m.mu.Lock()
		m.ready = false
		for _, v := range affected {
			if w := m.wifi[v.ID]; w != nil && w.running {
				w.Registered = false
				w.cancel()
			}
		}
		m.mu.Unlock()
		return errors.New("DATABASE_UNAVAILABLE")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, next := range changes {
		m.applyNetworkLocked(id, next)
	}
	committed = true
	return nil
}

func (m *moduleManager) applyNetworkLocked(id int64, next string) {
	m.networks[id] = next
	if w := m.wifi[id]; w != nil && w.Enabled {
		w.Registered, w.SMSReady = false, false
		if w.running {
			w.rebinding = true
			w.State = "stopping"
			w.cancel()
		} else {
			w.State = "waiting"
			if !time.Now().Before(m.recoveryUntil[w.candidate.Key]) {
				w.Issue = ""
			}
		}
	}
}

// Resume only the latest persisted choice after the old worker releases its gate.
func (m *moduleManager) resumeNetworkLocked(id int64) {
	sample, ok := m.values[id]
	if !ok || time.Since(m.lastScan) > 20*time.Second {
		return
	}
	current, present := m.seen[sample.Candidate.Key]
	if !present || !sameEndpoint(current, sample.Candidate) {
		return
	}
	m.startWiFiLocked(id, sample)
}
