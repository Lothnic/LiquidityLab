package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	pg "github.com/liquiditylab/liquiditylab/internal/store/postgres"
)

const replayPath = pathA + "/replays"
const replayArchive = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// The service owns historical spec semantics; these tests isolate HTTP behavior.
const replayBody = `{"id":"job-1","archive_id":"` + replayArchive + `","spec":{"schema":"explicit-spec"}}`

type fakeReplays struct {
	mu        sync.Mutex
	job       pg.ReplayJob
	jobs      []pg.ReplayJob
	err       error
	duplicate bool
	calls     int
	kind      string
	key       pg.Key
	id        string
	archive   string
	spec      json.RawMessage
	before    int64
	limit     int
	onGet     func(context.Context, pg.Key, string) (pg.ReplayJob, error)
}

func (f *fakeReplays) called(kind string, key pg.Key, id string) {
	f.calls++
	f.kind, f.key, f.id = kind, key, id
}
func (f *fakeReplays) Enqueue(_ context.Context, key pg.Key, id, archive string, spec json.RawMessage) (pg.ReplayJob, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.called("enqueue", key, id)
	f.archive, f.spec = archive, append(json.RawMessage{}, spec...)
	return f.job, f.duplicate, f.err
}
func (f *fakeReplays) Get(ctx context.Context, key pg.Key, id string) (pg.ReplayJob, error) {
	f.mu.Lock()
	f.called("get", key, id)
	job, err, callback := f.job, f.err, f.onGet
	f.mu.Unlock()
	if callback != nil {
		return callback(ctx, key, id)
	}
	return job, err
}
func (f *fakeReplays) List(_ context.Context, key pg.Key, before int64, limit int) ([]pg.ReplayJob, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.called("list", key, "")
	f.before, f.limit = before, limit
	return f.jobs, f.err
}
func (f *fakeReplays) Cancel(_ context.Context, key pg.Key, id string) (pg.ReplayJob, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.called("cancel", key, id)
	return f.job, f.err
}
func replaySample() pg.ReplayJob {
	input, result := json.RawMessage(`{"spec":"explicit-input"}`), json.RawMessage(`{"report":"result"}`)
	i, r := sha256.Sum256(input), sha256.Sum256(result)
	now := time.Unix(100, 0).UTC()
	return pg.ReplayJob{Key: keyA, ID: "job-1", Sequence: 9, Status: "succeeded", Attempts: 1, Input: input, Result: result, InputSHA256: hex.EncodeToString(i[:]), ResultSHA256: hex.EncodeToString(r[:]), WorkerID: "private-worker", LeaseUntil: &now, CreatedAt: now, UpdatedAt: now}
}
func setupReplays(t *testing.T) (*Handler, *fakeReplays, *fakeAuthority) {
	t.Helper()
	h, a, _ := setup(t)
	f := &fakeReplays{job: replaySample(), jobs: []pg.ReplayJob{replaySample()}}
	h.replays = f
	return h, f, a
}
func TestReplayRoutesAndProjection(t *testing.T) {
	h, f, a := setupReplays(t)
	w := request(h, "POST", replayPath, tokenA, replayBody)
	if w.Code != 202 || !strings.Contains(w.Body.String(), `"duplicate":false`) || f.key != keyA || f.id != "job-1" || f.archive != replayArchive || string(f.spec) != `{"schema":"explicit-spec"}` {
		t.Fatalf("enqueue: %d %s %+v", w.Code, w.Body, f)
	}
	f.duplicate = true
	w = request(h, "POST", replayPath, tokenA, replayBody)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"duplicate":true`) {
		t.Fatal(w.Code, w.Body)
	}
	for _, tt := range []struct{ method, path, body, kind string }{{"GET", replayPath + "/job-1", "", "get"}, {"GET", replayPath + "?before_sequence=10&limit=1", "", "list"}, {"POST", replayPath + "/job-1/cancel", `{}`, "cancel"}} {
		w = request(h, tt.method, tt.path, tokenA, tt.body)
		if w.Code != 200 || f.kind != tt.kind || f.key != keyA {
			t.Fatal(tt.path, w.Code, w.Body)
		}
		for _, private := range []string{"private-worker", "worker_id", "lease_until"} {
			if strings.Contains(w.Body.String(), private) {
				t.Fatalf("leaked %s: %s", private, w.Body)
			}
		}
		if tt.kind == "get" {
			if !strings.Contains(w.Body.String(), `"input":{"spec":"explicit-input"}`) || !strings.Contains(w.Body.String(), `"result":{"report":"result"}`) {
				t.Fatal(w.Body)
			}
		} else if strings.Contains(w.Body.String(), `"input":`) || strings.Contains(w.Body.String(), `"result":`) {
			t.Fatal("metadata included blobs", w.Body)
		}
	}
	if f.before != 10 || f.limit != 1 || a.calls != 0 {
		t.Fatal("wrong pagination or financial mutation")
	}
	f.jobs = nil
	w = request(h, "GET", replayPath, tokenA, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"entries":[]`) || f.before != 0 || f.limit != 20 {
		t.Fatal(w.Code, w.Body)
	}
	f.job.Result = nil
	w = request(h, "GET", replayPath+"/job-1", tokenA, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"result":null`) {
		t.Fatal(w.Code, w.Body)
	}
}
func TestReplayAuthenticationScopeAndDisabledBackend(t *testing.T) {
	h, f, a := setupReplays(t)
	for _, tt := range []struct {
		method, path, token string
		status              int
	}{
		{"GET", replayPath, "", 401}, {"GET", replayPath, "wrong", 401},
		{"GET", replayPath, tokenB, 404}, {"POST", replayPath, tokenB, 404},
		{"GET", replayPath + "/job-1", tokenB, 404}, {"POST", replayPath + "/job-1/cancel", tokenB, 404},
		{"GET", "/v1/accounts/tenant-a/missing/replays", tokenA, 404},
		{"GET", replayPath + "/job-1/unknown", tokenA, 404}, {"GET", replayPath + "/job-1/cancel/extra", tokenA, 404},
		{"GET", replayPath + "/", tokenA, 404},
	} {
		w := request(h, tt.method, tt.path, tt.token, `{}`)
		if w.Code != tt.status || f.calls != 0 || a.calls != 0 {
			t.Fatalf("%s %s: %d %s", tt.method, tt.path, w.Code, w.Body)
		}
	}
	h.replays = nil
	for _, path := range []string{replayPath, replayPath + "/job-1", replayPath + "/job-1/cancel"} {
		w := request(h, "POST", path, tokenA, `{}`)
		if w.Code != 404 || f.calls != 0 {
			t.Fatal(w.Code, w.Body)
		}
	}
}
func TestReplayStrictRequests(t *testing.T) {
	h, f, a := setupReplays(t)
	for _, tt := range []struct{ method, path, allow string }{{"DELETE", replayPath, "GET, POST"}, {"POST", replayPath + "/job-1", "GET"}, {"GET", replayPath + "/job-1/cancel", "POST"}} {
		w := request(h, tt.method, tt.path, tokenA, `{}`)
		if w.Code != 405 || w.Header().Get("Allow") != tt.allow {
			t.Fatal(w.Code, w.Header(), w.Body)
		}
	}
	for _, query := range []string{"limit=0", "limit=51", "limit=", "limit=-1", "limit=1.0", "limit=1e1", "limit=%2B1", "limit=1&limit=1", "before_sequence=-1", "before_sequence=9223372036854775808", "before_sequence=1&unknown=1", "limit=%zz", "limit=1;foo=2"} {
		w := request(h, "GET", replayPath+"?"+query, tokenA, "")
		if w.Code != 400 {
			t.Fatalf("query %s: %d %s", query, w.Code, w.Body)
		}
	}
	for _, body := range []string{`null`, `{}`, `[]`, `{`, replayBody + `{}`, strings.TrimSuffix(replayBody, "}") + `,"unknown":true}`, `{"id":"job-1","archive_id":"a","spec":null}`, `{"id":"job-1","archive_id":"a","spec":[]}`, `{"id":"../job","archive_id":"a","spec":{}}`, `{"id":"job-1","archive_id":"../a","spec":{}}`} {
		w := request(h, "POST", replayPath, tokenA, body)
		if w.Code != 400 {
			t.Fatalf("body %s: %d %s", body, w.Code, w.Body)
		}
	}
	for _, body := range []string{`null`, `[]`, `{"id":"job-1"}`, `{}` + `{}`, ``} {
		w := request(h, "POST", replayPath+"/job-1/cancel", tokenA, body)
		if w.Code != 400 {
			t.Fatal(body, w.Code, w.Body)
		}
	}
	for _, tt := range []struct{ method, path, body string }{{"POST", replayPath + "?limit=1", replayBody}, {"GET", replayPath + "/job-1?limit=1", ""}, {"POST", replayPath + "/job-1/cancel?limit=1", `{}`}} {
		w := request(h, tt.method, tt.path, tokenA, tt.body)
		if w.Code != 400 {
			t.Fatal(w.Code, w.Body)
		}
	}
	r := httptest.NewRequest("POST", replayPath, strings.NewReader(replayBody))
	r.Header.Set("Authorization", "Bearer "+tokenA)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 415 || f.calls != 0 || a.calls != 0 {
		t.Fatal(w.Code, f.calls, a.calls)
	}
}
func TestReplayRequestBodyBounds(t *testing.T) {
	h, f, _ := setupReplays(t)
	for _, size := range []int64{256, 16 << 10, 1 << 20} {
		h.maxBody = size
		limit := min(size, 64<<10)
		w := request(h, "POST", replayPath, tokenA, replayBody+strings.Repeat(" ", int(limit)))
		if w.Code != 413 || f.calls != 0 {
			t.Fatal(size, w.Code, w.Body)
		}
		w = request(h, "POST", replayPath+"/job-1/cancel", tokenA, `{}`+strings.Repeat(" ", int(limit)))
		if w.Code != 413 || f.calls != 0 {
			t.Fatal(size, w.Code, w.Body)
		}
	}
}
func TestReplayBackendErrorsAndProjectionBounds(t *testing.T) {
	h, f, _ := setupReplays(t)
	for _, tt := range []struct {
		err    error
		status int
		code   string
	}{{pg.ErrInvalid, 400, "invalid_replay"}, {pg.ErrConflict, 409, "idempotency_conflict"}, {pg.ErrReplayBusy, 503, "busy"}, {pg.ErrReplayConfig, 503, "unavailable"}, {pg.ErrNotFound, 404, "not_found"}, {context.Canceled, 408, "request_cancelled"}, {context.DeadlineExceeded, 504, "outcome_unknown"}, {errors.New("secret: password path"), 503, "unavailable"}} {
		f.err = tt.err
		for _, route := range []struct{ method, path, body string }{{"POST", replayPath, replayBody}, {"GET", replayPath, ""}, {"GET", replayPath + "/job-1", ""}, {"POST", replayPath + "/job-1/cancel", `{}`}} {
			w := request(h, route.method, route.path, tokenA, route.body)
			if w.Code != tt.status || strings.TrimSpace(w.Body.String()) != fmt.Sprintf(`{"error":%q}`, tt.code) {
				t.Fatal(route.path, w.Code, w.Body)
			}
		}
	}
	f.err = nil
	for _, mutate := range []func(*pg.ReplayJob){func(j *pg.ReplayJob) { j.Key = keyB }, func(j *pg.ReplayJob) { j.ID = "other" }, func(j *pg.ReplayJob) { j.Sequence = 0 }, func(j *pg.ReplayJob) { j.Attempts = 4 }, func(j *pg.ReplayJob) { j.Status = "secret-status" }, func(j *pg.ReplayJob) { j.ErrorCode = "secret-error" }, func(j *pg.ReplayJob) { j.InputSHA256 = "secret-digest" }, func(j *pg.ReplayJob) { j.Input = nil }, func(j *pg.ReplayJob) { j.Input = json.RawMessage(`[]`) }, func(j *pg.ReplayJob) {
		j.Result = json.RawMessage(`{"secret":"` + strings.Repeat("x", pg.ReplayMaxResultBytes) + `"}`)
	}} {
		f.job = replaySample()
		mutate(&f.job)
		w := request(h, "GET", replayPath+"/job-1", tokenA, "")
		if w.Code != 503 || strings.TrimSpace(w.Body.String()) != `{"error":"unavailable"}` {
			t.Fatal(w.Code, w.Body)
		}
	}
	f.job = replaySample()
	for _, jobs := range [][]pg.ReplayJob{make([]pg.ReplayJob, 21), {f.job, f.job}, {func() pg.ReplayJob { j := f.job; j.Key = keyB; return j }()}} {
		f.jobs = jobs
		w := request(h, "GET", replayPath, tokenA, "")
		if w.Code != 503 {
			t.Fatal(w.Code, w.Body)
		}
	}
	f.jobs = []pg.ReplayJob{f.job}
	w := request(h, "GET", replayPath+"?before_sequence=9", tokenA, "")
	if w.Code != 503 {
		t.Fatal("exclusive cursor accepted equal sequence", w.Code)
	}
}

type blockedReplayWriter struct {
	header  http.Header
	entered chan<- struct{}
	release <-chan struct{}
}

func (w *blockedReplayWriter) Header() http.Header { return w.header }
func (w *blockedReplayWriter) WriteHeader(int)     {}
func (w *blockedReplayWriter) Write(b []byte) (int, error) {
	w.entered <- struct{}{}
	<-w.release
	return len(b), nil
}
func TestReplaySlowResponsesHaveSeparateBoundedAdmission(t *testing.T) {
	h, f, a := setupReplays(t)
	h.slots = make(chan struct{}, 1)
	entered, release, done := make(chan struct{}, 4), make(chan struct{}), make(chan struct{}, 4)
	defer close(release)
	for i := range 4 {
		go func() {
			path := replayPath + "/job-1"
			if i%2 == 0 {
				path += "/result"
			}
			r := httptest.NewRequest("GET", path, nil)
			r.Header.Set("Authorization", "Bearer "+tokenA)
			w := &blockedReplayWriter{header: make(http.Header), entered: entered, release: release}
			h.ServeHTTP(w, r)
			done <- struct{}{}
		}()
	}
	for range 4 {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("replay response did not enter writer")
		}
	}
	w := request(h, "GET", replayPath+"/job-1/result", tokenA, "")
	if w.Code != 503 || w.Header().Get("Retry-After") != "1" || !strings.Contains(w.Body.String(), `"busy"`) {
		t.Fatal("replay lane not bounded", w.Code, w.Body)
	}
	for _, tt := range []struct{ method, path, body string }{{"GET", pathA, ""}, {"POST", pathA + "/commands", `{"id":"control-1","expected_revision":7,"kind":"cancel"}`}} {
		w = request(h, tt.method, tt.path, tokenA, tt.body)
		if w.Code != 200 {
			t.Fatal("replays exhausted control lane", w.Code, w.Body)
		}
	}
	w = request(h, "GET", replayPath, tokenB, "")
	if w.Code != 404 {
		t.Fatal("saturated lane leaked scoped route", w.Code)
	}
	w = request(h, "GET", replayPath, "", "")
	if w.Code != 401 {
		t.Fatal("saturated lane changed auth", w.Code)
	}
	f.mu.Lock()
	calls := f.calls
	f.mu.Unlock()
	if calls != 4 || a.calls != 1 {
		t.Fatal("unexpected service/authority calls", calls, a.calls)
	}
	// Completion is joined in cleanup after releasing all response writers.
	t.Cleanup(func() {
		for range 4 {
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Error("replay response did not finish")
			}
		}
	})
}
func TestReplayLaneUnaffectedByControlSaturation(t *testing.T) {
	h, _, _ := setupReplays(t)
	for range cap(h.slots) {
		h.slots <- struct{}{}
	}
	w := request(h, "GET", pathA, tokenA, "")
	if w.Code != 503 {
		t.Fatal(w.Code)
	}
	w = request(h, "GET", replayPath+"/job-1", tokenA, "")
	if w.Code != 200 {
		t.Fatal("control saturation blocked replay lane", w.Code, w.Body)
	}
}

func TestReplayConfiguredServiceAndTimeout(t *testing.T) {
	f := &fakeReplays{job: replaySample()}
	h, err := New(Config{Accounts: map[pg.Key]Authority{keyA: &fakeAuthority{view: sampleView(keyA)}}, Credentials: []Credential{{Token: tokenA, Scopes: []pg.Key{keyA}}}, Journal: &fakeJournal{}, Replays: f, MaxConcurrent: 1, RequestTimeout: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if cap(h.replaySlots) != 4 || cap(h.slots) != 1 {
		t.Fatal("unexpected admission capacity")
	}
	w := request(h, "GET", replayPath+"/job-1", tokenA, "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	f.onGet = func(ctx context.Context, key pg.Key, id string) (pg.ReplayJob, error) {
		if key != keyA || id != "job-1" {
			t.Error("incorrect replay scope")
		}
		<-ctx.Done()
		return pg.ReplayJob{}, ctx.Err()
	}
	w = request(h, "GET", replayPath+"/job-1", tokenA, "")
	if w.Code != 504 || len(h.replaySlots) != 0 {
		t.Fatal("replay deadline/admission not released", w.Code, w.Body)
	}
}

func setReplayResult(f *fakeReplays, raw []byte) {
	f.job.Result = append(json.RawMessage{}, raw...)
	digest := sha256.Sum256(raw)
	f.job.ResultSHA256 = hex.EncodeToString(digest[:])
}

func TestReplayResultDownloadRetainsExactReportBytes(t *testing.T) {
	h, f, a := setupReplays(t)
	raw := []byte("{\n  \"report\": { \"text\": \"<>&\", \"value\": 1.0 }\n}\n")
	setReplayResult(f, raw)
	f.job.Input = json.RawMessage(`{"private":"INPUT_SECRET"}`)
	w := request(h, "GET", replayPath+"/job-1/result", tokenA, "")
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), raw) {
		t.Fatalf("report bytes changed: %d %q", w.Code, w.Body.Bytes())
	}
	if w.Header().Get("Content-Type") != "application/json" || w.Header().Get("X-Content-SHA256") != f.job.ResultSHA256 || w.Header().Get("Content-Length") != strconv.Itoa(len(raw)) {
		t.Fatal(w.Header())
	}
	kind, parameters, err := mime.ParseMediaType(w.Header().Get("Content-Disposition"))
	if err != nil || kind != "attachment" || parameters["filename"] != "job-1.json" {
		t.Fatal("download disposition", w.Header(), err)
	}
	for _, private := range []string{"INPUT_SECRET", "private-worker", "worker_id", "lease_until", "input_sha256"} {
		if strings.Contains(w.Body.String(), private) {
			t.Fatalf("download included %s", private)
		}
	}
	if f.kind != "get" || f.key != keyA || f.id != "job-1" || a.calls != 0 {
		t.Fatal("download used incorrect scope or financial authority")
	}
	// Embedded detail remains normal JSON; its hash still identifies the raw
	// download bytes rather than the compact representation inside the envelope.
	w = request(h, "GET", replayPath+"/job-1", tokenA, "")
	var detail ReplayDetailDTO
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &detail) != nil || detail.ResultSHA256 != f.job.ResultSHA256 || !json.Valid(detail.Result) {
		t.Fatal("detail response changed", w.Code, w.Body)
	}
}

func TestReplayResultDownloadBoundsAndIntegrity(t *testing.T) {
	h, f, _ := setupReplays(t)
	prefix, suffix := `{"padding":"`, "\"}\n"
	full := []byte(prefix + strings.Repeat("x", pg.ReplayMaxResultBytes-len(prefix)-len(suffix)) + suffix)
	setReplayResult(f, full)
	w := request(h, "GET", replayPath+"/job-1/result", tokenA, "")
	if w.Code != 200 || w.Body.Len() != pg.ReplayMaxResultBytes || !bytes.Equal(w.Body.Bytes(), full) {
		t.Fatalf("bounded full report response failed: status=%d bytes=%d", w.Code, w.Body.Len())
	}
	for _, raw := range [][]byte{append(full, ' '), nil, []byte(`[]`), []byte(`null`), []byte(`{"malformed":`)} {
		setReplayResult(f, raw)
		w = request(h, "GET", replayPath+"/job-1/result", tokenA, "")
		if w.Code != 503 || strings.TrimSpace(w.Body.String()) != `{"error":"unavailable"}` || w.Header().Get("X-Content-SHA256") != "" || w.Header().Get("Content-Disposition") != "" {
			t.Fatal("invalid report was exposed", w.Code, w.Header(), w.Body)
		}
	}
	setReplayResult(f, []byte("{\"report\":true}\n"))
	f.job.ResultSHA256 = strings.Repeat("0", 64)
	w = request(h, "GET", replayPath+"/job-1/result", tokenA, "")
	if w.Code != 503 || strings.TrimSpace(w.Body.String()) != `{"error":"unavailable"}` {
		t.Fatal("digest mismatch was exposed", w.Code, w.Body)
	}
	f.job = replaySample()
	f.job.Key = keyB
	w = request(h, "GET", replayPath+"/job-1/result", tokenA, "")
	if w.Code != 503 || strings.Contains(w.Body.String(), "report") {
		t.Fatal("backend returned another scope's report", w.Code, w.Body)
	}
}

func TestReplayResultDownloadStateAndRequestGuards(t *testing.T) {
	h, f, _ := setupReplays(t)
	for _, status := range []string{"queued", "running", "failed", "cancelled"} {
		f.job = replaySample()
		f.job.Status, f.job.Result, f.job.ResultSHA256 = status, nil, ""
		if status == "failed" {
			f.job.ErrorCode = "execution_failed"
		}
		code := "result_unavailable"
		if status == "queued" || status == "running" {
			code = "result_not_ready"
		}
		w := request(h, "GET", replayPath+"/job-1/result", tokenA, "")
		if w.Code != 409 || strings.TrimSpace(w.Body.String()) != fmt.Sprintf(`{"error":%q}`, code) {
			t.Fatal(status, w.Code, w.Body)
		}
	}
	f.job = replaySample()
	for _, tt := range []struct {
		method, path, token, body string
		status                    int
	}{
		{"GET", replayPath + "/job-1/result", "", "", 401},
		{"GET", replayPath + "/job-1/result", tokenB, "", 404},
		{"GET", "/v1/accounts/tenant-a/missing/replays/job-1/result", tokenA, "", 404},
		{"GET", replayPath + "/job-1/result/extra", tokenA, "", 404},
		{"GET", replayPath + "/job-1/result?limit=1", tokenA, "", 400},
		{"GET", replayPath + "/job-1/result", tokenA, `{}`, 400},
		{"POST", replayPath + "/job-1/result", tokenA, "", 405},
		{"HEAD", replayPath + "/job-1/result", tokenA, "", 405},
	} {
		before := f.calls
		w := request(h, tt.method, tt.path, tt.token, tt.body)
		if w.Code != tt.status || f.calls != before {
			t.Fatalf("%s %s status=%d calls=%d body=%s", tt.method, tt.path, w.Code, f.calls-before, w.Body)
		}
		if tt.status == 405 && w.Header().Get("Allow") != "GET" {
			t.Fatal(w.Header())
		}
	}
	// Reject an unknown-length/chunked body before the backend is consulted.
	r := httptest.NewRequest("GET", replayPath+"/job-1/result", strings.NewReader(`{}`))
	r.Header.Set("Authorization", "Bearer "+tokenA)
	r.ContentLength = -1
	r.TransferEncoding = []string{"chunked"}
	before := f.calls
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 400 || f.calls != before {
		t.Fatal(w.Code, w.Body)
	}
	f.err = pg.ErrNotFound
	w = request(h, "GET", replayPath+"/job-1/result", tokenA, "")
	if w.Code != 404 {
		t.Fatal("missing report", w.Code, w.Body)
	}
	h.replays = nil
	w = request(h, "GET", replayPath+"/job-1/result", tokenA, "")
	if w.Code != 404 {
		t.Fatal("disabled report route", w.Code, w.Body)
	}
}
