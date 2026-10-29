package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	pg "github.com/liquiditylab/liquiditylab/internal/store/postgres"
)

// ReplayService accepts only analytical inputs in the authenticated account
// scope. It does not expose a financial authority or command interface.
type ReplayService interface {
	Enqueue(context.Context, pg.Key, string, string, json.RawMessage) (pg.ReplayJob, bool, error)
	Get(context.Context, pg.Key, string) (pg.ReplayJob, error)
	List(context.Context, pg.Key, int64, int) ([]pg.ReplayJob, error)
	Cancel(context.Context, pg.Key, string) (pg.ReplayJob, error)
}

// ReplayJobDTO deliberately excludes worker identity and lease internals.
// Inputs and results appear only on the individual GET endpoint.
type ReplayJobDTO struct {
	TenantID     string    `json:"tenant_id"`
	AccountID    string    `json:"account_id"`
	ID           string    `json:"id"`
	Sequence     int64     `json:"sequence"`
	Status       string    `json:"status"`
	Attempts     int       `json:"attempts"`
	InputSHA256  string    `json:"input_sha256"`
	ResultSHA256 string    `json:"result_sha256,omitempty"`
	ErrorCode    string    `json:"error_code,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type ReplayDetailDTO struct {
	ReplayJobDTO
	Input json.RawMessage `json:"input"`
	// The JSON encoder may compact this embedded value. ResultSHA256 identifies
	// the original bytes available from the individual /result download.
	Result json.RawMessage `json:"result"`
}

func (h *Handler) serveReplay(w http.ResponseWriter, r *http.Request, auth *credential) bool {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if len(parts) < 5 || parts[0] != "v1" || parts[1] != "accounts" || parts[4] != "replays" {
		return false
	}
	if len(parts) > 7 || !identifier(parts[2]) || !identifier(parts[3]) {
		fail(w, 404, "not_found")
		return true
	}
	key := pg.Key{TenantID: parts[2], AccountID: parts[3]}
	if _, exists := h.accounts[key]; !exists || !auth.scopes[key] || h.replays == nil {
		fail(w, 404, "not_found")
		return true
	}
	select {
	case h.replaySlots <- struct{}{}:
		defer func() { <-h.replaySlots }()
	default:
		w.Header().Set("Retry-After", "1")
		fail(w, 503, "busy")
		return true
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.timeout)
	defer cancel()
	h.replaysEndpoint(w, r.WithContext(ctx), key, parts)
	return true
}

func (h *Handler) replaysEndpoint(w http.ResponseWriter, r *http.Request, key pg.Key, parts []string) {
	if len(parts) == 5 {
		switch r.Method {
		case http.MethodGet:
			h.listReplays(w, r, key)
		case http.MethodPost:
			if !noQuery(w, r) {
				return
			}
			h.enqueueReplay(w, r, key)
		default:
			w.Header().Set("Allow", "GET, POST")
			fail(w, 405, "method_not_allowed")
		}
		return
	}
	if !identifier(parts[5]) || len(parts) == 7 && parts[6] != "cancel" && parts[6] != "result" {
		fail(w, 404, "not_found")
		return
	}
	allowed := http.MethodGet
	if len(parts) == 7 && parts[6] == "cancel" {
		allowed = http.MethodPost
	}
	if !method(w, r, allowed) || !noQuery(w, r) {
		return
	}
	if len(parts) == 7 && parts[6] == "result" {
		h.downloadReplayResult(w, r, key, parts[5])
		return
	}
	if len(parts) == 7 {
		// A pointer distinguishes the required empty object from JSON null.
		var body *struct{}
		if !h.decodeReplay(w, r, &body) {
			return
		}
		if body == nil {
			fail(w, 400, "invalid_replay")
			return
		}
		job, err := h.replays.Cancel(r.Context(), key, parts[5])
		if err != nil {
			replayBackendError(w, err)
			return
		}
		dto, err := projectReplay(job, key, parts[5])
		if err != nil {
			fail(w, 503, "unavailable")
			return
		}
		writeJSON(w, 200, dto)
		return
	}
	job, err := h.replays.Get(r.Context(), key, parts[5])
	if err != nil {
		replayBackendError(w, err)
		return
	}
	dto, err := projectReplay(job, key, parts[5])
	if err != nil || !replayObject(job.Input, pg.ReplayMaxInputBytes) || len(job.Result) != 0 && !replayObject(job.Result, pg.ReplayMaxResultBytes) {
		fail(w, 503, "unavailable")
		return
	}
	writeJSON(w, 200, ReplayDetailDTO{ReplayJobDTO: dto, Input: job.Input, Result: job.Result})
}

func (h *Handler) downloadReplayResult(w http.ResponseWriter, r *http.Request, key pg.Key, id string) {
	if r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
		fail(w, 400, "invalid_replay")
		return
	}
	job, err := h.replays.Get(r.Context(), key, id)
	if err != nil {
		replayBackendError(w, err)
		return
	}
	if _, err := projectReplay(job, key, id); err != nil {
		fail(w, 503, "unavailable")
		return
	}
	switch job.Status {
	case "queued", "running":
		fail(w, 409, "result_not_ready")
		return
	case "failed", "cancelled":
		fail(w, 409, "result_unavailable")
		return
	}
	if !replayObject(job.Result, pg.ReplayMaxResultBytes) {
		fail(w, 503, "unavailable")
		return
	}
	digest := sha256.Sum256(job.Result)
	if job.ResultSHA256 != hex.EncodeToString(digest[:]) {
		fail(w, 503, "unavailable")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": id + ".json"}))
	w.Header().Set("Content-Length", strconv.Itoa(len(job.Result)))
	w.Header().Set("X-Content-SHA256", job.ResultSHA256)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(job.Result)
}

func (h *Handler) decodeReplay(w http.ResponseWriter, r *http.Request, v any) bool {
	// Copy only decoder configuration: never mutate shared handler limits.
	bounded := Handler{maxBody: min(h.maxBody, int64(64<<10))}
	return bounded.decode(w, r, v)
}

func (h *Handler) enqueueReplay(w http.ResponseWriter, r *http.Request, key pg.Key) {
	var body struct {
		ID        string          `json:"id"`
		ArchiveID string          `json:"archive_id"`
		Spec      json.RawMessage `json:"spec"`
	}
	if !h.decodeReplay(w, r, &body) {
		return
	}
	if !identifier(body.ID) || !identifier(body.ArchiveID) || !replayObject(body.Spec, pg.ReplayMaxInputBytes) {
		fail(w, 400, "invalid_replay")
		return
	}
	job, duplicate, err := h.replays.Enqueue(r.Context(), key, body.ID, body.ArchiveID, body.Spec)
	if err != nil {
		replayBackendError(w, err)
		return
	}
	dto, err := projectReplay(job, key, body.ID)
	if err != nil {
		fail(w, 503, "unavailable")
		return
	}
	status := http.StatusAccepted
	if duplicate {
		status = http.StatusOK
	}
	writeJSON(w, status, struct {
		Job       ReplayJobDTO `json:"job"`
		Duplicate bool         `json:"duplicate"`
	}{dto, duplicate})
}

func (h *Handler) listReplays(w http.ResponseWriter, r *http.Request, key pg.Key) {
	q, err := strictHistoryQuery(r.URL.RawQuery, map[string]int64{"before_sequence": 0, "limit": 20})
	if err != nil || q["limit"] < 1 || q["limit"] > 50 {
		fail(w, 400, "invalid_query")
		return
	}
	jobs, err := h.replays.List(r.Context(), key, q["before_sequence"], int(q["limit"]))
	if err != nil {
		replayBackendError(w, err)
		return
	}
	if len(jobs) > int(q["limit"]) {
		fail(w, 503, "unavailable")
		return
	}
	entries := make([]ReplayJobDTO, 0, len(jobs))
	next := q["before_sequence"]
	for _, job := range jobs {
		dto, err := projectReplay(job, key, "")
		if err != nil || next != 0 && job.Sequence >= next {
			fail(w, 503, "unavailable")
			return
		}
		entries = append(entries, dto)
		next = job.Sequence
	}
	writeJSON(w, 200, struct {
		Entries []ReplayJobDTO `json:"entries"`
		Next    int64          `json:"next_before_sequence"`
		Limit   int64          `json:"limit"`
	}{entries, next, q["limit"]})
}

func replayObject(raw json.RawMessage, limit int) bool {
	b := bytes.TrimSpace(raw)
	return len(raw) > 0 && len(raw) <= limit && len(b) > 0 && b[0] == '{' && json.Valid(b)
}
func replayDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
func projectReplay(job pg.ReplayJob, key pg.Key, id string) (ReplayJobDTO, error) {
	if job.Key != key || !identifier(job.ID) || id != "" && job.ID != id || job.Sequence < 1 || job.Attempts < 0 || job.Attempts > pg.ReplayMaxAttempts || !replayDigest(job.InputSHA256) || job.ResultSHA256 != "" && !replayDigest(job.ResultSHA256) {
		return ReplayJobDTO{}, pg.ErrInvalid
	}
	switch job.Status {
	case "queued", "running", "succeeded", "failed", "cancelled":
	default:
		return ReplayJobDTO{}, pg.ErrInvalid
	}
	switch job.ErrorCode {
	case "", "input_invalid", "archive_unavailable", "execution_failed", "deadline_exceeded", "worker_lost":
	default:
		return ReplayJobDTO{}, pg.ErrInvalid
	}
	return ReplayJobDTO{TenantID: key.TenantID, AccountID: key.AccountID, ID: job.ID, Sequence: job.Sequence, Status: job.Status, Attempts: job.Attempts, InputSHA256: job.InputSHA256, ResultSHA256: job.ResultSHA256, ErrorCode: job.ErrorCode, CreatedAt: job.CreatedAt, UpdatedAt: job.UpdatedAt}, nil
}
func replayBackendError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, pg.ErrInvalid):
		fail(w, 400, "invalid_replay")
	case errors.Is(err, pg.ErrReplayBusy):
		w.Header().Set("Retry-After", "1")
		fail(w, 503, "busy")
	default:
		backendError(w, err)
	}
}
