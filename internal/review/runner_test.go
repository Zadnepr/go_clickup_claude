package review

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// writeFakeClaude создаёт исполняемый скрипт-заглушку вместо реального claude:
// печатает заданный JSON-ответ и проверяет, что ему передали ожидаемое окружение.
func writeFakeClaude(t *testing.T, jsonOut string, exitCode int) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake claude script requires a POSIX shell")
	}

	dir := t.TempDir()
	// echo/printf в разных shell по-разному интерпретируют "\n" в аргументах,
	// поэтому JSON кладём в отдельный файл и просто cat'аем его.
	dataPath := filepath.Join(dir, "output.json")
	if err := os.WriteFile(dataPath, []byte(jsonOut), 0o644); err != nil {
		t.Fatalf("write fake claude output: %v", err)
	}

	path := filepath.Join(dir, "fake-claude.sh")
	script := fmt.Sprintf(`#!/bin/sh
cat %q
exit %d
`, dataPath, exitCode)

	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake claude script: %v", err)
	}
	return path
}

func TestRunner_RunReview_ParsesOutputAndSession(t *testing.T) {
	out := claudeJSONResult{
		Type:      "result",
		Subtype:   "success",
		IsError:   false,
		Result:    "Ревью готово.\nИТОГ: критичных=1 важных=0 минор=0 статус=fail",
		SessionID: "sess-123",
	}
	b, _ := json.Marshal(out)

	repo := t.TempDir()
	r := &Runner{RepoPath: repo, Home: t.TempDir(), ClaudeBinary: writeFakeClaude(t, string(b), 0)}

	res, err := r.RunReview(context.Background(), "https://app.clickup.com/t/123", "")
	if err != nil {
		t.Fatalf("RunReview error: %v", err)
	}
	if res.SessionID != "sess-123" {
		t.Errorf("SessionID = %q, want sess-123", res.SessionID)
	}
	if res.Verdict.Status != StatusFail || res.Verdict.Critical != 1 {
		t.Errorf("unexpected verdict: %+v", res.Verdict)
	}
}

func TestRunner_RunReview_ClaudeReportsError(t *testing.T) {
	out := claudeJSONResult{
		Type:      "result",
		Subtype:   "error_max_turns",
		IsError:   true,
		Result:    "Failed to authenticate.",
		SessionID: "sess-err",
	}
	b, _ := json.Marshal(out)

	repo := t.TempDir()
	r := &Runner{RepoPath: repo, Home: t.TempDir(), ClaudeBinary: writeFakeClaude(t, string(b), 0)}

	_, err := r.RunReview(context.Background(), "https://app.clickup.com/t/123", "")
	if err == nil {
		t.Fatal("expected error when claude reports is_error=true")
	}
}

func TestRunner_RunReview_UnparsableOutput(t *testing.T) {
	repo := t.TempDir()
	r := &Runner{RepoPath: repo, Home: t.TempDir(), ClaudeBinary: writeFakeClaude(t, "not json at all", 0)}

	_, err := r.RunReview(context.Background(), "https://app.clickup.com/t/123", "")
	if err == nil {
		t.Fatal("expected error for unparsable claude output")
	}
}

func TestRunner_RunReview_WithSpecPath(t *testing.T) {
	out := claudeJSONResult{Type: "result", Result: "ok\nИТОГ: критичных=0 важных=0 минор=0 статус=pass", SessionID: "s1"}
	b, _ := json.Marshal(out)

	repo := t.TempDir()
	r := &Runner{RepoPath: repo, Home: t.TempDir(), ClaudeBinary: writeFakeClaude(t, string(b), 0)}

	res, err := r.RunReview(context.Background(), "https://app.clickup.com/t/123", r.SpecFilePath("123"))
	if err != nil {
		t.Fatalf("RunReview error: %v", err)
	}
	if res.Verdict.Status != StatusPass {
		t.Errorf("unexpected verdict: %+v", res.Verdict)
	}
}

func TestRunner_SpecFilePath(t *testing.T) {
	r := &Runner{RepoPath: "/repo"}
	got := r.SpecFilePath("42")
	want := filepath.Join("/repo", ".claude", "specs", "42.md")
	if got != want {
		t.Errorf("SpecFilePath = %q, want %q", got, want)
	}
}
