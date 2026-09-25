package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"
)

type messageContact struct {
	LineID   string `json:"lineId"`
	Number   string `json:"number"`
	Name     string `json:"name"`
	Revision int64  `json:"revision"`
}

func (s *server) messageContacts(ctx context.Context) ([]messageContact, error) {
	rows, err := s.db.Query(ctx, "SELECT line_id,peer,name,revision FROM message_contacts ORDER BY revision")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []messageContact{}
	for rows.Next() {
		var item messageContact
		if err := rows.Scan(&item.LineID, &item.Number, &item.Name, &item.Revision); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *server) messageContactAPI(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		fail(w, 405, "METHOD_NOT_ALLOWED")
		return
	}
	var in struct {
		LineID string `json:"lineId"`
		Number string `json:"number"`
		Name   string `json:"name"`
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2048))
	d.DisallowUnknownFields()
	if d.Decode(&in) != nil || d.Decode(new(any)) != io.EOF {
		fail(w, 400, "INVALID_INPUT")
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	in.Number = strings.TrimSpace(in.Number)
	if in.LineID == "" || len(in.LineID) > 128 || in.Number == "" || len(in.Number) > 128 || utf8.RuneCountInString(in.Name) > 24 || strings.IndexFunc(in.Name+in.Number, unicode.IsControl) >= 0 {
		fail(w, 400, "INVALID_INPUT")
		return
	}
	var exists bool
	if s.db.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM messages WHERE line_id=$1 AND deleted_at IS NULL)", in.LineID).Scan(&exists) != nil {
		fail(w, 503, "DATABASE_UNAVAILABLE")
		return
	}
	if !exists {
		fail(w, 404, "NOT_FOUND")
		return
	}
	var item messageContact
	err := s.db.QueryRow(ctx, `INSERT INTO message_contacts(line_id,peer,name) VALUES($1,$2,$3)
	ON CONFLICT(line_id,peer) DO UPDATE SET name=excluded.name,revision=nextval('message_contact_revision_seq')
	RETURNING line_id,peer,name,revision`, in.LineID, in.Number, in.Name).Scan(&item.LineID, &item.Number, &item.Name, &item.Revision)
	if err != nil {
		fail(w, 503, "DATABASE_UNAVAILABLE")
		return
	}
	reply(w, 200, map[string]any{"data": item})
}
