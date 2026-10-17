package gateway

import (
	"net/http"
	"strconv"

	"ai-engine/db"
)

// GET /api/logs returns recent engine log entries (newest first).
func (g *Gateway) apiLogs(w http.ResponseWriter, r *http.Request) {
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 2000 {
			limit = n
		}
	}
	level := r.URL.Query().Get("level")
	tables := []string{"engine_logs"}
	for i := 1; i <= 3; i++ {
		t := "engine_logs_" + strconv.Itoa(i)
		rows, _ := db.Query("SELECT 1 FROM sqlite_master WHERE type='table' AND name=?", t)
		if len(rows) > 0 {
			tables = append(tables, t)
		}
	}
	q := ""
	for i, t := range tables {
		if i > 0 {
			q += " UNION ALL "
		}
		q += "SELECT id, level, message, at, '" + t + "' AS tbl FROM " + t
	}
	if level != "" {
		q = "SELECT * FROM (" + q + ") WHERE level = ?"
	}
	q += " ORDER BY at DESC LIMIT " + strconv.Itoa(limit)
	var rows []map[string]any
	var err error
	if level != "" {
		rows, err = db.Query(q, level)
	} else {
		rows, err = db.Query(q)
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, "database_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"logs": rows, "count": len(rows)})
}
