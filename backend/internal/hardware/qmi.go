package hardware

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"
)

const qmiSocket = "/run/rykvo-voice-qmi.sock"

func proxyQuery(ctx context.Context, socket, device, command string) (string, error) {
	call, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(call, "unix", socket)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	deadline, _ := call.Deadline()
	conn.SetDeadline(deadline)
	stop := context.AfterFunc(call, func() { conn.Close() })
	defer stop()
	if err = json.NewEncoder(conn).Encode(map[string]string{"device": device, "command": command}); err != nil {
		return "", err
	}
	var response struct {
		Output string `json:"output"`
		Error  string `json:"error"`
	}
	if err = json.NewDecoder(io.LimitReader(conn, 131073)).Decode(&response); err != nil {
		return "", err
	}
	if response.Error != "" || len(response.Output) > 65536 {
		return "", errors.New("QMI_READ_FAILED")
	}
	return response.Output, nil
}

type boundedOutput struct{ bytes.Buffer }

func (b *boundedOutput) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 65536 {
		return 0, errors.New("response too large")
	}
	return b.Buffer.Write(p)
}

func queryQMI(ctx context.Context, device, op string) (string, error) {
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if _, err := os.Stat(qmiSocket); err == nil {
		return proxyQuery(ctx, qmiSocket, device, op)
	}
	path, err := exec.LookPath("qmicli")
	if err != nil {
		return "", errors.New("QMI_UNAVAILABLE")
	}
	call, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(call, path, "--device="+device, "--device-open-proxy", op)
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C"}
	cmd.WaitDelay = time.Second
	var out boundedOutput
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return out.String(), nil
}

func qmiOperatorName(status, plmn string) string {
	if !decimal(plmn, 5, 6) || qmiValue(status, "MCC") != plmn[:3] {
		return ""
	}
	mnc, expected := integer(qmiValue(status, "MNC")), integer(plmn[3:])
	if mnc == nil || expected == nil || *mnc != *expected {
		return ""
	}
	return qmiValue(status, "Description")
}

func readQMI(ctx context.Context, c Candidate) Reading {
	r := Reading{Model: c.Model, SIM: "unknown", Registration: "unknown"}
	query := func(op string) string {
		output, err := queryQMI(ctx, c.Control, op)
		if err != nil {
			r.Warnings = append(r.Warnings, op+":READ_FAILED")
			if err.Error() == "QMI_UNAVAILABLE" {
				r.Issue = "QMI_UNAVAILABLE"
			}
		}
		return output
	}
	ids := query("--dms-get-ids")
	r.IMEI = qmiValue(ids, "IMEI")
	if r.IMEI == "" {
		if r.Issue == "" {
			r.Issue = "QMI_READ_FAILED"
		}
		return r
	}
	r.Responsive = true
	if v := qmiValue(query("--dms-get-model"), "Model"); v != "" {
		r.Model = v
	}
	r.Firmware = qmiValue(query("--dms-get-revision"), "Revision")
	r.ICCID = qmiValue(query("--dms-uim-get-iccid"), "ICCID")
	if r.ICCID == "" {
		r.ICCID = qmiSlotICCID(query("--uim-get-slot-status"))
	}
	r.SIM = qmiSIMState(query("--uim-get-card-status"))
	status := query("--nas-get-serving-system")
	r.Registration = qmiValue(status, "Registration state")
	if r.Registration == "registration-denied" {
		r.Registration = "denied"
	}
	if r.Registration == "" {
		r.Registration, r.Issue = "unknown", "QMI_STATUS_FAILED"
	}
	r.Operator = qmiValue(status, "Description")
	r.PLMN = qmiValue(status, "MCC") + qmiValue(status, "MNC")
	if strings.Contains(strings.ToLower(status), "lte") {
		r.Technology = "LTE"
	}
	signal := query("--nas-get-signal-info")
	for _, field := range []struct {
		name string
		ptr  **int
	}{{"RSSI", &r.RSSI}, {"RSRP", &r.RSRP}, {"RSRQ", &r.RSRQ}, {"SNR", &r.SINR}} {
		v := strings.Fields(qmiValue(signal, field.name))
		if len(v) > 0 {
			*field.ptr = integer(v[0])
		}
	}
	r.Number = qmiValue(query("--dms-get-msisdn"), "MSISDN")
	if !validNumber(r.Number) {
		r.Number = ""
	}
	return r
}

func qmiSlotICCID(text string) string {
	for _, slot := range strings.Split(text, "Physical slot ") {
		if qmiValue(slot, "Slot status") == "active" && qmiValue(slot, "Logical slot") == "1" && qmiValue(slot, "Card status") == "present" {
			if v := qmiValue(slot, "ICCID"); decimal(v, 18, 22) {
				return v
			}
		}
	}
	return ""
}

func qmiSIMState(text string) string {
	for _, slot := range strings.Split(text, "Slot [") {
		if !strings.HasPrefix(slot, "1]:") {
			continue
		}
		if qmiValue(slot, "Card state") == "absent" {
			return "absent"
		}
		switch qmiValue(slot, "Application state") {
		case "ready":
			return "READY"
		case "pin1-or-upin-pin-required":
			return "SIM PIN"
		case "puk1-or-upin-puk-required":
			return "SIM PUK"
		}
	}
	return "unknown"
}

func qmiValue(text, key string) string {
	for _, line := range strings.Split(text, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), ":")
		if ok && strings.EqualFold(k, key) {
			return strings.Trim(strings.TrimSpace(v), "'\"")
		}
	}
	return ""
}
