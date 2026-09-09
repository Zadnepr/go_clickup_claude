package review

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
// ссылки на прогон, stderr процесса (для диагностики), разобранный вердикт
// и потраченные токены/стоимость именно этого вызова.
type Result struct {
	Output    string
	SessionID string
	Stderr    string
	Verdict   Verdict
	Usage     TokenUsage
}

// TokenUsage — токены и стоимость одного вызова `claude -p`. Складывается
// вызывающей стороной для /spec и /review, чтобы получить суммарный расход
// на одну задачу (см. Store.MarkDone/MarkFailed).
type TokenUsage struct {
	InputTokens  int
	OutputTokens int
	CostUSD      float64
}

// Add складывает два TokenUsage.
func (u TokenUsage) Add(o TokenUsage) TokenUsage {
	return TokenUsage{
		InputTokens:  u.InputTokens + o.InputTokens,
		OutputTokens: u.OutputTokens + o.OutputTokens,
		CostUSD:      u.CostUSD + o.CostUSD,
	}
}

// claudeJSONResult отражает нужные поля вывода `claude -p --output-format json`.
type claudeJSONResult struct {
	Type         string      `json:"type"`
	Subtype      string      `json:"subtype"`
	IsError      bool        `json:"is_error"`
	Result       string      `json:"result"`
	SessionID    string      `json:"session_id"`
	TotalCostUSD float64     `json:"total_cost_usd"`
	Usage        claudeUsage `json:"usage"`
}

type claudeUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

func (r claudeJSONResult) tokenUsage() TokenUsage {
	return TokenUsage{
		InputTokens:  r.Usage.InputTokens,
		OutputTokens: r.Usage.OutputTokens,
		CostUSD:      r.TotalCostUSD,
	}
}

// GitFetch подтягивает актуальное состояние веток репозитория(-ев) перед
// ревью: без этого проверяться будет вчерашнее состояние.
//
// RepoPath не обязан быть git-репозиторием сам по себе: это может быть общий
// корень с несколькими независимыми репозиториями как подпапками (типовая
// раскладка монорепо-из-репо) — в этом случае обновляются все найденные.
// Go по-прежнему не решает, какой из репозиториев относится к задаче, это
// делает /review через свой доступ к Bash; здесь только "обнови всё, что
// нашлось", без анализа конкретной задачи.
func (r *Runner) GitFetch(ctx context.Context) error {
	repos, err := discoverGitRepos(r.RepoPath)
	if err != nil {
		return fmt.Errorf("discover git repositories under %s: %w", r.RepoPath, err)
	}
	if len(repos) == 0 {
		return fmt.Errorf("no git repository found at or under %s", r.RepoPath)
	}

	var failures []string
	for _, repoDir := range repos {
		cmd := exec.CommandContext(ctx, "git", "fetch", "--prune", "origin")
		cmd.Dir = repoDir
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v (%s)", repoDir, err, bytes.TrimSpace(stderr.Bytes())))
		}
	}

	// Прогон падает только если не обновился ни один репозиторий — частичный
	// сбой (например, у одного репо временно недоступен origin) не должен
	// останавливать ревью остальных.
	if len(failures) == len(repos) {
		return fmt.Errorf("git fetch failed for all %d repositories: %s", len(repos), strings.Join(failures, "; "))
	}
	return nil
}

// discoverGitRepos ищет git-репозитории в root и среди его прямых подпапок
// (без рекурсии вглубь) — этого достаточно и для одиночного репозитория
// (root сам содержит .git), и для раскладки "корень + репозитории рядом".
func discoverGitRepos(root string) ([]string, error) {
	var repos []string

	if isGitRepo(root) {
		repos = append(repos, root)
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		dir := filepath.Join(root, e.Name())
		if isGitRepo(dir) {
			repos = append(repos, dir)
		}
	}
	return repos, nil
}

func isGitRepo(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

// SpecFilePath возвращает путь, по которому /spec обязан сохранить ТЗ задачи.
func (r *Runner) SpecFilePath(taskID string) string {
	return filepath.Join(r.RepoPath, ".claude", "specs", taskID+".md")
}

// RunSpec запускает "/spec\n<url задачи>" отдельной сессией claude. Команда
// сама пишет .claude/specs/<ID>.md — вызывающая сторона проверяет файл
// после возврата (см. RunSpec в очереди задач и Требование 5.2).
func (r *Runner) RunSpec(ctx context.Context, taskURL string) (sessionID string, usage TokenUsage, err error) {
	prompt := "/spec\n" + taskURL
	result, stderr, err := r.run(ctx, prompt)
	if err != nil {
		return result.SessionID, result.tokenUsage(), fmt.Errorf("/spec run failed: %w (stderr: %s)", err, truncate(stderr, 2000))
	}
	return result.SessionID, result.tokenUsage(), nil
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
			Usage:     result.tokenUsage(),
		}, fmt.Errorf("/review run failed: %w (stderr: %s)", err, truncate(stderr, 2000))
	}

	return Result{
		Output:    result.Result,
		SessionID: result.SessionID,
		Stderr:    stderr,
		Verdict:   ParseVerdict(result.Result),
		Usage:     result.tokenUsage(),
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
