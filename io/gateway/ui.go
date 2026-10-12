package gateway

// Server-driven UI (AX-UI). The app ships a fixed vocabulary of components and
// asks the daemon for a spec describing how to arrange and word them. Editing
// the spec changes the app's look and wording without rebuilding or
// reinstalling the APK — the phone picks the spec up on its next open.
//
// The spec is a JSON object stored in the D1 `state` table under `ui_spec`.
// Only the keys a client wants to change need to be written: reads deep-merge
// the stored override over the built-in default, so a client always receives a
// complete spec and a partial PUT is enough.

import (
	"encoding/json"
	"net/http"

	"ai-engine/db"
)

const uiStateKey = "ui_spec"

// defaultUISpec is the complete vocabulary the app understands. Every key here
// is optional to override; the values are the current shipping defaults.
func defaultUISpec() json.RawMessage {
	return json.RawMessage(`{
  "version": 1,
  "theme": {
    "seed": "#7C6CF0",
    "dark": true,
    "corner_radius": 18
  },
  "strings": {
    "app_name": "AIxodia",
    "composer_placeholder": "ส่งข้อความ…",
    "send": "ส่ง",
    "stop": "หยุดการทำงาน",
    "retry": "ลองอีกครั้ง",
    "dismiss": "ปิด",
    "new_chat": "แชทใหม่",
    "settings": "ตั้งค่า",
    "providers": "ผู้ให้บริการ",
    "refresh": "ดึงข้อมูลล่าสุด",
    "menu": "เมนูแชท",
    "agent_default": "Agent default",
    "pick_model": "เลือก model",
    "thinking": "กำลังคิด",
    "answering": "กำลังตอบ",
    "copied": "คัดลอกแล้ว",
    "copy_hint": "แตะค้างเพื่อคัดลอก",
    "online": "ออนไลน์",
    "connecting": "กำลังต่อ",
    "offline": "ออฟไลน์",
    "error_title": "เกิดข้อผิดพลาด",
    "collapse": "ยุบ",
    "expand": "ขยาย",
    "working": "agent กำลังทำงาน",
    "empty_title": "เริ่มสนทนากับ agent",
    "empty_hint": "พิมพ์งานด้านล่าง แล้ว agent จะทำต่อแม้ปิดแอป"
  },
  "topbar": {
    "show_status_dot": true,
    "show_uptime": true,
    "uptime_format": "HH:mm:ss",
    "show_refresh": true,
    "show_model_chip": true
  },
  "tool_block": {
    "collapsed_by_default": true,
    "header_template": "{icon} {name} {args}",
    "show_duration": true,
    "max_args_chars": 80,
    "show_icon": true
  },
  "error_card": {
    "show_retry": true,
    "show_dismiss": true,
    "show_raw": false
  },
  "composer": {
    "max_lines": 5,
    "show_stop_button": true
  }
}`)
}

// uiSpec returns the built-in default deep-merged with the stored override, so
// the client always receives a complete spec.
func (g *Gateway) uiSpec() (json.RawMessage, error) {
	base := map[string]any{}
	if err := json.Unmarshal(defaultUISpec(), &base); err != nil {
		return nil, err
	}
	rows, err := db.Select("state", db.Where{"key": uiStateKey})
	if err == nil && len(rows) > 0 {
		var override map[string]any
		if json.Unmarshal([]byte(rowString(rows[0], "value")), &override) == nil {
			mergeMaps(base, override)
		}
	}
	out, err := json.Marshal(base)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// apiGetUI answers the effective spec (default + override).
func (g *Gateway) apiGetUI(w http.ResponseWriter, r *http.Request) {
	spec, err := g.uiSpec()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	writeRaw(w, spec)
}

// apiPutUI stores a partial spec. It is merged over whatever override already
// exists, so two edits to different sections do not erase each other.
func (g *Gateway) apiPutUI(w http.ResponseWriter, r *http.Request) {
	var raw json.RawMessage
	if err := decodeBody(r, &raw); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	var incoming map[string]any
	if err := json.Unmarshal(raw, &incoming); err != nil || incoming == nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "ui spec must be a JSON object")
		return
	}

	stored := map[string]any{}
	if rows, err := db.Select("state", db.Where{"key": uiStateKey}); err == nil && len(rows) > 0 {
		_ = json.Unmarshal([]byte(rowString(rows[0], "value")), &stored)
	}
	if stored == nil {
		stored = map[string]any{}
	}
	mergeMaps(stored, incoming)
	merged, err := json.Marshal(stored)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	if _, err := db.Query(
		"INSERT INTO state (key, value, updated_at) VALUES (?, ?, unixepoch()) ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at",
		uiStateKey, string(merged),
	); err != nil {
		writeError(w, http.StatusBadGateway, "database_error", err.Error())
		return
	}
	effective, err := g.uiSpec()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	// Push the new spec to every connected phone. This is the whole update
	// path: no client polls, and one that is open changes as soon as the save
	// lands.
	if g.uiChanged != nil {
		g.uiChanged(effective)
	}
	writeRaw(w, effective)
}

// mergeMaps copies src into dst recursively: nested objects merge key by key,
// any other value replaces. A null in src removes the key, so a client can drop
// an override and fall back to the default.
func mergeMaps(dst, src map[string]any) {
	for key, value := range src {
		if value == nil {
			delete(dst, key)
			continue
		}
		srcChild, ok := value.(map[string]any)
		if !ok {
			dst[key] = value
			continue
		}
		dstChild, ok := dst[key].(map[string]any)
		if !ok {
			dst[key] = srcChild
			continue
		}
		mergeMaps(dstChild, srcChild)
	}
}
