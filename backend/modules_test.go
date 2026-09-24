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
