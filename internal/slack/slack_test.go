package slack

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNotifier_Send_Success(t *testing.T) {
	var gotBody message
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := NewNotifier(srv.URL)
	err := n.Send(context.Background(), "fallback text", []Block{Section("hello")})
	if err != nil {
		t.Fatalf("Send error: %v", err)
	}
	if gotBody.Text != "fallback text" {
		t.Errorf("Text = %q, want %q", gotBody.Text, "fallback text")
	}
	if len(gotBody.Blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(gotBody.Blocks))
	}
}

func TestNotifier_Send_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	n := NewNotifier(srv.URL)
	err := n.Send(context.Background(), "text", nil)
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
}

func TestBuildReviewMessage_PassVsFailVisuallyDistinct(t *testing.T) {
	passText, _ := BuildReviewMessage(ReviewNotification{TaskName: "T1", TaskURL: "https://x/1", Verdict: "pass"})
	failText, _ := BuildReviewMessage(ReviewNotification{TaskName: "T1", TaskURL: "https://x/1", Verdict: "fail"})

	if passText == failText {
		t.Fatal("expected pass and fail messages to differ")
	}
	if !strings.Contains(passText, "✅") {
		t.Errorf("expected pass message to contain success emoji, got: %s", passText)
	}
	if !strings.Contains(failText, "❌") {
		t.Errorf("expected fail message to contain failure emoji, got: %s", failText)
	}
}

func TestBuildReviewMessage_ContainsRequiredFields(t *testing.T) {
	_, blocks := BuildReviewMessage(ReviewNotification{
		TaskName:   "Fix login bug",
		TaskURL:    "https://app.clickup.com/t/123",
		Verdict:    "fail",
		Critical:   2,
		Important:  1,
		Minor:      0,
		FromStatus: "checking",
		ToStatus:   "to fix",
		Assignee:   "42",
		SessionID:  "sess-1",
	})

	dump, _ := json.Marshal(blocks)
	body := string(dump)
	for _, want := range []string{"Fix login bug", "app.clickup.com/t/123", "критичных=2", "важных=1", "минор=0", "checking", "to fix", "42", "sess-1"} {
		if !strings.Contains(body, want) {
			t.Errorf("expected message to contain %q, got: %s", want, body)
		}
	}
}

func TestBuildShortResultMessage(t *testing.T) {
	text, blocks := BuildShortResultMessage(ReviewNotification{
		TaskName:   "Fix login bug",
		TaskURL:    "https://app.clickup.com/t/123",
		Verdict:    "fail",
		FromStatus: "checking",
		ToStatus:   "rework",
		Assignee:   "81838079",
	})
	if !strings.Contains(text, "Fix login bug") || !strings.Contains(text, "checking") || !strings.Contains(text, "rework") {
		t.Errorf("expected task name and columns in text, got: %s", text)
	}
	dump, _ := json.Marshal(blocks)
	body := string(dump)
	for _, want := range []string{"Fix login bug", "checking", "rework", "81838079"} {
		if !strings.Contains(body, want) {
			t.Errorf("expected short message to contain %q, got: %s", want, body)
		}
	}
	// Короткое сообщение не должно содержать разбивку по уровням замечаний.
	if strings.Contains(body, "критичных") {
		t.Errorf("expected short message to omit finding counts, got: %s", body)
	}
}

func TestBuildStartedMessage(t *testing.T) {
	text, blocks := BuildStartedMessage("Fix login bug", "https://app.clickup.com/t/123")
	if !strings.Contains(text, "Fix login bug") || !strings.Contains(text, "app.clickup.com/t/123") {
		t.Errorf("expected task name and url in text, got: %s", text)
	}
	dump, _ := json.Marshal(blocks)
	if !strings.Contains(string(dump), "Fix login bug") {
		t.Errorf("expected task name in blocks, got: %s", string(dump))
	}
}

func TestBuildBlockedMessage(t *testing.T) {
	text, blocks := BuildBlockedMessage(ReviewNotification{TaskName: "T", TaskURL: "https://x/1"}, "ветка не найдена")
	if !strings.Contains(text, "заблокировано") {
		t.Errorf("expected blocked marker in text, got: %s", text)
	}
	dump, _ := json.Marshal(blocks)
	if !strings.Contains(string(dump), "ветка не найдена") {
		t.Errorf("expected reason in blocked message, got: %s", string(dump))
	}
}

func TestBuildServiceErrorMessage(t *testing.T) {
	text, blocks := BuildServiceErrorMessage("T", "https://x/1", "status update failed")
	if !strings.Contains(text, "Ошибка") {
		t.Errorf("expected error marker in text, got: %s", text)
	}
	dump, _ := json.Marshal(blocks)
	if !strings.Contains(string(dump), "status update failed") {
		t.Errorf("expected error text in blocks, got: %s", string(dump))
	}
}
