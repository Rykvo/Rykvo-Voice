package main

import (
	"context"
	"net/http"
	"strings"
	"time"
)

func (s *server) administrator(ctx context.Context, w http.ResponseWriter, r *http.Request, current session) {
	if r.Method == http.MethodGet {
		reply(w, 200, map[string]any{"data": map[string]string{"account": current.User.Username}})
		return
	}
	if r.Method != http.MethodPatch {
		w.Header().Set("Allow", "GET, PATCH")
		fail(w, 405, "METHOD_NOT_ALLOWED")
		return
	}
	var input struct {
		Account         string `json:"account"`
		CurrentPassword string `json:"currentPassword"`
		NewPassword     string `json:"newPassword"`
		ConfirmPassword string `json:"confirmPassword"`
	}
	if !decodeBody(w, r, &input) {
		return
	}
	input.Account = strings.TrimSpace(input.Account)
	if len(input.Account) > 64 || len(input.CurrentPassword) == 0 || len(input.CurrentPassword) > 128 || (input.NewPassword != "" && (len(input.NewPassword) < 8 || len(input.NewPassword) > 128 || input.NewPassword == input.CurrentPassword)) || input.NewPassword != input.ConfirmPassword || (input.Account == "" && input.NewPassword == "") {
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
	var count int
	err := s.db.QueryRow(ctx, `INSERT INTO administrator_attempts(administrator_id,count,started_at) VALUES($1,1,now()) ON CONFLICT(administrator_id) DO UPDATE SET count=CASE WHEN administrator_attempts.started_at<now()-interval '15 minutes' THEN 1 ELSE administrator_attempts.count+1 END, started_at=CASE WHEN administrator_attempts.started_at<now()-interval '15 minutes' THEN now() ELSE administrator_attempts.started_at END RETURNING count`, current.User.ID).Scan(&count)
	if err != nil {
		fail(w, 503, "DATABASE_UNAVAILABLE")
		return
	}
	if count > 5 {
		w.Header().Set("Retry-After", "900")
		fail(w, 429, "RATE_LIMITED")
		return
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		fail(w, 503, "DATABASE_UNAVAILABLE")
		return
	}
	defer tx.Rollback(ctx)
	var account string
	var salt, hash []byte
	var iterations int
	if err = tx.QueryRow(ctx, `SELECT username,password_salt,password_hash,password_iterations FROM administrators WHERE id=$1 FOR UPDATE`, current.User.ID).Scan(&account, &salt, &hash, &iterations); err != nil {
		fail(w, 503, "DATABASE_UNAVAILABLE")
		return
	}
	if !verifyPassword(input.CurrentPassword, salt, hash, iterations) {
		fail(w, 403, "INVALID_PASSWORD")
		return
	}
	if input.Account == "" {
		input.Account = account
	}
	if input.NewPassword != "" {
		salt, hash, err = newPassword(input.NewPassword)
		iterations = passwordIterations
		if err != nil {
			fail(w, 503, "SERVICE_UNAVAILABLE")
			return
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE administrators SET username=$2,password_salt=$3,password_hash=$4,password_iterations=$5 WHERE id=$1`, current.User.ID, input.Account, salt, hash, iterations); err != nil {
		fail(w, 409, "ACCOUNT_UPDATE_FAILED")
		return
	}
	if _, err = tx.Exec(ctx, `DELETE FROM login_sessions WHERE administrator_id=$1`, current.User.ID); err != nil {
		fail(w, 503, "DATABASE_UNAVAILABLE")
		return
	}
	if _, err = tx.Exec(ctx, `DELETE FROM administrator_attempts WHERE administrator_id=$1`, current.User.ID); err != nil {
		fail(w, 503, "DATABASE_UNAVAILABLE")
		return
	}
	if err = tx.Commit(ctx); err != nil {
		fail(w, 503, "DATABASE_UNAVAILABLE")
		return
	}
	s.cookie(w, "", time.Unix(0, 0), strings.HasPrefix(r.Header.Get("Origin"), "https://"))
	w.WriteHeader(204)
}
