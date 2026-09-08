package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
)

// newReadyzHandler проверяет: база данных открыта, репозиторий на месте,
// claude --version отвечает. Ловит протухший токен и битую конфигурацию
// сразу, а не через сутки молчания в проде.
func newReadyzHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), deps.ReadyzTimeout)
		defer cancel()

		if err := deps.Store.Ping(ctx); err != nil {
			writeNotReady(w, deps, "database not reachable", err)
			return
		}

		if info, err := os.Stat(deps.RepoPath); err != nil || !info.IsDir() {
			var err2 error
			if err != nil {
				err2 = err
			} else {
				err2 = fmt.Errorf("%s is not a directory", deps.RepoPath)
			}
			writeNotReady(w, deps, "repository path not available", err2)
			return
		}

		if err := exec.CommandContext(ctx, deps.ClaudeBinary, "--version").Run(); err != nil {
			writeNotReady(w, deps, "claude binary not responding", err)
			return
		}

		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}
}

func writeNotReady(w http.ResponseWriter, deps Deps, reason string, err error) {
	deps.Logger.Error("readyz check failed", "reason", reason, "error", err.Error())
	w.WriteHeader(http.StatusServiceUnavailable)
	fmt.Fprintf(w, "not ready: %s", reason)
}
