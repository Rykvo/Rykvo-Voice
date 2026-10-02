package main

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"unicode"

	"github.com/jackc/pgx/v5"
)

type contactSettings struct {
	URL      string `json:"url"`
	Revision int64  `json:"revision"`
}

func validContactURL(value string) bool {
	if value == "" {
		return true
	}
	if len(value) > 2048 || strings.ContainsAny(value, `\<>"`) || strings.IndexFunc(value, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return false
	}
	u, err := url.Parse(value)
	if err != nil || u.User != nil {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		return strings.HasPrefix(strings.ToLower(value), strings.ToLower(u.Scheme)+"://") && u.Hostname() != "" && u.Opaque == ""
	case "mailto", "tel":
		return u.Host == "" && u.Opaque != "" && !strings.HasPrefix(u.Opaque, "/")
	}
	return false
}

func (s *server) publicContactAPI(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		fail(w, 405, "METHOD_NOT_ALLOWED")
		return
	}
	var value string
	if err := s.db.QueryRow(ctx, "SELECT url FROM contact_settings").Scan(&value); err != nil {
		writeServiceError(w, err)
		return
	}
	if !validContactURL(value) {
		value = ""
	}
	reply(w, 200, map[string]any{"data": map[string]string{"url": value}})
}

func (s *server) contactSettingsAPI(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	var result contactSettings
	var err error
	switch r.Method {
	case http.MethodGet:
		err = s.db.QueryRow(ctx, "SELECT url,revision FROM contact_settings").Scan(&result.URL, &result.Revision)
	case http.MethodPut:
		var in struct {
			URL      *string `json:"url"`
			Revision int64   `json:"revision"`
		}
		if !decodeBody(w, r, &in) {
			return
		}
		if in.URL == nil || in.Revision < 1 || !validContactURL(strings.TrimSpace(*in.URL)) {
			fail(w, 400, "INVALID_CONTACT_URL")
			return
		}
		err = s.db.QueryRow(ctx, "UPDATE contact_settings SET url=$1,revision=revision+1 WHERE revision=$2 RETURNING url,revision", strings.TrimSpace(*in.URL), in.Revision).Scan(&result.URL, &result.Revision)
		if errors.Is(err, pgx.ErrNoRows) {
			fail(w, 409, "SETTINGS_CHANGED")
			return
		}
	default:
		fail(w, 405, "METHOD_NOT_ALLOWED")
		return
	}
	if err != nil {
		writeServiceError(w, err)
		return
	}
	reply(w, 200, map[string]any{"data": result})
}
