package queue

import (
	"context"
	"log/slog"
	"testing"
)

func TestRequestCancel_CancelsRegisteredContext(t *testing.T) {
	q := New(Deps{}, 10)
	ctx, cancel := context.WithCancel(context.Background())
	q.registerActive("t1", 42, cancel)
	defer q.unregisterActive("t1")

	if !q.RequestCancel("t1") {
		t.Fatal("expected RequestCancel to find the active task")
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("expected context to be cancelled")
	}
}

func TestRequestCancel_UnknownTask(t *testing.T) {
	q := New(Deps{}, 10)
	if q.RequestCancel("nope") {
		t.Fatal("expected RequestCancel to return false for a task that is not active")
	}
}

func TestRequestPause_ConsumedOnce(t *testing.T) {
	q := New(Deps{}, 10)
	_, cancel := context.WithCancel(context.Background())
	q.registerActive("t1", 1, cancel)
	defer q.unregisterActive("t1")

	if !q.RequestPause("t1") {
		t.Fatal("expected RequestPause to find the active task")
	}
	if !q.consumePauseRequest("t1") {
		t.Fatal("expected the pause request to be visible once")
	}
	if q.consumePauseRequest("t1") {
		t.Fatal("expected the pause request to be cleared after being consumed")
	}
}

func TestRequestPause_UnknownTask(t *testing.T) {
	q := New(Deps{}, 10)
	if q.RequestPause("nope") {
		t.Fatal("expected RequestPause to return false for a task that is not active")
	}
}

func TestActiveRuns_ReflectsRegisteredTasks(t *testing.T) {
	q := New(Deps{}, 10)
	_, cancel := context.WithCancel(context.Background())
	q.registerActive("t1", 7, cancel)
	defer q.unregisterActive("t1")

	active := q.ActiveRuns()
	if len(active) != 1 || active[0].TaskID != "t1" || active[0].RunID != 7 {
		t.Errorf("unexpected active runs: %+v", active)
	}

	q.unregisterActive("t1")
	if len(q.ActiveRuns()) != 0 {
		t.Error("expected no active runs after unregistering")
	}
}

func TestPending_TracksBufferedTasks(t *testing.T) {
	q := New(Deps{}, 10)
	q.Submit("a")
	q.Submit("b")

	pending := q.Pending()
	if len(pending) != 2 || pending[0] != "a" || pending[1] != "b" {
		t.Fatalf("unexpected pending list: %+v", pending)
	}

	q.removePending("a")
	pending = q.Pending()
	if len(pending) != 1 || pending[0] != "b" {
		t.Fatalf("expected only 'b' left pending, got: %+v", pending)
	}
}

func TestSubmit_SkipsDuplicateOfAlreadyPendingTask(t *testing.T) {
	// Воспроизводит "в очереди много дублей задач": сверка находит одну и
	// ту же ещё не взятую в обработку задачу на нескольких циклах подряд,
	// пока единственный воркер занят другой проверкой. Submit не должен
	// класть в буфер второй экземпляр того же task_id.
	q := New(Deps{Logger: slog.New(slog.NewTextHandler(nil2Writer{}, nil))}, 10)

	if !q.Submit("a") {
		t.Fatal("expected first Submit('a') to succeed")
	}
	if q.Submit("a") {
		t.Fatal("expected second Submit('a') to be skipped as a duplicate")
	}

	pending := q.Pending()
	if len(pending) != 1 {
		t.Fatalf("expected exactly one pending entry for 'a', got: %+v", pending)
	}
}

func TestSubmit_SkipsDuplicateOfActiveTask(t *testing.T) {
	q := New(Deps{Logger: slog.New(slog.NewTextHandler(nil2Writer{}, nil))}, 10)
	q.registerActive("a", 1, func() {})
	defer q.unregisterActive("a")

	if q.Submit("a") {
		t.Fatal("expected Submit('a') to be skipped while 'a' is already active")
	}
	if len(q.Pending()) != 0 {
		t.Fatalf("expected no pending entries, got: %+v", q.Pending())
	}
}
