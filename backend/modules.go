package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
	"net/http"
	"rykvo.local/auth/internal/hardware"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

type moduleRecord struct {
	ID                                     int64
	Identity, Endpoint, Kind, Model, Label string
	Custom                                 bool
	LastSeen                               time.Time
}

const moduleColumns = "id,hardware_key,endpoint,kind,model,label,label_custom,last_seen"

func scanModule(row interface{ Scan(...any) error }) (moduleRecord, error) {
	var v moduleRecord
	err := row.Scan(&v.ID, &v.Identity, &v.Endpoint, &v.Kind, &v.Model, &v.Label, &v.Custom, &v.LastSeen)
	return v, err
}
func moduleID(id int64) string   { return fmt.Sprintf("module-%02d", id) }
func moduleName(id int64) string { return fmt.Sprintf("模块 %02d", id) }
func labelKey(v string) string   { return cases.Fold().String(norm.NFKC.String(strings.TrimSpace(v))) }
func validModuleLabel(v string) bool {
	if !utf8.ValidString(v) || utf8.RuneCountInString(v) < 1 || utf8.RuneCountInString(v) > 20 {
		return false
	}
	for _, r := range v {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
			return false
		}
	}
	return true
}
func listModuleRecords(ctx context.Context, pool *pgxpool.Pool) ([]moduleRecord, error) {
	rows, err := pool.Query(ctx, "SELECT "+moduleColumns+" FROM modules ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []moduleRecord{}
	for rows.Next() {
		v, err := scanModule(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, v)
	}
	return result, rows.Err()
}
func lockModules(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(72859601)")
	return err
}
func bindModule(ctx context.Context, pool *pgxpool.Pool, c hardware.Candidate, r hardware.Reading) (moduleRecord, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return moduleRecord{}, err
	}
	defer tx.Rollback(ctx)
	if err = lockModules(ctx, tx); err != nil {
		return moduleRecord{}, err
	}
	identity := c.Identity(r)
	serial := c.Identity(hardware.Reading{})
	if !strings.HasPrefix(serial, "serial:") {
		serial = ""
	}
	v, err := scanModule(tx.QueryRow(ctx, "SELECT "+moduleColumns+" FROM modules WHERE hardware_key=$1", identity))
	if errors.Is(err, pgx.ErrNoRows) && serial != "" {
		v, err = scanModule(tx.QueryRow(ctx, "SELECT "+moduleColumns+" FROM modules WHERE hardware_key=$1 OR (serial_key=$1 AND $2 LIKE 'serial:%') ORDER BY id LIMIT 1", serial, identity))
	}
	if errors.Is(err, pgx.ErrNoRows) {
		v, err = scanModule(tx.QueryRow(ctx, "SELECT "+moduleColumns+" FROM modules WHERE endpoint=$1 AND (hardware_key LIKE 'path:%' OR ($2 LIKE 'path:%' AND endpoint_generation=$3)) ORDER BY id LIMIT 1", c.Key, identity, c.Generation))
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return v, err
	}
	if errors.Is(err, pgx.ErrNoRows) {
		if strings.HasPrefix(identity, "path:") && c.Kind != "reader" {
			return v, errors.New("IDENTITY_PENDING")
		}
		if err = tx.QueryRow(ctx, "SELECT nextval(pg_get_serial_sequence('modules','id'))").Scan(&v.ID); err != nil {
			return v, err
		}
		v.Label, err = freeModuleLabel(ctx, tx, v.ID)
		if err != nil {
			return v, err
		}
		_, err = tx.Exec(ctx, `INSERT INTO modules(id,hardware_key,endpoint,kind,model,label,label_key) OVERRIDING SYSTEM VALUE VALUES($1,$2,$3,$4,$5,$6,$7)`, v.ID, identity, c.Key, c.Kind, r.Model, v.Label, labelKey(v.Label))
	} else {
		// Keep a proven hardware identity across a temporary unreadable SIM/serial port.
		if strings.HasPrefix(identity, "path:") || (strings.HasPrefix(identity, "serial:") && strings.HasPrefix(v.Identity, "imei:")) {
			identity = v.Identity
		}
		_, err = tx.Exec(ctx, `UPDATE modules SET hardware_key=$2,endpoint=$3,kind=$4,model=$5,last_seen=now() WHERE id=$1`, v.ID, identity, c.Key, c.Kind, r.Model)
	}
	if err != nil {
		return v, err
	}
	if _, err = tx.Exec(ctx, "UPDATE modules SET serial_key=CASE WHEN $2='' THEN serial_key ELSE $2 END,endpoint_generation=$3 WHERE id=$1", v.ID, serial, c.Generation); err != nil {
		return v, err
	}
	v.Identity = identity
	v.Endpoint = c.Key
	v.Kind = c.Kind
	v.Model = r.Model
	err = tx.Commit(ctx)
	return v, err
}
func freeModuleLabel(ctx context.Context, tx pgx.Tx, id int64) (string, error) {
	for n := id; ; n++ {
		label := moduleName(n)
		var exists bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM modules WHERE label_key=$1 AND id<>$2)", labelKey(label), id).Scan(&exists); err != nil {
			return "", err
		}
		if !exists {
			return label, nil
		}
	}
}

func (s *server) modulesAPI(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	tail := strings.TrimPrefix(r.URL.Path, "/api/modules")
	parts := strings.Split(strings.TrimPrefix(tail, "/"), "/")
	if tail == "" && r.Method == http.MethodGet {
		records, err := listModuleRecords(ctx, s.db)
		if err != nil {
			fail(w, 503, "DATABASE_UNAVAILABLE")
			return
		}
		items := make([]any, 0, len(records))
		for _, v := range records {
			items = append(items, s.moduleView(v))
		}
		issue := ""
		if s.modules != nil {
			issue = s.modules.issue()
		}
		reply(w, 200, map[string]any{"data": map[string]any{"items": items, "discoveryIssue": issue}})
		return
	}
	if tail == "" {
		w.Header().Set("Allow", "GET")
		fail(w, 405, "METHOD_NOT_ALLOWED")
		return
	}
	idText := strings.TrimPrefix(parts[0], "module-")
	id, err := strconv.ParseInt(idText, 10, 64)
	if err != nil || id < 1 || moduleID(id) != parts[0] {
		fail(w, 404, "MODULE_NOT_FOUND")
		return
	}
	v, err := scanModule(s.db.QueryRow(ctx, "SELECT "+moduleColumns+" FROM modules WHERE id=$1", id))
	if errors.Is(err, pgx.ErrNoRows) {
		fail(w, 404, "MODULE_NOT_FOUND")
		return
	}
	if err != nil {
		fail(w, 503, "DATABASE_UNAVAILABLE")
		return
	}
	if len(parts) > 1 {
		if len(parts) == 2 && parts[1] == "lines" && r.Method == http.MethodGet {
			reply(w, 200, map[string]any{"data": s.moduleView(v)["sims"]})
			return
		}
		s.moduleControl(ctx, w, r, v, parts)
		return
	}
	if r.Method == http.MethodGet {
		reply(w, 200, map[string]any{"data": s.moduleView(v)})
		return
	}
	if r.Method != http.MethodPatch {
		w.Header().Set("Allow", "GET, PATCH")
		fail(w, 405, "METHOD_NOT_ALLOWED")
		return
	}
	var input struct {
		Label        *string `json:"label"`
		IfUnmodified bool    `json:"ifUnmodified,omitempty"`
	}
	if !decodeBody(w, r, &input) {
		return
	}
	if input.Label == nil {
		fail(w, 400, "INVALID_LABEL")
		return
	}
	label := norm.NFC.String(strings.TrimSpace(*input.Label))
	if label != "" && !validModuleLabel(label) {
		fail(w, 400, "INVALID_LABEL")
		return
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		fail(w, 503, "DATABASE_UNAVAILABLE")
		return
	}
	defer tx.Rollback(ctx)
	if err = lockModules(ctx, tx); err != nil {
		fail(w, 503, "DATABASE_UNAVAILABLE")
		return
	}
	if input.IfUnmodified {
		var custom bool
		if err = tx.QueryRow(ctx, "SELECT label_custom FROM modules WHERE id=$1", id).Scan(&custom); err != nil {
			fail(w, 503, "DATABASE_UNAVAILABLE")
			return
		}
		if custom {
			fail(w, 409, "LABEL_ALREADY_SET")
			return
		}
	}
	custom := label != ""
	if label == "" {
		label, err = freeModuleLabel(ctx, tx, id)
		if err != nil {
			fail(w, 503, "DATABASE_UNAVAILABLE")
			return
		}
	}
	_, err = tx.Exec(ctx, "UPDATE modules SET label=$2,label_key=$3,label_custom=$4 WHERE id=$1", id, label, labelKey(label), custom)
	var pgerr *pgconn.PgError
	if errors.As(err, &pgerr) && pgerr.Code == "23505" {
		fail(w, 409, "LABEL_EXISTS")
		return
	}
	if err != nil || tx.Commit(ctx) != nil {
		fail(w, 503, "DATABASE_UNAVAILABLE")
		return
	}
	v.Label = label
	v.Custom = custom
	reply(w, 200, map[string]any{"data": s.moduleView(v)})
}

func (s *server) moduleView(v moduleRecord) map[string]any {
	status := "offline"
	reading := hardware.Reading{Model: v.Model, SIM: "unknown", Registration: "unknown"}
	issue := ""
	present := false
	if s.modules != nil {
		reading, present, issue = s.modules.state(v)
		if present {
			status = "online"
			if !reading.Responsive || issue != "" {
				status = "error"
			}
		}
	}
	sims := []any{}
	esim := present && reading.ESIM != nil && reading.ESIM.EID != "" && reading.ESIM.Issue == ""
	job := moduleJob{}
	if s.modules != nil {
		job = s.modules.job(v.ID)
	}
	if esim {
		for _, p := range reading.ESIM.Profiles {
			number := ""
			if p.Enabled && p.ICCID == reading.ICCID {
				number = reading.Number
			}
			sims = append(sims, map[string]any{"id": hardware.ProfileID(reading.ESIM.EID, p.ICCID), "iccid": p.ICCID, "label": p.Label, "number": number, "enabled": p.Enabled, "esim": true, "readOnly": false, "canDisable": p.CanDisable, "canDelete": p.CanDelete, "provider": p.Provider})
		}
	} else if present && reading.ICCID != "" {
		sims = append(sims, map[string]any{"id": "line-" + hardware.Digest(reading.ICCID)[:24], "label": "SIM", "number": reading.Number, "enabled": reading.SIM == "READY", "readOnly": true})
	}
	signal := "none"
	if reading.Registration == "home" || reading.Registration == "roaming" || reading.Registration == "registered" {
		signal = "cellular"
	}
	return map[string]any{"id": moduleID(v.ID), "name": moduleName(v.ID), "label": v.Label, "labelCustom": v.Custom, "number": reading.Number, "status": status, "signal": signal, "sims": sims, "kind": v.Kind, "hardware": reading, "issue": issue, "managed": true, "job": job, "capabilities": map[string]bool{"read": true, "sms": false, "calls": false, "lineControl": false, "esim": esim && !job.active()}}
}
