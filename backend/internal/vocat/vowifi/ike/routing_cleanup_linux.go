//go:build linux

package ike

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
)

// Read-only proof for the session's private table; never flush shared routes.
func (h *linuxUserspaceHandle) confirmRoutingRemoved(ctx context.Context) error {
	table, priority := userspaceRoutingIdentifiers(h.config.InboundSPI)
	wantTable, wantPriority := strconv.FormatUint(uint64(table), 10), strconv.FormatUint(uint64(priority), 10)
	for _, family := range []string{"-4", "-6"} {
		for _, kind := range []string{"route", "rule"} {
			args := []string{family, "-j", "-N", kind, "show"}
			if kind == "route" {
				args = append(args, "table", "all")
			}
			body, err := exec.CommandContext(ctx, h.ipCommand, args...).Output()
			if err != nil {
				return fmt.Errorf("ike: verify %s %s cleanup: %w", family, kind, err)
			}
			var rows []map[string]any
			if err := json.Unmarshal(body, &rows); err != nil || rows == nil {
				return errors.New("ike: cleanup inventory invalid")
			}
			for _, row := range rows {
				if row == nil {
					return errors.New("ike: cleanup inventory invalid")
				}
				if routingTableValue(row["table"]) == wantTable || kind == "rule" && routingTableValue(row["priority"]) == wantPriority {
					return errors.New("ike: private routing cleanup remains pending")
				}
			}
		}
	}
	return ctx.Err()
}
