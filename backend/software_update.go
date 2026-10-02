package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"time"
)

const updateUploadLimit = 130 * 1024 * 1024
const updateChunkLimit = 1024 * 1024

var updateTicket = regexp.MustCompile(`^[a-f0-9]{32}$`)

type updateRequest struct {
	Action   string `json:"action"`
	Owner    string `json:"owner"`
	UploadID string `json:"uploadId,omitempty"`
	Offset   *int64 `json:"offset,omitempty"`
	Header   string `json:"header,omitempty"`
	Seal     string `json:"seal,omitempty"`
	Size     int64  `json:"size,omitempty"`
	Ticket   string `json:"ticket,omitempty"`
}
type updateStatus struct {
	State     string `json:"state"`
	UploadID  string `json:"uploadId,omitempty"`
	Offset    int64  `json:"offset"`
	Size      int64  `json:"size,omitempty"`
	ChunkSize int64  `json:"chunkSize,omitempty"`
	Version   string `json:"version,omitempty"`
	Ticket    string `json:"ticket,omitempty"`
	Error     string `json:"error,omitempty"`
}

func localUpdateCall(ctx context.Context, request updateRequest, input io.Reader) (updateStatus, error) {
	var result struct {
		Data  updateStatus `json:"data"`
		Error string       `json:"error"`
		Ready bool         `json:"ready"`
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", "/run/rykvo-voice-update.sock")
	if err != nil {
		return result.Data, errors.New("UPDATE_UNAVAILABLE")
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	decoder := json.NewDecoder(io.LimitReader(conn, 8192))
	if err = json.NewEncoder(conn).Encode(request); err == nil && input != nil {
		err = decoder.Decode(&result)
		if err == nil && result.Error != "" {
			return result.Data, errors.New(result.Error)
		}
		if err == nil && !result.Ready {
			err = errors.New("UPDATE_UNAVAILABLE")
		}
		if err == nil {
			_, err = io.CopyN(conn, input, request.Size)
		}
	}
	if err == nil {
		err = decoder.Decode(&result)
	}
	if err != nil {
		return result.Data, errors.New("UPDATE_UNAVAILABLE")
	}
	if result.Error != "" {
		return result.Data, errors.New(result.Error)
	}
	return result.Data, nil
}
func (s *server) softwareUpdateAPI(ctx context.Context, w http.ResponseWriter, r *http.Request, owner string) {
	request := updateRequest{Action: "status", Owner: hex.EncodeToString(tokenHash(owner))}
	var input io.Reader
	switch {
	case r.URL.Path == "/api/software-update" && r.Method == "GET":
	case r.URL.Path == "/api/software-update/upload/start" && r.Method == "POST":
		var data struct {
			Size   int64  `json:"size"`
			Header string `json:"header"`
			Seal   string `json:"seal"`
		}
		if !decodeBody(w, r, &data) {
			return
		}
		if data.Size < 141 || data.Size > updateUploadLimit || !hexField(data.Header, 56) || !hexField(data.Seal, 192) {
			fail(w, 400, "UPDATE_SIGNATURE_INVALID")
			return
		}
		request.Action, request.Size, request.Header, request.Seal = "start", data.Size, data.Header, data.Seal
	case r.URL.Path == "/api/software-update/upload/chunk" && r.Method == "POST":
		offset, err := strconv.ParseInt(r.Header.Get("Upload-Offset"), 10, 64)
		id := r.Header.Get("Upload-ID")
		if r.Header.Get("Content-Type") != "application/octet-stream" || r.ContentLength < 1 || r.ContentLength > updateChunkLimit || err != nil || offset < 0 || offset > updateUploadLimit-r.ContentLength || !updateTicket.MatchString(id) {
			fail(w, 400, "INVALID_UPDATE_PACKAGE")
			return
		}
		controller := http.NewResponseController(w)
		_ = controller.SetReadDeadline(time.Now().Add(60 * time.Second))
		_ = controller.SetWriteDeadline(time.Now().Add(70 * time.Second))
		request.Action, request.Size, request.UploadID, request.Offset = "chunk", r.ContentLength, id, &offset
		input = http.MaxBytesReader(w, r.Body, updateChunkLimit)
	case (r.URL.Path == "/api/software-update/upload/finish" || r.URL.Path == "/api/software-update/upload/cancel") && r.Method == "POST":
		var data struct {
			UploadID string `json:"uploadId"`
		}
		if !decodeBody(w, r, &data) {
			return
		}
		if !updateTicket.MatchString(data.UploadID) {
			fail(w, 400, "UPDATE_CONFLICT")
			return
		}
		request.Action = "finish"
		if r.URL.Path == "/api/software-update/upload/cancel" {
			request.Action = "cancel"
		}
		request.UploadID = data.UploadID
	case r.URL.Path == "/api/software-update/upload":
		fail(w, 415, "UPDATE_SIGNATURE_REQUIRED")
		return
	case r.URL.Path == "/api/software-update/apply" && r.Method == "POST":
		var data struct {
			Ticket string `json:"ticket"`
		}
		if !decodeBody(w, r, &data) {
			return
		}
		if !updateTicket.MatchString(data.Ticket) {
			fail(w, 400, "INVALID_UPDATE_PACKAGE")
			return
		}
		request.Action, request.Ticket = "apply", data.Ticket
	default:
		fail(w, 405, "METHOD_NOT_ALLOWED")
		return
	}
	call := s.updateCall
	if call == nil {
		call = localUpdateCall
	}
	result, err := call(ctx, request, input)
	if err != nil {
		code, status := err.Error(), 503
		switch code {
		case "UPDATE_BUSY", "UPDATE_CONFLICT", "UPDATE_CHUNK_INCOMPLETE":
			status = 409
		case "INVALID_UPDATE_PACKAGE", "UPDATE_DOWNGRADE", "UPDATE_STORAGE_LOW", "UPDATE_SIGNATURE_INVALID", "UPDATE_DECRYPT_FAILED":
			status = 400
		case "UPDATE_KEY_UNAVAILABLE":
		default:
			code = "UPDATE_UNAVAILABLE"
		}
		fail(w, status, code)
		return
	}
	reply(w, 200, map[string]any{"data": result})
}

func hexField(value string, size int) bool {
	if len(value) != size {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
