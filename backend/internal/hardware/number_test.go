package hardware

import "testing"

func TestSubscriberNumberType(t *testing.T) {
	for _, tc := range []struct{ number, toa, want string }{
		{"13325550123", "145", "+13325550123"}, {"+13325550123", "145", "+13325550123"},
		{"13325550123", "129", "13325550123"}, {"13325550123", "161", "13325550123"},
		{"13325550123", "bad", "13325550123"}, {"13325550123", "401", "13325550123"},
		{"13325550123", "", "13325550123"}, {"85255550123", "145", "+85255550123"},
		{"123abc", "145", ""},
	} {
		if got := subscriberNumber([]string{"", tc.number, tc.toa}); got != tc.want {
			t.Errorf("%+v got %q", tc, got)
		}
	}
	if subscriberNumber(nil) != "" {
		t.Fatal("empty accepted")
	}
}
