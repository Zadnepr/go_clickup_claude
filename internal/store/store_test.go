package store

import (
	"context"
	"path/filepath"
	"testing"
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

	if err := s.MarkDone(ctx, id, "pass", "session-1"); err != nil {
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
	if err := s.MarkFailed(ctx, id, "session-1", "boom"); err != nil {
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
