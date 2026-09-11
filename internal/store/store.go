// Package store хранит состояние прогонов ревью в SQLite: таблица runs
// используется для дедупликации событий, восстановления после рестарта и
// как источник статистики (/api/status, /api/stats).
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// Статусы прогона.
const (
	StatusQueued  = "queued"
	StatusRunning = "running"
	StatusDone    = "done"
	StatusFailed  = "failed"
	// StatusPaused — прогон остановлен из-за исчерпанного лимита Claude
	// (usage limit/rate limit), не из-за реальной ошибки. Как и failed, не
	// блокирует повторную постановку задачи в очередь (см. TryEnqueue) —
	// когда лимит освободится, сверка подберёт задачу заново.
	StatusPaused = "paused"
	// StatusInterrupted — прогон был прерван падением/убийством процесса
	// (см. RecoverFromRestart), в отличие от StatusFailed (реальная ошибка
	// обработки) не считается провалом: ReopenOrEnqueue находит такой
	// прогон по task_id и продолжает его же (тот же run_id, значит и уже
	// пройденные этапы из run_stages), а не начинает с нуля.
	StatusInterrupted = "interrupted"
)

// Run — одна запись таблицы runs.
type Run struct {
	ID     int64
	TaskID string
	// CustomID — человекочитаемый ID задачи в ClickUp (например, "PNL-4528"),
	// если он был на ней задан (см. SetRunTaskInfo) — отдельно от TaskID
	// (нативный ID ClickUp), чтобы дашборд показывал знакомое имя задачи, а
	// не непрозрачный ID. Пусто для прогонов, начатых до этой возможности,
	// или если у задачи не задан кастомный ID.
	CustomID string
	// TaskName — название задачи в ClickUp на момент старта прогона (см.
	// SetRunTaskInfo) — вместе с CustomID/TaskID нужно дашборду, чтобы
	// показать ссылку на задачу в таблице статистики с названием во
	// всплывающей подсказке, не обращаясь к ClickUp заново на каждый прогон
	// из истории (см. Требование «в статистике — ссылка на задачу с
	// названием при наведении»).
	TaskName     string
	Status       string
	Verdict      string
	SessionID    string
	StartedAt    time.Time
	FinishedAt   sql.NullTime
	Error        string
	InputTokens  int64
	OutputTokens int64
	CostUSD      float64
	// SlackMessage — точный текст последнего уведомления о результате
	// ревью, отправленного в Slack (см. Queue.notifyReviewResult) — чтобы
	// дашборд мог показать его по клику, не восстанавливая заново из
	// вердикта (см. Требование «посмотреть коммент, отправленный в slack»).
	SlackMessage string
}

// Usage — токены и стоимость одного прогона (сумма /spec-go + /review-go).
type Usage struct {
	InputTokens  int64
	OutputTokens int64
	CostUSD      float64
}

// Stats — агрегированная статистика прогонов за период (см. /api/stats).
type Stats struct {
	Since     time.Time
	TotalRuns int
	ByStatus  map[string]int
	ByVerdict map[string]int
	Tokens    Usage
	Runs      []Run
}

// Статусы этапа прогона (таблица run_stages).
const (
	StageStatusRunning = "running"
	StageStatusDone    = "done"
	StageStatusFailed  = "failed"
)

// RunStage — одна запись таблицы run_stages: журнал того, что уже сделано
// в рамках одного прогона (run_id), с данными, нужными, чтобы при
// возобновлении не повторять этот этап заново (см. Queue.stage).
type RunStage struct {
	RunID      int64           `json:"run_id"`
	Stage      string          `json:"stage"`
	Status     string          `json:"status"`
	StartedAt  time.Time       `json:"started_at"`
	FinishedAt sql.NullTime    `json:"finished_at"`
	Error      string          `json:"error"`
	Data       json.RawMessage `json:"data"`
}

// ClaudeInvocation — один вызов `claude -p` целиком: полный запрос и полный
// ответ claude, отдельно от run_stages (та таблица хранит только то, что
// нужно для возобновления прогона). Нужна для аудита реального расхода
// токенов по каждому вызову и для хранения полного текста ответа сессии
// (см. README, раздел «Таблицы claude_invocations и run_stages»).
type ClaudeInvocation struct {
	ID           int64     `json:"id"`
	RunID        int64     `json:"run_id"`
	Stage        string    `json:"stage"`
	SessionID    string    `json:"session_id"`
	Model        string    `json:"model"`
	Effort       string    `json:"effort"`
	Prompt       string    `json:"prompt"`
	Output       string    `json:"output"`
	Stderr       string    `json:"stderr"`
	Subtype      string    `json:"subtype"`
	IsError      bool      `json:"is_error"`
	InputTokens  int64     `json:"input_tokens"`
	OutputTokens int64     `json:"output_tokens"`
	CostUSD      float64   `json:"cost_usd"`
	StartedAt    time.Time `json:"started_at"`
	FinishedAt   time.Time `json:"finished_at"`
}

// Store — обёртка над SQLite-базой с таблицей runs.
type Store struct {
	db *sql.DB
}

// Open открывает (создавая при необходимости) SQLite-файл по path,
// применяет схему и докатывает недостающие колонки на уже существующей базе.
// Единственное открытое соединение сериализует доступ, что вместе с
// атомарным INSERT...WHERE NOT EXISTS в TryEnqueue исключает гонки между
// вебхуком и сверкой при постановке в очередь.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite db %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema to %s: %w", path, err)
	}

	if err := migrateRunColumns(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate run columns on %s: %w", path, err)
	}

	return &Store{db: db}, nil
}

const schema = `
CREATE TABLE IF NOT EXISTS runs (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	task_id     TEXT NOT NULL,
	status      TEXT NOT NULL,
	verdict     TEXT NOT NULL DEFAULT '',
	session_id  TEXT NOT NULL DEFAULT '',
	started_at  DATETIME NOT NULL,
	finished_at DATETIME,
	error       TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_runs_task_id ON runs(task_id);
CREATE INDEX IF NOT EXISTS idx_runs_task_status ON runs(task_id, status);

-- run_stages — журнал этапов одного прогона (setup/git_fetch/spec/review/
-- decide/comment/finalize, см. Queue.stage). Каждый этап — не более одной
-- строки на run_id (UNIQUE), data хранит то, что нужно для продолжения без
-- повторного вызова claude при возобновлении прерванного/поставленного на
-- паузу прогона (тот же run_id — см. ReopenOrEnqueue).
CREATE TABLE IF NOT EXISTS run_stages (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	run_id      INTEGER NOT NULL,
	stage       TEXT NOT NULL,
	status      TEXT NOT NULL,
	started_at  DATETIME NOT NULL,
	finished_at DATETIME,
	error       TEXT NOT NULL DEFAULT '',
	data        TEXT NOT NULL DEFAULT '{}',
	UNIQUE(run_id, stage)
);
CREATE INDEX IF NOT EXISTS idx_run_stages_run_id ON run_stages(run_id);

-- claude_invocations — полный журнал вызовов "claude -p": один вызов —
-- одна строка, с полным запросом, полным ответом и токенами именно этого
-- вызова. id растёт монотонно, поэтому порядок строк по run_id — это и есть
-- хронология сессий этого прогона (см. Требование 4: "таблица с сессиями
-- claude ... сохранять хронологию"). Отдельно от run_stages: там — только
-- то, что нужно для возобновления, здесь — полный лог для аудита расхода
-- токенов и разбора ответов claude, без влияния на логику возобновления.
CREATE TABLE IF NOT EXISTS claude_invocations (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	run_id        INTEGER NOT NULL,
	stage         TEXT NOT NULL,
	session_id    TEXT NOT NULL DEFAULT '',
	model         TEXT NOT NULL DEFAULT '',
	effort        TEXT NOT NULL DEFAULT '',
	prompt        TEXT NOT NULL DEFAULT '',
	output        TEXT NOT NULL DEFAULT '',
	stderr        TEXT NOT NULL DEFAULT '',
	subtype       TEXT NOT NULL DEFAULT '',
	is_error      INTEGER NOT NULL DEFAULT 0,
	input_tokens  INTEGER NOT NULL DEFAULT 0,
	output_tokens INTEGER NOT NULL DEFAULT 0,
	cost_usd      REAL NOT NULL DEFAULT 0,
	started_at    DATETIME NOT NULL,
	finished_at   DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_claude_invocations_run_id ON claude_invocations(run_id);

-- settings — простое хранилище "ключ-значение" для настроек, которые можно
-- менять на лету через веб-интерфейс (сейчас — модель/effort claude, см.
-- GetSetting/SetSetting), без перезапуска сервиса и без потери значения
-- при перезапуске (в отличие от .env, который читается только один раз
-- при старте).
CREATE TABLE IF NOT EXISTS settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
`

// migrateRunColumns докатывает недостающие колонки на базу, созданную более
// ранней версией сервиса (учёт токенов, затем custom_id — см. Run.CustomID).
// ALTER TABLE ADD COLUMN в SQLite не поддерживает IF NOT EXISTS во всех
// версиях, поэтому наличие колонки проверяется через PRAGMA table_info вручную.
func migrateRunColumns(db *sql.DB) error {
	existing := map[string]bool{}
	rows, err := db.Query(`PRAGMA table_info(runs)`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notNull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		existing[name] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()

	columns := []struct{ name, decl string }{
		{"input_tokens", "INTEGER NOT NULL DEFAULT 0"},
		{"output_tokens", "INTEGER NOT NULL DEFAULT 0"},
		{"cost_usd", "REAL NOT NULL DEFAULT 0"},
		{"custom_id", "TEXT NOT NULL DEFAULT ''"},
		{"task_name", "TEXT NOT NULL DEFAULT ''"},
		{"slack_message", "TEXT NOT NULL DEFAULT ''"},
	}
	for _, c := range columns {
		if existing[c.name] {
			continue
		}
		if _, err := db.Exec(fmt.Sprintf(`ALTER TABLE runs ADD COLUMN %s %s`, c.name, c.decl)); err != nil {
			return fmt.Errorf("add column %s: %w", c.name, err)
		}
	}
	return nil
}

// Close закрывает базу данных.
func (s *Store) Close() error {
	return s.db.Close()
}

// TryEnqueue атомарно создаёт запись о прогоне в статусе queued, если по
// этой задаче нет записи в состоянии queued или running — только они
// означают уже идущую обработку того же запроса (тот же тег ещё не снят).
// Возвращает enqueued=false, если такая запись уже блокирует постановку
// (дедупликация повторного вебхука/скана по одному и тому же запросу).
//
// done намеренно НЕ блокирует: тег-триггер снимается только на этапе decide
// успешно завершённого прогона (см. Queue.runReview) — то есть к моменту,
// когда прогон становится done, тег уже снят, и задача больше не матчится
// условием триггера при сверке. Если тег и статус на задаче снова совпали
// с условием — это не эхо старого запроса, а осознанный новый запрос на
// повторную проверку (человек перетегировал задачу руками), и его нужно
// обработать, а не отбросить молча из-за того, что когда-то раньше эта же
// задача уже проверялась. failed так же не блокирует — это заведомо не
// результат ревью, а инфраструктурный сбой, тег в этом случае не снимается,
// и повторную попытку должна суметь запустить сама сверка.
func (s *Store) TryEnqueue(ctx context.Context, taskID string) (runID int64, enqueued bool, err error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO runs (task_id, status, started_at)
		SELECT ?, ?, ?
		WHERE NOT EXISTS (
			SELECT 1 FROM runs WHERE task_id = ? AND status IN (?, ?)
		)
	`, taskID, StatusQueued, time.Now().UTC(), taskID, StatusQueued, StatusRunning)
	if err != nil {
		return 0, false, fmt.Errorf("enqueue task %s: %w", taskID, err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return 0, false, fmt.Errorf("enqueue task %s: rows affected: %w", taskID, err)
	}
	if affected == 0 {
		return 0, false, nil
	}

	id, err := res.LastInsertId()
	if err != nil {
		return 0, false, fmt.Errorf("enqueue task %s: last insert id: %w", taskID, err)
	}
	return id, true, nil
}

// ReopenOrEnqueue — основная точка постановки задачи в очередь (см.
// Queue.processTask/resumeTask). Если для task_id уже есть прогон в
// возобновляемом состоянии (StatusPaused — пауза на лимите, StatusInterrupted
// — прервано падением процесса), переоткрывает именно его: тот же run_id,
// значит и уже пройденные этапы из run_stages не нужно проходить заново
// (см. Queue.stage). Иначе ведёт себя как обычный TryEnqueue: создаёт новый
// прогон, если по задаче нет активной/завершённой записи.
//
// StatusFailed (настоящая ошибка обработки, не лимит и не падение процесса)
// сюда намеренно не входит — такой прогон должен начинаться с чистого листа
// после вмешательства человека, а не молча переиспользовать состояние
// сломанной попытки.
//
// Единственное открытое соединение к базе (см. Open) сериализует доступ,
// поэтому гонки между воркерами здесь исключены без отдельных транзакций.
func (s *Store) ReopenOrEnqueue(ctx context.Context, taskID string) (runID int64, enqueued bool, err error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id FROM runs
		WHERE task_id = ? AND status IN (?, ?)
		ORDER BY id DESC LIMIT 1
	`, taskID, StatusPaused, StatusInterrupted)

	var existingID int64
	switch scanErr := row.Scan(&existingID); {
	case scanErr == nil:
		if _, err := s.db.ExecContext(ctx, `
			UPDATE runs SET status = ?, error = '', finished_at = NULL WHERE id = ?
		`, StatusRunning, existingID); err != nil {
			return 0, false, fmt.Errorf("reopen run %d for task %s: %w", existingID, taskID, err)
		}
		return existingID, true, nil
	case errors.Is(scanErr, sql.ErrNoRows):
		return s.TryEnqueue(ctx, taskID)
	default:
		return 0, false, fmt.Errorf("reopen or enqueue task %s: %w", taskID, scanErr)
	}
}

// MarkRunning переводит прогон в статус running.
func (s *Store) MarkRunning(ctx context.Context, runID int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE runs SET status = ? WHERE id = ?`, StatusRunning, runID)
	if err != nil {
		return fmt.Errorf("mark run %d running: %w", runID, err)
	}
	return nil
}

// SetRunTaskInfo записывает человекочитаемый ID задачи (например,
// "PNL-4528") и её название на уже созданную запись прогона — вызывается
// сразу после ReopenOrEnqueue/TryEnqueue, как только известна карточка
// задачи (см. Requirement: дашборд должен показывать знакомое имя задачи, а
// не непрозрачный ID ClickUp, и ссылку на задачу с названием во всплывающей
// подсказке в таблице статистики). Пустые значения — не редкость (не у
// каждой задачи задан custom ID) и не считаются ошибкой.
func (s *Store) SetRunTaskInfo(ctx context.Context, runID int64, customID, name string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE runs SET custom_id = ?, task_name = ? WHERE id = ?`, customID, name, runID)
	if err != nil {
		return fmt.Errorf("set task info for run %d: %w", runID, err)
	}
	return nil
}

// SetRunSlackMessage записывает точный текст последнего уведомления в
// Slack о результате ревью (см. Queue.notifyReviewResult) — чтобы дашборд
// мог показать его по клику на прогоне из истории, без восстановления по
// вердикту (см. Требование «посмотреть коммент, отправленный в slack»).
func (s *Store) SetRunSlackMessage(ctx context.Context, runID int64, text string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE runs SET slack_message = ? WHERE id = ?`, text, runID)
	if err != nil {
		return fmt.Errorf("set slack message for run %d: %w", runID, err)
	}
	return nil
}

// MarkDone переводит прогон в статус done с вердиктом, session id и
// суммарным расходом токенов/стоимости на эту задачу.
func (s *Store) MarkDone(ctx context.Context, runID int64, verdict, sessionID string, usage Usage) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE runs
		SET status = ?, verdict = ?, session_id = ?, finished_at = ?,
		    input_tokens = ?, output_tokens = ?, cost_usd = ?
		WHERE id = ?
	`, StatusDone, verdict, sessionID, time.Now().UTC(), usage.InputTokens, usage.OutputTokens, usage.CostUSD, runID)
	if err != nil {
		return fmt.Errorf("mark run %d done: %w", runID, err)
	}
	return nil
}

// MarkFailed переводит прогон в статус failed. Failed-прогоны не блокируют
// повторную постановку задачи в очередь.
func (s *Store) MarkFailed(ctx context.Context, runID int64, sessionID, errMsg string, usage Usage) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE runs
		SET status = ?, session_id = ?, error = ?, finished_at = ?,
		    input_tokens = ?, output_tokens = ?, cost_usd = ?
		WHERE id = ?
	`, StatusFailed, sessionID, errMsg, time.Now().UTC(), usage.InputTokens, usage.OutputTokens, usage.CostUSD, runID)
	if err != nil {
		return fmt.Errorf("mark run %d failed: %w", runID, err)
	}
	return nil
}

// MarkPaused переводит прогон в статус paused: claude сообщил об исчерпанном
// лимите использования, это не результат ревью и не сбой — сервис сам
// поставит проверку на паузу и повторит её позже (см. Queue.pauseFor).
func (s *Store) MarkPaused(ctx context.Context, runID int64, sessionID, errMsg string, usage Usage) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE runs
		SET status = ?, session_id = ?, error = ?, finished_at = ?,
		    input_tokens = ?, output_tokens = ?, cost_usd = ?
		WHERE id = ?
	`, StatusPaused, sessionID, errMsg, time.Now().UTC(), usage.InputTokens, usage.OutputTokens, usage.CostUSD, runID)
	if err != nil {
		return fmt.Errorf("mark run %d paused: %w", runID, err)
	}
	return nil
}

// RecoveredRun — один прогон, найденный в состоянии running при старте
// сервиса (см. RecoverFromRestart).
type RecoveredRun struct {
	RunID  int64
	TaskID string
}

// RecoverFromRestart переводит все прогоны в состоянии running в interrupted
// при старте сервиса: контейнер мог перезапуститься посреди ревью, зависшая
// задача не должна блокировать повторную обработку. В отличие от StatusFailed,
// interrupted — возобновляемый статус (см. ReopenOrEnqueue): вызывающая
// сторона доводит прогон до конца тем же run_id (см. Queue.SubmitResume),
// переиспользуя уже пройденные этапы из run_stages, а не начиная с нуля.
func (s *Store) RecoverFromRestart(ctx context.Context) ([]RecoveredRun, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, task_id FROM runs WHERE status = ?`, StatusRunning)
	if err != nil {
		return nil, fmt.Errorf("recover from restart: list running: %w", err)
	}
	var recovered []RecoveredRun
	for rows.Next() {
		var r RecoveredRun
		if err := rows.Scan(&r.RunID, &r.TaskID); err != nil {
			rows.Close()
			return nil, fmt.Errorf("recover from restart: scan run: %w", err)
		}
		recovered = append(recovered, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("recover from restart: iterate rows: %w", err)
	}
	rows.Close()

	if _, err := s.db.ExecContext(ctx, `
		UPDATE runs SET status = ?, error = ?
		WHERE status = ?
	`, StatusInterrupted, "restarted: service was terminated mid-run", StatusRunning); err != nil {
		return nil, fmt.Errorf("recover from restart: %w", err)
	}
	return recovered, nil
}

// GetStage возвращает запись этапа прогона, если она есть. ok=false и без
// ошибки означает, что этап ещё ни разу не запускался для этого run_id —
// обычное дело для нового прогона, не повод логировать ошибку.
func (s *Store) GetStage(ctx context.Context, runID int64, stage string) (rs RunStage, ok bool, err error) {
	var data string
	err = s.db.QueryRowContext(ctx, `
		SELECT run_id, stage, status, started_at, finished_at, error, data
		FROM run_stages WHERE run_id = ? AND stage = ?
	`, runID, stage).Scan(&rs.RunID, &rs.Stage, &rs.Status, &rs.StartedAt, &rs.FinishedAt, &rs.Error, &data)
	if errors.Is(err, sql.ErrNoRows) {
		return RunStage{}, false, nil
	}
	if err != nil {
		return RunStage{}, false, fmt.Errorf("get stage %s of run %d: %w", stage, runID, err)
	}
	rs.Data = json.RawMessage(data)
	return rs, true, nil
}

// StartStage создаёт (или сбрасывает на running, если этап уже начинался,
// но не был отмечен готовым/проваленным — например, оборвался вместе со
// всем процессом) запись о начале этапа.
func (s *Store) StartStage(ctx context.Context, runID int64, stage string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO run_stages (run_id, stage, status, started_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(run_id, stage) DO UPDATE SET
			status = excluded.status, started_at = excluded.started_at,
			finished_at = NULL, error = ''
	`, runID, stage, StageStatusRunning, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("start stage %s of run %d: %w", stage, runID, err)
	}
	return nil
}

// FinishStage отмечает этап готовым и сохраняет его результат (data — JSON,
// например id сессии claude, путь к файлу спеки, вердикт) — это и есть
// «результат /spec-go, сохранённый в табличку», который переиспользуется при
// возобновлении прогона вместо повторного вызова claude (см. Queue.stage).
func (s *Store) FinishStage(ctx context.Context, runID int64, stage string, data []byte) error {
	if len(data) == 0 {
		data = []byte("{}")
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE run_stages SET status = ?, finished_at = ?, data = ?
		WHERE run_id = ? AND stage = ?
	`, StageStatusDone, time.Now().UTC(), string(data), runID, stage)
	if err != nil {
		return fmt.Errorf("finish stage %s of run %d: %w", stage, runID, err)
	}
	return nil
}

// FailStage отмечает этап проваленным. Проваленный этап никогда не
// считается «уже готовым» — следующая попытка (StartStage) запускает его
// заново.
func (s *Store) FailStage(ctx context.Context, runID int64, stage, errMsg string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE run_stages SET status = ?, finished_at = ?, error = ?
		WHERE run_id = ? AND stage = ?
	`, StageStatusFailed, time.Now().UTC(), errMsg, runID, stage)
	if err != nil {
		return fmt.Errorf("fail stage %s of run %d: %w", stage, runID, err)
	}
	return nil
}

// ListStages возвращает все этапы прогона в порядке выполнения — для
// диагностики (например, будущего расширения /api/status).
func (s *Store) ListStages(ctx context.Context, runID int64) ([]RunStage, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT run_id, stage, status, started_at, finished_at, error, data
		FROM run_stages WHERE run_id = ? ORDER BY id ASC
	`, runID)
	if err != nil {
		return nil, fmt.Errorf("list stages of run %d: %w", runID, err)
	}
	defer rows.Close()

	var stages []RunStage
	for rows.Next() {
		var rs RunStage
		var data string
		if err := rows.Scan(&rs.RunID, &rs.Stage, &rs.Status, &rs.StartedAt, &rs.FinishedAt, &rs.Error, &data); err != nil {
			return nil, fmt.Errorf("scan stage row: %w", err)
		}
		rs.Data = json.RawMessage(data)
		stages = append(stages, rs)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate stage rows: %w", err)
	}
	return stages, nil
}

// RecordInvocation сохраняет один завершённый вызов `claude -p` целиком:
// запрос, полный ответ, метаданные и токены именно этого вызова (Требование:
// «лог процесса работы claude ... с количеством потраченных токенов на этот
// этап. И чтобы ответ всей сессии тоже писался в таблицу»). Вызывается уже
// после завершения claude — для расхода токенов по ходу выполнения
// см. UpdateRunningUsage.
func (s *Store) RecordInvocation(ctx context.Context, inv ClaudeInvocation) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO claude_invocations
			(run_id, stage, session_id, model, effort, prompt, output, stderr, subtype, is_error,
			 input_tokens, output_tokens, cost_usd, started_at, finished_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, inv.RunID, inv.Stage, inv.SessionID, inv.Model, inv.Effort, inv.Prompt, inv.Output, inv.Stderr,
		inv.Subtype, inv.IsError, inv.InputTokens, inv.OutputTokens, inv.CostUSD, inv.StartedAt.UTC(), inv.FinishedAt.UTC())
	if err != nil {
		return 0, fmt.Errorf("record claude invocation for run %d stage %s: %w", inv.RunID, inv.Stage, err)
	}
	return res.LastInsertId()
}

// ListInvocations возвращает все вызовы claude одного прогона в порядке
// выполнения — для диагностики и будущего расширения API статуса.
func (s *Store) ListInvocations(ctx context.Context, runID int64) ([]ClaudeInvocation, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, run_id, stage, session_id, model, effort, prompt, output, stderr, subtype, is_error,
		       input_tokens, output_tokens, cost_usd, started_at, finished_at
		FROM claude_invocations WHERE run_id = ? ORDER BY id ASC
	`, runID)
	if err != nil {
		return nil, fmt.Errorf("list claude invocations of run %d: %w", runID, err)
	}
	defer rows.Close()

	var invocations []ClaudeInvocation
	for rows.Next() {
		var inv ClaudeInvocation
		if err := rows.Scan(&inv.ID, &inv.RunID, &inv.Stage, &inv.SessionID, &inv.Model, &inv.Effort,
			&inv.Prompt, &inv.Output, &inv.Stderr, &inv.Subtype, &inv.IsError,
			&inv.InputTokens, &inv.OutputTokens, &inv.CostUSD, &inv.StartedAt, &inv.FinishedAt); err != nil {
			return nil, fmt.Errorf("scan claude invocation row: %w", err)
		}
		invocations = append(invocations, inv)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate claude invocation rows: %w", err)
	}
	return invocations, nil
}

// ListInvocationsSince возвращает вызовы claude по всем прогонам начиная с
// since, в хронологическом порядке — плоский "лог сессий" для дашборда
// (см. GET /api/invocations), в отличие от ListInvocations (один прогон).
func (s *Store) ListInvocationsSince(ctx context.Context, since time.Time) ([]ClaudeInvocation, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, run_id, stage, session_id, model, effort, prompt, output, stderr, subtype, is_error,
		       input_tokens, output_tokens, cost_usd, started_at, finished_at
		FROM claude_invocations WHERE started_at >= ? ORDER BY id ASC
	`, since.UTC())
	if err != nil {
		return nil, fmt.Errorf("list claude invocations since %s: %w", since, err)
	}
	defer rows.Close()

	var invocations []ClaudeInvocation
	for rows.Next() {
		var inv ClaudeInvocation
		if err := rows.Scan(&inv.ID, &inv.RunID, &inv.Stage, &inv.SessionID, &inv.Model, &inv.Effort,
			&inv.Prompt, &inv.Output, &inv.Stderr, &inv.Subtype, &inv.IsError,
			&inv.InputTokens, &inv.OutputTokens, &inv.CostUSD, &inv.StartedAt, &inv.FinishedAt); err != nil {
			return nil, fmt.Errorf("scan claude invocation row: %w", err)
		}
		invocations = append(invocations, inv)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate claude invocation rows: %w", err)
	}
	return invocations, nil
}

// UpdateRunningUsage обновляет расход токенов/стоимости прогона, ещё не
// завершённого, — вызывается по ходу стриминга ответа claude (см.
// review.Runner.run и его параметр onUsage), чтобы расход был виден в БД
// в реальном времени, а не только после окончания этапа (Требование:
// контроль подозреваемого перерасхода токенов). Не трогает статус прогона.
func (s *Store) UpdateRunningUsage(ctx context.Context, runID int64, usage Usage) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE runs SET input_tokens = ?, output_tokens = ?, cost_usd = ? WHERE id = ?
	`, usage.InputTokens, usage.OutputTokens, usage.CostUSD, runID)
	if err != nil {
		return fmt.Errorf("update running usage for run %d: %w", runID, err)
	}
	return nil
}

// GetSetting возвращает значение настройки key. ok=false и без ошибки
// означает, что настройка не задана — вызывающая сторона должна тогда
// использовать значение по умолчанию (обычно из .env/Config).
func (s *Store) GetSetting(ctx context.Context, key string) (value string, ok bool, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("get setting %s: %w", key, err)
	}
	return value, true, nil
}

// SetSetting сохраняет значение настройки key, перезаписывая существующее.
// Используется веб-интерфейсом, чтобы менять модель/effort claude на лету,
// без перезапуска сервиса и без потери значения при перезапуске.
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value
	`, key, value)
	if err != nil {
		return fmt.Errorf("set setting %s: %w", key, err)
	}
	return nil
}

// Ping проверяет, что база данных открыта и отвечает (используется в /readyz).
func (s *Store) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

// GetRun возвращает запись прогона по id (используется в тестах и диагностике).
func (s *Store) GetRun(ctx context.Context, runID int64) (*Run, error) {
	var r Run
	err := s.db.QueryRowContext(ctx, `
		SELECT id, task_id, custom_id, task_name, status, verdict, session_id, started_at, finished_at, error,
		       input_tokens, output_tokens, cost_usd, slack_message
		FROM runs WHERE id = ?
	`, runID).Scan(&r.ID, &r.TaskID, &r.CustomID, &r.TaskName, &r.Status, &r.Verdict, &r.SessionID, &r.StartedAt, &r.FinishedAt, &r.Error,
		&r.InputTokens, &r.OutputTokens, &r.CostUSD, &r.SlackMessage)
	if err != nil {
		return nil, fmt.Errorf("get run %d: %w", runID, err)
	}
	return &r, nil
}

// ListActive возвращает прогоны в состоянии queued или running — для
// эндпоинта GET /api/status.
func (s *Store) ListActive(ctx context.Context) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, task_id, custom_id, task_name, status, verdict, session_id, started_at, finished_at, error,
		       input_tokens, output_tokens, cost_usd, slack_message
		FROM runs WHERE status IN (?, ?) ORDER BY started_at ASC
	`, StatusQueued, StatusRunning)
	if err != nil {
		return nil, fmt.Errorf("list active runs: %w", err)
	}
	defer rows.Close()

	return scanRuns(rows)
}

// Stats возвращает агрегированную статистику и список прогонов, начатых
// не раньше since — для эндпоинта GET /api/stats.
func (s *Store) Stats(ctx context.Context, since time.Time) (Stats, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, task_id, custom_id, task_name, status, verdict, session_id, started_at, finished_at, error,
		       input_tokens, output_tokens, cost_usd, slack_message
		FROM runs WHERE started_at >= ? ORDER BY started_at DESC
	`, since.UTC())
	if err != nil {
		return Stats{}, fmt.Errorf("stats since %s: %w", since, err)
	}
	defer rows.Close()

	runs, err := scanRuns(rows)
	if err != nil {
		return Stats{}, err
	}

	stats := Stats{
		Since:     since,
		TotalRuns: len(runs),
		ByStatus:  map[string]int{},
		ByVerdict: map[string]int{},
		Runs:      runs,
	}
	for _, r := range runs {
		stats.ByStatus[r.Status]++
		if r.Verdict != "" {
			stats.ByVerdict[r.Verdict]++
		}
		stats.Tokens.InputTokens += r.InputTokens
		stats.Tokens.OutputTokens += r.OutputTokens
		stats.Tokens.CostUSD += r.CostUSD
	}
	return stats, nil
}

func scanRuns(rows *sql.Rows) ([]Run, error) {
	var runs []Run
	for rows.Next() {
		var r Run
		if err := rows.Scan(&r.ID, &r.TaskID, &r.CustomID, &r.TaskName, &r.Status, &r.Verdict, &r.SessionID, &r.StartedAt, &r.FinishedAt, &r.Error,
			&r.InputTokens, &r.OutputTokens, &r.CostUSD, &r.SlackMessage); err != nil {
			return nil, fmt.Errorf("scan run row: %w", err)
		}
		runs = append(runs, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate run rows: %w", err)
	}
	return runs, nil
}
