package gateway

// POST /api/engine/update replaces the running engine with the newest build.
// It is the answer to "why does an engine change need a dataset version and a
// kernel restart": it does not, any more. The handler is a thin wrapper — the
// work (download, verify, re-exec) lives in the process, which the gateway
// reaches through a callback so this package stays free of process concerns.

import "net/http"

func (g *Gateway) apiEngineUpdate(w http.ResponseWriter, r *http.Request) {
	if g.engineUpdater == nil {
		writeError(w, http.StatusNotImplemented, "unsupported", "this deployment cannot update itself")
		return
	}
	if err := g.engineUpdater(); err != nil {
		writeError(w, http.StatusBadGateway, "update_failed", err.Error())
		return
	}
	// A successful update re-executes the process, so reaching here means the
	// exec did not happen after all.
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
