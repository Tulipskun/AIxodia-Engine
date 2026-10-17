package gateway

import (
	"net/http"
	"strconv"
)

// GET /api/logs returns recent engine log entries (newest first).
// Logs now live on the engine machine (local SQLite), not in D1.
func (g *Gateway) apiLogs(w http.ResponseWriter, r *http.Request) {
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 2000 {
			limit = n
		}
	}
	level := r.URL.Query().Get("level")
	var logs []map[string]any
	if g.logReader != nil {
		logs = g.logReader(limit, level)
	}
	if logs == nil {
		logs = []map[string]any{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"logs": logs, "count": len(logs)})
}
