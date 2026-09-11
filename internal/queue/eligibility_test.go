package queue

import (
	"testing"

	"github.com/Zadnepr/go_clickup_claude/internal/clickup"
	"github.com/Zadnepr/go_clickup_claude/internal/config"
)

func baseCfg() *config.Config {
	return &config.Config{
		CUListID:      "list1",
		TriggerTag:    "ai",
		StatusTrigger: "to check",
		StatusRunning: "checking",
	}
}

func TestIsEligible_AllConditionsMet(t *testing.T) {
	task := &clickup.Task{ListID: "list1", Tags: []string{"ai"}, Status: "to check"}
	if !isEligible(task, baseCfg()) {
		t.Fatal("expected task to be eligible")
	}
}

func TestIsEligible_CaseAndWhitespaceInsensitive(t *testing.T) {
	task := &clickup.Task{ListID: "list1", Tags: []string{"  AI "}, Status: " To Check "}
	if !isEligible(task, baseCfg()) {
		t.Fatal("expected eligibility check to ignore case and surrounding whitespace")
	}
}

func TestIsEligible_WrongList(t *testing.T) {
	task := &clickup.Task{ListID: "other-list", Tags: []string{"ai"}, Status: "to check"}
	if isEligible(task, baseCfg()) {
		t.Fatal("expected task in a different list to be ineligible")
	}
}

func TestIsEligible_MissingTag(t *testing.T) {
	task := &clickup.Task{ListID: "list1", Tags: []string{"urgent"}, Status: "to check"}
	if isEligible(task, baseCfg()) {
		t.Fatal("expected task without trigger tag to be ineligible")
	}
}

func TestIsEligible_WrongStatus(t *testing.T) {
	task := &clickup.Task{ListID: "list1", Tags: []string{"ai"}, Status: "in progress"}
	if isEligible(task, baseCfg()) {
		t.Fatal("expected task with a different status to be ineligible")
	}
}

func TestBelongsToConfiguredList_SameList(t *testing.T) {
	task := &clickup.Task{ListID: "list1"}
	if !belongsToConfiguredList(task, baseCfg()) {
		t.Fatal("expected task in the configured list to belong to it, regardless of tag/status")
	}
}

func TestBelongsToConfiguredList_DifferentList(t *testing.T) {
	task := &clickup.Task{ListID: "other-list"}
	if belongsToConfiguredList(task, baseCfg()) {
		t.Fatal("expected task in a different list to not belong to it")
	}
}

func TestSpecFileID_PrefersCustomID(t *testing.T) {
	task := &clickup.Task{ID: "869d9kt6a", CustomID: "PNL-4528"}
	if got := specFileID(task); got != "PNL-4528" {
		t.Errorf("specFileID = %q, want %q", got, "PNL-4528")
	}
}

func TestSpecFileID_FallsBackToNativeIDWithoutCustomID(t *testing.T) {
	task := &clickup.Task{ID: "869d9kt6a"}
	if got := specFileID(task); got != "869d9kt6a" {
		t.Errorf("specFileID = %q, want %q", got, "869d9kt6a")
	}
}

func TestResumable_StillInTriggerStatusWithTag(t *testing.T) {
	task := &clickup.Task{Tags: []string{"ai"}, Status: "to check"}
	if !resumable(task, baseCfg()) {
		t.Fatal("expected task still in STATUS_TRIGGER with the trigger tag to be resumable")
	}
}

func TestResumable_StillInRunningStatusWithTag(t *testing.T) {
	task := &clickup.Task{Tags: []string{"ai"}, Status: "checking"}
	if !resumable(task, baseCfg()) {
		t.Fatal("expected task still in STATUS_RUNNING with the trigger tag to be resumable")
	}
}

func TestResumable_IgnoresList(t *testing.T) {
	// В отличие от isEligible, resumable не проверяет список: прогон уже
	// был начат для этой задачи, продолжается независимо от ListID.
	task := &clickup.Task{Status: "checking", Tags: []string{"ai"}}
	cfg := baseCfg()
	cfg.CUListID = "completely-different-list"
	if !resumable(task, cfg) {
		t.Fatal("expected resumable to ignore list membership")
	}
}

func TestResumable_TagRemoved(t *testing.T) {
	task := &clickup.Task{Tags: []string{"other"}, Status: "checking"}
	if resumable(task, baseCfg()) {
		t.Fatal("expected task without the trigger tag to not be resumable — looks handled manually")
	}
}

func TestResumable_StatusMovedElsewhere(t *testing.T) {
	task := &clickup.Task{Tags: []string{"ai"}, Status: "rework"}
	if resumable(task, baseCfg()) {
		t.Fatal("expected task moved to an unrelated status to not be resumable — looks handled manually")
	}
}

func TestStaleAssignees_ExcludesWantedIDs(t *testing.T) {
	got := staleAssignees([]int{1, 2, 3}, []int{2})
	if len(got) != 2 || got[0] != 1 || got[1] != 3 {
		t.Errorf("expected [1 3], got %+v", got)
	}
}

func TestStaleAssignees_NoneStaleWhenAllWanted(t *testing.T) {
	if got := staleAssignees([]int{1, 2}, []int{1, 2}); len(got) != 0 {
		t.Errorf("expected no stale assignees, got %+v", got)
	}
}

func TestStaleAssignees_EmptyCurrentIsNoop(t *testing.T) {
	if got := staleAssignees(nil, []int{1}); got != nil {
		t.Errorf("expected nil for empty current assignees, got %+v", got)
	}
}

func TestStaleAssignees_AllStaleWhenNothingWanted(t *testing.T) {
	got := staleAssignees([]int{1, 2}, nil)
	if len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Errorf("expected all current assignees to be stale, got %+v", got)
	}
}
