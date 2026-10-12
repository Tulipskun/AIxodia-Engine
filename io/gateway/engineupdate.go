package gateway

// POST /api/engine/update replaces the running engine with the newest build.
// It is the answer to "why does an engine change need a dataset version and a
// kernel restart": it does not, any more. The handler is a thin wrapper — the
// work (download, verify, re-exec) lives in the process, which the gateway
// reaches through a callback so this package stays free of process concerns.

import (
	"log"
	"net/http"
	"time"
)

func (g *Gateway) apiEngineUpdate(w http.ResponseWriter, r *http.Request) {
	if g.engineUpdater == nil {
		writeError(w, http.StatusNotImplemented, "unsupported", "this deployment cannot update itself")
		return
	}
	commit, err := g.engineUpdater()
	if err != nil {
		writeError(w, http.StatusBadGateway, "update_failed", err.Error())
		return
	}
	// Answer before the swap: once the process image is replaced the connection
	// is gone, so a response written afterwards would never arrive. The client
	// gets "accepted" and the daemon comes back with a new tunnel URL.
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "status": "updating"})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	go func() {
		// A short pause lets the response drain before the connection dies.
		time.Sleep(400 * time.Millisecond)
		if err := commit(); err != nil {
			log.Printf("engine update: %v", err)
		}
	}()
}
