package main

import (
	"context"
	"github.com/jackc/pgx/v5/pgxpool"
	"rykvo.local/auth/internal/carrierconfig"
	"rykvo.local/auth/internal/hardware"
	"strings"
	"sync"
	"time"
)

type moduleSample struct {
	Candidate hardware.Candidate
	Reading   hardware.Reading
}
type moduleManager struct {
	roaming         map[string]bool
	roamingReady    bool
	carrierConfigs  map[string]carrierconfig.Selection
	phoneNumbers    map[string]string
	wifi            map[int64]*moduleWiFi
	mu              sync.RWMutex
	source          hardware.Source
	wifiEngine      wifiSource
	db              *pgxpool.Pool
	seen            map[string]hardware.Candidate
	values          map[int64]moduleSample
	lastScan        time.Time
	discoveryIssue  string
	done            chan struct{}
	ctx             context.Context
	gates           map[string]chan struct{}
	operations      sync.WaitGroup
	operationSlots  chan struct{}
	jobs            map[int64]moduleJob
	recoveryProof   map[string]moduleSample
	recoveryReady   bool
	recoveryPending map[int64]bool
	recovery        map[string]recoveryStreak
	recoveryUntil   map[string]time.Time
	ready           bool
}

func newModuleManager(pool *pgxpool.Pool, source hardware.Source) *moduleManager {
	engine, _ := source.(wifiSource)
	return newModuleManagerWithWiFi(pool, source, engine)
}

// Select one engine at construction; never switch protocols inside a live session.
func newModuleManagerWithWiFi(pool *pgxpool.Pool, source hardware.Source, engine wifiSource) *moduleManager {
	return &moduleManager{roaming: map[string]bool{}, carrierConfigs: map[string]carrierconfig.Selection{}, phoneNumbers: map[string]string{}, wifi: map[int64]*moduleWiFi{}, db: pool, source: source, wifiEngine: engine, seen: map[string]hardware.Candidate{}, values: map[int64]moduleSample{}, done: make(chan struct{}), gates: map[string]chan struct{}{}, jobs: map[int64]moduleJob{}, operationSlots: make(chan struct{}, 4), recoveryProof: map[string]moduleSample{}, recoveryPending: map[int64]bool{}, recovery: map[string]recoveryStreak{}, recoveryUntil: map[string]time.Time{}}
}
func (m *moduleManager) run(ctx context.Context) {
	defer close(m.done)
	m.mu.Lock()
	m.ctx = ctx
	m.mu.Unlock()
	m.loadJobs(ctx)
	m.loadRecovery(ctx)
	m.loadPhoneNumbers(ctx)
	m.loadCarrierConfigs(ctx)
	m.loadRoaming(ctx)
	m.loadWiFi(ctx)
	defer m.operations.Wait()
	jobs := make(chan hardware.Candidate, 4)
	results := make(chan moduleSample, 4)
	var workers sync.WaitGroup
	for i := 0; i < 4; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case c := <-jobs:
					readCtx, cancel := context.WithTimeout(ctx, 50*time.Second)
					r := m.read(readCtx, c)
					cancel()
					select {
					case results <- moduleSample{c, r}:
					case <-ctx.Done():
						return
					}
				}
			}
		}()
	}
	defer workers.Wait()
	pending := map[string]bool{}
	due := map[string]time.Time{}
	scan := func() {
		call, cancel := context.WithTimeout(ctx, 5*time.Second)
		candidates, err := m.source.Discover(call)
		cancel()
		m.mu.Lock()
		if err != nil {
			m.discoveryIssue = "DISCOVERY_FAILED"
			m.mu.Unlock()
			return
		}
		m.discoveryIssue = ""
		m.lastScan = time.Now()
		next := map[string]hardware.Candidate{}
		serials := map[string]int{}
		for _, c := range candidates {
			if c.Serial != "" {
				serials[c.Vendor+":"+c.Product+":"+c.Serial]++
			}
		}
		for _, c := range candidates {
			if serials[c.Vendor+":"+c.Product+":"+c.Serial] > 1 {
				c.Serial = ""
			}
			previous, exists := m.seen[c.Key]
			if !exists || !sameEndpoint(previous, c) {
				delete(due, c.Key)
				delete(m.recoveryProof, c.Key)
			}
			next[c.Key] = c
		}
		for key := range m.recovery {
			if _, present := next[key]; !present {
				delete(m.recovery, key)
				delete(m.recoveryProof, key)
			}
		}
		for _, w := range m.wifi {
			if w.running {
				c, ok := next[w.candidate.Key]
				if !ok || !sameEndpoint(c, w.candidate) {
					w.cancel()
				}
			}
		}
		m.seen = next
		m.mu.Unlock()
		for _, candidate := range candidates {
			c := next[candidate.Key]
			if pending[c.Key] || time.Now().Before(due[c.Key]) {
				continue
			}
			select {
			case jobs <- c:
				pending[c.Key] = true
			default:
			}
		}
	}
	scan()
	events := hardware.Watch(ctx)
	var hotplug <-chan time.Time
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			scan()
		case _, ok := <-events:
			if !ok {
				events = nil
			} else if hotplug == nil {
				hotplug = time.After(700 * time.Millisecond)
			}
		case <-hotplug:
			hotplug = nil
			scan()
		case sample := <-results:
			delete(pending, sample.Candidate.Key)
			due[sample.Candidate.Key] = time.Now().Add(25 * time.Second)
			m.accept(ctx, sample)
		}
	}
}
func sameEndpoint(a, b hardware.Candidate) bool {
	if a.Generation != b.Generation || a.Control != b.Control || a.Reader != b.Reader || len(a.Ports) != len(b.Ports) {
		return false
	}
	for i := range a.Ports {
		if a.Ports[i] != b.Ports[i] {
			return false
		}
	}
	return true
}
func (m *moduleManager) accept(ctx context.Context, sample moduleSample) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := sample.Candidate
	if sample.Reading.Issue == "OPERATION_ACTIVE" {
		return
	}
	for id, old := range m.values {
		if old.Candidate.Key == c.Key && (m.jobs[id].active() || old.Reading.UpdatedAt.After(sample.Reading.UpdatedAt)) {
			return
		}
	}
	current, ok := m.seen[c.Key]
	if !ok || !sameEndpoint(current, c) {
		return
	}
	identity := c.Identity(sample.Reading)
	if !strings.HasPrefix(identity, "path:") {
		for id, other := range m.values {
			if other.Candidate.Key == c.Key || other.Candidate.Identity(other.Reading) != identity {
				continue
			}
			if _, present := m.seen[other.Candidate.Key]; present {
				other.Reading.Issue = "IDENTITY_CONFLICT"
				m.values[id] = other
				m.discoveryIssue = "IDENTITY_CONFLICT"
				return
			}
		}
	}
	call, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	v, err := bindModule(call, m.db, c, sample.Reading)
	if err != nil {
		m.discoveryIssue = "MODULE_SAVE_FAILED"
		if err.Error() == "IDENTITY_PENDING" {
			m.discoveryIssue = "IDENTITY_PENDING"
		}
		return
	}
	if sample.Reading.Responsive && len(sample.Reading.IMEI) >= 14 {
		m.recoveryProof[c.Key] = sample
	}
	m.values[v.ID] = sample
	if sample.Reading.CarrierConfig != nil {
		m.saveCarrierConfig(call, sample.Reading.ICCID, *sample.Reading.CarrierConfig)
	}
	m.verifyRecovery(call, v.ID, sample.Reading)
	m.startWiFiLocked(v.ID, sample)
	job := m.jobs[v.ID]
	if job.State == "uncertain" && job.confirm(sample.Reading) {
		if m.persistJob(call, job) == nil {
			m.jobs[v.ID] = job
		}
	}
}
func (m *moduleManager) issue() string { m.mu.RLock(); defer m.mu.RUnlock(); return m.discoveryIssue }
func (m *moduleManager) state(v moduleRecord) (hardware.Reading, bool, string) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	empty := hardware.Reading{Model: v.Model, SIM: "unknown", Registration: "unknown"}
	current, present := m.seen[v.Endpoint]
	if time.Now().Before(m.recoveryUntil[v.Endpoint]) {
		return empty, present, "RECOVERING"
	}
	if !present {
		return empty, false, ""
	}
	sample, ok := m.values[v.ID]
	if !ok || sample.Candidate.Key != v.Endpoint || !sameEndpoint(sample.Candidate, current) {
		return empty, true, "READING"
	}
	wifi := m.wifi[v.ID]
	wifiActive := wifi != nil && wifi.running && wifi.ICCID == sample.Reading.ICCID
	wifiRefreshing := wifi != nil && !wifi.running && wifi.ICCID == sample.Reading.ICCID && time.Now().Before(wifi.refreshUntil)
	if time.Since(m.lastScan) > 20*time.Second || (!m.jobs[v.ID].active() && !wifiActive && !wifiRefreshing && time.Since(sample.Reading.UpdatedAt) > 90*time.Second) {
		return empty, true, "STATE_STALE"
	}
	reading := sample.Reading
	if reading.SIM == "READY" {
		confirmed := m.phoneNumbers[reading.ICCID]
		if reading.Number == "" || (hardware.ValidAssociatedNumber(confirmed) && confirmed == "+"+reading.Number) {
			reading.Number = confirmed
		}
	}
	if wifiActive || wifiRefreshing {
		reading.Registration, reading.Operator, reading.PLMN, reading.Technology = "unknown", "", "", ""
		reading.RSSI, reading.RSRP, reading.RSRQ, reading.SINR = nil, nil, nil, nil
	}
	return reading, true, reading.Issue
}
