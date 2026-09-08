package queue

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Zadnepr/go_clickup_claude/internal/clickup"
	"github.com/Zadnepr/go_clickup_claude/internal/config"
	"github.com/Zadnepr/go_clickup_claude/internal/review"
	"github.com/Zadnepr/go_clickup_claude/internal/slack"
	"github.com/Zadnepr/go_clickup_claude/internal/store"
)

// fakeClickUp — фейковая реализация интерфейса ClickUp для тестов воркера.
type fakeClickUp struct {
	mu        sync.Mutex
	task      *clickup.Task
	statuses  []string
	assignees [][]int
	comments  []string
}

func (f *fakeClickUp) GetTask(ctx context.Context, taskID string) (*clickup.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *f.task
	return &cp, nil
}

func (f *fakeClickUp) SetStatus(ctx context.Context, taskID, status string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statuses = append(f.statuses, status)
	return nil
}

func (f *fakeClickUp) AddAssignees(ctx context.Context, taskID string, userIDs []int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.assignees = append(f.assignees, userIDs)
	return nil
}

func (f *fakeClickUp) AddComment(ctx context.Context, taskID, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.comments = append(f.comments, text)
	return nil
}

// fakeRunner — фейковая реализация интерфейса Runner.
type fakeRunner struct {
	specErr    error
	specExists bool
	result     review.Result
	reviewErr  error
	gitErr     error
}

func (f *fakeRunner) GitFetch(ctx context.Context) error { return f.gitErr }

func (f *fakeRunner) RunSpec(ctx context.Context, taskURL string) (string, error) {
	return "spec-session", f.specErr
}

func (f *fakeRunner) RunReview(ctx context.Context, taskURL, specPath string) (review.Result, error) {
	return f.result, f.reviewErr
}

func (f *fakeRunner) SpecFilePath(taskID string) string {
	if f.specExists {
		// Существующий файл: сам тестовый бинарь на диске годится как заглушка.
		return filepath.Join(".", "worker_test.go")
	}
	return filepath.Join(string(filepath.Separator), "nonexistent", "spec-"+taskID+".md")
}

func newTestDeps(t *testing.T, cu *fakeClickUp, runner *fakeRunner, cfg *config.Config) (Deps, *httptest.Server, *[]slackCall) {
	t.Helper()

	var calls []slackCall
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Text   string        `json:"text"`
			Blocks []slack.Block `json:"blocks"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		calls = append(calls, slackCall{Text: body.Text, Blocks: body.Blocks})
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	deps := Deps{
		ClickUp: cu,
		Store:   st,
		Slack:   slack.NewNotifier(srv.URL),
		Runner:  runner,
		Cfg:     cfg,
		Logger:  slog.New(slog.NewTextHandler(nil2Writer{}, nil)),
	}
	return deps, srv, &calls
}

type slackCall struct {
	Text   string
	Blocks []slack.Block
}

// nil2Writer отбрасывает лог-вывод в тестах, чтобы не засорять их вывод.
type nil2Writer struct{}

func (nil2Writer) Write(p []byte) (int, error) { return len(p), nil }

func testConfig() *config.Config {
	return &config.Config{
		CUListID:          "list1",
		TriggerTag:        "ai",
		StatusTrigger:     "to check",
		StatusRunning:     "checking",
		StatusPass:        "done",
		StatusFail:        "to fix",
		AssigneeOnFail:    "20",
		WorkerConcurrency: 1,
		ReviewTimeout:     10 * time.Second,
	}
}

func TestProcessTask_PassVerdict_FullPipeline(t *testing.T) {
	task := &clickup.Task{ID: "1", Name: "Task 1", URL: "https://app.clickup.com/t/1", ListID: "list1", Tags: []string{"ai"}, Status: "to check", CreatorID: 5}
	cu := &fakeClickUp{task: task}
	runner := &fakeRunner{
		specExists: true,
		result: review.Result{
			Output:    "Всё отлично.\nИТОГ: критичных=0 важных=0 минор=1 статус=pass",
			SessionID: "sess-1",
			Verdict:   review.Verdict{Status: review.StatusPass, Minor: 1, Raw: "ИТОГ: критичных=0 важных=0 минор=1 статус=pass"},
		},
	}

	deps, _, calls := newTestDeps(t, cu, runner, testConfig())
	q := New(deps, 10)
	q.processTask("1")

	cu.mu.Lock()
	defer cu.mu.Unlock()
	if len(cu.statuses) != 2 || cu.statuses[0] != "checking" || cu.statuses[1] != "done" {
		t.Errorf("unexpected status transitions: %+v", cu.statuses)
	}
	if len(cu.assignees) != 0 {
		t.Errorf("expected no assignee change on pass, got: %+v", cu.assignees)
	}
	if len(cu.comments) != 1 {
		t.Fatalf("expected 1 comment, got %d", len(cu.comments))
	}

	if len(*calls) != 1 {
		t.Fatalf("expected 1 slack notification, got %d", len(*calls))
	}
}

func TestProcessTask_FailVerdict_AssignsConfiguredUser(t *testing.T) {
	task := &clickup.Task{ID: "2", Name: "Task 2", URL: "https://app.clickup.com/t/2", ListID: "list1", Tags: []string{"ai"}, Status: "to check", CreatorID: 5}
	cu := &fakeClickUp{task: task}
	runner := &fakeRunner{
		result: review.Result{
			Output:    "Есть проблема.\nИТОГ: критичных=1 важных=0 минор=0 статус=fail",
			SessionID: "sess-2",
			Verdict:   review.Verdict{Status: review.StatusFail, Critical: 1, Raw: "x"},
		},
	}

	deps, _, _ := newTestDeps(t, cu, runner, testConfig())
	q := New(deps, 10)
	q.processTask("2")

	cu.mu.Lock()
	defer cu.mu.Unlock()
	if len(cu.statuses) != 2 || cu.statuses[1] != "to fix" {
		t.Errorf("unexpected status transitions: %+v", cu.statuses)
	}
	if len(cu.assignees) != 1 || len(cu.assignees[0]) != 1 || cu.assignees[0][0] != 20 {
		t.Errorf("expected assignee 20 (ASSIGNEE_ON_FAIL), got: %+v", cu.assignees)
	}
}

func TestProcessTask_ReviewProcessError_MarksRunFailedAndNotifies(t *testing.T) {
	task := &clickup.Task{ID: "3", Name: "Task 3", URL: "https://app.clickup.com/t/3", ListID: "list1", Tags: []string{"ai"}, Status: "to check"}
	cu := &fakeClickUp{task: task}
	runner := &fakeRunner{
		result:    review.Result{Verdict: review.Verdict{Status: review.StatusBlocked}},
		reviewErr: context.DeadlineExceeded,
	}

	deps, _, calls := newTestDeps(t, cu, runner, testConfig())
	q := New(deps, 10)
	q.processTask("3")

	if len(*calls) != 1 {
		t.Fatalf("expected 1 slack notification for service error, got %d", len(*calls))
	}

	// Прогон должен быть отмечен как failed и не блокировать повторную постановку.
	runID, enqueued, err := deps.Store.TryEnqueue(context.Background(), "3")
	if err != nil {
		t.Fatalf("TryEnqueue error: %v", err)
	}
	if !enqueued {
		t.Fatal("expected task to be re-enqueueable after a failed run")
	}
	_ = runID
}

func TestProcessTask_NotEligible_SkipsWithoutDedupRecord(t *testing.T) {
	task := &clickup.Task{ID: "4", Name: "Task 4", URL: "https://app.clickup.com/t/4", ListID: "list1", Tags: []string{"other"}, Status: "to check"}
	cu := &fakeClickUp{task: task}
	runner := &fakeRunner{}

	deps, _, calls := newTestDeps(t, cu, runner, testConfig())
	q := New(deps, 10)
	q.processTask("4")

	if len(*calls) != 0 {
		t.Fatalf("expected no slack notification for ineligible task, got %d", len(*calls))
	}

	// Не должно остаться дедуп-записи: постановка должна быть возможна снова.
	_, enqueued, err := deps.Store.TryEnqueue(context.Background(), "4")
	if err != nil {
		t.Fatalf("TryEnqueue error: %v", err)
	}
	if !enqueued {
		t.Fatal("expected no dedup record to have been left for an ineligible task")
	}
}

func TestProcessTask_DuplicateEvent_DoesNotProduceSecondRun(t *testing.T) {
	task := &clickup.Task{ID: "5", Name: "Task 5", URL: "https://app.clickup.com/t/5", ListID: "list1", Tags: []string{"ai"}, Status: "to check"}
	cu := &fakeClickUp{task: task}
	runner := &fakeRunner{
		result: review.Result{
			Output:  "ok\nИТОГ: критичных=0 важных=0 минор=0 статус=pass",
			Verdict: review.Verdict{Status: review.StatusPass},
		},
	}

	deps, _, calls := newTestDeps(t, cu, runner, testConfig())
	q := New(deps, 10)

	q.processTask("5")
	q.processTask("5") // повторное событие по той же задаче

	if len(*calls) != 1 {
		t.Fatalf("expected exactly 1 slack notification despite duplicate event, got %d", len(*calls))
	}
}
