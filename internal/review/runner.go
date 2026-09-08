package review

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// Runner запускает claude -p в рабочей копии репозитория. ClaudeBinary
// подставляется в тестах вместо реального "claude".
type Runner struct {
	RepoPath     string
	Home         string
	ClaudeBinary string
}

// NewRunner создаёт Runner с настоящим бинарником claude.
func NewRunner(repoPath, home string) *Runner {
	return &Runner{RepoPath: repoPath, Home: home, ClaudeBinary: "claude"}
}

// Result — итог запуска /review: полный текст ревью, id сессии для лога и
// ссылки на прогон, stderr процесса (для диагностики) и разобранный вердикт.
type Result struct {
	Output    string
	SessionID string
	Stderr    string
	Verdict   Verdict
}

// claudeJSONResult отражает нужные поля вывода `claude -p --output-format json`.
type claudeJSONResult struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	IsError   bool   `json:"is_error"`
	Result    string `json:"result"`
	SessionID string `json:"session_id"`
}

// GitFetch подтягивает актуальное состояние веток репозитория перед ревью:
// без этого проверяться будет вчерашнее состояние.
func (r *Runner) GitFetch(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "git", "fetch", "--prune", "origin")
	cmd.Dir = r.RepoPath
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git fetch --prune origin: %w (stderr: %s)", err, stderr.String())
	}
	return nil
}

// SpecFilePath возвращает путь, по которому /spec обязан сохранить ТЗ задачи.
func (r *Runner) SpecFilePath(taskID string) string {
	return filepath.Join(r.RepoPath, ".claude", "specs", taskID+".md")
}

// RunSpec запускает "/spec\n<url задачи>" отдельной сессией claude. Команда
// сама пишет .claude/specs/<ID>.md — вызывающая сторона проверяет файл
// после возврата (см. RunSpec в очереди задач и Требование 5.2).
func (r *Runner) RunSpec(ctx context.Context, taskURL string) (sessionID string, err error) {
	prompt := "/spec\n" + taskURL
	result, stderr, err := r.run(ctx, prompt)
	if err != nil {
		return result.SessionID, fmt.Errorf("/spec run failed: %w (stderr: %s)", err, truncate(stderr, 2000))
	}
	return result.SessionID, nil
}

// RunReview запускает "/review\n<url задачи>[\n<путь к спеке>]" отдельной
// сессией claude и возвращает полный вывод, id сессии и разобранный вердикт.
// specPath пустой означает, что /spec не создал файл: /review соберёт ТЗ сама.
func (r *Runner) RunReview(ctx context.Context, taskURL, specPath string) (Result, error) {
	prompt := "/review\n" + taskURL
	if specPath != "" {
		prompt += "\n" + specPath
	}

	result, stderr, err := r.run(ctx, prompt)
	if err != nil {
		return Result{
			SessionID: result.SessionID,
			Stderr:    stderr,
			Verdict:   Verdict{Status: StatusBlocked},
		}, fmt.Errorf("/review run failed: %w (stderr: %s)", err, truncate(stderr, 2000))
	}

	return Result{
		Output:    result.Result,
		SessionID: result.SessionID,
		Stderr:    stderr,
		Verdict:   ParseVerdict(result.Result),
	}, nil
}

// run выполняет один процесс claude -p и возвращает разобранный JSON-вывод.
func (r *Runner) run(ctx context.Context, prompt string) (claudeJSONResult, string, error) {
	binary := r.ClaudeBinary
	if binary == "" {
		binary = "claude"
	}

	cmd := exec.CommandContext(ctx, binary, "-p", prompt,
		"--output-format", "json",
		"--allowedTools", "Bash(git:*)", "Bash(cup:*)", "Read", "Grep", "Glob")
	cmd.Dir = r.RepoPath
	// os.Environ() дополняется, а не заменяется: иначе claude не увидит
	// CLAUDE_CODE_OAUTH_TOKEN и упадёт на авторизации.
	cmd.Env = append(os.Environ(), "HOME="+r.Home)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()

	var result claudeJSONResult
	if parseErr := json.Unmarshal(stdout.Bytes(), &result); parseErr != nil {
		if runErr != nil {
			return claudeJSONResult{}, stderr.String(), fmt.Errorf("claude process failed: %w", runErr)
		}
		return claudeJSONResult{}, stderr.String(), fmt.Errorf("parse claude json output: %w (raw: %s)", parseErr, truncate(stdout.String(), 2000))
	}

	if runErr != nil {
		return result, stderr.String(), fmt.Errorf("claude process failed: %w", runErr)
	}
	if result.IsError {
		return result, stderr.String(), fmt.Errorf("claude reported an error: %s", result.Result)
	}

	return result, stderr.String(), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}
