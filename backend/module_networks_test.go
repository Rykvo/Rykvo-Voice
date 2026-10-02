package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"rykvo.local/auth/internal/hardware"
	"rykvo.local/auth/internal/uplink"
)

type fixedNetworkFake struct{ started chan string }

func (*fixedNetworkFake) FixedNetworks() {}
func (f *fixedNetworkFake) WiFi(ctx context.Context, _ hardware.Candidate, _, _ string, emit func(string)) error {
	f.started <- uplink.Network(ctx)
	emit("connected")
	<-ctx.Done()
	return ctx.Err()
}
func TestModuleFixedNetworkNeverFallsBack(t *testing.T) {
	f := &fixedNetworkFake{started: make(chan string, 1)}
	m := newModuleManagerWithWiFi(nil, nil, f)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.ctx, m.ready = ctx, true
	sample := wifiModuleFixture()
	m.values[1] = sample
	m.seen[sample.Candidate.Key] = sample.Candidate
	id := strings.Repeat("a", 32)
	m.networks[1] = id
	m.wifi[1] = &moduleWiFi{ICCID: sample.Reading.ICCID, Enabled: true, State: "waiting"}
	m.mu.Lock()
	m.startWiFiLocked(1, sample)
	m.mu.Unlock()
	select {
	case <-f.started:
		t.Fatal("missing network used host fallback")
	default:
	}
	if m.wifiView(1, sample.Reading.ICCID)["issue"] != "NETWORK_UNAVAILABLE" {
		t.Fatal("unavailable network hidden")
	}
	m.mu.Lock()
	m.networkLinks[id] = "verified-generation"
	m.startWiFiLocked(1, sample)
	m.mu.Unlock()
	select {
	case got := <-f.started:
		if got != id {
			t.Fatal("worker lost binding")
		}
	case <-time.After(time.Second):
		t.Fatal("same network did not resume")
	}
	cancel()
	m.operations.Wait()
}

func TestModuleNetworkAdmission(t *testing.T) {
	m := newModuleManager(nil, &restartFake{})
	m.ctx, m.ready, m.lastScan = context.Background(), true, time.Now()
	sample := wifiModuleFixture()
	key := sample.Candidate.Key
	m.values[1], m.seen[key] = sample, sample.Candidate
	v := moduleRecord{ID: 1, Endpoint: key}
	if m.networkBusyLocked(v) {
		t.Fatal("idle module busy")
	}
	done := m.reserveModuleWork(sample)
	if done == nil || !m.networkBusyLocked(v) {
		t.Fatal("business not protected")
	}
	done()
	m.controlPending[key] = true
	if m.reserveModuleWork(sample) != nil {
		t.Fatal("work admitted while network saved")
	}
	delete(m.controlPending, key)
	m.wifi[1] = &moduleWiFi{Enabled: true, running: true, State: "connected", rebinding: true}
	if m.reserveModuleWork(sample) != nil || m.networkBusyLocked(v) {
		t.Fatal("cleanup must block business, not saving desired routing")
	}
	m.wifi[1].rebinding = false
	if m.networkBusyLocked(v) {
		t.Fatal("idle registration incorrectly busy")
	}
	m.wifi[1].State = "stopping"
	if m.networkBusyLocked(v) {
		t.Fatal("route cleanup blocked changing desired routing")
	}
	m.recoveryUntil[key] = time.Now().Add(time.Minute)
	if m.networkBusyLocked(v) || m.reserveModuleWork(sample) != nil {
		t.Fatal("recovery must protect hardware without blocking desired routing")
	}
}

func testModuleNetworkDatabase(t *testing.T, s *server) {
	t.Helper()
	ctx := context.Background()
	// Model 1.1.5: the three new tables do not exist; existing business data stays.
	var before, after [3]int64
	counts := "SELECT (SELECT count(*) FROM administrators),(SELECT count(*) FROM modules),(SELECT count(*) FROM messages)"
	if err := s.db.QueryRow(ctx, counts).Scan(&before[0], &before[1], &before[2]); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(ctx, "DROP TABLE module_network_bindings; DROP TABLE module_network_catalog; DROP TABLE module_network_settings"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(ctx, schema); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(ctx, counts).Scan(&after[0], &after[1], &after[2]); err != nil || before != after {
		t.Fatalf("migration changed business data: %v %v", before, after)
	}
	m := newModuleManager(s.db, &restartFake{})
	m.ctx, m.ready, m.lastScan = ctx, true, time.Now()
	a, b := strings.Repeat("a", 32), strings.Repeat("b", 32)
	m.networkReadOK = true
	m.networkList = []uplink.NetworkInfo{{ID: a, Name: "net-a", Label: "net-a", State: "configured"}, {ID: b, Name: "net-b", Label: "net-b", State: "configured"}}
	var id int64
	if err := s.db.QueryRow(ctx, `INSERT INTO modules(hardware_key,endpoint,kind,label,label_key) VALUES('network-test','network-test','modem','network-test','network-test') RETURNING id`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	selected := []string{moduleID(id)}
	empty := []string{}
	set := func(network string, ids *[]string, revision int64) error {
		return m.changeNetwork(ctx, networkChange{ID: network, Modules: ids, Revision: revision})
	}
	if err := m.loadNetworks(ctx); err != nil {
		t.Fatal(err)
	}
	if len(m.networks) != 0 {
		t.Fatal("migration silently assigned networks")
	}
	m.work["network-test"] = 1
	if err := set(a, &selected, 1); err == nil || err.Error() != "DEVICE_BUSY" {
		t.Fatalf("busy: %v", err)
	}
	delete(m.work, "network-test")
	if err := set(a, &selected, 1); err != nil {
		t.Fatal(err)
	}
	if m.networks[id] != a {
		t.Fatal("binding not applied")
	}
	if err := set(b, &selected, 1); err == nil || err.Error() != "NETWORK_CONFLICT" {
		t.Fatalf("stale revision: %v", err)
	}
	if err := set(b, &selected, 2); err == nil || err.Error() != "NETWORK_ASSIGNED" {
		t.Fatalf("cross-network takeover: %v", err)
	}
	if m.networks[id] != a {
		t.Fatal("rejected takeover changed binding")
	}
	if err := set(a, &empty, 2); err != nil {
		t.Fatal(err)
	}
	if err := set(b, &selected, 3); err != nil {
		t.Fatal(err)
	}
	fresh := newModuleManager(s.db, &restartFake{})
	if err := fresh.loadNetworks(ctx); err != nil || fresh.networks[id] != b {
		t.Fatalf("restart lost binding: %v", err)
	}
	// Offline networks retain bindings; clearing explicitly is allowed.
	m.networkList = nil
	label := "备用网线"
	if err := m.changeNetwork(ctx, networkChange{ID: b, Label: &label, Revision: 4}); err != nil {
		t.Fatal(err)
	}
	if err := set(b, &empty, 5); err != nil {
		t.Fatal(err)
	}
	if m.networks[id] != "" {
		t.Fatal("unassign did not restore legacy behavior")
	}
	if err := set(a, &selected, 6); err == nil || err.Error() != "NETWORK_UNAVAILABLE" {
		t.Fatalf("offline assignment: %v", err)
	}
	if _, err := s.db.Exec(ctx, schema); err != nil {
		t.Fatal(err)
	}
	var saved string
	if err := s.db.QueryRow(ctx, "SELECT label FROM module_network_catalog WHERE id=$1", b).Scan(&saved); err != nil || saved != label {
		t.Fatalf("migration reset label %q %v", saved, err)
	}
	// A rapid off/on persists immediately while the old connection is cleaning.
	m.networkList = []uplink.NetworkInfo{{ID: a, Name: "net-a", Label: "net-a", State: "configured", HostDefault: []string{"IPv4"}}, {ID: b, Name: "net-b", Label: "net-b", State: "configured"}}
	if err := set(a, &selected, 6); err != nil {
		t.Fatalf("explicit host-default binding: %v", err)
	}
	if err := set(a, &empty, 7); err != nil {
		t.Fatal(err)
	}
	m.wifi[id] = &moduleWiFi{Enabled: true, running: true, rebinding: true, State: "stopping", cancel: func() {}}
	if err := set(b, &selected, 8); err != nil {
		t.Fatal(err)
	}
	if err := set(b, &empty, 9); err != nil {
		t.Fatal(err)
	}
	if err := set(b, &selected, 10); err != nil {
		t.Fatal("rapid re-enable rejected", err)
	}
	m.work["network-test"] = 1
	if err := set(b, &empty, 11); err == nil || err.Error() != "DEVICE_BUSY" {
		t.Fatal("in-progress business lost protection", err)
	}
	delete(m.work, "network-test")
	if err := fresh.loadNetworks(ctx); err != nil || fresh.networks[id] != b {
		t.Fatal("latest choice was not durable", err)
	}

}
