package localdb

import (
	"database/sql"
	"errors"
)

// GetState reads one key of the engine's own state: settings, and the notes a
// session leaves for the one that replaces it. found is false for a key that
// was never written, which is not an error — a fresh install asks for settings
// and is told the defaults.
func GetState(handle *sql.DB, key string) (string, bool, error) {
	var value string
	err := handle.QueryRow(`SELECT value FROM state WHERE key=?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return value, true, nil
}

// PutState writes one key, replacing an earlier value.
func PutState(handle *sql.DB, key, value string) error {
	_, err := handle.Exec(
		`INSERT INTO state (key, value, updated_at) VALUES (?,?, unixepoch())
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value)
	return err
}
