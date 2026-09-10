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
