// Package config загружает и валидирует конфигурацию сервиса из переменных окружения.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config содержит все параметры сервиса, прочитанные из окружения при старте.
type Config struct {
	ClaudeCodeOAuthToken string
	CUAPIToken           string
	CUTeamID             string
	CUListID             string
	CUWebhookSecret      string
	SlackWebhookURL      string
	RepoPath             string
	RepoURL              string
	TriggerTag           string
	StatusTrigger        string
	StatusRunning        string
	StatusPass           string
	StatusFail           string
	AssigneeOnFail       string
	AssigneeOnPass       string
	WorkerConcurrency    int
	ReviewTimeout        time.Duration
	ReconcileInterval    time.Duration
	UsageLimitPause      time.Duration
	Port                 int
	DBPath               string
	Home                 string
	// ClaudeModel/ClaudeEffort — модель и уровень усилий для каждого вызова
	// `claude -p` (флаги --model/--effort). По умолчанию — sonnet/high, но
	// на время обкатки функционала можно временно выставить в .env более
	// дешёвую/быструю пару (например haiku/low), не трогая код.
	ClaudeModel  string
	ClaudeEffort string
}

// Load читает конфигурацию через getenv (os.Getenv в проде, произвольная map в тестах)
// и возвращает ошибку со списком всех проблем сразу, если что-то не так.
func Load(getenv func(string) string) (*Config, error) {
	if getenv == nil {
		getenv = os.Getenv
	}

	var problems []string

	required := func(name string) string {
		v := getenv(name)
		if strings.TrimSpace(v) == "" {
			problems = append(problems, fmt.Sprintf("отсутствует обязательная переменная окружения %s", name))
		}
		return v
	}

	cfg := &Config{
		ClaudeCodeOAuthToken: required("CLAUDE_CODE_OAUTH_TOKEN"),
		CUAPIToken:           required("CU_API_TOKEN"),
		CUTeamID:             required("CU_TEAM_ID"),
		CUListID:             required("CU_LIST_ID"),
		// CU_WEBHOOK_SECRET не обязателен: его отсутствие переводит сервис
		// в режим "только сверка" (см. Требование 1.2), а не является ошибкой.
		CUWebhookSecret: getenv("CU_WEBHOOK_SECRET"),
		SlackWebhookURL: required("SLACK_WEBHOOK_URL"),
		RepoPath:        required("REPO_PATH"),
		RepoURL:         getenv("REPO_URL"),
		TriggerTag:      withDefault(getenv("TRIGGER_TAG"), "ai"),
		StatusTrigger:   withDefault(getenv("STATUS_TRIGGER"), "to check"),
		StatusRunning:   withDefault(getenv("STATUS_RUNNING"), "checking"),
		StatusPass:      getenv("STATUS_PASS"),
		StatusFail:      getenv("STATUS_FAIL"),
		AssigneeOnFail:  getenv("ASSIGNEE_ON_FAIL"),
		AssigneeOnPass:  getenv("ASSIGNEE_ON_PASS"),
		DBPath:          withDefault(getenv("DB_PATH"), "/data/state.db"),
		Home:            getenv("HOME"),
		ClaudeModel:     withDefault(getenv("CLAUDE_MODEL"), "sonnet"),
		ClaudeEffort:    withDefault(getenv("CLAUDE_EFFORT"), "high"),
	}

	cfg.WorkerConcurrency = parseIntDefault(getenv("WORKER_CONCURRENCY"), 1, "WORKER_CONCURRENCY", &problems)
	cfg.Port = parseIntDefault(getenv("PORT"), 8080, "PORT", &problems)
	cfg.ReviewTimeout = parseDurationDefault(getenv("REVIEW_TIMEOUT"), 15*time.Minute, "REVIEW_TIMEOUT", &problems)
	cfg.ReconcileInterval = parseDurationDefault(getenv("RECONCILE_INTERVAL"), 5*time.Minute, "RECONCILE_INTERVAL", &problems)
	// Пауза после исчерпания лимита Claude (Требование: не проваливать
	// проверку, а ждать обновления лимитов). Сверка (ReconcileInterval) —
	// естественный "будильник" для повторной попытки, поэтому по умолчанию
	// пауза дольше него: иначе сверка снова наткнётся на тот же лимит через
	// считанные минуты.
	cfg.UsageLimitPause = parseDurationDefault(getenv("USAGE_LIMIT_PAUSE"), 30*time.Minute, "USAGE_LIMIT_PAUSE", &problems)

	if len(problems) > 0 {
		return nil, fmt.Errorf("некорректная конфигурация:\n  - %s", strings.Join(problems, "\n  - "))
	}

	return cfg, nil
}

// NormalizeStatus приводит название колонки/тега к виду для сравнения:
// без учёта регистра и лишних пробелов по краям.
func NormalizeStatus(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

func withDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func parseIntDefault(v string, def int, name string, problems *[]string) int {
	if strings.TrimSpace(v) == "" {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		*problems = append(*problems, fmt.Sprintf("%s должно быть целым числом, получено %q", name, v))
		return def
	}
	return n
}

func parseDurationDefault(v string, def time.Duration, name string, problems *[]string) time.Duration {
	if strings.TrimSpace(v) == "" {
		return def
	}
	d, err := time.ParseDuration(strings.TrimSpace(v))
	if err != nil {
		*problems = append(*problems, fmt.Sprintf("%s должно быть длительностью вида \"5m\", получено %q", name, v))
		return def
	}
	return d
}
