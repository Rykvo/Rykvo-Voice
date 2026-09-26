package hardware

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

func readFile(path string) string {
	b, _ := os.ReadFile(path)
	return strings.TrimSpace(string(b))
}

func (s *System) Discover(ctx context.Context) ([]Candidate, error) {
	root := filepath.Join(s.Sys, "bus/usb/devices")
	entries, err := os.ReadDir(root)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if _, err := os.Stat(s.Sys); err != nil {
		return nil, fmt.Errorf("hardware discovery unavailable")
	}
	groups := map[string]*Candidate{}
	bound := map[string]bool{}
	for _, e := range entries {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		base, _, isInterface := strings.Cut(e.Name(), ":")
		if !isInterface {
			continue
		}
		p := filepath.Join(root, e.Name())
		real, err := filepath.EvalSymlinks(p)
		if err == nil {
			p = real
		}
		driver, _ := filepath.EvalSymlinks(filepath.Join(p, "driver"))
		if filepath.Base(driver) == "qmi_wwan" {
			bound[base] = true
		}
		if groups[base] == nil {
			d := filepath.Join(root, base)
			groups[base] = &Candidate{Key: "usb:" + base, Kind: "usb", Vendor: strings.ToLower(readFile(filepath.Join(d, "idVendor"))), Product: strings.ToLower(readFile(filepath.Join(d, "idProduct"))), Serial: readFile(filepath.Join(d, "serial")), Model: readFile(filepath.Join(d, "product")), Generation: readFile(filepath.Join(d, "busnum")) + ":" + readFile(filepath.Join(d, "devnum"))}
		}
		c := groups[base]
		n, _ := strconv.ParseInt(readFile(filepath.Join(p, "bInterfaceNumber")), 16, 32)
		if readFile(filepath.Join(p, "bInterfaceClass")) == "0b" {
			c.Kind = "reader"
		}
		_ = filepath.WalkDir(p, func(path string, e fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			name := e.Name()
			if strings.HasPrefix(name, "ttyUSB") || strings.HasPrefix(name, "ttyACM") {
				node := filepath.Join(s.Dev, name)
				exists := false
				for _, port := range c.Ports {
					if port.Path == node {
						exists = true
					}
				}
				if !exists {
					c.Ports = append(c.Ports, Port{node, int(n)})
				}
			}
			if strings.HasPrefix(name, "cdc-wdm") {
				c.Control = filepath.Join(s.Dev, name)
			}
			if filepath.Base(filepath.Dir(path)) == "net" {
				c.Network = name
			}
			return nil
		})
	}
	var result []Candidate
	for key, c := range groups {
		if !bound[key] && c.Vendor != "2c7c" && !(c.Vendor == "2ca3" && c.Product == "4006") && c.Kind != "reader" {
			continue
		}
		if c.Model == "" || strings.EqualFold(c.Model, "android") {
			c.Model = "蜂窝模块"
		}
		if len(c.Ports) > 0 || c.Control != "" {
			c.Kind = "usb"
		}
		orderPorts(c)
		result = append(result, *c)
	}
	wwan, err := os.ReadDir(filepath.Join(s.Sys, "class/wwan"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	portNames := map[string]bool{}
	for _, e := range wwan {
		portNames[e.Name()] = true
	}
	if entries, e := os.ReadDir(s.Dev); e == nil {
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), "wwan") {
				portNames[entry.Name()] = true
			}
		}
	}
	wgroups := map[string]*Candidate{}
	names := make([]string, 0, len(portNames))
	for name := range portNames {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		split := strings.Index(name, "at")
		kind := "at"
		if split < 0 {
			split = strings.Index(name, "qmi")
			kind = "qmi"
		}
		if !strings.HasPrefix(name, "wwan") || split < 5 {
			continue
		}
		prefix := name[:split]
		if _, err := strconv.Atoi(prefix[4:]); err != nil {
			continue
		}
		if _, err := strconv.Atoi(name[split+len(kind):]); err != nil {
			continue
		}
		c := wgroups[prefix]
		if c == nil {
			c = &Candidate{Key: "wwan:" + prefix, Kind: "wwan", Model: "高通 WWAN 模块"}
			wgroups[prefix] = c
			if p, err := filepath.EvalSymlinks(filepath.Join(s.Sys, "class/wwan", name, "device")); err == nil {
				if before, _, ok := strings.Cut(p, string(filepath.Separator)+"wwan"+string(filepath.Separator)); ok {
					p = before
				}
				c.Key = "wwan:" + p
			}
		}
		if kind == "at" {
			n, _ := strconv.Atoi(name[split+2:])
			c.Ports = append(c.Ports, Port{filepath.Join(s.Dev, name), n})
		} else {
			c.Control = filepath.Join(s.Dev, name)
		}
	}
	for _, c := range wgroups {
		if info, e := os.Stat(c.Control); e == nil {
			c.Generation = info.ModTime().UTC().String()
		}
		orderPorts(c)
		result = append(result, *c)
	}
	// PC/SC supplies reader names; USB-only candidates remain visible if pcscd is unavailable.
	readerSource := s.Readers
	if readerSource == nil {
		readerSource = cardReaders
	}
	readers, readerErr := readerSource(ctx)
	if readerErr == nil && len(readers) > 0 {
		filtered := result[:0]
		for _, c := range result {
			if c.Kind != "reader" {
				filtered = append(filtered, c)
			}
		}
		result = filtered
		for _, name := range readers {
			result = append(result, Candidate{Key: "reader:" + name, Kind: "reader", Reader: name, Model: name})
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Key < result[j].Key })
	return result, ctx.Err()
}

func orderPorts(c *Candidate) {
	base := 0
	min := 256
	for _, p := range c.Ports {
		if p.Interface < min {
			min = p.Interface
		}
	}
	if min >= 2 {
		base = 2
	}
	rank := func(p Port) int {
		if c.Vendor == "2c7c" && c.Product == "6005" && len(c.Ports) == 1 {
			return 0
		}
		if c.Kind == "wwan" {
			if p.Interface == 1 {
				return 0
			}
			return 1
		}
		if p.Interface == base+2 {
			return 0
		}
		if strings.HasPrefix(filepath.Base(p.Path), "ttyACM") {
			return 1
		}
		if p.Interface == base+3 {
			return 2
		}
		return 9
	}
	sort.Slice(c.Ports, func(i, j int) bool {
		a, b := rank(c.Ports[i]), rank(c.Ports[j])
		if a != b {
			return a < b
		}
		return c.Ports[i].Path < c.Ports[j].Path
	})
	// Never probe diagnostics or GNSS interfaces as a fallback AT terminal.
	ports := c.Ports[:0]
	for _, p := range c.Ports {
		if c.Kind == "usb" && c.Vendor == "2c7c" && c.Product == "0125" && p.Interface == 1 {
			c.Audio = p.Path
		}
		if rank(p) < 9 {
			ports = append(ports, p)
		}
	}
	c.Ports = ports
}
