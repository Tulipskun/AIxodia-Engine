package gateway

// POST /restart asks the machine this engine runs on to restart the session.
//
// Nothing goes to D1: the engine runs beside the notebook, and the notebook
// watches for ~/.exit, backs the filesystem up to the snapshot repo, and starts
// the next run itself. Creating the file is the whole handshake.
import (
	"log"
	"net/http"
	"os"
	"path/filepath"
)

func (g *Gateway) apiRestart(w http.ResponseWriter, r *http.Request) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		writeError(w, http.StatusInternalServerError, "restart_failed", "cannot find the home directory")
		return
	}
	path := filepath.Join(home, ".exit")
	f, err := os.Create(path)
	if err != nil {
		log.Printf("restart: cannot create %s: %v", path, err)
		writeError(w, http.StatusInternalServerError, "restart_failed", err.Error())
		return
	}
	f.Close()
	log.Printf("restart: wrote %s", path)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "file": path})
}
