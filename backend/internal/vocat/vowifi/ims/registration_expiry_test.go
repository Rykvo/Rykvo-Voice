package ims

import (
	"bufio"
	"context"
	"rykvo.local/auth/internal/vocat/vowifi"
	"strings"
	"testing"
)

func TestRegistrationMinimumValidation(t *testing.T) {
	for _, tc := range []struct {
		values []string
		want   int
	}{
		{[]string{"3600"}, 3600}, {[]string{"86400"}, 86400}, {nil, 0}, {[]string{"3600", "7200"}, 0},
		{[]string{"600"}, 0}, {[]string{"0"}, 0}, {[]string{"-1"}, 0}, {[]string{"+3600"}, 0},
		{[]string{"3600x"}, 0}, {[]string{"86401"}, 0}, {[]string{"4294967296"}, 0},
	} {
		got, e := registrationMinimum(&sipResponse{Headers: map[string][]string{"min-expires": tc.values}}, 600)
		if got != tc.want || (e == nil) != (tc.want > 0) {
			t.Fatalf("%v got %d %v", tc.values, got, e)
		}
	}
}
func TestRegisterNegotiates423AndKeepsExpiryForRefresh(t *testing.T) {
	s := newStableInstanceTestSession(t, vowifi.IMSRequest{Identity: vowifi.SIMIdentity{IMSI: "001010123456789", HomeMCC: "001", HomeMNC: "01"}})
	var requests []string
	s.conn.(*fakeConn).onWrite = func(b []byte) { requests = append(requests, string(b)) }
	s.reader = bufio.NewReader(strings.NewReader(string(testResponse(423, "Interval Too Brief", s.callID, "1 REGISTER", []string{"Min-Expires: 3600"})) + string(testResponse(200, "OK", s.callID, "2 REGISTER", nil))))
	r, e := s.register(context.Background(), 600)
	if e != nil || r.StatusCode != 200 || len(requests) != 2 {
		t.Fatalf("register %v %v writes=%d", r, e, len(requests))
	}
	if !strings.Contains(requests[0], "Expires: 600\r\n") || !strings.Contains(requests[1], "Expires: 3600\r\n") {
		t.Fatal("expiry not negotiated")
	}
	for _, tc := range []struct{ want, input int }{{3600, 600}, {0, 0}} {
		b, e := s.buildRegister(3, tc.input, "", "")
		if e != nil {
			t.Fatal(e)
		}
		want := "Expires: 3600\r\n"
		if tc.want == 0 {
			want = "Expires: 0\r\n"
		}
		if !strings.Contains(string(b), want) {
			t.Fatal("refresh/deregistration expiry wrong")
		}
	}
}
func TestRegister423IsBoundedAndDoesNotRetryDeregistration(t *testing.T) {
	for _, expires := range []int{0, 600} {
		s := newStableInstanceTestSession(t, vowifi.IMSRequest{Identity: vowifi.SIMIdentity{IMSI: "001010123456789", HomeMCC: "001", HomeMNC: "01"}})
		count := 0
		s.conn.(*fakeConn).onWrite = func([]byte) { count++ }
		s.reader = bufio.NewReader(strings.NewReader(string(testResponse(423, "Interval Too Brief", s.callID, "1 REGISTER", []string{"Min-Expires: 3600"})) + string(testResponse(423, "Interval Too Brief", s.callID, "2 REGISTER", []string{"Min-Expires: 7200"}))))
		r, e := s.register(context.Background(), expires)
		want := 2
		if expires == 0 {
			want = 1
		}
		if e != nil || r.StatusCode != 423 || count != want {
			t.Fatalf("%d: response=%v err=%v count=%d", expires, r, e, count)
		}
	}
}
