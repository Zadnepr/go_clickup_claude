package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open error: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestTryEnqueue_Deduplicates(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	id1, ok1, err := s.TryEnqueue(ctx, "task-1")
	if err != nil {
		t.Fatalf("TryEnqueue error: %v", err)
	}
	if !ok1 {
		t.Fatal("expected first enqueue to succeed")
	}

	// Повторное событие по той же задаче, пока прогон queued — не должно
	// породить второй прогон.
	id2, ok2, err := s.TryEnqueue(ctx, "task-1")
	if err != nil {
		t.Fatalf("TryEnqueue error: %v", err)
	}
	if ok2 {
		t.Fatalf("expected second enqueue to be deduplicated, got new run id %d", id2)
	}
	if id1 == 0 {
		t.Fatal("expected non-zero run id")
	}
}

func TestTryEnqueue_BlockedWhileRunningOrDone(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	id, ok, err := s.TryEnqueue(ctx, "task-2")
	if err != nil || !ok {
		t.Fatalf("initial enqueue failed: ok=%v err=%v", ok, err)
	}

	if err := s.MarkRunning(ctx, id); err != nil {
		t.Fatalf("MarkRunning error: %v", err)
	}
	if _, ok, err := s.TryEnqueue(ctx, "task-2"); err != nil || ok {
		t.Fatalf("expected enqueue blocked while running: ok=%v err=%v", ok, err)
	}

	if err := s.MarkDone(ctx, id, "pass", "session-1", Usage{}); err != nil {
		t.Fatalf("MarkDone error: %v", err)
	}
	if _, ok, err := s.TryEnqueue(ctx, "task-2"); err != nil || ok {
		t.Fatalf("expected enqueue blocked while done: ok=%v err=%v", ok, err)
	}
}

func TestTryEnqueue_ReopensAfterFailed(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	id, ok, err := s.TryEnqueue(ctx, "task-3")
	if err != nil || !ok {
		t.Fatalf("initial enqueue failed: ok=%v err=%v", ok, err)
	}
	if err := s.MarkRunning(ctx, id); err != nil {
		t.Fatalf("MarkRunning error: %v", err)
	}
	if err := s.MarkFailed(ctx, id, "session-1", "boom", Usage{}); err != nil {
		t.Fatalf("MarkFailed error: %v", err)
	}

	// Failed не должен блокировать повторную постановку.
	newID, ok, err := s.TryEnqueue(ctx, "task-3")
	if err != nil {
		t.Fatalf("TryEnqueue after failed error: %v", err)
	}
	if !ok {
		t.Fatal("expected enqueue to succeed after previous run failed")
	}
	if newID == id {
		t.Fatal("expected a new run id, not reuse of the failed one")
	}
}

func TestRecoverFromRestart(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	id, _, err := s.TryEnqueue(ctx, "task-4")
	if err != nil {
		t.Fatalf("TryEnqueue error: %v", err)
	}
	if err := s.MarkRunning(ctx, id); err != nil {
		t.Fatalf("MarkRunning error: %v", err)
	}

	n, err := s.RecoverFromRestart(ctx)
	if err != nil {
		t.Fatalf("RecoverFromRestart error: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 recovered run, got %d", n)
	}

	run, err := s.GetRun(ctx, id)
	if err != nil {
		t.Fatalf("GetRun error: %v", err)
	}
	if run.Status != StatusFailed {
		t.Errorf("run status = %q, want %q", run.Status, StatusFailed)
	}

	// После восстановления задача должна снова браться в работу.
	if _, ok, err := s.TryEnqueue(ctx, "task-4"); err != nil || !ok {
		t.Fatalf("expected enqueue to succeed after restart recovery: ok=%v err=%v", ok, err)
	}
}

func TestPing(t *testing.T) {
	s := openTestStore(t)
	if err := s.Ping(context.Background()); err != nil {
		t.Fatalf("Ping error: %v", err)
	}
}

func TestMarkDone_StoresTokenUsage(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	id, _, err := s.TryEnqueue(ctx, "task-tokens")
	if err != nil {
		t.Fatalf("TryEnqueue error: %v", err)
	}
	if err := s.MarkDone(ctx, id, "pass", "sess-1", Usage{InputTokens: 1000, OutputTokens: 200, CostUSD: 0.05}); err != nil {
		t.Fatalf("MarkDone error: %v", err)
	}

	run, err := s.GetRun(ctx, id)
	if err != nil {
		t.Fatalf("GetRun error: %v", err)
	}
	if run.InputTokens != 1000 || run.OutputTokens != 200 || run.CostUSD != 0.05 {
		t.Errorf("unexpected token usage on run: %+v", run)
	}
}

func TestListActive_ReturnsOnlyQueuedAndRunning(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	queuedID, _, _ := s.TryEnqueue(ctx, "task-a")
	runningID, _, _ := s.TryEnqueue(ctx, "task-b")
	s.MarkRunning(ctx, runningID)
	doneID, _, _ := s.TryEnqueue(ctx, "task-c")
	s.MarkDone(ctx, doneID, "pass", "s", Usage{})

	active, err := s.ListActive(ctx)
	if err != nil {
		t.Fatalf("ListActive error: %v", err)
	}
	if len(active) != 2 {
		t.Fatalf("expected 2 active runs, got %d: %+v", len(active), active)
	}
	ids := map[int64]bool{active[0].ID: true, active[1].ID: true}
	if !ids[queuedID] || !ids[runningID] {
		t.Errorf("expected queued(%d) and running(%d) runs in active list, got %+v", queuedID, runningID, active)
	}
}

func TestStats_AggregatesCountsAndTokens(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	id1, _, _ := s.TryEnqueue(ctx, "task-x")
	s.MarkDone(ctx, id1, "pass", "s1", Usage{InputTokens: 100, OutputTokens: 10, CostUSD: 0.01})

	id2, _, _ := s.TryEnqueue(ctx, "task-y")
	s.MarkDone(ctx, id2, "fail", "s2", Usage{InputTokens: 200, OutputTokens: 20, CostUSD: 0.02})

	id3, _, _ := s.TryEnqueue(ctx, "task-z")
	s.MarkFailed(ctx, id3, "s3", "boom", Usage{InputTokens: 50, OutputTokens: 5, CostUSD: 0.005})

	stats, err := s.Stats(ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("Stats error: %v", err)
	}
	if stats.TotalRuns != 3 {
		t.Fatalf("expected 3 runs, got %d", stats.TotalRuns)
	}
	if stats.ByStatus[StatusDone] != 2 || stats.ByStatus[StatusFailed] != 1 {
		t.Errorf("unexpected ByStatus: %+v", stats.ByStatus)
	}
	if stats.ByVerdict["pass"] != 1 || stats.ByVerdict["fail"] != 1 {
		t.Errorf("unexpected ByVerdict: %+v", stats.ByVerdict)
	}
	if stats.Tokens.InputTokens != 350 || stats.Tokens.OutputTokens != 35 {
		t.Errorf("unexpected token totals: %+v", stats.Tokens)
	}
	if stats.Tokens.CostUSD < 0.0349 || stats.Tokens.CostUSD > 0.0351 {
		t.Errorf("unexpected cost total: %v", stats.Tokens.CostUSD)
	}
	if len(stats.Runs) != 3 {
		t.Errorf("expected 3 runs in log, got %d", len(stats.Runs))
	}
}

func TestStats_ExcludesRunsBeforeSince(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	id, _, _ := s.TryEnqueue(ctx, "task-old")
	s.MarkDone(ctx, id, "pass", "s", Usage{})

	stats, err := s.Stats(ctx, time.Now().Add(time.Hour)) // since в будущем
	if err != nil {
		t.Fatalf("Stats error: %v", err)
	}
	if stats.TotalRuns != 0 {
		t.Errorf("expected 0 runs when since is in the future, got %d", stats.TotalRuns)
	}
}

func TestMigrateTokenColumns_IdempotentOnReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")

	s1, err := Open(path)
	if err != nil {
		t.Fatalf("first Open error: %v", err)
	}
	id, _, _ := s1.TryEnqueue(context.Background(), "task-1")
	s1.MarkDone(context.Background(), id, "pass", "s", Usage{InputTokens: 42})
	s1.Close()

	// Переоткрытие уже существующей базы не должно падать на повторной миграции.
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open error: %v", err)
	}
	defer s2.Close()

	run, err := s2.GetRun(context.Background(), id)
	if err != nil {
		t.Fatalf("GetRun error: %v", err)
	}
	if run.InputTokens != 42 {
		t.Errorf("expected token data to survive reopen, got: %+v", run)
	}
}
