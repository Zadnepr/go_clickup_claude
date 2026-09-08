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
)

type fakeSubmitter struct {
	submitted []string
}

func (f *fakeSubmitter) Submit(taskID string) bool {
	f.submitted = append(f.submitted, taskID)
	return true
}

type fakePinger struct{ err error }

func (f *fakePinger) Ping(ctx context.Context) error { return f.err }

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
