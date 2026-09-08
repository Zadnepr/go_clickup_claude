package queue

import (
	"strings"
	"testing"

	"github.com/Zadnepr/go_clickup_claude/internal/config"
	"github.com/Zadnepr/go_clickup_claude/internal/review"
)

func TestDecide_Pass(t *testing.T) {
	cfg := &config.Config{StatusPass: "done", StatusFail: "to fix", AssigneeOnPass: "10"}
	status, assignees := Decide(review.Verdict{Status: review.StatusPass}, cfg, 999)
	if status != "done" {
		t.Errorf("status = %q, want done", status)
	}
	if len(assignees) != 1 || assignees[0] != 10 {
		t.Errorf("assignees = %+v, want [10]", assignees)
	}
}

func TestDecide_Pass_NoAssigneeConfigured(t *testing.T) {
	cfg := &config.Config{StatusPass: "done", StatusFail: "to fix"}
	_, assignees := Decide(review.Verdict{Status: review.StatusPass}, cfg, 999)
	if len(assignees) != 0 {
		t.Errorf("expected no assignee change on pass, got: %+v", assignees)
	}
}

func TestDecide_Fail_UsesConfiguredAssignee(t *testing.T) {
	cfg := &config.Config{StatusPass: "done", StatusFail: "to fix", AssigneeOnFail: "20"}
	status, assignees := Decide(review.Verdict{Status: review.StatusFail}, cfg, 999)
	if status != "to fix" {
		t.Errorf("status = %q, want %q", status, "to fix")
	}
	if len(assignees) != 1 || assignees[0] != 20 {
		t.Errorf("assignees = %+v, want [20]", assignees)
	}
}

func TestDecide_Fail_FallsBackToCreator(t *testing.T) {
	cfg := &config.Config{StatusPass: "done", StatusFail: "to fix"}
	_, assignees := Decide(review.Verdict{Status: review.StatusFail}, cfg, 42)
	if len(assignees) != 1 || assignees[0] != 42 {
		t.Errorf("expected fallback to creator id 42, got: %+v", assignees)
	}
}

func TestDecide_Blocked_GoesToFailStatusLikeCriticalFindings(t *testing.T) {
	cfg := &config.Config{StatusPass: "done", StatusFail: "to fix"}
	status, _ := Decide(review.Verdict{Status: review.StatusBlocked}, cfg, 0)
	if status != "to fix" {
		t.Errorf("blocked verdict should map to STATUS_FAIL, got %q", status)
	}
}

func TestDecide_EmptyStatusMeansNoTransition(t *testing.T) {
	cfg := &config.Config{} // STATUS_PASS/STATUS_FAIL not configured
	status, _ := Decide(review.Verdict{Status: review.StatusPass}, cfg, 0)
	if status != "" {
		t.Errorf("expected empty target status when not configured, got %q", status)
	}
}

func TestSplitComment_ShortTextUnchanged(t *testing.T) {
	chunks := SplitComment("short review text", 100)
	if len(chunks) != 1 || chunks[0] != "short review text" {
		t.Errorf("unexpected chunks: %+v", chunks)
	}
}

func TestSplitComment_SplitsOnParagraphBoundaries(t *testing.T) {
	text := "## Раздел 1\nтекст первого раздела\n\n## Раздел 2\nтекст второго раздела"
	chunks := SplitComment(text, 30)

	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d: %+v", len(chunks), chunks)
	}
	// Порядок должен сохраниться при сборке обратно.
	joined := strings.Join(chunks, "\n\n")
	if !strings.Contains(joined, "Раздел 1") || !strings.Contains(joined, "Раздел 2") {
		t.Errorf("expected both sections preserved, got: %s", joined)
	}
	if strings.Index(joined, "Раздел 1") > strings.Index(joined, "Раздел 2") {
		t.Error("expected section order to be preserved")
	}
	for i, c := range chunks {
		if len(c) > 30 {
			t.Errorf("chunk %d exceeds max length: %d chars", i, len(c))
		}
	}
}

func TestSplitComment_HardSplitsOversizedParagraph(t *testing.T) {
	text := strings.Repeat("a", 250)
	chunks := SplitComment(text, 100)
	if len(chunks) != 3 {
		t.Fatalf("expected 3 chunks for a 250-char paragraph with max 100, got %d", len(chunks))
	}
	joined := strings.Join(chunks, "")
	if joined != text {
		t.Error("expected hard-split chunks to reconstruct the original text losslessly")
	}
}
