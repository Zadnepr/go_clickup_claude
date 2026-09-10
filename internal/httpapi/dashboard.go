package httpapi

import (
	"embed"
	"net/http"
)

//go:embed static/dashboard.html
var dashboardFS embed.FS

// newDashboardHandler отдаёт статический веб-дашборд (см. static/dashboard.html)
// — сам он работает целиком через уже существующие JSON-эндпоинты
// (/api/config, /api/queue, /api/stats, /api/invocations, /api/tasks/*),
// никакого отдельного бэкенда для страницы не требуется. Регистрируется на
// "GET /" — единственный маршрут, который иначе ничем не занят.
func newDashboardHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		data, err := dashboardFS.ReadFile("static/dashboard.html")
		if err != nil {
			http.Error(w, "dashboard not available", http.StatusInternalServerError)
			return
		}
		w.Write(data)
	}
}
