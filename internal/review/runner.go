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
	"sync"
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

	// modelMu защищает model/effort — значения по умолчанию для вызовов, у
	// которых нет собственного переопределения (см. CallOptions). Меняются
	// на лету через SetModelEffort (веб-интерфейс, см. Требование «менять
	// модель и effort в веб-интерфейсе»), пока другие горутины могут в этот
	// момент читать их для уже идущего вызова — отсюда мьютекс, а не голые
	// строковые поля.
	modelMu sync.RWMutex
	model   string
	effort  string
}

// NewRunner создаёт Runner с настоящим бинарником claude.
func NewRunner(repoPath, home, model, effort string) *Runner {
	r := &Runner{RepoPath: repoPath, Home: home, ClaudeBinary: "claude"}
	r.SetModelEffort(model, effort)
	return r
}

// SetModelEffort меняет модель/effort по умолчанию для всех последующих
// вызовов, у которых нет собственного переопределения в CallOptions, —
// используется веб-интерфейсом, чтобы поменять их без перезапуска сервиса.
// Уже идущие вызовы claude этим не затрагиваются (значение читается один
// раз в начале run).
func (r *Runner) SetModelEffort(model, effort string) {
	r.modelMu.Lock()
	defer r.modelMu.Unlock()
	r.model = model
	r.effort = effort
}

// ModelEffort возвращает текущие модель/effort по умолчанию (см.
// SetModelEffort) — используется, например, чтобы показать их в
// веб-интерфейсе.
func (r *Runner) ModelEffort() (model, effort string) {
	r.modelMu.RLock()
	defer r.modelMu.RUnlock()
	return r.model, r.effort
}

// Result — итог запуска /review: полный текст ревью, id сессии для лога и
// ссылки на прогон, служебные поля вызова claude (для лога и сохранения
// в БД, см. store.ClaudeInvocation) и разобранный вердикт.
type Result struct {
	Output    string
	SessionID string
	Stderr    string
	Subtype   string
	IsError   bool
	Verdict   Verdict
	Usage     TokenUsage
	Prompt    string
	// Model/Effort — то, что было реально использовано для этого вызова
	// (после разрешения CallOptions.Model/Effort против текущих значений
	// по умолчанию, см. Runner.SetModelEffort) — именно это, а не текущая
	// конфигурация сервиса, должно попадать в лог/БД (см.
	// store.ClaudeInvocation): конфигурация могла уже смениться к моменту
	// записи, а этот вызов был сделан с тем, что было тогда.
	Model      string
	Effort     string
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
	Model      string
	Effort     string
	StartedAt  time.Time
	FinishedAt time.Time
}

// CallOptions — параметры одного вызова RunSpec/RunReview, помимо самой
// задачи: OnUsage — колбэк с промежуточным расходом токенов (см. run);
// Model/Effort — переопределение на этот конкретный вызов (например, при
// ручном запуске из веб-интерфейса — см. Требование «выбрать модель для
// текущей задачи»); пустая строка — использовать текущее значение по
// умолчанию (см. Runner.SetModelEffort), а не отключить флаг вовсе.
type CallOptions struct {
	OnUsage func(TokenUsage)
	Model   string
	Effort  string
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
func (r *Runner) RunSpec(ctx context.Context, taskURL string, opts CallOptions) (SpecResult, error) {
	prompt := "/spec\n" + taskURL
	started := time.Now()
	result, model, effort, stderr, err := r.run(ctx, prompt, opts)
	sr := SpecResult{
		Content: result.Result, SessionID: result.SessionID, Stderr: stderr,
		Subtype: result.Subtype, IsError: result.IsError, Usage: result.tokenUsage(),
		Prompt: prompt, Model: model, Effort: effort, StartedAt: started, FinishedAt: time.Now(),
	}
	if err != nil {
		return sr, fmt.Errorf("/spec run failed: %w (stderr: %s)", err, truncate(stderr, 2000))
	}
	return sr, nil
}

// RunReview запускает "/review\n<url задачи>[\n<путь к спеке>]" отдельной
// сессией claude и возвращает полный вывод, id сессии и разобранный вердикт.
// specPath пустой означает, что готового ТЗ нет: /review соберёт его сама.
func (r *Runner) RunReview(ctx context.Context, taskURL, specPath string, opts CallOptions) (Result, error) {
	prompt := "/review\n" + taskURL
	if specPath != "" {
		prompt += "\n" + specPath
	}

	started := time.Now()
	result, model, effort, stderr, err := r.run(ctx, prompt, opts)
	res := Result{
		SessionID: result.SessionID, Stderr: stderr, Subtype: result.Subtype, IsError: result.IsError,
		Usage: result.tokenUsage(), Prompt: prompt, Model: model, Effort: effort,
		StartedAt: started, FinishedAt: time.Now(),
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
// разобранную финальную строку ("result") вместе с фактическими model/effort
// (после разрешения opts против текущих значений по умолчанию — см.
// Runner.SetModelEffort). opts.OnUsage (может быть nil) вызывается по ходу
// выполнения с токенами, потреблёнными до этого момента сессии, — это и
// есть отслеживание расхода в реальном времени; финальные точные цифры
// (включая стоимость) всё равно берутся из результирующей строки после
// завершения процесса.
func (r *Runner) run(ctx context.Context, prompt string, opts CallOptions) (result claudeJSONResult, model, effort, stderrOut string, err error) {
	binary := r.ClaudeBinary
	if binary == "" {
		binary = "claude"
	}

	model, effort = opts.Model, opts.Effort
	if model == "" || effort == "" {
		defModel, defEffort := r.ModelEffort()
		if model == "" {
			model = defModel
		}
		if effort == "" {
			effort = defEffort
		}
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
	if model != "" {
		args = append(args, "--model", model)
	}
	if effort != "" {
		args = append(args, "--effort", effort)
	}

	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = r.RepoPath
	// os.Environ() дополняется, а не заменяется: иначе claude не увидит
	// CLAUDE_CODE_OAUTH_TOKEN и упадёт на авторизации.
	cmd.Env = append(os.Environ(), "HOME="+r.Home)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return claudeJSONResult{}, model, effort, "", fmt.Errorf("open claude stdout pipe: %w", err)
	}
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		return claudeJSONResult{}, model, effort, "", fmt.Errorf("start claude process: %w", err)
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
			if opts.OnUsage == nil {
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
			opts.OnUsage(TokenUsage{
				InputTokens:  evt.Message.Usage.InputTokens,
				OutputTokens: evt.Message.Usage.OutputTokens,
			})
		}
	}
	scanErr := scanner.Err()
	runErr := cmd.Wait()
	stderrText := stderrBuf.String()

	if !haveFinal {
		if runErr != nil {
			if isUsageLimitMessage(stderrText) {
				return claudeJSONResult{}, model, effort, stderrText, fmt.Errorf("%w: %s", ErrUsageLimit, truncate(stderrText, 500))
			}
			return claudeJSONResult{}, model, effort, stderrText, fmt.Errorf("claude process failed: %w", runErr)
		}
		detail := "no result event in claude output"
		if scanErr != nil {
			detail = scanErr.Error()
		}
		return claudeJSONResult{}, model, effort, stderrText, fmt.Errorf("parse claude json output: %s", detail)
	}

	if runErr != nil {
		if isUsageLimitMessage(final.Result) || isUsageLimitMessage(stderrText) {
			return final, model, effort, stderrText, fmt.Errorf("%w: %s", ErrUsageLimit, truncate(final.Result, 500))
		}
		return final, model, effort, stderrText, fmt.Errorf("claude process failed: %w", runErr)
	}
	if final.IsError {
		if isUsageLimitMessage(final.Result) {
			return final, model, effort, stderrText, fmt.Errorf("%w: %s", ErrUsageLimit, truncate(final.Result, 500))
		}
		return final, model, effort, stderrText, fmt.Errorf("claude reported an error: %s", final.Result)
	}

	return final, model, effort, stderrText, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}
