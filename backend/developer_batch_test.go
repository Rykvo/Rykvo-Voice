package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDeveloperBatchIDs(t *testing.T) {
	for _, ids := range [][]string{nil, {"same-id-123", "same-id-123"}, {"bad/id"}} {
		if validBatchIDs(ids, 25, jobIDPattern.MatchString) {
			t.Fatal("invalid batch accepted", ids)
		}
	}
	if !validBatchIDs([]string{"valid-request-001"}, 25, jobIDPattern.MatchString) {
		t.Fatal("singleton rejected")
	}
}

func TestDeveloperLookupReceivedIDs(t *testing.T) {
	rx := "rx-" + strings.Repeat("a", 64)
	if !validBatchIDs([]string{rx, "valid-request-001"}, 100, validLookupID) {
		t.Fatal("mixed inbound/outbound lookup rejected")
	}
	if validBatchIDs([]string{rx}, 25, jobIDPattern.MatchString) {
		t.Fatal("lookup fix relaxed send request validation")
	}
	for _, ids := range [][]string{nil, {rx, rx}, {"rx-" + strings.Repeat("g", 64)}, {rx + "a"}, {"../" + rx}, {"invalid/id"}, make([]string, 101)} {
		if validBatchIDs(ids, 100, validLookupID) {
			t.Fatal("invalid lookup accepted", ids)
		}
	}
}

func testDeveloperBatchDatabase(t *testing.T, s *server, key, module string) {
	t.Helper()
	request := func(path string, body any, want int) []developerBatchItem {
		t.Helper()
		data, _ := json.Marshal(body)
		r := localRequest("POST", "/api/v1/messages/"+path, bytes.NewReader(data))
		r.Header.Set("X-API-Key", key)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.developerAPI(context.Background(), w, r)
		if w.Code != want {
			t.Fatalf("batch %s: %d %s", path, w.Code, w.Body.String())
		}
		var out struct {
			Data struct{ Items []developerBatchItem }
		}
		if want == 200 {
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
		}
		return out.Data.Items
	}
	good := messageInput{RequestID: "developer-batch-valid-01", ModuleID: module, To: "+19990000001", Text: "virtual fixture"}
	bad := good
	bad.RequestID = "developer-batch-invalid-01"
	bad.ModuleID = "missing"
	items := []messageInput{good, bad}
	result := request("batch", map[string]any{"items": items}, 200)
	if len(result) != 2 || result[0].Status != 202 || result[1].Status != 400 {
		t.Fatal("partial acceptance", result)
	}
	result = request("batch", map[string]any{"items": items}, 200)
	if result[0].Status != 200 || result[0].Data.ID != good.RequestID {
		t.Fatal("retry not idempotent", result)
	}
	good.Text = "changed"
	result = request("batch", map[string]any{"items": []messageInput{good}}, 200)
	if result[0].Status != 409 || result[0].Error != "REQUEST_CONFLICT" {
		t.Fatal("body conflict", result)
	}
	request("batch", map[string]any{"items": []messageInput{good, good}}, 400)
	request("batch", map[string]any{"items": make([]messageInput, 26)}, 400)
	result = request("lookup", map[string]any{"ids": []string{good.RequestID, bad.RequestID}}, 200)
	if result[0].Data.ModuleID != module || result[0].Data.CardVersion < 1 || result[1].Status != 404 {
		t.Fatal("lookup identity", result)
	}
	rx := "rx-" + strings.Repeat("a", 64)
	mid, err := parseModuleID(module)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(context.Background(), `INSERT INTO messages(id,module_id,iccid,line_id,peer,mine,kind,body,state)
	 SELECT $1,id,active_card,'lookup-fixture','12345',false,'sms','inbound fixture','received' FROM modules WHERE id=$2`, rx, mid); err != nil {
		t.Fatal(err)
	}
	result = request("lookup", map[string]any{"ids": []string{rx, good.RequestID, bad.RequestID}}, 200)
	if len(result) != 3 || result[0].Status != 200 || result[0].Data == nil || result[0].Data.Text != "inbound fixture" || result[0].Data.Mine || result[1].Status != 200 || result[2].Status != 404 {
		t.Fatal("inbound/outbound lookup mismatch", result)
	}
	request("lookup", map[string]any{"ids": []string{rx, rx}}, 400)
	config, err := s.readDeveloper(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	priority := good
	priority.RequestID = "developer-batch-priority-01"
	priority.Text = "reply"
	if _, _, err = s.enqueueMessage(context.Background(), priority, config.Revision); err != nil {
		t.Fatal(err)
	}
	var next string
	if err = s.db.QueryRow(context.Background(), "SELECT id FROM messages WHERE id=ANY($1::text[]) ORDER BY "+messagePriorityOrder+" LIMIT 1", []string{good.RequestID, priority.RequestID}).Scan(&next); err != nil || next != priority.RequestID {
		t.Fatal("reply queued behind bulk", next, err)
	}
	bulk, urgent, err := s.modules.pendingMessageModules(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var priorityModule bool
	for _, id := range urgent {
		if moduleID(id) == module {
			priorityModule = true
		}
	}
	for _, id := range bulk {
		if moduleID(id) == module {
			t.Fatal("reply module stayed bulk")
		}
	}
	if !priorityModule {
		t.Fatal("reply module not prioritized")
	}
	_, err = s.db.Exec(context.Background(), "UPDATE messages SET deleted_at=now() WHERE id=$1", good.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	result = request("lookup", map[string]any{"ids": []string{good.RequestID}}, 200)
	if !result[0].Data.Deleted || result[0].Data.Text != "" {
		t.Fatal("lookup leaked deleted message", result)
	}
	s.developerWork.mu.Lock()
	s.developerWork.buckets["bulk-send"] = apiBucket{tokens: 0, at: time.Now().Add(time.Second)}
	s.developerWork.mu.Unlock()
	result = request("batch", map[string]any{"items": []messageInput{good}}, 200)
	if result[0].Status != 429 || result[0].RetryAfter < 1 {
		t.Fatal("no per-item retry delay", result)
	}
	s.developerWork.mu.Lock()
	delete(s.developerWork.buckets, "bulk-send")
	s.developerWork.mu.Unlock()
}
