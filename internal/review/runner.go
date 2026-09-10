package review

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// ErrUsageLimit — claude сообщил об исчерпанном лимите использования (usage
// limit/rate limit подписки или API), а не о реальном сбое. Вызывающая
// сторона (см. очередь) должна поставить проверку этой задачи на паузу и
// дать сверке повторить попытку позже, а не считать прогон окончательно
// проваленным. Проверяется через errors.Is — RunSpec/RunReview оборачивают
// эту ошибку через %w.
var ErrUsageLimit = errors.New("claude usage limit reached")

// usageLimitPattern ищет в тексте ответа claude или в stderr процесса
// признаки исчерпанного лимита использования. claude -p в этом случае
// завершается штатно (is_error=true, subtype="error_during_execution") и
// не даёт отдельного структурированного поля с точным временем сброса —
// поэтому определяем по формулировке, а не по коду ошибки.
var usageLimitPattern = regexp.MustCompile(`(?i)usage limit|rate limit|usage credit|credit balance is too low|out of usage credits|quota exceeded|overloaded_error`)

func isUsageLimitMessage(s string) bool {
	return usageLimitPattern.MatchString(s)
}

// Runner запускает claude -p в рабочей копии репозитория. ClaudeBinary
// подставляется в тестах вместо реального "claude".
type Runner struct {
	RepoPath     string
	Home         string
	ClaudeBinary string
	// Model/Effort — флаги --model/--effort (пусто — использовать выбор
	// claude по умолчанию). См. config.Config.ClaudeModel/ClaudeEffort.
	Model  string
	Effort string
}

// NewRunner создаёт Runner с настоящим бинарником claude.
func NewRunner(repoPath, home, model, effort string) *Runner {
	return &Runner{RepoPath: repoPath, Home: home, ClaudeBinary: "claude", Model: model, Effort: effort}
}

// Result — итог запуска /review: полный текст ревью, id сессии для лога и
// ссылки на прогон, служебные поля вызова claude (для лога и сохранения
// в БД, см. store.ClaudeInvocation) и разобранный вердикт.
type Result struct {
	Output     string
	SessionID  string
	Stderr     string
	Subtype    string
	IsError    bool
	Verdict    Verdict
	Usage      TokenUsage
	Prompt     string
	StartedAt  time.Time
	FinishedAt time.Time
}

// SpecResult — итог запуска /spec: содержимое собранного ТЗ (Content —
// то, что раньше писалось в файл самой командой claude; теперь claude
// только выводит текст в ответ, а сохраняет его вызывающая сторона —
// см. Требование «ТЗ хранится в БД, не в файле») плюс те же служебные поля,
// что и у Result.
type SpecResult struct {
	Content    string
	SessionID  string
	Stderr     string
	Subtype    string
	IsError    bool
	Usage      TokenUsage
	Prompt     string
	StartedAt  time.Time
	FinishedAt time.Time
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

// claudeJSONResult отражает нужные поля финальной строки `claude -p
// --output-format stream-json` (type="result") — по форме совпадает с тем,
// что раньше отдавал --output-format json целиком.
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

// SpecFilePath возвращает путь, по которому вызывающая сторона обязана
// сохранить ТЗ задачи, собранное /spec (см. Требование: ТЗ хранится в БД —
// БД является источником истины, этот файл — производный от неё артефакт,
// нужный только затем, что /review читает ТЗ из файла тулом Read).
//
// Каталог специально НЕ внутри .claude/: Claude Code относит всё под
// .claude/ к чувствительным путям и блокирует запись в них тулом Write
// независимо от --allowedTools (проверено эмпирически). Здесь это уже не
// имеет значения для самого /spec (он больше не пишет файл — это делает Go
// через os.WriteFile), но /review по-прежнему ищет готовый документ по
// этому пути, поэтому каталог остаётся прежним.
func (r *Runner) SpecFilePath(taskID string) string {
	return filepath.Join(r.RepoPath, "specs", taskID+".md")
}

// RunSpec запускает "/spec\n<url задачи>" отдельной сессией claude и
// возвращает собранное ТЗ как обычный текст ответа — команда больше не
// сохраняет файл сама (см. .claude/commands/spec.md, раздел 6): сохранение
// в БД и на диск делает вызывающая сторона (см. queue.stageSpec).
func (r *Runner) RunSpec(ctx context.Context, taskURL string, onUsage func(TokenUsage)) (SpecResult, error) {
	prompt := "/spec\n" + taskURL
	started := time.Now()
	result, stderr, err := r.run(ctx, prompt, onUsage)
	sr := SpecResult{
		Content: result.Result, SessionID: result.SessionID, Stderr: stderr,
		Subtype: result.Subtype, IsError: result.IsError, Usage: result.tokenUsage(),
		Prompt: prompt, StartedAt: started, FinishedAt: time.Now(),
	}
	if err != nil {
		return sr, fmt.Errorf("/spec run failed: %w (stderr: %s)", err, truncate(stderr, 2000))
	}
	return sr, nil
}

// RunReview запускает "/review\n<url задачи>[\n<путь к спеке>]" отдельной
// сессией claude и возвращает полный вывод, id сессии и разобранный вердикт.
// specPath пустой означает, что готового ТЗ нет: /review соберёт его сама.
func (r *Runner) RunReview(ctx context.Context, taskURL, specPath string, onUsage func(TokenUsage)) (Result, error) {
	prompt := "/review\n" + taskURL
	if specPath != "" {
		prompt += "\n" + specPath
	}

	started := time.Now()
	result, stderr, err := r.run(ctx, prompt, onUsage)
	res := Result{
		SessionID: result.SessionID, Stderr: stderr, Subtype: result.Subtype, IsError: result.IsError,
		Usage: result.tokenUsage(), Prompt: prompt, StartedAt: started, FinishedAt: time.Now(),
	}
	if err != nil {
		res.Verdict = Verdict{Status: StatusBlocked}
		return res, fmt.Errorf("/review run failed: %w (stderr: %s)", err, truncate(stderr, 2000))
	}

	res.Output = result.Result
	res.Verdict = ParseVerdict(result.Result)
	return res, nil
}

// streamEventHead — только то, что нужно, чтобы понять тип строки потокового
// вывода claude (--output-format stream-json), не разбирая её целиком.
type streamEventHead struct {
	Type string `json:"type"`
}

// streamAssistantUsage — промежуточное потребление токенов одного сообщения
// ассистента внутри сессии. Используется для обновления расхода токенов
// в БД по ходу выполнения, до завершения всего вызова claude (Требование:
// видеть расход токенов в реальном времени, а не только по окончании этапа).
type streamAssistantUsage struct {
	Message struct {
		Usage claudeUsage `json:"usage"`
	} `json:"message"`
}

// usageReportInterval — не чаще какого интервала дёргать onUsage: сообщения
// ассистента могут идти пачками (в частности, при использовании инструментов),
// а каждый вызов onUsage — это, как правило, запись в БД; ограничение
// оставляет ощущение "почти реального времени", не создавая паразитную
// нагрузку на единственное соединение к SQLite.
const usageReportInterval = 2 * time.Second

// run выполняет один процесс claude -p в потоковом режиме и возвращает
// разобранную финальную строку ("result"). onUsage (может быть nil)
// вызывается по ходу выполнения с токенами, потреблёнными до этого момента
// сессии, — это и есть отслеживание расхода в реальном времени; финальные
// точные цифры (включая стоимость) всё равно берутся из результирующей
// строки после завершения процесса.
func (r *Runner) run(ctx context.Context, prompt string, onUsage func(TokenUsage)) (claudeJSONResult, string, error) {
	binary := r.ClaudeBinary
	if binary == "" {
		binary = "claude"
	}

	// Список — объединение allowed-tools обеих команд (.claude/commands/spec.md
	// и review.md): обеим нужен Bash(notion-cli:*) для сбора ТЗ из Notion.
	// Write здесь больше не нужен — ни /spec, ни /review не пишут файлы сами
	// (см. SpecFilePath и .claude/commands/spec.md, раздел 6): наименьший
	// достаточный набор прав для пайплайна, обрабатывающего непроверенное
	// содержимое задач ClickUp.
	args := []string{"-p", prompt,
		"--output-format", "stream-json", "--verbose",
		"--allowedTools", "Bash(git:*)", "Bash(cup:*)", "Bash(notion-cli:*)", "Read", "Grep", "Glob"}
	if r.Model != "" {
		args = append(args, "--model", r.Model)
	}
	if r.Effort != "" {
		args = append(args, "--effort", r.Effort)
	}

	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = r.RepoPath
	// os.Environ() дополняется, а не заменяется: иначе claude не увидит
	// CLAUDE_CODE_OAUTH_TOKEN и упадёт на авторизации.
	cmd.Env = append(os.Environ(), "HOME="+r.Home)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return claudeJSONResult{}, "", fmt.Errorf("open claude stdout pipe: %w", err)
	}
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		return claudeJSONResult{}, "", fmt.Errorf("start claude process: %w", err)
	}

	var final claudeJSONResult
	var haveFinal bool
	var lastReport time.Time

	scanner := bufio.NewScanner(stdout)
	// Строка потокового вывода — это, в частности, целиком текст итогового
	// ревью; дефолтный буфер bufio.Scanner (64KiB) на нём переполняется.
	scanner.Buffer(make([]byte, 64*1024), 32*1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}

		var head streamEventHead
		if err := json.Unmarshal(line, &head); err != nil {
			continue
		}

		switch head.Type {
		case "result":
			json.Unmarshal(line, &final)
			haveFinal = true
		case "assistant":
			if onUsage == nil {
				continue
			}
			var evt streamAssistantUsage
			if err := json.Unmarshal(line, &evt); err != nil {
				continue
			}
			if evt.Message.Usage.InputTokens == 0 && evt.Message.Usage.OutputTokens == 0 {
				continue
			}
			if time.Since(lastReport) < usageReportInterval {
				continue
			}
			lastReport = time.Now()
			onUsage(TokenUsage{
				InputTokens:  evt.Message.Usage.InputTokens,
				OutputTokens: evt.Message.Usage.OutputTokens,
			})
		}
	}
	scanErr := scanner.Err()
	runErr := cmd.Wait()
	stderr := stderrBuf.String()

	if !haveFinal {
		if runErr != nil {
			if isUsageLimitMessage(stderr) {
				return claudeJSONResult{}, stderr, fmt.Errorf("%w: %s", ErrUsageLimit, truncate(stderr, 500))
			}
			return claudeJSONResult{}, stderr, fmt.Errorf("claude process failed: %w", runErr)
		}
		detail := "no result event in claude output"
		if scanErr != nil {
			detail = scanErr.Error()
		}
		return claudeJSONResult{}, stderr, fmt.Errorf("parse claude json output: %s", detail)
	}

	if runErr != nil {
		if isUsageLimitMessage(final.Result) || isUsageLimitMessage(stderr) {
			return final, stderr, fmt.Errorf("%w: %s", ErrUsageLimit, truncate(final.Result, 500))
		}
		return final, stderr, fmt.Errorf("claude process failed: %w", runErr)
	}
	if final.IsError {
		if isUsageLimitMessage(final.Result) {
			return final, stderr, fmt.Errorf("%w: %s", ErrUsageLimit, truncate(final.Result, 500))
		}
		return final, stderr, fmt.Errorf("claude reported an error: %s", final.Result)
	}

	return final, stderr, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}
