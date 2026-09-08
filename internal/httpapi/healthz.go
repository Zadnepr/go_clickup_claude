package httpapi

import "net/http"

// handleHealthz — процесс жив. Никаких внешних проверок: та проверка,
// нужен ли перезапуск процесса, а не готовность обслуживать нагрузку.
func handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}
