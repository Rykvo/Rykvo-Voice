package main

import (
	"context"
	"errors"
	"math"
	"net/http"
	"regexp"
	"time"
)

const developerBatchLimit = 25
const developerLookupLimit = 100

var receivedMessageIDPattern = regexp.MustCompile(`^rx-[a-f0-9]{64}$`)

func validLookupID(id string) bool {
	return jobIDPattern.MatchString(id) || receivedMessageIDPattern.MatchString(id)
}

type developerBatchItem struct {
	RequestID  string            `json:"requestId"`
	Status     int               `json:"status"`
	Data       *developerMessage `json:"data,omitempty"`
	Error      string            `json:"error,omitempty"`
	RetryAfter int               `json:"retryAfter,omitempty"`
}

func batchFailure(id string, err error) developerBatchItem {
	v := developerBatchItem{RequestID: id, Status: 503, Error: "DATABASE_UNAVAILABLE", RetryAfter: 5}
	var coded *serviceError
	if errors.As(err, &coded) {
		v.Status, v.Error, v.RetryAfter = coded.Status, coded.Code, 0
		if coded.Status == 429 {
			v.RetryAfter = 60
		}
	}
	return v
}

func validBatchIDs(ids []string, limit int, validID func(string) bool) bool {
	if len(ids) == 0 || len(ids) > limit {
		return false
	}
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if !validID(id) || seen[id] {
			return false
		}
		seen[id] = true
	}
	return true
}

func (s *server) developerMessageBatch(ctx context.Context, w http.ResponseWriter, r *http.Request, revision int64) {
	var in struct {
		Items []messageInput `json:"items"`
	}
	if !decodeJSONBody(w, r, &in, 128*1024, "INVALID_BATCH") {
		return
	}
	ids := make([]string, len(in.Items))
	for i, item := range in.Items {
		ids[i] = item.RequestID
	}
	if !validBatchIDs(ids, developerBatchLimit, jobIDPattern.MatchString) {
		fail(w, 400, "INVALID_BATCH")
		return
	}
	release := s.developerWork.bulk()
	if release == nil {
		developerReject(w, 503, "API_BUSY", time.Second)
		return
	}
	defer release()
	items := make([]developerBatchItem, 0, len(in.Items))
	for _, item := range in.Items {
		if item.Image != "" {
			items = append(items, batchFailure(item.RequestID, rejected(400, "USE_ATTACHMENT_ID")))
			continue
		}
		// Charge each item, not each HTTP envelope. Reserve headroom for replies.
		if delay := s.developerWork.take(time.Now(), developerCategory("POST", "/messages"), developerBulkBudget()); delay > 0 {
			items = append(items, developerBatchItem{RequestID: item.RequestID, Status: 429, Error: "API_RATE_LIMIT", RetryAfter: max(1, int(math.Ceil(delay.Seconds())))})
			continue
		}
		var releaseImage func()
		if item.AttachmentID != "" {
			releaseImage = s.developerWork.image()
			if releaseImage == nil {
				items = append(items, developerBatchItem{RequestID: item.RequestID, Status: 503, Error: "API_BUSY", RetryAfter: 1})
				continue
			}
		}
		item.bulk = true
		v, created, err := s.enqueueMessage(ctx, item, revision)
		if releaseImage != nil {
			releaseImage()
		}
		if err != nil {
			items = append(items, batchFailure(item.RequestID, err))
			continue
		}
		var epoch int64
		if err = s.db.QueryRow(ctx, "SELECT alert_epoch FROM messages WHERE id=$1", v.ID).Scan(&epoch); err != nil {
			items = append(items, batchFailure(item.RequestID, err))
			continue
		}
		data := developerMessageView(v, epoch)
		status := 200
		if created {
			status = 202
		}
		items = append(items, developerBatchItem{RequestID: item.RequestID, Status: status, Data: &data})
	}
	reply(w, 200, map[string]any{"data": map[string]any{"items": items}})
}

func (s *server) developerMessageLookup(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	var in struct {
		IDs []string `json:"ids"`
	}
	if !decodeJSONBody(w, r, &in, 16*1024, "INVALID_BATCH") {
		return
	}
	if !validBatchIDs(in.IDs, developerLookupLimit, validLookupID) {
		fail(w, 400, "INVALID_BATCH")
		return
	}
	rows, err := s.db.Query(ctx, "SELECT "+messageColumns+",alert_epoch FROM messages WHERE id=ANY($1::text[])", in.IDs)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	defer rows.Close()
	found := make(map[string]developerMessage, len(in.IDs))
	for rows.Next() {
		var v messageView
		var module, epoch int64
		var image bool
		var at time.Time
		if err = rows.Scan(&v.ID, &module, &v.LineID, &v.Number, &v.Text, &image, &v.Mine, &v.Kind, &v.State, &v.Issue, &at, &v.Revision, &v.Deleted, &epoch); err != nil {
			writeServiceError(w, err)
			return
		}
		v.SenderID, v.At = moduleID(module), at.UnixMilli()
		if v.State == "mms_report" {
			v.Deleted = true
		}
		if image {
			v.Image = v.ID
		}
		found[v.ID] = developerMessageView(v, epoch)
	}
	if err = rows.Err(); err != nil {
		writeServiceError(w, err)
		return
	}
	items := make([]developerBatchItem, 0, len(in.IDs))
	for _, id := range in.IDs {
		if v, ok := found[id]; ok {
			items = append(items, developerBatchItem{RequestID: id, Status: 200, Data: &v})
		} else {
			items = append(items, batchFailure(id, rejected(404, "NOT_FOUND")))
		}
	}
	reply(w, 200, map[string]any{"data": map[string]any{"items": items}})
}
