package hardware

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// Explicit local opt-in. RAM round trips only; no PDP activation or MMS send.
func TestMMSModemFileRoundTrip(t *testing.T) {
	path := os.Getenv("RYKVO_MMS_FILE_TEST_PORT")
	if path == "" {
		t.Skip("no modem file-test port")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	port, e := openAT(path)
	if e != nil {
		t.Fatal(e)
	}
	b := &mmsBearer{at: &atSession{port: port}, healthy: true}
	defer b.close()
	lines, e := b.command(ctx, "AT+CGMM", 2*time.Second)
	if e != nil || !strings.Contains(strings.Join(lines, " "), "EC20") {
		t.Fatal("not EC20", e)
	}
	fixtures := [][]byte{bytes.Repeat([]byte("ab12"), 700), bytes.Repeat([]byte("a+++b\r\nOK\r\n"), 200)}
	if input := os.Getenv("RYKVO_MMS_FILE_TEST_INPUT"); input != "" {
		data, err := os.ReadFile(input)
		if err != nil {
			t.Fatal(err)
		}
		fixtures = [][]byte{data}
	}
	for i, data := range fixtures {
		name := []string{"RAM:rvmmsq01.txt", "RAM:rvmmsq02.txt"}[i]
		if e = b.fileAvailable(ctx, name); e != nil {
			t.Fatal(e)
		}
		e = b.upload(ctx, name, data)
		if b.healthy {
			_, _ = b.command(ctx, "AT+QFDEL=\""+name+"\"", 2*time.Second)
		}
		if e != nil {
			t.Fatal(e)
		}
	}
}
