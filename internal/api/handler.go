// Package api exposes the authenticated trader control surface. It never owns
// execution state; commands pass to the injected account authority unchanged.
package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	d "github.com/liquiditylab/liquiditylab/internal/domain"
	x "github.com/liquiditylab/liquiditylab/internal/execution"
	rt "github.com/liquiditylab/liquiditylab/internal/runtime"
	pg "github.com/liquiditylab/liquiditylab/internal/store/postgres"
)

// Authority is implemented by *runtime.Actor. Implementations must honor ctx;
// accepted commands may nevertheless finish durably after caller cancellation.
type Authority interface {
	View(context.Context) (rt.View, error)
	Do(context.Context, rt.Command) (pg.Result, error)
}
type JournalReader interface {
	Journal(context.Context, pg.Key, int64, int) ([]pg.JournalEntry, error)
}

// Credential is an explicit static bearer credential. Scopes are exact pairs;
// there are no implicit tenant-wide or administrator grants.
type Credential struct {
	Token  string
	Scopes []pg.Key
}
type Config struct {
	Accounts       map[pg.Key]Authority
	Credentials    []Credential
	Journal        JournalReader
	Sessions       SessionReader
	MaxBodyBytes   int64
	RequestTimeout time.Duration
	MaxConcurrent  int
}
type credential struct {
	digest [32]byte
	scopes map[pg.Key]bool
}
type Handler struct {
	accounts    map[pg.Key]Authority
	credentials []credential
	journal     JournalReader
	sessions    SessionReader
	maxBody     int64
	timeout     time.Duration
	slots       chan struct{}
}

var errConfig = errors.New("invalid trader API configuration")

// New copies configuration and retains only SHA-256 credential digests. The
// caller owns listener security, HTTP server timeouts, and authority lifecycle.
func New(c Config) (*Handler, error) {
	if len(c.Accounts) == 0 || len(c.Accounts) > 256 || len(c.Credentials) == 0 || len(c.Credentials) > 256 || c.Journal == nil {
		return nil, errConfig
	}
	if c.MaxBodyBytes == 0 {
		c.MaxBodyBytes = 16 << 10
	}
	if c.RequestTimeout == 0 {
		c.RequestTimeout = 5 * time.Second
	}
	if c.MaxConcurrent == 0 {
		c.MaxConcurrent = 64
	}
	if c.MaxBodyBytes < 256 || c.MaxBodyBytes > 1<<20 || c.RequestTimeout < time.Millisecond || c.RequestTimeout > 30*time.Second || c.MaxConcurrent < 1 || c.MaxConcurrent > 1024 {
		return nil, errConfig
	}
	h := &Handler{accounts: make(map[pg.Key]Authority, len(c.Accounts)), journal: c.Journal, maxBody: c.MaxBodyBytes, timeout: c.RequestTimeout, slots: make(chan struct{}, c.MaxConcurrent)}
	for k, a := range c.Accounts {
		if !identifier(k.TenantID) || !identifier(k.AccountID) || a == nil {
			return nil, errConfig
		}
		h.accounts[k] = a
	}
	h.sessions = c.Sessions
	if h.sessions == nil {
		h.sessions, _ = c.Journal.(SessionReader)
	}
	seen := map[[32]byte]bool{}
	for _, v := range c.Credentials {
		if len(v.Token) < 32 || len(v.Token) > 4096 || strings.ContainsAny(v.Token, " \t\r\n") || len(v.Scopes) == 0 || len(v.Scopes) > 256 {
			return nil, errConfig
		}
		hash := sha256.Sum256([]byte(v.Token))
		if seen[hash] {
			return nil, errConfig
		}
		seen[hash] = true
		out := credential{digest: hash, scopes: map[pg.Key]bool{}}
		for _, k := range v.Scopes {
			if _, ok := h.accounts[k]; !ok {
				return nil, errConfig
			}
			out.scopes[k] = true
		}
		h.credentials = append(h.credentials, out)
	}
	return h, nil
}
func identifier(s string) bool {
	if len(s) == 0 || len(s) > 128 || s == "." || s == ".." {
		return false
	}
	for _, v := range s {
		if !(v >= 'a' && v <= 'z' || v >= 'A' && v <= 'Z' || v >= '0' && v <= '9' || v == '-' || v == '_' || v == '.') {
			return false
		}
	}
	return true
}
func (h *Handler) authenticate(r *http.Request) *credential {
	headers := r.Header.Values("Authorization")
	if len(headers) != 1 || !strings.HasPrefix(headers[0], "Bearer ") || len(headers[0]) > 4103 {
		return nil
	}
	token := strings.TrimPrefix(headers[0], "Bearer ")
	if token == "" || strings.ContainsAny(token, " \t\r\n") {
		return nil
	}
	hash := sha256.Sum256([]byte(token))
	found := -1
	for i := range h.credentials {
		if subtle.ConstantTimeCompare(hash[:], h.credentials[i].digest[:]) == 1 {
			found = i
		}
	}
	if found < 0 {
		return nil
	}
	return &h.credentials[found]
}
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.URL.Path == "/healthz" {
		if !method(w, r, http.MethodGet) {
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
		return
	}
	auth := h.authenticate(r)
	if auth == nil {
		w.Header().Set("WWW-Authenticate", `Bearer realm="liquiditylab"`)
		fail(w, 401, "unauthorized")
		return
	}
	select {
	case h.slots <- struct{}{}:
		defer func() { <-h.slots }()
	default:
		w.Header().Set("Retry-After", "1")
		fail(w, 503, "busy")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.timeout)
	defer cancel()
	r = r.WithContext(ctx)
	if r.URL.Path == "/v1/accounts" {
		if !method(w, r, http.MethodGet) {
			return
		}
		if r.URL.RawQuery != "" {
			fail(w, 400, "invalid_query")
			return
		}
		h.list(w, r, auth)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if len(parts) < 4 || len(parts) > 7 || parts[0] != "v1" || parts[1] != "accounts" || !identifier(parts[2]) || !identifier(parts[3]) {
		fail(w, 404, "not_found")
		return
	}
	key := pg.Key{TenantID: parts[2], AccountID: parts[3]}
	authority, exists := h.accounts[key]
	if !auth.scopes[key] || !exists {
		fail(w, 404, "not_found")
		return
	}
	if len(parts) >= 5 && parts[4] == "sessions" {
		h.sessionsEndpoint(w, r, key, parts)
		return
	}
	if len(parts) > 5 {
		fail(w, 404, "not_found")
		return
	}
	endpoint := ""
	if len(parts) == 5 {
		endpoint = parts[4]
	}
	switch endpoint {
	case "":
		if !method(w, r, http.MethodGet) {
			return
		}
		if !noQuery(w, r) {
			return
		}
		view, err := authority.View(ctx)
		if err != nil {
			backendError(w, err)
			return
		}
		if view.Account.Key != key {
			fail(w, 503, "unavailable")
			return
		}
		dto, err := project(view)
		if err != nil {
			backendError(w, err)
			return
		}
		writeJSON(w, 200, dto)
	case "commands":
		if !method(w, r, http.MethodPost) || !noQuery(w, r) {
			return
		}
		h.command(w, r, authority)
	case "preview":
		if !method(w, r, http.MethodPost) || !noQuery(w, r) {
			return
		}
		h.preview(w, r, key, authority)
	case "journal":
		if !method(w, r, http.MethodGet) {
			return
		}
		h.history(w, r, key)
	default:
		fail(w, 404, "not_found")
	}
}
func noQuery(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.RawQuery != "" {
		fail(w, 400, "invalid_query")
		return false
	}
	return true
}
func method(w http.ResponseWriter, r *http.Request, allowed string) bool {
	if r.Method != allowed {
		w.Header().Set("Allow", allowed)
		fail(w, 405, "method_not_allowed")
		return false
	}
	return true
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, struct {
		Error string `json:"error"`
	}{code})
}
func backendError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, context.Canceled):
		fail(w, 408, "request_cancelled")
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, rt.ErrOutcomeUnknown):
		fail(w, 504, "outcome_unknown")
	case errors.Is(err, pg.ErrNotFound):
		fail(w, 404, "not_found")
	case errors.Is(err, pg.ErrStaleRevision):
		fail(w, 409, "stale_revision")
	case errors.Is(err, pg.ErrConflict):
		fail(w, 409, "idempotency_conflict")
	case errors.Is(err, pg.ErrInsufficientFunds):
		fail(w, 409, "insufficient_funds")
	case errors.Is(err, pg.ErrExpired):
		fail(w, 409, "expired")
	case errors.Is(err, rt.ErrInvalid), errors.Is(err, pg.ErrInvalid):
		fail(w, 400, "invalid_command")
	case errors.Is(err, rt.ErrRecoveryRequired), errors.Is(err, pg.ErrOwnership), errors.Is(err, pg.ErrNotReady), errors.Is(err, pg.ErrIsolationRequired):
		fail(w, 409, "recovery_required")
	case errors.Is(err, rt.ErrMailboxFull), errors.Is(err, rt.ErrClosed), errors.Is(err, rt.ErrHistoryLimit), errors.Is(err, pg.ErrInboxFull):
		w.Header().Set("Retry-After", "1")
		fail(w, 503, "unavailable")
	default:
		fail(w, 503, "unavailable")
	}
}
func (h *Handler) decode(w http.ResponseWriter, r *http.Request, v any) bool {
	content, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || content != "application/json" {
		fail(w, 415, "json_required")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, h.maxBody)
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err = dec.Decode(v); err == nil {
		var extra any
		if nextErr := dec.Decode(&extra); nextErr != io.EOF {
			if nextErr != nil {
				err = nextErr
			} else {
				err = errors.New("trailing JSON")
			}
		}
	}
	if err != nil {
		var large *http.MaxBytesError
		if errors.As(err, &large) {
			fail(w, 413, "body_too_large")
		} else {
			fail(w, 400, "invalid_json")
		}
		return false
	}
	return true
}

type commandRequest struct {
	ID                        string  `json:"id"`
	ExpectedRevision          *int64  `json:"expected_revision"`
	ExpectedAuthorityRevision *int64  `json:"expected_authority_revision,omitempty"`
	Kind                      string  `json:"kind"`
	PlanID                    string  `json:"plan_id,omitempty"`
	Plan                      *d.Plan `json:"plan,omitempty"`
	Policy                    string  `json:"policy,omitempty"`
}

func publicID(id string) bool {
	if !identifier(id) {
		return false
	}
	for _, p := range []string{"boot-", "deadline-", "dispatch-market-", "market-invalid-", "clock-invalid-", "observation-", "paper-", "transport-", "runtime-"} {
		if strings.HasPrefix(id, p) {
			return false
		}
	}
	return true
}
func validPlan(p d.Plan) bool { return p.Validate() == nil && p.Deadline <= int64(24*time.Hour) }
func (h *Handler) command(w http.ResponseWriter, r *http.Request, a Authority) {
	var body commandRequest
	if !h.decode(w, r, &body) {
		return
	}
	if !publicID(body.ID) || body.ExpectedRevision == nil || *body.ExpectedRevision < 0 || body.ExpectedAuthorityRevision != nil && *body.ExpectedAuthorityRevision < 0 {
		fail(w, 400, "invalid_command")
		return
	}
	switch body.Kind {
	case "create":
		if body.Plan == nil || !publicID(body.PlanID) || !validPlan(*body.Plan) {
			fail(w, 400, "invalid_command")
			return
		}
		if body.Policy != "" && x.ValidateBaselinePolicy(body.Policy) != nil {
			fail(w, 400, "invalid_policy")
			return
		}
	case "start", "pause", "resume", "cancel":
		if body.Plan != nil || body.PlanID != "" || body.Policy != "" {
			fail(w, 400, "invalid_command")
			return
		}
	default:
		fail(w, 400, "command_not_allowed")
		return
	}
	result, err := a.Do(r.Context(), rt.Command{ID: body.ID, ExpectedRevision: *body.ExpectedRevision, ExpectedAuthorityRevision: body.ExpectedAuthorityRevision, Kind: body.Kind, PlanID: body.PlanID, Plan: body.Plan, Policy: body.Policy})
	if err != nil {
		backendError(w, err)
		return
	}
	writeJSON(w, 200, result)
}
func (h *Handler) list(w http.ResponseWriter, r *http.Request, auth *credential) {
	keys := make([]pg.Key, 0, len(auth.scopes))
	for key := range auth.scopes {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].TenantID != keys[j].TenantID {
			return keys[i].TenantID < keys[j].TenantID
		}
		return keys[i].AccountID < keys[j].AccountID
	})
	accounts := make([]AccountSummary, 0, len(keys))
	for _, key := range keys {
		view, err := h.accounts[key].View(r.Context())
		if err != nil {
			backendError(w, err)
			return
		}
		if view.Account.Key != key {
			fail(w, 503, "unavailable")
			return
		}
		accounts = append(accounts, summary(view))
	}
	writeJSON(w, 200, struct {
		Accounts []AccountSummary `json:"accounts"`
	}{accounts})
}
func (h *Handler) history(w http.ResponseWriter, r *http.Request, key pg.Key) {
	values, err := parseJournalQuery(r)
	if err != nil {
		fail(w, 400, "invalid_query")
		return
	}
	entries, err := h.journal.Journal(r.Context(), key, values.after, values.limit)
	if err != nil {
		backendError(w, err)
		return
	}
	if len(entries) > values.limit {
		fail(w, 503, "unavailable")
		return
	}
	if entries == nil {
		entries = []pg.JournalEntry{}
	}
	next := values.after
	for _, entry := range entries {
		if entry.Revision <= next {
			fail(w, 503, "unavailable")
			return
		}
		next = entry.Revision
	}
	writeJSON(w, 200, struct {
		Entries []pg.JournalEntry `json:"entries"`
		Next    int64             `json:"next_after_revision"`
		Limit   int               `json:"limit"`
	}{entries, next, values.limit})
}

type journalQuery struct {
	after int64
	limit int
}

func parseJournalQuery(r *http.Request) (journalQuery, error) {
	result := journalQuery{limit: 100}
	values, err := url.ParseQuery(r.URL.RawQuery)
	// ParseQuery rejects malformed escaping instead of silently discarding it.
	if err != nil {
		return result, err
	}
	for key, v := range values {
		if len(v) != 1 {
			return result, pg.ErrInvalid
		}
		switch key {
		case "after_revision":
			result.after, err = strconv.ParseInt(v[0], 10, 64)
		case "limit":
			result.limit, err = strconv.Atoi(v[0])
		default:
			return result, pg.ErrInvalid
		}
		if err != nil {
			return result, err
		}
	}
	if result.after < 0 || result.limit < 1 || result.limit > 200 {
		return result, pg.ErrInvalid
	}
	return result, nil
}
