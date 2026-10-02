package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"unicode"

	"github.com/jackc/pgx/v5"
)

type developerSettings struct {
	Webhook, Secret, HostID string
	KeyHash                 []byte
	Revision                int64
}
type developerPatch struct {
	Key      json.RawMessage `json:"apiKey"`
	Webhook  *string         `json:"webhook"`
	Revision int64           `json:"apiRevision"`
}

func (s *server) readDeveloper(ctx context.Context) (v developerSettings, err error) {
	err = s.db.QueryRow(ctx, "SELECT key_hash,webhook,webhook_secret,revision,host_id::text FROM developer_settings").Scan(&v.KeyHash, &v.Webhook, &v.Secret, &v.Revision, &v.HostID)
	return
}
func validDeveloperSecret(v string) bool {
	return len(v) >= 6 && len(v) <= 256 && !strings.ContainsFunc(v, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) })
}
func webhookSigningSecret(keyHash []byte) string {
	if len(keyHash) == 0 {
		return ""
	}
	sum := sha256.Sum256([]byte("rykvo-webhook-v1:" + hex.EncodeToString(keyHash)))
	return hex.EncodeToString(sum[:])
}
func validWebhook(raw string) bool {
	if raw == "" {
		return true
	}
	u, err := url.Parse(raw)
	if err == nil {
		if ip, e := netip.ParseAddr(u.Hostname()); e == nil && !webhookPublicIP(ip) {
			return false
		}
	}
	return err == nil && len(raw) <= 2048 && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && u.Fragment == "" &&
		!strings.ContainsAny(raw, "\r\n\x00") && (u.Port() == "" || u.Port() == "443")
}
func (p developerPatch) apply(v developerSettings) (developerSettings, bool, error) {
	requested := p.Key != nil || p.Webhook != nil
	if !requested {
		return v, false, nil
	}
	if p.Revision != v.Revision {
		return v, false, rejected(409, "SETTINGS_CHANGED")
	}
	old := v
	keyUpdated, keyCleared := false, false
	if p.Key != nil {
		value, err := secretPatch(p.Key, "")
		if err != nil || value != "" && !validDeveloperSecret(value) {
			return v, false, rejected(400, "INVALID_API_KEY")
		}
		if string(p.Key) == "null" {
			v.KeyHash = []byte{}
			keyCleared = true
		} else if value != "" {
			v.KeyHash = tokenHash(value)
			keyUpdated = true
		}
	}
	if p.Webhook != nil {
		v.Webhook = strings.TrimSpace(*p.Webhook)
	}
	if !validWebhook(v.Webhook) {
		return v, false, rejected(400, "INVALID_WEBHOOK_URL")
	}
	if keyCleared || p.Webhook != nil && v.Webhook == "" {
		v.Secret = ""
	} else if v.Webhook != "" && (keyUpdated || old.Webhook != v.Webhook || v.Secret == "" && len(v.KeyHash) > 0) {
		if len(v.KeyHash) == 0 {
			return v, false, rejected(400, "WEBHOOK_API_KEY_REQUIRED")
		}
		// Keep existing integrations until their destination or API key is changed.
		v.Secret = webhookSigningSecret(v.KeyHash)
	}
	changed := !bytes.Equal(old.KeyHash, v.KeyHash) || old.Webhook != v.Webhook || old.Secret != v.Secret
	return v, changed, nil
}
func saveDeveloper(ctx context.Context, tx pgx.Tx, v *developerSettings) error {
	var previousURL, previousSecret string
	if err := tx.QueryRow(ctx, "SELECT webhook,webhook_secret FROM developer_settings FOR UPDATE").Scan(&previousURL, &previousSecret); err != nil {
		return err
	}
	err := tx.QueryRow(ctx, `UPDATE developer_settings SET key_hash=$1,webhook=$2,webhook_secret=$3,revision=revision+1 WHERE revision=$4 RETURNING revision`, v.KeyHash, v.Webhook, v.Secret, v.Revision).Scan(&v.Revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return rejected(409, "SETTINGS_CHANGED")
	}
	if err == nil && (previousURL != v.Webhook || previousSecret != v.Secret) {
		_, err = tx.Exec(ctx, "UPDATE developer_events SET state='cancelled',issue='WEBHOOK_SETTINGS_CHANGED',finished_at=now(),lease='' WHERE state IN ('pending','sending')")
		if err == nil {
			_, err = tx.Exec(ctx, "DELETE FROM developer_module_state")
		}
	}
	return err
}
func writeServiceError(w http.ResponseWriter, err error) {
	var coded *serviceError
	if errors.As(err, &coded) {
		if coded.Status == 429 {
			w.Header().Set("Retry-After", "60")
		}
		fail(w, coded.Status, coded.Code)
	} else {
		fail(w, 503, "DATABASE_UNAVAILABLE")
	}
}
