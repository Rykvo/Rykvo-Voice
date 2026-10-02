package ims

import (
	"errors"
	"testing"
	"time"
)

func TestRegistration480RetryPreservesCooldown(t *testing.T) {
	for _, tc := range []struct {
		name   string
		code   int
		header []string
		want   time.Duration
	}{
		{"default", 480, nil, time.Minute},
		{"minimum", 480, []string{"2"}, time.Minute},
		{"longer", 480, []string{"900"}, 15 * time.Minute},
		{"comment", 480, []string{"120 (maintenance)"}, 2 * time.Minute},
		{"parameter", 480, []string{"18000;duration=3600"}, 5 * time.Hour},
		{"day", 480, []string{"86400"}, 24 * time.Hour},
		{"zero", 480, []string{"0"}, 0},
		{"empty", 480, []string{""}, 0},
		{"negative", 480, []string{"-1"}, 0},
		{"fraction", 480, []string{"0.5"}, 0},
		{"junk", 480, []string{"60seconds"}, 0},
		{"duplicate", 480, []string{"60", "900"}, 0},
		{"overflow", 480, []string{"18446744073709551615"}, 0},
		{"excessive", 480, []string{"86401"}, 0},
		{"auth", 401, []string{"60"}, 0},
		{"forbidden", 403, []string{"60"}, 0},
		{"other_status", 503, []string{"60"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := registrationRejectionError(&sipResponse{StatusCode: tc.code, Headers: map[string][]string{"retry-after": tc.header}}, "initial")
			if !errors.Is(err, ErrRegistrationRejected) {
				t.Fatal("lost registration error", err)
			}
			var retry interface{ IMSRegistrationRetryAfter() time.Duration }
			got := time.Duration(0)
			if errors.As(err, &retry) {
				got = retry.IMSRegistrationRetryAfter()
			}
			if got != tc.want {
				t.Fatalf("cooldown %v, want %v", got, tc.want)
			}
		})
	}
}
