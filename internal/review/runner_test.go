package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
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
		Type:         "result",
		Subtype:      "success",
		IsError:      false,
		Result:       "Ревью готово.\nИТОГ: критичных=1 важных=0 минор=0 статус=fail",
		SessionID:    "sess-123",
		TotalCostUSD: 0.0234,
		Usage:        claudeUsage{InputTokens: 1200, OutputTokens: 340},
	}
	b, _ := json.Marshal(out)

	repo := t.TempDir()
	r := &Runner{RepoPath: repo, Home: t.TempDir(), ClaudeBinary: writeFakeClaude(t, string(b), 0)}

	res, err := r.RunReview(context.Background(), "https://app.clickup.com/t/123", "", nil)
	if err != nil {
		t.Fatalf("RunReview error: %v", err)
	}
	if res.SessionID != "sess-123" {
		t.Errorf("SessionID = %q, want sess-123", res.SessionID)
	}
	if res.Verdict.Status != StatusFail || res.Verdict.Critical != 1 {
		t.Errorf("unexpected verdict: %+v", res.Verdict)
	}
	if res.Usage.InputTokens != 1200 || res.Usage.OutputTokens != 340 || res.Usage.CostUSD != 0.0234 {
		t.Errorf("unexpected token usage: %+v", res.Usage)
	}
}

func TestRunner_RunSpec_ReturnsUsage(t *testing.T) {
	out := claudeJSONResult{
		Type:         "result",
		Result:       "spec written",
		SessionID:    "spec-sess",
		TotalCostUSD: 0.001,
		Usage:        claudeUsage{InputTokens: 500, OutputTokens: 50},
	}
	b, _ := json.Marshal(out)

	repo := t.TempDir()
	r := &Runner{RepoPath: repo, Home: t.TempDir(), ClaudeBinary: writeFakeClaude(t, string(b), 0)}

	sr, err := r.RunSpec(context.Background(), "https://app.clickup.com/t/123", nil)
	if err != nil {
		t.Fatalf("RunSpec error: %v", err)
	}
	if sr.SessionID != "spec-sess" {
		t.Errorf("sessionID = %q, want spec-sess", sr.SessionID)
	}
	if sr.Content != "spec written" {
		t.Errorf("Content = %q, want %q", sr.Content, "spec written")
	}
	if sr.Usage.InputTokens != 500 || sr.Usage.OutputTokens != 50 || sr.Usage.CostUSD != 0.001 {
		t.Errorf("unexpected usage: %+v", sr.Usage)
	}
}

func TestTokenUsage_Add(t *testing.T) {
	a := TokenUsage{InputTokens: 100, OutputTokens: 20, CostUSD: 0.01}
	b := TokenUsage{InputTokens: 50, OutputTokens: 5, CostUSD: 0.002}
	sum := a.Add(b)
	if sum.InputTokens != 150 || sum.OutputTokens != 25 || sum.CostUSD != 0.012 {
		t.Errorf("unexpected sum: %+v", sum)
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

	_, err := r.RunReview(context.Background(), "https://app.clickup.com/t/123", "", nil)
	if err == nil {
		t.Fatal("expected error when claude reports is_error=true")
	}
}

func TestRunner_RunReview_UsageLimitReached(t *testing.T) {
	out := claudeJSONResult{
		Type:      "result",
		Subtype:   "error_during_execution",
		IsError:   true,
		Result:    "Claude AI usage limit reached. Your limit will reset in 3 hours.",
		SessionID: "sess-limit",
	}
	b, _ := json.Marshal(out)

	repo := t.TempDir()
	r := &Runner{RepoPath: repo, Home: t.TempDir(), ClaudeBinary: writeFakeClaude(t, string(b), 0)}

	_, err := r.RunReview(context.Background(), "https://app.clickup.com/t/123", "", nil)
	if err == nil {
		t.Fatal("expected error when usage limit is reached")
	}
	if !errors.Is(err, ErrUsageLimit) {
		t.Errorf("expected errors.Is(err, ErrUsageLimit), got: %v", err)
	}
}

func TestRunner_RunSpec_UsageLimitReached(t *testing.T) {
	out := claudeJSONResult{
		Type:    "result",
		Subtype: "error_during_execution",
		IsError: true,
		Result:  "rate limit exceeded, please retry later",
	}
	b, _ := json.Marshal(out)

	repo := t.TempDir()
	r := &Runner{RepoPath: repo, Home: t.TempDir(), ClaudeBinary: writeFakeClaude(t, string(b), 0)}

	_, err := r.RunSpec(context.Background(), "https://app.clickup.com/t/123", nil)
	if !errors.Is(err, ErrUsageLimit) {
		t.Errorf("expected errors.Is(err, ErrUsageLimit), got: %v", err)
	}
}

func TestRunner_RunReview_UnparsableOutput(t *testing.T) {
	repo := t.TempDir()
	r := &Runner{RepoPath: repo, Home: t.TempDir(), ClaudeBinary: writeFakeClaude(t, "not json at all", 0)}

	_, err := r.RunReview(context.Background(), "https://app.clickup.com/t/123", "", nil)
	if err == nil {
		t.Fatal("expected error for unparsable claude output")
	}
}

func TestRunner_RunReview_WithSpecPath(t *testing.T) {
	out := claudeJSONResult{Type: "result", Result: "ok\nИТОГ: критичных=0 важных=0 минор=0 статус=pass", SessionID: "s1"}
	b, _ := json.Marshal(out)

	repo := t.TempDir()
	r := &Runner{RepoPath: repo, Home: t.TempDir(), ClaudeBinary: writeFakeClaude(t, string(b), 0)}

	res, err := r.RunReview(context.Background(), "https://app.clickup.com/t/123", r.SpecFilePath("123"), nil)
	if err != nil {
		t.Fatalf("RunReview error: %v", err)
	}
	if res.Verdict.Status != StatusPass {
		t.Errorf("unexpected verdict: %+v", res.Verdict)
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v (dir=%s) failed: %v\n%s", args, dir, err, out)
	}
}

// initRepoWithOrigin делает dir git-репозиторием с рабочим remote "origin"
// (локальный bare-репозиторий), чтобы `git fetch --prune origin` реально
// отрабатывал успешно в тестах, без сети.
func initRepoWithOrigin(t *testing.T, dir string) {
	t.Helper()
	bareDir := filepath.Join(t.TempDir(), "origin.git")
	runGit(t, "", "init", "--bare", "--initial-branch=main", bareDir)
	runGit(t, dir, "init", "--initial-branch=main")
	runGit(t, dir, "remote", "add", "origin", bareDir)
}

func TestGitFetch_SingleRepoAtRoot(t *testing.T) {
	root := t.TempDir()
	initRepoWithOrigin(t, root)

	r := &Runner{RepoPath: root}
	if err := r.GitFetch(context.Background()); err != nil {
		t.Fatalf("GitFetch error: %v", err)
	}
}

func TestGitFetch_MultipleReposAsSubdirsOfPlainRoot(t *testing.T) {
	root := t.TempDir() // сам root НЕ git-репозиторий
	repoA := filepath.Join(root, "panels")
	repoB := filepath.Join(root, "sommerce")
	if err := os.MkdirAll(repoA, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(repoB, 0o755); err != nil {
		t.Fatal(err)
	}
	initRepoWithOrigin(t, repoA)
	initRepoWithOrigin(t, repoB)

	r := &Runner{RepoPath: root}
	if err := r.GitFetch(context.Background()); err != nil {
		t.Fatalf("GitFetch error: %v", err)
	}
}

func TestGitFetch_NoRepositoriesFound(t *testing.T) {
	root := t.TempDir() // пустой каталог, ни один git-репозиторий не найден

	r := &Runner{RepoPath: root}
	if err := r.GitFetch(context.Background()); err == nil {
		t.Fatal("expected error when no git repository is found")
	}
}

func TestGitFetch_PartialFailureStillSucceeds(t *testing.T) {
	root := t.TempDir()
	goodRepo := filepath.Join(root, "good")
	badRepo := filepath.Join(root, "bad")
	if err := os.MkdirAll(goodRepo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(badRepo, 0o755); err != nil {
		t.Fatal(err)
	}
	initRepoWithOrigin(t, goodRepo)
	runGit(t, badRepo, "init", "--initial-branch=main") // без remote "origin" — fetch здесь упадёт

	r := &Runner{RepoPath: root}
	if err := r.GitFetch(context.Background()); err != nil {
		t.Fatalf("expected success when at least one repo fetches OK, got error: %v", err)
	}
}

func TestGitFetch_AllRepositoriesFail(t *testing.T) {
	root := t.TempDir()
	badRepo := filepath.Join(root, "bad")
	if err := os.MkdirAll(badRepo, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, badRepo, "init", "--initial-branch=main") // без remote "origin"

	r := &Runner{RepoPath: root}
	if err := r.GitFetch(context.Background()); err == nil {
		t.Fatal("expected error when every discovered repository fails to fetch")
	}
}

func TestRunner_SpecFilePath(t *testing.T) {
	r := &Runner{RepoPath: "/repo"}
	got := r.SpecFilePath("42")
	want := filepath.Join("/repo", "specs", "42.md")
	if got != want {
		t.Errorf("SpecFilePath = %q, want %q", got, want)
	}
}
