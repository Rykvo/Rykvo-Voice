package main

import (
	"context"
	"errors"
	"testing"

	"github.com/emiago/sipgo/sip"
)

func TestSIPByeConfirmationRequiresTerminalReply(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
		err  error
		want bool
	}{
		{"ok", 200, nil, true},
		{"already-gone", 481, nil, true},
		{"unauthorized", 401, nil, false},
		{"server-error", 500, nil, false},
		{"timeout", 0, context.DeadlineExceeded, false},
		{"transport", 0, errors.New("test reset"), false},
		{"absent", 0, nil, false},
		{"failed-read", 200, context.DeadlineExceeded, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var res *sip.Response
			if tc.code != 0 {
				res = sip.NewResponse(tc.code, "Test")
			}
			if got := sipByeConfirmed("fixture", res, tc.err); got != tc.want {
				t.Fatal(got)
			}
		})
	}
}
