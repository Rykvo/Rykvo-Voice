package hardware

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Isolate native driver calls so a hung reader cannot hold the service shutdown.
func cardCall(ctx context.Context, reader string, value any) error {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, exe, "-hardware-card", reader)
	cmd.WaitDelay = time.Second
	var out boundedOutput
	cmd.Stdout = &out
	if err = cmd.Run(); err != nil {
		return errors.New("PCSC_UNAVAILABLE")
	}
	return json.Unmarshal(out.Bytes(), value)
}
func cardReaders(ctx context.Context) ([]string, error) {
	var names []string
	err := cardCall(ctx, "", &names)
	return names, err
}
func readCard(ctx context.Context, c Candidate) Reading {
	r := Reading{Model: c.Model, SIM: "unknown", UpdatedAt: time.Now().UTC()}
	if c.Reader == "" {
		r.Issue = "PCSC_UNAVAILABLE"
		return r
	}
	if err := cardCall(ctx, c.Reader, &r); err != nil {
		r.Issue = "PCSC_UNAVAILABLE"
	}
	return r
}
func CardHelper(reader string) error {
	v, err := nativeCard(reader)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(v)
}
func decodeICCID(data []byte) string {
	var b strings.Builder
	for _, v := range data {
		for _, n := range []byte{v & 15, v >> 4} {
			if n < 10 {
				b.WriteByte('0' + n)
			} else if n != 15 {
				return ""
			}
		}
	}
	if b.Len() < 18 || b.Len() > 22 {
		return ""
	}
	return b.String()
}
