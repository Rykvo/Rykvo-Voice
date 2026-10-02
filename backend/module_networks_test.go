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
	m.work["network-test"] = 1
	if err := set(b, &selected, 2); err == nil || err.Error() != "DEVICE_BUSY" {
		t.Fatalf("busy cross-network switch: %v", err)
	}
	delete(m.work, "network-test")
	m.networkList[1].State = "unavailable"
	if err := set(b, &selected, 2); err == nil || err.Error() != "NETWORK_UNAVAILABLE" {
		t.Fatalf("offline cross-network switch: %v", err)
	}
	if m.networks[id] != a {
		t.Fatal("rejected switch changed binding")
	}
	m.networkList[1].State = "configured"
	if err := set(b, &selected, 2); err != nil {
		t.Fatal(err)
	}
	if m.networks[id] != b {
		t.Fatal("direct switch did not replace old binding")
	}
	// Clearing A must not clear a module that has already moved to B.
	if err := set(a, &empty, 3); err != nil {
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
	if err := set(a, &selected, 9); err != nil {
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
	if err := set(b, &empty, 11); err != nil {
		t.Fatal(err)
	}
	if err := fresh.loadNetworks(ctx); err != nil || fresh.networks[id] != "" {
		t.Fatal("clearing B did not restore the default route", err)
	}
	var count int
	if err := s.db.QueryRow(ctx, "SELECT count(*) FROM module_network_bindings WHERE module_id=$1", id).Scan(&count); err != nil || count != 0 {
		t.Fatal("clearing B resurrected an old assignment", err)
	}
	var other int64
	if err := s.db.QueryRow(ctx, `INSERT INTO modules(hardware_key,endpoint,kind,label,label_key) VALUES('network-other','network-other','modem','network-other','network-other') RETURNING id`).Scan(&other); err != nil {
		t.Fatal(err)
	}
	both := []string{moduleID(id), moduleID(other)}
	if err := set(a, &both, 12); err != nil {
		t.Fatal(err)
	}
	// Two stale pages cannot both commit; the unrelated module stays on A.
	type outcome struct {
		network string
		err     error
	}
	results := make(chan outcome, 2)
	go func() { results <- outcome{a, set(a, &both, 13)} }()
	go func() { results <- outcome{b, set(b, &selected, 13)} }()
	winner, conflicts := "", 0
	for range 2 {
		result := <-results
		if result.err == nil {
			if winner != "" {
				t.Fatal("concurrent stale writes both committed")
			}
			winner = result.network
		} else if result.err.Error() == "NETWORK_CONFLICT" {
			conflicts++
		} else {
			t.Fatal(result.err)
		}
	}
	if conflicts != 1 || m.networks[id] != winner || m.networks[other] != a {
		t.Fatal("concurrent switch changed the wrong module")
	}
	if err := set(b, &selected, 14); err != nil {
		t.Fatal(err)
	}
	// A failed database write leaves both the live and persisted assignments intact.
	if _, err := s.db.Exec(ctx, `CREATE FUNCTION test_reject_network_write() RETURNS trigger LANGUAGE plpgsql AS $$
	BEGIN RAISE EXCEPTION 'test network write failure'; END $$;
	CREATE TRIGGER test_network_write_failure BEFORE INSERT OR UPDATE ON module_network_bindings
	FOR EACH ROW EXECUTE FUNCTION test_reject_network_write()`); err != nil {
		t.Fatal(err)
	}
	defer s.db.Exec(ctx, "DROP TRIGGER test_network_write_failure ON module_network_bindings; DROP FUNCTION test_reject_network_write()")
	if err := set(a, &both, 15); err == nil || err.Error() != "DATABASE_UNAVAILABLE" {
		t.Fatal("database failure not propagated", err)
	}
	if err := fresh.loadNetworks(ctx); err != nil || fresh.networks[id] != b || fresh.networks[other] != a || m.networks[id] != b || !m.ready {
		t.Fatal("failed switch changed assignments or readiness", err)
	}
	var revision int64
	if err := s.db.QueryRow(ctx, "SELECT revision FROM module_network_settings WHERE singleton").Scan(&revision); err != nil || revision != 15 {
		t.Fatal("failed switch changed revision", err)
	}
	if len(m.controlPending) != 0 || len(m.networkChanging) != 0 {
		t.Fatal("failed switch retained admission locks")
	}

}
