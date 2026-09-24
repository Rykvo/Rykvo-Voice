package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
)

type gesture struct {
	count int
	last  time.Time
}
type queryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

var featureNames = map[string]bool{"modules": true, "phone": true, "messages": true, "general": true, "administrator": true, "sip": true, "server": true, "developer": true, "cleanup": true, "updates": true}

func (s *server) activate(key string, reset bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gestures == nil {
		s.gestures = make(map[string]gesture)
	}
	now := time.Now()
	for id, value := range s.gestures {
		if now.Sub(value.last) > 2*time.Second {
			delete(s.gestures, id)
		}
	}
	if reset {
		delete(s.gestures, key)
		return false
	}
	if len(s.gestures) >= 4096 {
		return false
	}
	value := s.gestures[key]
	value.count++
	value.last = now
	if value.count == 10 {
		delete(s.gestures, key)
		return true
	}
	s.gestures[key] = value
	return false
}
func visibilityAccess(ctx context.Context, db queryer, r *http.Request) (bool, error) {
	var valid bool
	err := db.QueryRow(ctx, `SELECT COALESCE(s.visibility_until>now() AND s.visibility_version=g.version,false) FROM login_sessions s CROSS JOIN visibility_security g WHERE s.token_hash=$1 AND s.expires_at>now()`, tokenHash(requestToken(r))).Scan(&valid)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return valid, err
}
func requireVisibility(ctx context.Context, db queryer, w http.ResponseWriter, r *http.Request) bool {
	valid, err := visibilityAccess(ctx, db, r)
	if err != nil {
		fail(w, 503, "DATABASE_UNAVAILABLE")
		return false
	}
	if !valid {
		fail(w, 403, "VERIFICATION_REQUIRED")
		return false
	}
	return true
}
func (s *server) visibility(ctx context.Context, w http.ResponseWriter, r *http.Request, current session) {
	switch {
	case r.URL.Path == "/api/ui/activation" && r.Method == http.MethodPost:
		var input struct {
			Reset bool `json:"reset"`
		}
		if !decodeBody(w, r, &input) {
			return
		}
		reply(w, 200, map[string]any{"data": map[string]bool{"challenge": s.activate(string(tokenHash(requestToken(r))), input.Reset)}})
	case r.URL.Path == "/api/settings/visibility" && r.Method == http.MethodGet:
		var features []byte
		err := s.db.QueryRow(ctx, `SELECT features FROM visibility_preferences WHERE administrator_id=$1`, current.User.ID).Scan(&features)
		if errors.Is(err, pgx.ErrNoRows) {
			reply(w, 200, map[string]any{"data": map[string]any{"features": nil}})
			return
		}
		if err != nil {
			fail(w, 503, "DATABASE_UNAVAILABLE")
			return
		}
		reply(w, 200, map[string]any{"data": map[string]any{"features": json.RawMessage(features)}})
	case r.URL.Path == "/api/settings/visibility/access" && r.Method == http.MethodGet:
		if requireVisibility(ctx, s.db, w, r) {
			reply(w, 200, map[string]any{"data": map[string]bool{"verified": true}})
		}
	case r.URL.Path == "/api/settings/visibility/access" && r.Method == http.MethodDelete:
		if _, err := s.db.Exec(ctx, `UPDATE login_sessions SET visibility_until=NULL,visibility_version=NULL WHERE token_hash=$1`, tokenHash(requestToken(r))); err != nil {
			fail(w, 503, "DATABASE_UNAVAILABLE")
			return
		}
		w.WriteHeader(204)
	case r.URL.Path == "/api/settings/visibility/unlock" && r.Method == http.MethodPost:
		s.visibilityPassword(ctx, w, r, current, false)
	case r.URL.Path == "/api/settings/visibility/password" && r.Method == http.MethodPut:
		s.visibilityPassword(ctx, w, r, current, true)
	case r.URL.Path == "/api/settings/visibility" && r.Method == http.MethodPut:
		s.saveVisibility(ctx, w, r, current)
	default:
		fail(w, 405, "METHOD_NOT_ALLOWED")
	}
}
func (s *server) saveVisibility(ctx context.Context, w http.ResponseWriter, r *http.Request, current session) {
	var input struct {
		Features map[string]bool `json:"features"`
	}
	if !decodeBody(w, r, &input) {
		return
	}
	if len(input.Features) == 0 {
		fail(w, 400, "INVALID_INPUT")
		return
	}
	for key := range input.Features {
		if !featureNames[key] {
			fail(w, 400, "INVALID_INPUT")
			return
		}
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		fail(w, 503, "DATABASE_UNAVAILABLE")
		return
	}
	defer tx.Rollback(ctx)
	var version int64
	// 与改密互斥，旧授权不能在改密后提交。
	if err = tx.QueryRow(ctx, `SELECT version FROM visibility_security FOR SHARE`).Scan(&version); err != nil {
		fail(w, 503, "NOT_CONFIGURED")
		return
	}
	if !requireVisibility(ctx, tx, w, r) {
		return
	}
	patch, _ := json.Marshal(input.Features)
	var result []byte
	err = tx.QueryRow(ctx, `INSERT INTO visibility_preferences(administrator_id,features) VALUES($1,$2::jsonb) ON CONFLICT(administrator_id) DO UPDATE SET features=visibility_preferences.features||EXCLUDED.features RETURNING features`, current.User.ID, patch).Scan(&result)
	if err != nil {
		fail(w, 503, "DATABASE_UNAVAILABLE")
		return
	}
	if err = tx.Commit(ctx); err != nil {
		fail(w, 503, "DATABASE_UNAVAILABLE")
		return
	}
	reply(w, 200, map[string]any{"data": map[string]any{"features": json.RawMessage(result)}})
}
func (s *server) visibilityPassword(ctx context.Context, w http.ResponseWriter, r *http.Request, current session, changing bool) {
	var input struct {
		Password string `json:"password"`
		Current  string `json:"currentPassword"`
		New      string `json:"newPassword"`
	}
	if !decodeBody(w, r, &input) {
		return
	}
	secret := input.Password
	if changing {
		if !requireVisibility(ctx, s.db, w, r) {
			return
		}
		secret = input.Current
		if len(input.New) < 8 || len(input.New) > 128 || input.New == secret {
			fail(w, 400, "INVALID_NEW_PASSWORD")
			return
		}
	}
	if len(secret) == 0 || len(secret) > 128 {
		fail(w, 400, "INVALID_INPUT")
		return
	}
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		fail(w, 429, "BUSY")
		return
	}
	// 限流记录存数据库，服务重启或换 IP 不会重置同账号的尝试次数。
	var count int
	err := s.db.QueryRow(ctx, `INSERT INTO visibility_attempts(administrator_id,count) VALUES($1,1) ON CONFLICT(administrator_id) DO UPDATE SET count=CASE WHEN visibility_attempts.started_at<=now()-interval '15 minutes' THEN 1 ELSE visibility_attempts.count+1 END, started_at=CASE WHEN visibility_attempts.started_at<=now()-interval '15 minutes' THEN now() ELSE visibility_attempts.started_at END WHERE visibility_attempts.count<5 OR visibility_attempts.started_at<=now()-interval '15 minutes' RETURNING count`, current.User.ID).Scan(&count)
	if errors.Is(err, pgx.ErrNoRows) {
		w.Header().Set("Retry-After", "900")
		fail(w, 429, "RATE_LIMITED")
		return
	}
	if err != nil {
		fail(w, 503, "DATABASE_UNAVAILABLE")
		return
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		fail(w, 503, "DATABASE_UNAVAILABLE")
		return
	}
	defer tx.Rollback(ctx)
	var salt, hash []byte
	var iterations int
	var version int64
	if err = tx.QueryRow(ctx, `SELECT password_salt,password_hash,password_iterations,version FROM visibility_security FOR UPDATE`).Scan(&salt, &hash, &iterations, &version); err != nil {
		fail(w, 503, "NOT_CONFIGURED")
		return
	}
	if changing && !requireVisibility(ctx, tx, w, r) {
		return
	}
	if !verifyPassword(secret, salt, hash, iterations) {
		fail(w, 403, "INVALID_PASSWORD")
		return
	}
	if changing {
		newSalt, newHash, hashError := newPassword(input.New)
		if hashError != nil {
			fail(w, 500, "INTERNAL")
			return
		}
		if _, err = tx.Exec(ctx, `UPDATE visibility_security SET password_salt=$1,password_hash=$2,password_iterations=$3,version=version+1`, newSalt, newHash, passwordIterations); err != nil {
			fail(w, 503, "DATABASE_UNAVAILABLE")
			return
		}
		if _, err = tx.Exec(ctx, `UPDATE login_sessions SET visibility_until=NULL,visibility_version=NULL`); err != nil {
			fail(w, 503, "DATABASE_UNAVAILABLE")
			return
		}
	} else {
		result, e := tx.Exec(ctx, `UPDATE login_sessions SET visibility_until=now()+interval '5 minutes',visibility_version=$1 WHERE token_hash=$2 AND expires_at>now()`, version, tokenHash(requestToken(r)))
		if e != nil {
			fail(w, 503, "DATABASE_UNAVAILABLE")
			return
		}
		if result.RowsAffected() != 1 {
			fail(w, 401, "UNAUTHENTICATED")
			return
		}
	}
	if _, err = tx.Exec(ctx, `DELETE FROM visibility_attempts WHERE administrator_id=$1`, current.User.ID); err != nil {
		fail(w, 503, "DATABASE_UNAVAILABLE")
		return
	}
	if err = tx.Commit(ctx); err != nil {
		fail(w, 503, "DATABASE_UNAVAILABLE")
		return
	}
	if changing {
		w.WriteHeader(204)
	} else {
		reply(w, 200, map[string]any{"data": map[string]bool{"verified": true}})
	}
}
