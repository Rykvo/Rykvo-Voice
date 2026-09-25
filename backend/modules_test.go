package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"rykvo.local/auth/internal/hardware"
	"strings"
	"testing"
	"time"
)

func TestModuleLabelNormalization(t *testing.T) {
	for _, v := range []string{"", "a\nb", "\u200b", "123456789012345678901"} {
		if validModuleLabel(v) {
			t.Fatal(v)
		}
	}
	if !validModuleLabel("工作卡") || labelKey(" Ａbc ") != labelKey("abc") {
		t.Fatal("label normalization")
	}
}

func TestModuleJobConfirmsOnlyMatchingCardAndTarget(t *testing.T) {
	info := &hardware.ESIMInfo{EID: "89049032001001234500012345678901", Profiles: []hardware.ESIMProfile{{ICCID: "89123456789012345678", Enabled: true, Label: "主号"}}}
	reading := hardware.Reading{Responsive: true, IMEI: "123456789012345", ESIM: info}
	job := moduleJob{Action: "enable", State: "uncertain", Issue: "ESIM_RESULT_UNKNOWN", Verification: &moduleVerification{EID: info.EID, ICCID: info.Profiles[0].ICCID, IMEI: reading.IMEI}}
	if !job.confirm(reading) || job.State != "succeeded" || job.Issue != "" || job.Warning != "" {
		t.Fatal("confirmed state did not clear uncertainty")
	}
	info.Pending = 1
	if !job.confirm(reading) || job.Warning != "ESIM_NOTIFICATION_PENDING" {
		t.Fatal("pending notification lost")
	}
	reading.IMEI = "999999999999999"
	if job.confirmed(reading) {
		t.Fatal("different modem accepted")
	}
	reading.IMEI = job.Verification.IMEI
	info.EID = "89049032001001234500012345678902"
	if job.confirmed(reading) {
		t.Fatal("different card accepted")
	}
	info.EID = job.Verification.EID
	info.Profiles[0].Enabled = false
	if job.confirmed(reading) {
		t.Fatal("disabled target accepted")
	}
	job.Action = "disable"
	if !job.confirmed(reading) {
		t.Fatal("disable not verified")
	}
	job.Action = "download"
	if !job.confirmed(reading) {
		t.Fatal("installed profile not verified")
	}
	job.Verification = nil
	if job.confirmed(reading) {
		t.Fatal("legacy job guessed")
	}
}

func TestModulePendingESIMDoesNotBecomePhysicalSIM(t *testing.T) {
	m := newModuleManager(nil, nil)
	c := hardware.Candidate{Key: "usb:esim", Generation: "1"}
	v := moduleRecord{ID: 1, Endpoint: c.Key}
	m.seen[c.Key], m.lastScan = c, time.Now()
	m.values[v.ID] = moduleSample{c, hardware.Reading{Responsive: true, SIM: "READY", ICCID: "89123456789012345678", UpdatedAt: time.Now(), ESIM: &hardware.ESIMInfo{Issue: "READ_TIMEOUT"}}}
	s := &server{modules: m}
	view := s.moduleView(v)
	if view["cardReading"] != true || len(view["sims"].([]any)) != 0 {
		t.Fatal("transient eSIM failure presented as physical SIM")
	}
	sample := m.values[v.ID]
	sample.Reading.ESIM.Issue = "NO_EUICC"
	m.values[v.ID] = sample
	if len(s.moduleView(v)["sims"].([]any)) != 1 {
		t.Fatal("physical SIM hidden")
	}
}
func TestModuleStateAndStaleSIM(t *testing.T) {
	m := newModuleManager(nil, nil)
	s := &server{modules: m}
	c := hardware.Candidate{Key: "usb:1", Generation: "1"}
	v := moduleRecord{ID: 1, Endpoint: c.Key, Label: "工作卡"}
	m.seen[c.Key] = c
	m.lastScan = time.Now()
	m.values[1] = moduleSample{c, hardware.Reading{Responsive: true, ICCID: "89123456789012345678", Number: "12345", SIM: "READY", UpdatedAt: time.Now()}}
	if s.moduleView(v)["status"] != "online" {
		t.Fatal("not online")
	}
	if s.moduleView(v)["capabilities"].(map[string]bool)["esimDownload"] {
		t.Fatal("physical SIM offered eSIM download")
	}
	line := s.moduleView(v)["sims"].([]any)[0].(map[string]any)
	if line["iccid"] != "89123456789012345678" || line["number"] != "12345" {
		t.Fatal("physical SIM identity missing or used as number")
	}
	delete(m.seen, c.Key)
	view := s.moduleView(v)
	if view["status"] != "offline" || view["number"] != "" || len(view["sims"].([]any)) != 0 {
		t.Fatal("stale SIM leaked")
	}
	m.seen[c.Key] = hardware.Candidate{Key: c.Key, Generation: "2"}
	if s.moduleView(v)["issue"] != "READING" {
		t.Fatal("old descriptor generation trusted")
	}
}

// Invoked inside the existing isolated PostgreSQL test, before its session revocation checks.
func testModuleDatabase(t *testing.T, s *server, cookie, csrf string) {
	t.Helper()
	ctx := context.Background()
	var records []moduleRecord
	for i := 0; i < 8; i++ {
		v, err := bindModule(ctx, s.db, hardware.Candidate{Key: fmt.Sprintf("usb:%d", i), Kind: "usb"}, hardware.Reading{Model: "EC20", IMEI: fmt.Sprintf("1234567890123%02d", i)})
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, v)
	}
	moved, err := bindModule(ctx, s.db, hardware.Candidate{Key: "usb:moved", Kind: "usb"}, hardware.Reading{Model: "EC20", IMEI: "123456789012300"})
	if err != nil || moved.ID != records[0].ID {
		t.Fatalf("rebind %v %v", moved, err)
	}
	request := func(id int64, label string, want int) {
		payload, _ := json.Marshal(map[string]string{"label": label})
		r := localRequest(http.MethodPatch, "/api/modules/"+moduleID(id), strings.NewReader(string(payload)))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", "http://test.local")
		r.Header.Set("X-CSRF-Token", csrf)
		r.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("label %q: %d %s", label, w.Code, w.Body.String())
		}
	}
	request(records[0].ID, "工作卡", 200)
	request(records[0].ID, "工作卡", 200)
	request(records[1].ID, " 工作卡 ", 409)
	request(records[0].ID, "另一个标签", 200)
	request(records[1].ID, "工作卡", 200)
	request(records[0].ID, moduleName(records[2].ID), 409)
	request(records[0].ID, "", 200)
	request(records[1].ID, "a\nb", 400)
	all, err := listModuleRecords(ctx, s.db)
	if err != nil || len(all) != 8 {
		t.Fatalf("module count %d %v", len(all), err)
	}
	for _, v := range all {
		if v.Label == "" {
			t.Fatal("empty label")
		}
	}
	job := moduleJob{ID: "verification-fixture", Module: records[0].ID, Action: "enable", State: "uncertain", Stage: "done", Issue: "ESIM_RESULT_UNKNOWN", Verification: &moduleVerification{EID: "89049032001001234500012345678901", ICCID: "89123456789012345678", IMEI: "123456789012300"}}
	if _, err := s.db.Exec(ctx, "INSERT INTO module_jobs(id,module_id,action,state) VALUES($1,$2,$3,$4)", job.ID, job.Module, job.Action, job.State); err != nil {
		t.Fatal(err)
	}
	m := newModuleManager(s.db, nil)
	if !m.saveJob(job) {
		t.Fatal("job persistence")
	}
	loaded, err := scanJob(s.db.QueryRow(ctx, "SELECT "+jobColumns+" FROM module_jobs WHERE id=$1", job.ID))
	if err != nil || loaded.Verification == nil || *loaded.Verification != *job.Verification {
		t.Fatalf("verification roundtrip: %v", err)
	}
	serialized, _ := json.Marshal(loaded)
	if strings.Contains(string(serialized), job.Verification.EID) {
		t.Fatal("verification exposed in job API")
	}
	if _, err := s.db.Exec(ctx, "INSERT INTO module_jobs(id,module_id,action,state) VALUES('retired-network-test',$1,'network-select','uncertain')", job.Module); err != nil {
		t.Fatal(err)
	}
	m = newModuleManager(s.db, nil)
	m.loadJobs(ctx)
	if m.job(job.Module).ID != "" {
		t.Fatal("retired network task restored into the UI")
	}
	var history int
	if err := s.db.QueryRow(ctx, "SELECT count(*) FROM module_jobs WHERE id='retired-network-test'").Scan(&history); err != nil || history != 1 {
		t.Fatal("retired task history lost")
	}
	if _, err := s.db.Exec(ctx, "DELETE FROM module_jobs WHERE id='retired-network-test'"); err != nil {
		t.Fatal(err)
	}
	m.loadJobs(ctx)
	c := hardware.Candidate{Key: "usb:moved", Kind: "usb"}
	m.seen[c.Key], m.lastScan = c, time.Now()
	reading := hardware.Reading{Responsive: true, IMEI: job.Verification.IMEI, UpdatedAt: time.Now(), ESIM: &hardware.ESIMInfo{EID: job.Verification.EID, Profiles: []hardware.ESIMProfile{{ICCID: job.Verification.ICCID, Enabled: true}}}}
	m.accept(ctx, moduleSample{c, reading})
	if m.job(job.Module).State != "succeeded" {
		t.Fatal("late read did not reconcile uncertain job")
	}
	if _, err := s.db.Exec(ctx, "DELETE FROM module_jobs WHERE id=$1", job.ID); err != nil {
		t.Fatal(err)
	}
	testRecoveryDatabase(t, s, records[0].ID)
	testWiFiDatabase(t, s, moved, cookie, csrf)
	testESIMWiFiHandoffDatabase(t, s, moved, cookie, csrf)
	testRestartDatabase(t, s, moved, cookie, csrf)
	testAPNDatabase(t, s, moved, cookie, csrf)
	testPhoneDatabase(t, s)
	testCarrierDatabase(t, s)
	testRoamingDatabase(t, s, moved, cookie, csrf)
	testMessagesDatabase(t, s, moved, cookie, csrf)
}

func TestRetiredNetworkRequestsDoNotReachHardware(t *testing.T) {
	s := &server{modules: newModuleManager(nil, nil)}
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		r := httptest.NewRequest(method, "/api/modules/module-01/lines/line-test/networks", nil)
		w := httptest.NewRecorder()
		s.moduleControl(context.Background(), w, r, moduleRecord{}, []string{"module-01", "lines", "line-test", "networks"})
		if w.Code != http.StatusNotFound {
			t.Fatalf("retired route: %d %s", w.Code, w.Body.String())
		}
	}
	for _, body := range []string{`{"requestId":"retired-network-test","networkAutomatic":true}`, `{"requestId":"retired-network-test","operator":"46000","accessTechnology":7}`} {
		r := httptest.NewRequest(http.MethodPatch, "/api/modules/module-01/lines/line-test", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.moduleControl(context.Background(), w, r, moduleRecord{}, []string{"module-01", "lines", "line-test"})
		if w.Code != http.StatusBadRequest {
			t.Fatalf("retired selection: %d %s", w.Code, w.Body.String())
		}
	}
}

func TestModuleViewReturnsEveryESIMProfile(t *testing.T) {
	for _, count := range []int{0, 1, 2, 9, 37} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			m := newModuleManager(nil, nil)
			s := &server{modules: m}
			c := hardware.Candidate{Key: "usb:esim", Generation: "1"}
			v := moduleRecord{ID: 1, Endpoint: c.Key, Label: "工作卡"}
			info := &hardware.ESIMInfo{EID: "89049032001001234500012345678901"}
			for i := 0; i < count; i++ {
				info.Profiles = append(info.Profiles, hardware.ESIMProfile{
					ICCID: fmt.Sprintf("891234567890%06d", i), Label: fmt.Sprintf("号码 %d", i+1), Enabled: i == 0,
				})
			}
			r := hardware.Reading{Responsive: true, SIM: "READY", Number: "12345", UpdatedAt: time.Now(), ESIM: info}
			if count > 0 {
				r.ICCID = info.Profiles[0].ICCID
			}
			m.seen[c.Key] = c
			m.lastScan = time.Now()
			m.values[v.ID] = moduleSample{c, r}
			m.jobs[v.ID] = moduleJob{State: "running"}
			capabilities := s.moduleView(v)["capabilities"].(map[string]bool)
			if !capabilities["esimDownload"] || capabilities["esim"] {
				t.Fatal("card capability confused with operation availability")
			}
			rows := s.moduleView(v)["sims"].([]any)
			if len(rows) != count {
				t.Fatalf("got %d profiles, want %d", len(rows), count)
			}
			ids := map[string]bool{}
			for i, row := range rows {
				sim := row.(map[string]any)
				id := sim["id"].(string)
				if ids[id] {
					t.Fatal("duplicate profile identity")
				}
				ids[id] = true
				if sim["iccid"] != info.Profiles[i].ICCID {
					t.Fatal("profile ICCID missing")
				}
				if sim["esim"] != true || sim["enabled"] != (i == 0) {
					t.Fatal("profile state lost")
				}
				if i > 0 && sim["number"] != "" {
					t.Fatal("active phone number copied to another profile")
				}
			}
		})
	}
}

func TestModuleDefaultName(t *testing.T) {
	for id, want := range map[int64]string{1: "模块 01", 16: "模块 16", 100: "模块 100"} {
		if got := moduleName(id); got != want {
			t.Fatalf("name %d: %q", id, got)
		}
	}
}
