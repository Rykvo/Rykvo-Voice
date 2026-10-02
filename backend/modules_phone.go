package main

import (
	"context"
	"log"
	"time"

	"rykvo.local/auth/internal/hardware"
)

func (m *moduleManager) loadPhoneNumbers(ctx context.Context) error {
	if m.db == nil {
		return nil
	}
	rows, err := m.db.Query(ctx, "SELECT iccid,number FROM card_phone_numbers")
	if err != nil {
		return err
	}
	defer rows.Close()
	numbers := make(map[string]string)
	for rows.Next() {
		var iccid, number string
		if err := rows.Scan(&iccid, &number); err != nil {
			return err
		}
		if hardware.ValidAssociatedNumber(number) {
			numbers[iccid] = number
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	m.phoneNumbers = numbers
	m.mu.Unlock()
	return nil
}

func (m *moduleManager) acceptWiFiNumber(ctx context.Context, id int64, w *moduleWiFi, sample moduleSample, number string) {
	if !hardware.ValidAssociatedNumber(number) || ctx.Err() != nil {
		return
	}
	iccid := sample.Reading.ICCID
	m.mu.Lock()
	current, present := m.seen[sample.Candidate.Key]
	live, exists := m.values[id]
	valid := m.wifi[id] == w && w.running && w.Enabled && w.ICCID == iccid &&
		present && sameEndpoint(current, sample.Candidate) && exists &&
		sameEndpoint(live.Candidate, sample.Candidate) && live.Reading.ICCID == iccid && live.Reading.SIM == "READY"
	if valid {
		m.phoneNumbers[iccid] = number
	}
	m.mu.Unlock()
	if !valid || m.db == nil {
		return
	}
	// Do not hold the device-manager mutex across database IO. Per-card storage
	// remains correctly bound even if the card is removed while this write ends.
	write, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_, err := m.db.Exec(write, `INSERT INTO card_phone_numbers(iccid,number) VALUES($1,$2)
		ON CONFLICT(iccid) DO UPDATE SET number=$2,updated_at=now()`, iccid, number)
	if err != nil {
		log.Printf("module %d IMS number persistence pending", id)
	}
}
