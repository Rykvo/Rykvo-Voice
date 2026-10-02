package hardware

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

const cleanupSMS = "000405912143F500004210203040500005C82293F904"

func TestCellularCleanupPersistence(t *testing.T) {
	for _, tc := range []struct {
		name, pdu, status string
		keep              bool
	}{
		{"received", cleanupSMS, "0", false},
		{"read", cleanupSMS, "1", false},
		{"multipart", "004405912143F50008421020304050000A0500037A02014F60597D", "0", false},
		{"wap-push", "004405912143F50004421020304050000A0605040B8423F0010203", "0", false},
		{"report", "00022A05912143F5421020304050004210203050500000", "0", false},
		{"unsent", "00010005912143F500000100", "2", true},
		{"sent", "00010005912143F500000100", "3", true},
		{"malformed", "not-hex", "0", true},
		{"empty", "00", "0", true},
		{"sim-data", "00440C919471071610007FF6629041718111403D02700000381516001212B201000D5F284696D1470A06A44E649D62B3BC7B6A11D49874DBE86C379BD4A87805BDA5ED2FF2DE9416A43640832306C159E1", "0", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			rows := []string{"+CMGL: 7," + tc.status + ",,22", tc.pdu}
			records, err := storeCellularPDU(context.Background(), rows, func(_ context.Context, d SMSDelivery) error {
				calls++
				if d.TPDU != tc.pdu[2:] || d.DecodeError != "" {
					t.Fatalf("raw PDU not archived correctly: %+v", d)
				}
				return nil
			})
			want := 1
			if tc.keep {
				want = 0
			}
			if err != nil || len(records) != want || calls != want {
				t.Fatal(records, calls, err)
			}
			if want == 1 && (records[0].index != 7 || records[0].pdu != tc.pdu) {
				t.Fatal(records)
			}
		})
	}
	for _, index := range []string{"-1", "65536", "1;AT+CMGD=0,4", "bad"} {
		records, err := storeCellularPDU(context.Background(), []string{"+CMGL: " + index + ",0,,22", cleanupSMS}, func(context.Context, SMSDelivery) error { t.Fatal("invalid index"); return nil })
		if len(records) != 0 || err != nil {
			t.Fatal(records, err)
		}
	}
	storageErr := errors.New("commit failed")
	records, err := storeCellularPDU(context.Background(), []string{"+CMGL: 1,0,,22", cleanupSMS}, func(context.Context, SMSDelivery) error { return storageErr })
	if !errors.Is(err, storageErr) || len(records) != 0 {
		t.Fatal("failed commit was eligible for deletion", records, err)
	}
}

func TestCellularCleanupExactSlot(t *testing.T) {
	for _, tc := range []struct {
		name, reply, deleteReply string
		cardChanged, cancel      bool
		wantDelete, wantError    bool
	}{
		{"matching", "+CMGR: 1,,22\r\n" + cleanupSMS, "OK", false, false, true, false},
		{"lowercase", "+CMGR: 0,,22\r\n" + strings.ToLower(cleanupSMS), "OK", false, false, true, false},
		{"reused", "+CMGR: 1,,22\r\n00010005912143F500000100", "OK", false, false, false, true},
		{"missing", "", "OK", false, false, false, true},
		{"unsupported", "ERROR", "OK", false, false, false, true},
		{"bad-header", "+CMGR: 1\r\n" + cleanupSMS, "OK", false, false, false, true},
		{"not-received", "+CMGR: 2,,22\r\n" + cleanupSMS, "OK", false, false, false, true},
		{"duplicate-header", "+CMGR: 1,,22\r\n" + cleanupSMS + "\r\n+CMGR: 1,,22\r\n" + cleanupSMS, "OK", false, false, false, true},
		{"changed-card", "+CMGR: 1,,22\r\n" + cleanupSMS, "OK", true, false, false, true},
		{"cancelled", "+CMGR: 1,,22\r\n" + cleanupSMS, "OK", false, true, false, true},
		{"delete-failed", "+CMGR: 1,,22\r\n" + cleanupSMS, "ERROR", false, false, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			deleted := false
			port := &mmsTestPort{onWrite: func(b []byte) string {
				switch strings.TrimSpace(string(b)) {
				case "AT+CMGR=7":
					return tc.reply + "\r\nOK\r\n"
				case "AT+CMGD=7,0":
					deleted = true
					return tc.deleteReply + "\r\n"
				default:
					t.Fatal("unexpected command", string(b))
					return "ERROR\r\n"
				}
			}}
			err := deleteCellularPDU(ctx, &atSession{port: port}, cellularStoredPDU{7, cleanupSMS}, func(context.Context) error {
				if tc.cancel {
					cancel()
				}
				if tc.cardChanged {
					return errors.New("DEVICE_CHANGED")
				}
				return nil
			})
			if (err != nil) != tc.wantError || deleted != tc.wantDelete {
				t.Fatal(deleted, err, port.commands)
			}
		})
	}
}

func TestCellularCleanupRetryAndStores(t *testing.T) {
	// A failed delete leaves the archived PDU available for the next poll.
	// A second memory bank is still read, and the original CPMS is restored.
	for _, failCommit := range []bool{false, true} {
		current := "ME"
		pending := map[string]bool{"ME": true, "SM": true, "SR": true}
		durable := map[string]bool{}
		failDelete := true
		port := &mmsTestPort{onWrite: func(b []byte) string {
			cmd := strings.TrimSpace(string(b))
			switch {
			case cmd == "AT+CPMS?":
				return "+CPMS: \"ME\",1,255,\"ME\",1,255,\"ME\",1,255\r\nOK\r\n"
			case strings.HasPrefix(cmd, "AT+CPMS="):
				current = strings.Trim(strings.TrimPrefix(cmd, "AT+CPMS="), `"`)
				return "OK\r\n"
			case cmd == "AT+CMGL=4":
				if pending[current] {
					return "+CMGL: 7,0,,22\r\n" + cleanupSMS + "\r\nOK\r\n"
				}
				return "OK\r\n"
			case cmd == "AT+CMGR=7":
				return "+CMGR: 1,,22\r\n" + cleanupSMS + "\r\nOK\r\n"
			case cmd == "AT+CMGD=7,0":
				if !durable[current] {
					t.Fatal("deleted before commit")
				}
				if failDelete {
					return "ERROR\r\n"
				}
				pending[current] = false
				return "OK\r\n"
			default:
				t.Fatal(cmd)
				return "ERROR\r\n"
			}
		}}
		poll := func() error {
			return readCellularInbox(context.Background(), &atSession{port: port}, func(context.Context) error { return nil }, func(_ context.Context, d SMSDelivery) error {
				if failCommit {
					return errors.New("database unavailable")
				}
				durable[current] = true
				return nil
			})
		}
		if err := poll(); err == nil || current != "ME" {
			t.Fatal(current, err)
		}
		if !failCommit && len(durable) != 3 {
			t.Fatal("cleanup failure blocked another store", durable)
		}
		for _, exists := range pending {
			if !exists {
				t.Fatal("unexpected deletion")
			}
		}
		failCommit, failDelete = false, false
		if err := poll(); err != nil || current != "ME" {
			t.Fatal(current, err)
		}
		for bank, exists := range pending {
			if exists {
				t.Fatal("retry did not clean", bank)
			}
		}
		if err := poll(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCellularCleanupCardChangedBeforeStore(t *testing.T) {
	port := &mmsTestPort{onWrite: func(b []byte) string {
		switch strings.TrimSpace(string(b)) {
		case "AT+CPMS?":
			return "+CPMS: \"ME\",1,255\r\nOK\r\n"
		case "AT+CMGL=4":
			return fmt.Sprintf("+CMGL: 7,0,,22\r\n%s\r\nOK\r\n", cleanupSMS)
		default:
			t.Fatal("unexpected write", string(b))
			return "ERROR\r\n"
		}
	}}
	err := readCellularInbox(context.Background(), &atSession{port: port}, func(context.Context) error { return errors.New("DEVICE_CHANGED") }, func(context.Context, SMSDelivery) error { t.Fatal("stored a different SIM's message"); return nil })
	if err == nil || err.Error() != "DEVICE_CHANGED" {
		t.Fatal(err)
	}
}

func TestCellularInboxRejectedBankDoesNotBlockOthers(t *testing.T) {
	for _, rejected := range []string{"SR", "SM", "ME"} {
		t.Run(rejected, func(t *testing.T) {
			current := rejected
			stored, deleted, read := map[string]int{}, map[string]int{}, map[string]int{}
			port := &mmsTestPort{onWrite: func(b []byte) string {
				cmd := strings.TrimSpace(string(b))
				switch {
				case cmd == "AT+CPMS?":
					return fmt.Sprintf("+CPMS: %q,0,50,\"ME\",59,255,\"ME\",59,255\r\nOK\r\n", rejected)
				case strings.HasPrefix(cmd, "AT+CPMS="):
					current = strings.Trim(strings.TrimPrefix(cmd, "AT+CPMS="), `"`)
					return "OK\r\n"
				case cmd == "AT+CMGL=4":
					read[current]++
					response := "+CMGL: 7,0,,22\r\n" + cleanupSMS + "\r\n"
					if current == rejected {
						return response + "+CMS ERROR: 321\r\n"
					}
					return response + "OK\r\n"
				case cmd == "AT+CMGR=7":
					return "+CMGR: 1,,22\r\n" + cleanupSMS + "\r\nOK\r\n"
				case cmd == "AT+CMGD=7,0":
					if current == rejected || stored[current] != 1 {
						t.Fatal("delete before confirmed persistence", current)
					}
					deleted[current]++
					return "OK\r\n"
				default:
					t.Fatal("unexpected command", cmd)
					return "ERROR\r\n"
				}
			}}
			err := readCellularInbox(context.Background(), &atSession{port: port}, func(context.Context) error { return nil }, func(_ context.Context, _ SMSDelivery) error {
				stored[current]++
				return nil
			})
			if err == nil || current != rejected || len(read) != 3 || len(stored) != 2 || len(deleted) != 2 || stored[rejected] != 0 {
				t.Fatal("rejected bank blocked an inbox or accepted partial data", current, read, stored, deleted, err)
			}
		})
	}
}

func TestCellularInboxInvalidRecordDoesNotBlockLaterRecords(t *testing.T) {
	lines := []string{}
	for _, index := range []int{1, 2, 3} {
		lines = append(lines, fmt.Sprintf("+CMGL: %d,0,,22", index), cleanupSMS)
	}
	calls := 0
	records, err := storeCellularPDU(context.Background(), lines, func(context.Context, SMSDelivery) error {
		calls++
		if calls == 2 {
			return errors.New("INVALID_MESSAGE")
		}
		return nil
	})
	if !errors.Is(err, errCellularRecordRejected) || calls != 3 || len(records) != 2 || records[0].index != 1 || records[1].index != 3 {
		t.Fatal(calls, records, err)
	}
}
