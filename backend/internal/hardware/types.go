// Package hardware reads devices and applies explicit per-device operations.
package hardware

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
)

type Port struct {
	Path      string
	Interface int
}

type Candidate struct {
	Key        string
	Kind       string
	Vendor     string
	Product    string
	Model      string
	Serial     string
	Generation string
	Ports      []Port
	Control    string
	Network    string
	Reader     string
}

type Reading struct {
	NetworkMode      *int      `json:"networkMode"`
	AccessTechnology *int      `json:"accessTechnology"`
	ReaderSerial     string    `json:"readerSerial,omitempty"`
	Model            string    `json:"model"`
	Firmware         string    `json:"firmware"`
	IMEI             string    `json:"imei"`
	ICCID            string    `json:"iccid"`
	Number           string    `json:"number"`
	SIM              string    `json:"simState"`
	Operator         string    `json:"operator"`
	PLMN             string    `json:"plmn"`
	Technology       string    `json:"technology"`
	Registration     string    `json:"registration"`
	RSSI             *int      `json:"rssi"`
	RSRP             *int      `json:"rsrp"`
	RSRQ             *int      `json:"rsrq"`
	SINR             *int      `json:"sinr"`
	Responsive       bool      `json:"responsive"`
	Issue            string    `json:"issue"`
	Warnings         []string  `json:"warnings,omitempty"`
	UpdatedAt        time.Time `json:"updatedAt"`
	ESIM             *ESIMInfo `json:"esim,omitempty"`
}

type Source interface {
	Discover(context.Context) ([]Candidate, error)
	Read(context.Context, Candidate) Reading
}

func Digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func (c Candidate) Identity(r Reading) string {
	if c.Kind == "reader" && r.ReaderSerial != "" {
		return "reader:" + Digest(r.ReaderSerial)
	}
	if len(r.IMEI) >= 14 {
		return "imei:" + Digest(r.IMEI)
	}
	s := strings.TrimSpace(c.Serial)
	if s != "" && !strings.EqualFold(s, "android") && strings.Trim(s, "0") != "" {
		return "serial:" + Digest(c.Vendor+":"+c.Product+":"+s)
	}
	return "path:" + Digest(c.Key)
}

type System struct {
	Sys     string
	Dev     string
	Readers func(context.Context) ([]string, error)
}

func NewSystem() *System { return &System{Sys: "/sys", Dev: "/dev"} }

func (s *System) Read(ctx context.Context, c Candidate) Reading {
	var r Reading
	if c.Kind == "reader" {
		r = readCard(ctx, c)
	} else {
		r = readAT(ctx, c)
		if !r.Responsive && c.Control != "" && ctx.Err() == nil {
			atIssue := r.Issue
			r = readQMI(ctx, c)
			r.Warnings = append(r.Warnings, "AT:"+atIssue)
		}
	}
	if r.Responsive && r.Issue == "" && r.Operator == "" && r.PLMN != "" && c.Control != "" {
		// Numeric COPS format persists after manual selection; do not change it to fetch a label.
		if status, err := queryQMI(ctx, c.Control, "--nas-get-serving-system"); err == nil {
			r.Operator = qmiOperatorName(status, r.PLMN)
		}
	}
	if r.Responsive && r.SIM != "absent" && r.SIM != "SIM PIN" && r.SIM != "SIM PUK" && ctx.Err() == nil {
		call, cancel := context.WithTimeout(ctx, 30*time.Second)
		result := ESIMCall(call, ESIMRequest{Candidate: c, Action: "read", ExpectedIMEI: r.IMEI}, nil)
		cancel()
		r.ESIM = result.Info
		if r.ESIM == nil {
			r.ESIM = &ESIMInfo{Issue: result.Issue}
		}
	}
	r.Model = strings.TrimSpace(r.Model)
	if r.Model == "" {
		r.Model = c.Model
	}
	r.UpdatedAt = time.Now().UTC()
	return r
}
