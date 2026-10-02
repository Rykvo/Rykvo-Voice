package main

import (
	"context"
	"github.com/jackc/pgx/v5/pgxpool"
	"rykvo.local/auth/internal/carrierconfig"
	"rykvo.local/auth/internal/dispatch"
	"rykvo.local/auth/internal/durable"
	"rykvo.local/auth/internal/hardware"
	"rykvo.local/auth/internal/uplink"
	"strings"
	"sync"
	"time"
)

type moduleSample struct {
	Candidate hardware.Candidate
	Reading   hardware.Reading
}
type moduleManager struct {
	draining        bool
	messageRuntime  messageRuntime
	messageResults  durable.Journal
	stateWrites     dispatch.Locks[string]
	controlWrites   dispatch.Locks[int64]
	work            map[string]int
	controlPending  map[string]bool
	roaming         map[string]bool
	roamingReady    bool
	carrierConfigs  map[string]carrierconfig.Selection
	phoneNumbers    map[string]string
	networks        map[int64]string
	networkChanging map[int64]bool
	networkWrites   dispatch.Locks[string]
	networkLinks    map[string]string
	networkList     []uplink.NetworkInfo
	networkReadOK   bool
	wifi            map[int64]*moduleWiFi
	mu              sync.RWMutex
	source          hardware.Source
	wifiEngine      wifiSource
	wifiRetryLimit  int
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
	return &moduleManager{networks: map[int64]string{}, networkChanging: map[int64]bool{}, networkLinks: map[string]string{}, work: map[string]int{}, controlPending: map[string]bool{}, roaming: map[string]bool{}, carrierConfigs: map[string]carrierconfig.Selection{}, phoneNumbers: map[string]string{}, wifi: map[int64]*moduleWiFi{}, db: pool, source: source, wifiEngine: engine, seen: map[string]hardware.Candidate{}, values: map[int64]moduleSample{}, done: make(chan struct{}), gates: map[string]chan struct{}{}, jobs: map[int64]moduleJob{}, operationSlots: make(chan struct{}, 4), recoveryProof: map[string]moduleSample{}, recoveryPending: map[int64]bool{}, recovery: map[string]recoveryStreak{}, recoveryUntil: map[string]time.Time{}}
}
func (m *moduleManager) run(ctx context.Context) {
	defer close(m.done)
	m.mu.Lock()
	m.ctx = ctx
	m.mu.Unlock()
	if !m.initialize(ctx) {
		return
	}
	defer m.operations.Wait()
	m.operations.Add(1)
	go m.watchNetworks(ctx)
	jobs := make(chan hardware.Candidate, moduleReadWorkers)
	results := make(chan modulePollResult, moduleReadWorkers)
	var workers sync.WaitGroup
	for i := 0; i < moduleReadWorkers; i++ {
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
					if r.UpdatedAt.IsZero() {
						r.UpdatedAt = time.Now().UTC()
					}
					cancel()
					sample := moduleSample{c, r}
					accepted := m.accept(ctx, sample)
					select {
					case results <- modulePollResult{sample, accepted}:
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
	dispatchReads := func() {
		m.mu.RLock()
		candidates := pollingCandidates(m.seen, pending, due, time.Now())
		m.mu.RUnlock()
		for _, c := range candidates {
			select {
			case jobs <- c:
				pending[c.Key] = true
			default:
				return
			}
		}
	}
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
		for key := range due {
			if _, ok := next[key]; !ok {
				delete(due, key)
			}
		}
		dispatchReads()
	}
	scan()
	events := hardware.Watch(ctx)
	var hotplug <-chan time.Time
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	pollTicker := time.NewTicker(time.Second)
	defer pollTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			scan()
		case <-pollTicker.C:
			dispatchReads()
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
			due[sample.Candidate.Key] = time.Now().Add(modulePollDelay(sample.accepted))
			dispatchReads()
		}
	}
}

func (m *moduleManager) initialize(ctx context.Context) bool {
	for ctx.Err() == nil {
		call, cancel := context.WithTimeout(ctx, 10*time.Second)
		var err error
		for _, load := range []func(context.Context) error{m.loadJobs, m.loadRecovery, m.loadPhoneNumbers, m.loadCarrierConfigs, m.loadRoaming, m.loadNetworks, m.loadWiFi} {
			if err = load(call); err != nil {
				break
			}
		}
		cancel()
		m.mu.Lock()
		m.ready = err == nil
		if err != nil {
			m.discoveryIssue = "MODULE_INITIALIZATION_FAILED"
		} else {
			m.discoveryIssue = ""
		}
		m.mu.Unlock()
		if err == nil {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(5 * time.Second):
		}
	}
	return false
}
func sameEndpoint(a, b hardware.Candidate) bool {
	if a.Generation != b.Generation || a.Control != b.Control || a.Reader != b.Reader || a.Audio != b.Audio || len(a.Ports) != len(b.Ports) {
		return false
	}
	for i := range a.Ports {
		if a.Ports[i] != b.Ports[i] {
			return false
		}
	}
	return true
}
func (m *moduleManager) accept(ctx context.Context, sample moduleSample) bool {
	gate := m.gate(sample.Candidate.Key)
	select {
	case gate <- struct{}{}:
		defer func() { <-gate }()
	default:
		return false
	}
	m.mu.Lock()
	valid := m.sampleCurrentLocked(sample)
	m.mu.Unlock()
	if !valid {
		return false
	}
	call, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	v, err := bindModule(call, m.db, sample.Candidate, sample.Reading)
	m.mu.Lock()
	if !m.sampleCurrentLocked(sample) {
		m.mu.Unlock()
		return false
	}
	if err != nil {
		m.discoveryIssue = "MODULE_SAVE_FAILED"
		if err.Error() == "IDENTITY_PENDING" {
			m.discoveryIssue = "IDENTITY_PENDING"
		}
		m.mu.Unlock()
		return false
	}
	if sample.Reading.Responsive && len(sample.Reading.IMEI) >= 14 {
		m.recoveryProof[sample.Candidate.Key] = sample
	}
	m.values[v.ID] = sample
	job := m.jobs[v.ID]
	m.mu.Unlock()
	if sample.Reading.CarrierConfig != nil {
		m.saveCarrierConfig(call, sample.Reading.ICCID, *sample.Reading.CarrierConfig)
	}
	m.verifyRecovery(call, v.ID, sample.Reading)
	if job.State == "uncertain" && (job.confirmRestart(sample) || job.confirm(sample.Reading)) {
		if m.persistJob(call, job) == nil {
			m.mu.Lock()
			if current := m.jobs[v.ID]; current.ID == job.ID && current.State == "uncertain" {
				m.jobs[v.ID] = job
				if job.Action == "restart" {
					resumeWiFiIntent(m.wifi[v.ID], sample.Reading)
				}
			}
			m.mu.Unlock()
		}
	}
	m.mu.Lock()
	if m.sampleCurrentLocked(sample) {
		m.startWiFiLocked(v.ID, sample)
	}
	m.mu.Unlock()
	return true
}

// Called with m.mu held; never performs I/O.
func (m *moduleManager) sampleCurrentLocked(sample moduleSample) bool {
	c := sample.Candidate
	if sample.Reading.Issue == "OPERATION_ACTIVE" {
		return false
	}
	for id, old := range m.values {
		if old.Candidate.Key == c.Key && (m.jobs[id].active() || old.Reading.UpdatedAt.After(sample.Reading.UpdatedAt)) {
			return false
		}
	}
	current, ok := m.seen[c.Key]
	if !ok || !sameEndpoint(current, c) {
		return false
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
				return false
			}
		}
	}
	return true
}
func (m *moduleManager) issue() string { m.mu.RLock(); defer m.mu.RUnlock(); return m.discoveryIssue }
func (m *moduleManager) state(v moduleRecord) (hardware.Reading, bool, string) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	empty := hardware.Reading{Model: v.Model, SIM: "unknown", Registration: "unknown"}
	current, present := m.seen[v.Endpoint]
	if time.Now().Before(m.recoveryUntil[v.Endpoint]) {
		if w := m.wifi[v.ID]; w != nil && hardware.WiFiCleanupUnconfirmed(w.Issue) {
			return empty, present, w.Issue
		}
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
