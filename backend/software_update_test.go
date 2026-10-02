package main

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSoftwareUpdateAPI(t *testing.T) {
	for _, tc := range []struct {
		method, path, body, content string
		size                        int64
		want                        int
		action                      string
	}{
		{"GET", "/api/software-update", "", "", 0, 200, "status"},
		{"POST", "/api/software-update/upload", "ZIP", "application/zip", 3, 415, ""},
		{"POST", "/api/software-update/upload", "ZIP", "application/json", 3, 415, ""},
		{"POST", "/api/software-update/upload", "", "application/zip", updateUploadLimit + 1, 415, ""},
		{"POST", "/api/software-update/upload", "", "application/zip", -1, 415, ""},
		{"POST", "/api/software-update/apply", `{"ticket":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`, "application/json", 0, 200, "apply"},
		{"POST", "/api/software-update/apply", `{"ticket":"../path"}`, "application/json", 0, 400, ""},
		{"POST", "/api/software-update/apply", `{"ticket":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","command":"id"}`, "application/json", 0, 400, ""},
		{"GET", "/api/software-update/apply", "", "", 0, 405, ""},
	} {
		t.Run(tc.method+tc.path+tc.content+tc.body, func(t *testing.T) {
			called := false
			s := &server{updateCall: func(ctx context.Context, r updateRequest, input io.Reader) (updateStatus, error) {
				called = true
				if r.Action != tc.action {
					t.Fatalf("action %q", r.Action)
				}
				if input != nil {
					data, _ := io.ReadAll(input)
					if string(data) != "ZIP" || r.Size != 3 {
						t.Fatal("stream changed")
					}
				}
				return updateStatus{State: "ready", Version: "1.1.1"}, nil
			}}
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", tc.content)
			if tc.path == "/api/software-update/upload" {
				req.ContentLength = tc.size
			}
			out := httptest.NewRecorder()
			s.softwareUpdateAPI(context.Background(), out, req, "admin-test")
			if out.Code != tc.want || called != (tc.want == 200) {
				t.Fatalf("status=%d called=%v body=%s", out.Code, called, out.Body)
			}
		})
	}
}
func TestSoftwareUpdateRedactsPrivateErrors(t *testing.T) {
	for code, want := range map[string]int{"UPDATE_BUSY": 409, "UPDATE_CONFLICT": 409, "UPDATE_DOWNGRADE": 400, "secret internal path": 503} {
		s := &server{updateCall: func(context.Context, updateRequest, io.Reader) (updateStatus, error) {
			return updateStatus{}, errors.New(code)
		}}
		out := httptest.NewRecorder()
		s.softwareUpdateAPI(context.Background(), out, httptest.NewRequest("GET", "/api/software-update", nil), "admin-test")
		if out.Code != want || strings.Contains(out.Body.String(), "secret internal") {
			t.Fatal(out.Code, out.Body)
		}
	}
}
func TestSoftwareUpdateRequiresAuthentication(t *testing.T) {
	s := &server{origin: "http://example.test"}
	for _, path := range []string{"/api/software-update", "/api/software-update/upload", "/api/software-update/apply", "/api/software-update/upload/start", "/api/software-update/upload/chunk", "/api/software-update/upload/finish", "/api/software-update/upload/cancel"} {
		req := localRequest("POST", path, strings.NewReader("ZIP"))
		req.Header.Set("Origin", "http://example.test")
		out := httptest.NewRecorder()
		s.ServeHTTP(out, req)
		if out.Code != 401 {
			t.Fatal(path, out.Code, out.Body)
		}
		req.Header.Set("Origin", "http://evil.test")
		out = httptest.NewRecorder()
		s.ServeHTTP(out, req)
		if out.Code != 403 {
			t.Fatal(path, out.Code)
		}
	}
}

func TestSoftwareUpdateChunkBoundsAndActor(t *testing.T) {
	for _, tc := range []struct {
		offset, id string
		size       int64
		want       int
	}{
		{"0", strings.Repeat("a", 32), 3, 200}, {"-1", strings.Repeat("a", 32), 3, 400},
		{"0", "../bad", 3, 400}, {"0", strings.Repeat("a", 32), updateChunkLimit + 1, 400},
		{"0", strings.Repeat("a", 32), -1, 400},
	} {
		called := false
		s := &server{updateCall: func(ctx context.Context, r updateRequest, body io.Reader) (updateStatus, error) {
			called = true
			if r.Owner == "" || len(r.Owner) != 64 || r.Action != "chunk" || r.Offset == nil || *r.Offset != 0 {
				t.Fatal(r)
			}
			return updateStatus{State: "uploading", Offset: 3}, nil
		}}
		req := httptest.NewRequest("POST", "/api/software-update/upload/chunk", strings.NewReader("RVU"))
		req.ContentLength = tc.size
		req.Header.Set("Content-Type", "application/octet-stream")
		req.Header.Set("Upload-ID", tc.id)
		req.Header.Set("Upload-Offset", tc.offset)
		out := httptest.NewRecorder()
		s.softwareUpdateAPI(context.Background(), out, req, "admin-test")
		if out.Code != tc.want || called != (tc.want == 200) {
			t.Fatal(out.Code, called, out.Body)
		}
	}
}

func TestSoftwareUpdateStartRejectsUnknownFields(t *testing.T) {
	s := &server{updateCall: func(context.Context, updateRequest, io.Reader) (updateStatus, error) {
		t.Fatal("untrusted input forwarded")
		return updateStatus{}, nil
	}}
	for _, body := range []string{`{"size":100}`, `{"size":10000,"header":"aa","seal":"bb"}`, `{"owner":"forged"}`} {
		req := httptest.NewRequest("POST", "/api/software-update/upload/start", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		out := httptest.NewRecorder()
		s.softwareUpdateAPI(context.Background(), out, req, "admin-test")
		if out.Code != 400 {
			t.Fatal(out.Code)
		}
	}
}
