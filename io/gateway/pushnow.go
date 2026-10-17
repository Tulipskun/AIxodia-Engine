package gateway

// POST /api/push-now sets the D1 push flag and leaves a sentinel file so the
// notebook can act on it immediately instead of waiting for its next poll.
import (
	"log"
	"net/http"
	"os"

	"ai-engine/db"
)

func (g *Gateway) apiPushNow(w http.ResponseWriter, r *http.Request) {
	if _, err := db.Query(
		"INSERT INTO system_state (key, value, updated_at) VALUES (?, ?, unixepoch()) "+
			"ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at",
		"kernel/push-now", "1",
	); err != nil {
		log.Printf("push-now: d1 write failed: %v", err)
	}
	// Sentinel for the notebook's serve() loop: it deletes this file and runs
	// check_push_flag() on the next tick, so a push is not delayed by the
	// 60-second poll.
	if f, err := os.Create("/kaggle/working/.push-now"); err == nil {
		f.Close()
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
