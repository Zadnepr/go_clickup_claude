package httpapi

import (
	"encoding/json"
	"net/http"
)

// newInvocationsHandler обрабатывает GET /api/invocations?window=hour|day|all
// — плоский лог вызовов claude (сессий /spec и /review) по всем прогонам за
// период, с токенами и стоимостью каждого (см. Требование «кол-во
// потраченных токенов по каждой сессии и по указанным промежуткам»). Для
// токенов конкретного прогона см. GET /api/runs/{id}.
func newInvocationsHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		since, windowName := resolveStatsWindow(r.URL.Query().Get("window"))

		invocations, err := deps.Store.ListInvocationsSince(r.Context(), since)
		if err != nil {
			deps.Logger.Error("failed to list invocations", "error", err.Error())
			http.Error(w, "failed to list invocations", http.StatusInternalServerError)
			return
		}

		var totalInput, totalOutput int64
		var totalCost float64
		for _, inv := range invocations {
			totalInput += inv.InputTokens
			totalOutput += inv.OutputTokens
			totalCost += inv.CostUSD
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"window":      windowName,
			"since":       since,
			"invocations": invocations,
			"totals": map[string]any{
				"input_tokens":  totalInput,
				"output_tokens": totalOutput,
				"cost_usd":      totalCost,
				"count":         len(invocations),
			},
		})
	}
}
