// Package store хранит состояние прогонов ревью в SQLite: таблица runs
// используется для дедупликации событий и восстановления после рестарта.
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
	ID         int64
	TaskID     string
	Status     string
	Verdict    string
	SessionID  string
	StartedAt  time.Time
	FinishedAt sql.NullTime
	Error      string
}

// Store — обёртка над SQLite-базой с таблицей runs.
type Store struct {
	db *sql.DB
}

// Open открывает (создавая при необходимости) SQLite-файл по path и
// применяет схему. Единственное открытое соединение сериализует доступ,
// что вместе с атомарным INSERT...WHERE NOT EXISTS в TryEnqueue исключает
// гонки между вебхуком и сверкой при постановке в очередь.
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

// MarkDone переводит прогон в статус done с вердиктом и session id.
func (s *Store) MarkDone(ctx context.Context, runID int64, verdict, sessionID string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE runs SET status = ?, verdict = ?, session_id = ?, finished_at = ?
		WHERE id = ?
	`, StatusDone, verdict, sessionID, time.Now().UTC(), runID)
	if err != nil {
		return fmt.Errorf("mark run %d done: %w", runID, err)
	}
	return nil
}

// MarkFailed переводит прогон в статус failed. Failed-прогоны не блокируют
// повторную постановку задачи в очередь.
func (s *Store) MarkFailed(ctx context.Context, runID int64, sessionID, errMsg string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE runs SET status = ?, session_id = ?, error = ?, finished_at = ?
		WHERE id = ?
	`, StatusFailed, sessionID, errMsg, time.Now().UTC(), runID)
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
		SELECT id, task_id, status, verdict, session_id, started_at, finished_at, error
		FROM runs WHERE id = ?
	`, runID).Scan(&r.ID, &r.TaskID, &r.Status, &r.Verdict, &r.SessionID, &r.StartedAt, &r.FinishedAt, &r.Error)
	if err != nil {
		return nil, fmt.Errorf("get run %d: %w", runID, err)
	}
	return &r, nil
}
