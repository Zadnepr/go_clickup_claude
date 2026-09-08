package config

import (
	"strings"
	"testing"
	"time"
)

func envMap(overrides map[string]string) func(string) string {
	base := map[string]string{
		"CLAUDE_CODE_OAUTH_TOKEN": "tok",
		"CU_API_TOKEN":            "pk_123",
		"CU_TEAM_ID":              "team1",
		"CU_LIST_ID":              "list1",
		"SLACK_WEBHOOK_URL":       "https://hooks.slack.test/x",
		"REPO_PATH":               "/repo",
	}
	for k, v := range overrides {
		base[k] = v
	}
	return func(name string) string { return base[name] }
}

func TestLoad_Defaults(t *testing.T) {
	cfg, err := Load(envMap(nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.TriggerTag != "ai" {
		t.Errorf("TriggerTag = %q, want ai", cfg.TriggerTag)
	}
	if cfg.StatusTrigger != "to check" {
		t.Errorf("StatusTrigger = %q, want %q", cfg.StatusTrigger, "to check")
	}
	if cfg.StatusRunning != "checking" {
		t.Errorf("StatusRunning = %q, want checking", cfg.StatusRunning)
	}
	if cfg.WorkerConcurrency != 1 {
		t.Errorf("WorkerConcurrency = %d, want 1", cfg.WorkerConcurrency)
	}
	if cfg.ReviewTimeout != 15*time.Minute {
		t.Errorf("ReviewTimeout = %v, want 15m", cfg.ReviewTimeout)
	}
	if cfg.ReconcileInterval != 5*time.Minute {
		t.Errorf("ReconcileInterval = %v, want 5m", cfg.ReconcileInterval)
	}
	if cfg.Port != 8080 {
		t.Errorf("Port = %d, want 8080", cfg.Port)
	}
	if cfg.DBPath != "/data/state.db" {
		t.Errorf("DBPath = %q, want /data/state.db", cfg.DBPath)
	}
	if cfg.CUWebhookSecret != "" {
		t.Errorf("CUWebhookSecret = %q, want empty (optional)", cfg.CUWebhookSecret)
	}
}

func TestLoad_MissingRequired(t *testing.T) {
	getenv := envMap(map[string]string{"CU_API_TOKEN": ""})
	_, err := Load(getenv)
	if err == nil {
		t.Fatal("expected error for missing CU_API_TOKEN, got nil")
	}
}

func TestLoad_MissingMultipleReportedTogether(t *testing.T) {
	getenv := envMap(map[string]string{"CU_API_TOKEN": "", "REPO_PATH": ""})
	_, err := Load(getenv)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "CU_API_TOKEN") || !strings.Contains(msg, "REPO_PATH") {
		t.Errorf("error should mention both missing vars, got: %s", msg)
	}
}

func TestLoad_ReconcileIntervalZeroDisables(t *testing.T) {
	cfg, err := Load(envMap(map[string]string{"RECONCILE_INTERVAL": "0"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ReconcileInterval != 0 {
		t.Errorf("ReconcileInterval = %v, want 0", cfg.ReconcileInterval)
	}
}

func TestLoad_InvalidDuration(t *testing.T) {
	_, err := Load(envMap(map[string]string{"REVIEW_TIMEOUT": "not-a-duration"}))
	if err == nil {
		t.Fatal("expected error for invalid REVIEW_TIMEOUT")
	}
}

func TestLoad_InvalidInt(t *testing.T) {
	_, err := Load(envMap(map[string]string{"WORKER_CONCURRENCY": "abc"}))
	if err == nil {
		t.Fatal("expected error for invalid WORKER_CONCURRENCY")
	}
}

func TestNormalizeStatus(t *testing.T) {
	cases := map[string]string{
		"  To Check ": "to check",
		"CHECKING":    "checking",
		"checking":    "checking",
	}
	for in, want := range cases {
		if got := NormalizeStatus(in); got != want {
			t.Errorf("NormalizeStatus(%q) = %q, want %q", in, got, want)
		}
	}
}
