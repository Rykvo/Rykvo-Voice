package hardware

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"
	"time"
)

type boundedOutput struct{ bytes.Buffer }

func (b *boundedOutput) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 65536 {
		return 0, errors.New("response too large")
	}
	return b.Buffer.Write(p)
}

func readQMI(ctx context.Context, c Candidate) Reading {
	r := Reading{Model: c.Model, SIM: "unknown", Registration: "unknown"}
	path, err := exec.LookPath("qmicli")
	if err != nil {
		r.Issue = "QMI_UNAVAILABLE"
		return r
	}
	query := func(op string) string {
		if ctx.Err() != nil {
			return ""
		}
		call, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		cmd := exec.CommandContext(call, path, "--device="+c.Control, "--device-open-proxy", op)
		cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C"}
		cmd.WaitDelay = time.Second
		var out boundedOutput
		cmd.Stdout = &out
		if cmd.Run() != nil {
			r.Warnings = append(r.Warnings, op+":READ_FAILED")
			return ""
		}
		return out.String()
	}
	ids := query("--dms-get-ids")
	r.IMEI = qmiValue(ids, "IMEI")
	if r.IMEI == "" {
		r.Issue = "QMI_READ_FAILED"
		return r
	}
	r.Responsive = true
	if v := qmiValue(query("--dms-get-model"), "Model"); v != "" {
		r.Model = v
	}
	r.Firmware = qmiValue(query("--dms-get-revision"), "Revision")
	r.ICCID = qmiValue(query("--dms-uim-get-iccid"), "ICCID")
	if r.ICCID != "" {
		r.SIM = "READY"
	}
	status := query("--nas-get-serving-system")
	r.Registration = qmiValue(status, "Registration state")
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
func qmiValue(text, key string) string {
	for _, line := range strings.Split(text, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), ":")
		if ok && strings.EqualFold(k, key) {
			return strings.Trim(strings.TrimSpace(v), "'\"")
		}
	}
	return ""
}
