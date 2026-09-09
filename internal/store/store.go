// Package store хранит состояние прогонов ревью в SQLite: таблица runs
// используется для дедупликации событий, восстановления после рестарта и
// как источник статистики (/api/status, /api/stats).
package store

import (
	"context"
	"database/sql"
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
)

// Run — одна запись таблицы runs.
type Run struct {
	ID           int64
	TaskID       string
	Status       string
	Verdict      string
	SessionID    string
	StartedAt    time.Time
	FinishedAt   sql.NullTime
	Error        string
	InputTokens  int64
	OutputTokens int64
	CostUSD      float64
}

// Usage — токены и стоимость одного прогона (сумма /spec + /review).
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

	if err := migrateTokenColumns(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate token columns on %s: %w", path, err)
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
`

// migrateTokenColumns докатывает колонки учёта токенов на базу, созданную
// более ранней версией сервиса, где их ещё не было. ALTER TABLE ADD COLUMN
// в SQLite не поддерживает IF NOT EXISTS во всех версиях, поэтому проверяем
// наличие колонки через PRAGMA table_info вручную.
func migrateTokenColumns(db *sql.DB) error {
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
// этой задаче нет записи в состоянии queued, running или done. Возвращает
// enqueued=false, если запись уже блокирует постановку (дедупликация).
func (s *Store) TryEnqueue(ctx context.Context, taskID string) (runID int64, enqueued bool, err error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO runs (task_id, status, started_at)
		SELECT ?, ?, ?
		WHERE NOT EXISTS (
			SELECT 1 FROM runs WHERE task_id = ? AND status IN (?, ?, ?)
		)
	`, taskID, StatusQueued, time.Now().UTC(), taskID, StatusQueued, StatusRunning, StatusDone)
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

// MarkRunning переводит прогон в статус running.
func (s *Store) MarkRunning(ctx context.Context, runID int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE runs SET status = ? WHERE id = ?`, StatusRunning, runID)
	if err != nil {
		return fmt.Errorf("mark run %d running: %w", runID, err)
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

// RecoverFromRestart переводит все прогоны в состоянии running в failed при
// старте сервиса: контейнер мог перезапуститься посреди ревью, зависшая
// задача не должна блокировать повторную обработку.
func (s *Store) RecoverFromRestart(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE runs SET status = ?, error = ?, finished_at = ?
		WHERE status = ?
	`, StatusFailed, "restarted: service was terminated mid-run", time.Now().UTC(), StatusRunning)
	if err != nil {
		return 0, fmt.Errorf("recover from restart: %w", err)
	}
	return res.RowsAffected()
}

// Ping проверяет, что база данных открыта и отвечает (используется в /readyz).
func (s *Store) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

// GetRun возвращает запись прогона по id (используется в тестах и диагностике).
func (s *Store) GetRun(ctx context.Context, runID int64) (*Run, error) {
	var r Run
	err := s.db.QueryRowContext(ctx, `
		SELECT id, task_id, status, verdict, session_id, started_at, finished_at, error,
		       input_tokens, output_tokens, cost_usd
		FROM runs WHERE id = ?
	`, runID).Scan(&r.ID, &r.TaskID, &r.Status, &r.Verdict, &r.SessionID, &r.StartedAt, &r.FinishedAt, &r.Error,
		&r.InputTokens, &r.OutputTokens, &r.CostUSD)
	if err != nil {
		return nil, fmt.Errorf("get run %d: %w", runID, err)
	}
	return &r, nil
}

// ListActive возвращает прогоны в состоянии queued или running — для
// эндпоинта GET /api/status.
func (s *Store) ListActive(ctx context.Context) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, task_id, status, verdict, session_id, started_at, finished_at, error,
		       input_tokens, output_tokens, cost_usd
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
		SELECT id, task_id, status, verdict, session_id, started_at, finished_at, error,
		       input_tokens, output_tokens, cost_usd
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
		if err := rows.Scan(&r.ID, &r.TaskID, &r.Status, &r.Verdict, &r.SessionID, &r.StartedAt, &r.FinishedAt, &r.Error,
			&r.InputTokens, &r.OutputTokens, &r.CostUSD); err != nil {
			return nil, fmt.Errorf("scan run row: %w", err)
		}
		runs = append(runs, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate run rows: %w", err)
	}
	return runs, nil
}
