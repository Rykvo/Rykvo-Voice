package vowifi

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func isimTLV(tag byte, body []byte) []byte {
	result := []byte{tag}
	if len(body) < 128 {
		result = append(result, byte(len(body)))
	} else {
		result = append(result, 0x82, byte(len(body)>>8), byte(len(body)))
	}
	return append(result, body...)
}

func isimFCP(size, recordLength, records int) []byte {
	descriptor := []byte{0x41, 0x21}
	if records != 0 {
		descriptor = []byte{0x42, 0x21, byte(recordLength >> 8), byte(recordLength), byte(records)}
	}
	body := isimTLV(0x82, descriptor)
	body = append(body, isimTLV(0x80, []byte{byte(size >> 8), byte(size)})...)
	return isimTLV(0x62, body)
}

type isimAPDUStep struct{ command, response []byte }

func isimFileSteps(private, domain string, publics []string) []isimAPDUStep {
	var steps []isimAPDUStep
	for i, value := range []string{private, domain} {
		data := isimTLV(0x80, []byte(value))
		steps = append(steps, isimAPDUStep{[]byte{0, 0xa4, 0, 4, 2, 0x6f, byte(2 + i), 0}, append(isimFCP(len(data), 0, 0), 0x90, 0)})
		for offset := 0; offset < len(data); {
			size := min(255, len(data)-offset)
			steps = append(steps, isimAPDUStep{[]byte{0, 0xb0, byte(offset >> 8), byte(offset), byte(size)}, append(append([]byte{}, data[offset:offset+size]...), 0x90, 0)})
			offset += size
		}
	}
	length := 0
	for _, public := range publics {
		length = max(length, len(isimTLV(0x80, []byte(public))))
	}
	steps = append(steps, isimAPDUStep{[]byte{0, 0xa4, 0, 4, 2, 0x6f, 4, 0}, append(isimFCP(length*len(publics), length, len(publics)), 0x90, 0)})
	for i, public := range publics {
		data := isimTLV(0x80, []byte(public))
		data = append(data, bytes.Repeat([]byte{0xff}, length-len(data))...)
		steps = append(steps, isimAPDUStep{[]byte{0, 0xb2, byte(i + 1), 4, byte(length)}, append(data, 0x90, 0)})
	}
	return steps
}

func TestReadISIMFilesMultiRecordAndChunked(t *testing.T) {
	private := strings.Repeat("x", 270) + "@private.example"
	publics := []string{"sip:subscriber@voice.example", "sip:+85255550123@voice.example"}
	steps := isimFileSteps(private, "ims.example", publics)
	next := 0
	got, err := readISIMIdentityFiles(func(apdu []byte) ([]byte, error) {
		if next >= len(steps) || !bytes.Equal(apdu, steps[next].command) {
			t.Fatalf("unexpected APDU %X at %d", apdu, next)
		}
		raw := steps[next].response
		next++
		return raw, nil
	})
	if err != nil || next != len(steps) || got.PrivateIdentity != private || got.Domain != "ims.example" || !reflect.DeepEqual(got.PublicIdentities, publics) {
		t.Fatalf("read failed: %v, steps %d/%d", err, next, len(steps))
	}
}

func TestISIMRejectsPartialMalformedAndInjection(t *testing.T) {
	for _, raw := range [][]byte{{0x80, 2, 'a'}, {0x80, 0}, {0x80, 1, 0xff}, {0x80, 1, 'a', 0x80, 1, 'b'}, {0x80, 1, 'a', 0xff, 0}} {
		if _, err := decodeISIMString(raw); err == nil {
			t.Fatalf("accepted bad TLV %X", raw)
		}
	}
	for _, value := range []ProvisionedIMSIdentity{
		{PrivateIdentity: "subscriber@private.example", Domain: "ims.example"},
		{PrivateIdentity: "subscriber@private.example\r\nX: y", Domain: "ims.example", PublicIdentities: []string{"sip:a@ims.example"}},
		{PrivateIdentity: "a@private.example", Domain: "ims.example:5060", PublicIdentities: []string{"sip:a@ims.example"}},
		{PrivateIdentity: "a@private.example", Domain: "ims.example", PublicIdentities: []string{"sip:a@ims.example\nX:y"}},
	} {
		if value.Validate() == nil {
			t.Fatal("accepted invalid identity")
		}
	}
	for _, raw := range [][]byte{isimFCP(0, 0, 0), isimFCP(4097, 0, 0), {0x62, 4, 0x80, 2, 0, 5}, {0x62, 0x82, 0xff, 0xff}} {
		if _, err := parseISIMFile(raw, false); err == nil {
			t.Fatalf("accepted bad FCP %X", raw)
		}
	}
	for _, args := range [][3]int{{34, 1, 34}, {30, 16, 2}, {600, 300, 2}, {0, 0, 0}} {
		if _, err := parseISIMFile(isimFCP(args[0], args[1], args[2]), true); err == nil {
			t.Fatal("accepted invalid records")
		}
	}
	for _, failAt := range []int{0, 1, 3, 5} {
		steps := isimFileSteps("a@private.example", "ims.example", []string{"sip:a@ims.example"})
		next := 0
		got, err := readISIMIdentityFiles(func([]byte) ([]byte, error) {
			defer func() { next++ }()
			if next == failAt {
				return []byte{0x69, 0x82}, nil
			}
			return steps[next].response, nil
		})
		if got != nil || err == nil || next != failAt+1 {
			t.Fatal("partial identity was accepted")
		}
	}
}

func TestEC20ReadProvisionedIMSIdentity(t *testing.T) {
	for _, basic := range []bool{false, true} {
		for _, scenario := range []string{"success", "read_error", "sim_changed", "cancelled"} {
			t.Run(fmt.Sprintf("basic=%v/%s", basic, scenario), func(t *testing.T) {
				const iccid = "8985203000000000001"
				const isimAID = "A0000000871004FFFFFFFF8901"
				aid, _ := hex.DecodeString(isimAID)
				cuad := isimTLV(0x61, isimTLV(0x4f, aid))
				steps := []ec20TranscriptStep{{command: "AT+CCID", lines: []string{"+CCID: " + iccid}}, {command: "AT+CUAD", lines: []string{fmt.Sprintf(`+CUAD: "%X"`, cuad)}}}
				open := ec20TranscriptStep{command: `AT+CCHO="` + isimAID + `"`, lines: []string{"+CCHO: 1"}}
				if basic {
					open.lines, open.err = nil, errors.New("unsupported")
				}
				steps = append(steps, open)
				if basic {
					selectAPDU := append([]byte{0, 0xa4, 4, 4, byte(len(aid))}, aid...)
					steps = append(steps, ec20TranscriptStep{command: fmt.Sprintf(`AT+CSIM=%d,"%X"`, len(selectAPDU)*2, selectAPDU), lines: []string{`+CSIM: 4,"9000"`}})
				}
				for i, apdu := range isimFileSteps("a@private.example", "ims.example", []string{"sip:a@ims.example"}) {
					prefix, command := "+CGLA:", fmt.Sprintf(`AT+CGLA=1,%d,"%X"`, len(apdu.command)*2, apdu.command)
					if basic {
						prefix, command = "+CSIM:", fmt.Sprintf(`AT+CSIM=%d,"%X"`, len(apdu.command)*2, apdu.command)
					}
					step := ec20TranscriptStep{command: command, sensitive: true, lines: []string{fmt.Sprintf(`%s %d,"%X"`, prefix, len(apdu.response)*2, apdu.response)}}
					if scenario == "read_error" || scenario == "cancelled" {
						step.lines, step.err = nil, context.Canceled
					}
					steps = append(steps, step)
					if i == 0 && (scenario == "read_error" || scenario == "cancelled") {
						break
					}
				}
				if scenario == "success" || scenario == "sim_changed" {
					live := iccid
					if scenario == "sim_changed" {
						live = "8985203000000000002"
					}
					steps = append(steps, ec20TranscriptStep{command: "AT+CCID", lines: []string{"+CCID: " + live}})
				}
				if basic {
					steps = append(steps, ec20TranscriptStep{command: `AT+CSIM=24,"00A4040407A0000000871002"`, lines: []string{`+CSIM: 4,"9000"`}})
				} else {
					steps = append(steps, ec20TranscriptStep{command: "AT+CCHC=1"})
				}
				transcript := &ec20Transcript{t: t, steps: steps}
				adapter, _ := NewEC20Adapter(transcript, EC20AdapterOptions{})
				adapter.bindings[iccid] = ec20SIMBinding{deviceID: "ec20", iccid: iccid, imsi: "454030000000001", aid: usimAIDPrefix, application: "USIM"}
				got, err := adapter.ReadProvisionedIMSIdentity(context.Background(), SIMIdentity{ICCID: iccid, IMSI: "454030000000001"})
				transcript.assertDone()
				if scenario == "success" {
					if err != nil || got == nil || got.PrivateIdentity != "a@private.example" {
						t.Fatalf("read failed: %v", err)
					}
					binding := adapter.bindings[iccid]
					if binding.application != "USIM" || binding.isimAID != isimAID || binding.isimBasicChannel != basic {
						t.Fatal("lost USIM binding or ISIM selection")
					}
				} else if got != nil || err == nil {
					t.Fatal("accepted failed read")
				}
			})
		}
	}
}

func TestEC20ISIMAbsentDoesNotProbeOrModifyUSIM(t *testing.T) {
	const iccid = "8985203000000000001"
	transcript := &ec20Transcript{t: t, steps: []ec20TranscriptStep{
		{command: "AT+CCID", lines: []string{"+CCID: " + iccid}},
		{command: "AT+CUAD", lines: []string{`+CUAD: "61094F07A0000000871002"`}},
	}}
	adapter, _ := NewEC20Adapter(transcript, EC20AdapterOptions{})
	adapter.bindings[iccid] = ec20SIMBinding{deviceID: "ec20", iccid: iccid, imsi: "454030000000001", aid: usimAIDPrefix, application: "USIM"}
	got, err := adapter.ReadProvisionedIMSIdentity(context.Background(), SIMIdentity{ICCID: iccid, IMSI: "454030000000001"})
	if got != nil || !errors.Is(err, ErrISIMUnavailable) {
		t.Fatalf("got %v", err)
	}
	transcript.assertDone()
}

func TestEC20ISIMDirectoryAbsenceRestoresBasicChannel(t *testing.T) {
	for _, cleanupFails := range []bool{false, true} {
		const iccid = "8985203000000000001"
		cleanup := ec20TranscriptStep{command: `AT+CSIM=24,"00A4040407A0000000871002"`, lines: []string{`+CSIM: 4,"9000"`}}
		if cleanupFails {
			cleanup.lines = []string{`+CSIM: 4,"6982"`}
		}
		transcript := &ec20Transcript{t: t, steps: []ec20TranscriptStep{
			{command: "AT+CCID", lines: []string{"+CCID: " + iccid}},
			{command: "AT+CUAD", err: errors.New("unsupported")},
			{command: `AT+CSIM=16,"00A40004023F0000"`, lines: []string{`+CSIM: 4,"9000"`}},
			{command: `AT+CSIM=16,"00A40004022F0000"`, lines: []string{`+CSIM: 4,"9000"`}},
			{command: `AT+CSIM=10,"00B2010400"`, lines: []string{`+CSIM: 26,"61094F07A00000008710029000"`}},
			{command: `AT+CSIM=10,"00B2020400"`, lines: []string{`+CSIM: 4,"6A83"`}},
			cleanup,
		}}
		adapter, _ := NewEC20Adapter(transcript, EC20AdapterOptions{})
		adapter.bindings[iccid] = ec20SIMBinding{deviceID: "ec20", iccid: iccid, imsi: "454030000000001", aid: usimAIDPrefix, application: "USIM"}
		got, err := adapter.ReadProvisionedIMSIdentity(context.Background(), SIMIdentity{ICCID: iccid, IMSI: "454030000000001"})
		if got != nil || err == nil || errors.Is(err, ErrISIMUnavailable) == cleanupFails {
			t.Fatalf("cleanup-failed=%v, error=%v", cleanupFails, err)
		}
		transcript.assertDone()
	}
}

func TestEC20LogicalReadCorrectedLengthAndGetResponse(t *testing.T) {
	transcript := &ec20Transcript{t: t, steps: []ec20TranscriptStep{
		{command: `AT+CGLA=1,10,"00B0000000"`, sensitive: true, lines: []string{`+CGLA: 4,"6C05"`}},
		{command: `AT+CGLA=1,10,"00B0000005"`, sensitive: true, lines: []string{`+CGLA: 4,"6105"`}},
		{command: `AT+CGLA=1,10,"00C0000005"`, sensitive: true, lines: []string{`+CGLA: 14,"80036162639000"`}},
	}}
	adapter, _ := NewEC20Adapter(transcript, EC20AdapterOptions{})
	raw, err := adapter.transmitLogicalAPDU(context.Background(), "ec20", 1, []byte{0, 0xb0, 0, 0, 0}, true)
	if err != nil || !bytes.Equal(raw, []byte{0x80, 3, 'a', 'b', 'c', 0x90, 0}) {
		t.Fatalf("corrected read failed: %v", err)
	}
	transcript.assertDone()
}

type provisionedFakeAKA struct {
	fakeAKA
	identity *ProvisionedIMSIdentity
	err      error
}

func (fake provisionedFakeAKA) ReadProvisionedIMSIdentity(ctx context.Context, _ SIMIdentity) (*ProvisionedIMSIdentity, error) {
	if err := fake.environment.record(ctx, "isim.read"); err != nil {
		return nil, err
	}
	return fake.identity, fake.err
}

func TestOrchestratorProvisionedIdentity(t *testing.T) {
	identity := &ProvisionedIMSIdentity{PrivateIdentity: "a@private.example", Domain: "ims.example", PublicIdentities: []string{"sip:a@ims.example"}}
	for _, scenario := range []string{"provisioned", "absent", "malformed", "changed", "nil"} {
		t.Run(scenario, func(t *testing.T) {
			env := newFakeEnvironment()
			o := newTestOrchestrator(t, env, false)
			aka := provisionedFakeAKA{fakeAKA: fakeAKA{env}, identity: identity}
			switch scenario {
			case "absent":
				aka.identity, aka.err = nil, ErrISIMUnavailable
			case "malformed":
				aka.identity = &ProvisionedIMSIdentity{Domain: "ims.example"}
			case "changed":
				aka.identity, aka.err = nil, ErrEC20IdentityChanged
			case "nil":
				aka.identity = nil
			}
			o.deps.AKA = aka
			state, err := o.Enable(context.Background())
			defer o.Disable(context.Background())
			if scenario == "provisioned" || scenario == "absent" {
				if err != nil || !state.IMSReady || len(env.tunnelRequests) != 1 {
					t.Fatalf("enable failed: %v", err)
				}
				want := "derived"
				if scenario == "provisioned" {
					want = "isim"
				}
				if state.IMSIdentitySource != want || !reflect.DeepEqual(env.tunnelRequests[0].Identity.ProvisionedIMS, aka.identity) {
					t.Fatal("identity not forwarded")
				}
			} else if err == nil || len(env.tunnelRequests) != 0 {
				t.Fatal("bad identity reached network")
			}
		})
	}
}

func FuzzISIMDecoders(f *testing.F) {
	f.Add(isimTLV(0x80, []byte("sip:a@ims.example")))
	f.Add(isimFCP(16, 0, 0))
	f.Add([]byte{0x80, 0x82, 0xff, 0xff})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 8192 {
			return
		}
		_, _ = decodeISIMString(data)
		_, _ = parseISIMFile(data, false)
		_, _ = parseISIMFile(data, true)
	})
}
