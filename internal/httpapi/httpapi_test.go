package httpapi

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/Zadnepr/go_clickup_claude/internal/queue"
	"github.com/Zadnepr/go_clickup_claude/internal/store"
)

// fakeSubmitter — фейковая реализация QueueControl. По умолчанию (нулевые
// значения полей) ведёт себя как пустая очередь: ничего не активно, ничего
// не в буфере, любое ручное действие над задачей "не найдено".
type fakeSubmitter struct {
	submitted    []string
	active       []queue.ActiveRunInfo
	pending      []string
	cancelResult bool
	pauseResult  bool
	resumeResult bool
	cancelledID  string
	pausedID     string
	resumedID    string
}

func (f *fakeSubmitter) Submit(taskID string) bool {
	f.submitted = append(f.submitted, taskID)
	return true
}

func (f *fakeSubmitter) ActiveRuns() []queue.ActiveRunInfo { return f.active }
func (f *fakeSubmitter) Pending() []string                 { return f.pending }

func (f *fakeSubmitter) RequestCancel(taskID string) bool {
	f.cancelledID = taskID
	return f.cancelResult
}

func (f *fakeSubmitter) RequestPause(taskID string) bool {
	f.pausedID = taskID
	return f.pauseResult
}

func (f *fakeSubmitter) SubmitResume(taskID string) bool {
	f.resumedID = taskID
	return f.resumeResult
}

type fakePinger struct {
	err         error
	active      []store.Run
	stats       store.Stats
	statsErr    error
	statsSince  time.Time
	run         *store.Run
	runErr      error
	stages      []store.RunStage
	invocations []store.ClaudeInvocation
}

func (f *fakePinger) Ping(ctx context.Context) error { return f.err }

func (f *fakePinger) ListActive(ctx context.Context) ([]store.Run, error) {
	return f.active, f.err
}

func (f *fakePinger) Stats(ctx context.Context, since time.Time) (store.Stats, error) {
	f.statsSince = since
	if f.statsErr != nil {
		return store.Stats{}, f.statsErr
	}
	return f.stats, nil
}

func (f *fakePinger) GetRun(ctx context.Context, runID int64) (*store.Run, error) {
	return f.run, f.runErr
}

func (f *fakePinger) ListStages(ctx context.Context, runID int64) ([]store.RunStage, error) {
	return f.stages, nil
}

func (f *fakePinger) ListInvocations(ctx context.Context, runID int64) ([]store.ClaudeInvocation, error) {
	return f.invocations, nil
}

func (f *fakePinger) ListInvocationsSince(ctx context.Context, since time.Time) ([]store.ClaudeInvocation, error) {
	return f.invocations, nil
}

type fakeManualRunner struct {
	submitted []string
	taskIDArg string
	err       error
}

func (f *fakeManualRunner) RunNow(ctx context.Context, taskID string) ([]string, error) {
	f.taskIDArg = taskID
	if f.err != nil {
		return nil, f.err
	}
	return f.submitted, nil
}

func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(discardWriter{}, nil))
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

func TestWebhook_ValidSignature_EnqueuesAndReturns200(t *testing.T) {
	sub := &fakeSubmitter{}
	deps := Deps{Queue: sub, WebhookSecret: "s3cr3t", Store: &fakePinger{}, Logger: discardLogger()}
	mux := NewMux(deps)

	body := []byte(`{"event":"taskStatusUpdated","task_id":"abc123"}`)
	req := httptest.NewRequest(http.MethodPost, "/webhook/clickup", bytes.NewReader(body))
	req.Header.Set("X-Signature", sign("s3cr3t", body))
	rec := httptest.NewRecorder()

	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if len(sub.submitted) != 1 || sub.submitted[0] != "abc123" {
		t.Errorf("expected task abc123 to be submitted, got: %+v", sub.submitted)
	}
}

func TestWebhook_TamperedBody_Returns401AndDoesNotEnqueue(t *testing.T) {
	sub := &fakeSubmitter{}
	deps := Deps{Queue: sub, WebhookSecret: "s3cr3t", Store: &fakePinger{}, Logger: discardLogger()}
	mux := NewMux(deps)

	original := []byte(`{"event":"taskStatusUpdated","task_id":"abc123"}`)
	validSig := sign("s3cr3t", original)

	tampered := []byte(`{"event":"taskStatusUpdated","task_id":"evil999"}`)
	req := httptest.NewRequest(http.MethodPost, "/webhook/clickup", bytes.NewReader(tampered))
	req.Header.Set("X-Signature", validSig)
	rec := httptest.NewRecorder()

	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if len(sub.submitted) != 0 {
		t.Fatalf("expected no task submitted for tampered body, got: %+v", sub.submitted)
	}
}

func TestWebhook_MissingSignature_Returns401(t *testing.T) {
	sub := &fakeSubmitter{}
	deps := Deps{Queue: sub, WebhookSecret: "s3cr3t", Store: &fakePinger{}, Logger: discardLogger()}
	mux := NewMux(deps)

	body := []byte(`{"event":"taskCreated","task_id":"abc123"}`)
	req := httptest.NewRequest(http.MethodPost, "/webhook/clickup", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestWebhook_UninterestingEvent_StillReturns200ButNoEnqueue(t *testing.T) {
	sub := &fakeSubmitter{}
	deps := Deps{Queue: sub, WebhookSecret: "s3cr3t", Store: &fakePinger{}, Logger: discardLogger()}
	mux := NewMux(deps)

	body := []byte(`{"event":"taskDeleted","task_id":"abc123"}`)
	req := httptest.NewRequest(http.MethodPost, "/webhook/clickup", bytes.NewReader(body))
	req.Header.Set("X-Signature", sign("s3cr3t", body))
	rec := httptest.NewRecorder()

	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if len(sub.submitted) != 0 {
		t.Errorf("expected no submission for uninteresting event, got: %+v", sub.submitted)
	}
}

func TestNewMux_NoWebhookSecret_DisablesEndpoint(t *testing.T) {
	deps := Deps{Queue: &fakeSubmitter{}, WebhookSecret: "", Store: &fakePinger{}, Logger: discardLogger()}
	mux := NewMux(deps)

	req := httptest.NewRequest(http.MethodPost, "/webhook/clickup", bytes.NewReader([]byte(`{}`)))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected webhook endpoint to be absent (404), got %d", rec.Code)
	}
}

func TestHealthz_AlwaysOK(t *testing.T) {
	deps := Deps{Queue: &fakeSubmitter{}, Store: &fakePinger{}, Logger: discardLogger()}
	mux := NewMux(deps)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func fakeClaudeBinary(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake claude script requires a POSIX shell")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-claude.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho '2.1.0'\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	return path
}

func TestReadyz_AllHealthy_Returns200(t *testing.T) {
	repo := t.TempDir()
	deps := Deps{
		Queue:         &fakeSubmitter{},
		Store:         &fakePinger{},
		RepoPath:      repo,
		ClaudeBinary:  fakeClaudeBinary(t),
		Logger:        discardLogger(),
		ReadyzTimeout: 3 * time.Second,
	}
	mux := NewMux(deps)

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
}

func TestReadyz_DBDown_Returns503(t *testing.T) {
	repo := t.TempDir()
	deps := Deps{
		Queue:        &fakeSubmitter{},
		Store:        &fakePinger{err: errors.New("db closed")},
		RepoPath:     repo,
		ClaudeBinary: fakeClaudeBinary(t),
		Logger:       discardLogger(),
	}
	mux := NewMux(deps)

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestReadyz_RepoMissing_Returns503(t *testing.T) {
	deps := Deps{
		Queue:        &fakeSubmitter{},
		Store:        &fakePinger{},
		RepoPath:     filepath.Join(t.TempDir(), "does-not-exist"),
		ClaudeBinary: fakeClaudeBinary(t),
		Logger:       discardLogger(),
	}
	mux := NewMux(deps)

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestRun_WithTaskID_SubmitsAndReturns202(t *testing.T) {
	trigger := &fakeManualRunner{submitted: []string{"123"}}
	deps := Deps{Queue: &fakeSubmitter{}, Trigger: trigger, Store: &fakePinger{}, Logger: discardLogger()}
	mux := NewMux(deps)

	req := httptest.NewRequest(http.MethodPost, "/api/run", bytes.NewReader([]byte(`{"task_id":"123"}`)))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202, body: %s", rec.Code, rec.Body.String())
	}
	if trigger.taskIDArg != "123" {
		t.Errorf("expected RunNow called with task_id=123, got %q", trigger.taskIDArg)
	}
}

func TestRun_WithoutBody_TriggersFullScan(t *testing.T) {
	trigger := &fakeManualRunner{submitted: []string{"1", "2", "3"}}
	deps := Deps{Queue: &fakeSubmitter{}, Trigger: trigger, Store: &fakePinger{}, Logger: discardLogger()}
	mux := NewMux(deps)

	req := httptest.NewRequest(http.MethodPost, "/api/run", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202, body: %s", rec.Code, rec.Body.String())
	}
	if trigger.taskIDArg != "" {
		t.Errorf("expected RunNow called with empty task_id for a full scan, got %q", trigger.taskIDArg)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"1"`)) {
		t.Errorf("expected submitted task ids in response, got: %s", rec.Body.String())
	}
}

func TestRun_TriggerError_Returns502(t *testing.T) {
	trigger := &fakeManualRunner{err: errors.New("clickup down")}
	deps := Deps{Queue: &fakeSubmitter{}, Trigger: trigger, Store: &fakePinger{}, Logger: discardLogger()}
	mux := NewMux(deps)

	req := httptest.NewRequest(http.MethodPost, "/api/run", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
}

func TestStatus_ReturnsActiveRuns(t *testing.T) {
	active := []store.Run{{ID: 1, TaskID: "123", Status: store.StatusRunning}}
	deps := Deps{Queue: &fakeSubmitter{}, Store: &fakePinger{active: active}, Logger: discardLogger()}
	mux := NewMux(deps)

	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"123"`)) {
		t.Errorf("expected active task id in response, got: %s", rec.Body.String())
	}
}

func TestStats_DefaultWindowIsAll(t *testing.T) {
	fake := &fakePinger{stats: store.Stats{TotalRuns: 5, ByStatus: map[string]int{"done": 5}}}
	deps := Deps{Queue: &fakeSubmitter{}, Store: fake, Logger: discardLogger()}
	mux := NewMux(deps)

	req := httptest.NewRequest(http.MethodGet, "/api/stats", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	if !fake.statsSince.IsZero() {
		t.Errorf("expected zero 'since' for the default 'all' window, got %v", fake.statsSince)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"total_runs":5`)) {
		t.Errorf("expected total_runs in response, got: %s", rec.Body.String())
	}
}

func TestStats_HourWindow_PassesRecentSince(t *testing.T) {
	fake := &fakePinger{}
	deps := Deps{Queue: &fakeSubmitter{}, Store: fake, Logger: discardLogger()}
	mux := NewMux(deps)

	before := time.Now().Add(-time.Hour)
	req := httptest.NewRequest(http.MethodGet, "/api/stats?window=hour", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	after := time.Now().Add(-time.Hour)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if fake.statsSince.Before(before.Add(-time.Second)) || fake.statsSince.After(after.Add(time.Second)) {
		t.Errorf("expected since ~1h ago, got %v (window [%v, %v])", fake.statsSince, before, after)
	}
}

func TestStats_StoreError_Returns500(t *testing.T) {
	fake := &fakePinger{statsErr: errors.New("db error")}
	deps := Deps{Queue: &fakeSubmitter{}, Store: fake, Logger: discardLogger()}
	mux := NewMux(deps)

	req := httptest.NewRequest(http.MethodGet, "/api/stats", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestReadyz_ClaudeNotResponding_Returns503(t *testing.T) {
	repo := t.TempDir()
	deps := Deps{
		Queue:        &fakeSubmitter{},
		Store:        &fakePinger{},
		RepoPath:     repo,
		ClaudeBinary: filepath.Join(t.TempDir(), "does-not-exist-binary"),
		Logger:       discardLogger(),
	}
	mux := NewMux(deps)

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}
