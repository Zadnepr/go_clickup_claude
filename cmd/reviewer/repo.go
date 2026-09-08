package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/Zadnepr/go_clickup_claude/internal/config"
)

// cloneRepoIfMissing клонирует REPO_URL в REPO_PATH при старте, если там ещё
// нет рабочей копии (доступ по deploy key, смонтированному как секрет —
// см. README.md и Dockerfile).
func cloneRepoIfMissing(cfg *config.Config, logger *slog.Logger) error {
	if _, err := os.Stat(filepath.Join(cfg.RepoPath, ".git")); err == nil {
		logger.Info("repository already present, skipping clone", "path", cfg.RepoPath)
		return nil
	}

	logger.Info("cloning repository", "url", cfg.RepoURL, "path", cfg.RepoPath)

	if err := os.MkdirAll(cfg.RepoPath, 0o755); err != nil {
		return fmt.Errorf("create repo path %s: %w", cfg.RepoPath, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", "clone", cfg.RepoURL, cfg.RepoPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git clone %s into %s: %w", cfg.RepoURL, cfg.RepoPath, err)
	}
	return nil
}
