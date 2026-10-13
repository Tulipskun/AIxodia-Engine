package db

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

var (
	AccountID string
	DBID      string
	token     string
)

var httpClient = &http.Client{Timeout: 15 * time.Second}

const apiBase = "https://api.cloudflare.com/client/v4"

var identifierPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type Where map[string]any

type apiResponse struct {
	Success bool            `json:"success"`
	Errors  []apiError      `json:"errors"`
	Result  json.RawMessage `json:"result"`
}

type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type tokenVerifyResult struct {
	Status string `json:"status"`
}

type accountResult struct {
	ID string `json:"id"`
}

type databaseResult struct {
	UUID string `json:"uuid"`
	Name string `json:"name"`
}

type queryRequest struct {
	SQL    string `json:"sql"`
	Params []any  `json:"params,omitempty"`
}

type queryResult struct {
	Success bool             `json:"success"`
	Errors  []apiError       `json:"errors"`
	Rows    []map[string]any `json:"results"`
	Meta    map[string]any   `json:"meta"`
}

func Verify(cfToken string) bool {
	cfToken = strings.TrimSpace(cfToken)
	if cfToken == "" {
		return false
	}

	var verify tokenVerifyResult
	if err := get("/user/tokens/verify", cfToken, &verify); err != nil || verify.Status != "active" {
		return false
	}

	accountID, dbID, err := getDatabaseLocation(cfToken)
	if err != nil {
		return false
	}

	token = cfToken
	AccountID = accountID
	DBID = dbID
	return true
}

func getDatabaseLocation(cfToken string) (string, string, error) {
	var accounts []accountResult
	if err := get("/accounts", cfToken, &accounts); err != nil {
		return "", "", err
	}
	if len(accounts) == 0 {
		return "", "", fmt.Errorf("db: token has no account")
	}

	for _, account := range accounts {
		var databases []databaseResult
		if err := get("/accounts/"+account.ID+"/d1/database", cfToken, &databases); err != nil {
			continue
		}
		for _, database := range databases {
			if database.Name == "aixodia" {
				return account.ID, database.UUID, nil
			}
		}
	}
	return "", "", fmt.Errorf("db: D1 database named aixodia was not found in any accessible account")
}

// SaveTunnel replaces the single tunnel row with this daemon's address, its
// version (which carries the start time) and its first heartbeat. Everything a
// client needs to find the daemon and read its uptime lives in this one row,
// so there is no separate nodes table.
func SaveTunnel(url, version string, heartbeat int64) error {
	url = strings.TrimSpace(url)
	if url == "" {
		return fmt.Errorf("db: tunnel url is empty")
	}

	if _, err := Query("CREATE TABLE IF NOT EXISTS tunnel (url TEXT NOT NULL, version TEXT NOT NULL DEFAULT '', heartbeat INTEGER NOT NULL DEFAULT 0)"); err != nil {
		return err
	}
	// Older databases have a tunnel table with only `url`. Add the new columns
	// once; a duplicate-column error means the column is already there.
	_, _ = Query("ALTER TABLE tunnel ADD COLUMN version TEXT NOT NULL DEFAULT ''")
	_, _ = Query("ALTER TABLE tunnel ADD COLUMN heartbeat INTEGER NOT NULL DEFAULT 0")
	if err := Delete("tunnel"); err != nil {
		return err
	}

	_, err := Insert("tunnel", map[string]any{"url": url, "version": version, "heartbeat": heartbeat})
	if err != nil {
		return err
	}
	// Keep reads fast as tables grow. Idempotent: safe to run on every daemon start.
	for _, idx := range []string{
		"CREATE INDEX IF NOT EXISTS idx_sessions_updated ON sessions(updated_at)",
		"CREATE INDEX IF NOT EXISTS idx_turns_session_seq ON turns(session_id, seq)",
	} {
		if _, err := Query(idx); err != nil {
			log.Printf("db index %s: %v", idx, err)
		}
	}
	return nil
}

// TouchTunnel refreshes the version and heartbeat of the single tunnel row.
// The row is replaced on every daemon start, so there is always exactly one.
func TouchTunnel(version string, heartbeat int64) error {
	_, err := Query("UPDATE tunnel SET version = ?, heartbeat = ?", version, heartbeat)
	return err
}

func Insert(table string, data map[string]any) (int64, error) {
	if err := validateIdentifier(table); err != nil {
		return 0, err
	}
	if len(data) == 0 {
		return 0, fmt.Errorf("db: insert data is empty")
	}

	columns, values, err := buildColumnsAndValues(data)
	if err != nil {
		return 0, err
	}

	sql := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", table, strings.Join(columns, ", "), placeholders(len(values)))
	rows, err := Query(sql, values...)
	if err != nil {
		return 0, err
	}
	return affectedRows(rows), nil
}

func Select(table string, where ...Where) ([]map[string]any, error) {
	if err := validateIdentifier(table); err != nil {
		return nil, err
	}

	sql := "SELECT * FROM " + table
	var params []any

	if len(where) > 0 && len(where[0]) > 0 {
		clause, values, err := buildWhere(where[0])
		if err != nil {
			return nil, err
		}
		sql += " WHERE " + clause
		params = values
	}

	return Query(sql, params...)
}

func Update(table string, data map[string]any, where ...Where) (int64, error) {
	if err := validateIdentifier(table); err != nil {
		return 0, err
	}
	if len(data) == 0 {
		return 0, fmt.Errorf("db: update data is empty")
	}
	if len(where) == 0 || len(where[0]) == 0 {
		return 0, fmt.Errorf("db: update requires Where")
	}

	setClause, setValues, err := buildSet(data)
	if err != nil {
		return 0, err
	}
	whereClause, whereValues, err := buildWhere(where[0])
	if err != nil {
		return 0, err
	}

	params := append(setValues, whereValues...)
	rows, err := Query(fmt.Sprintf("UPDATE %s SET %s WHERE %s", table, setClause, whereClause), params...)
	if err != nil {
		return 0, err
	}
	return affectedRows(rows), nil
}

func Delete(table string, where ...Where) error {
	if err := validateIdentifier(table); err != nil {
		return err
	}

	sql := "DELETE FROM " + table
	var params []any

	if len(where) > 0 && len(where[0]) > 0 {
		clause, values, err := buildWhere(where[0])
		if err != nil {
			return err
		}
		sql += " WHERE " + clause
		params = values
	}

	_, err := Query(sql, params...)
	return err
}

func Query(sql string, params ...any) ([]map[string]any, error) {
	sql = strings.TrimSpace(sql)
	if sql == "" {
		return nil, fmt.Errorf("db: SQL is empty")
	}
	if token == "" || AccountID == "" || DBID == "" {
		return nil, fmt.Errorf("db: Verify must succeed first")
	}

	body, err := json.Marshal(queryRequest{SQL: sql, Params: params})
	if err != nil {
		return nil, err
	}

	endpoint := fmt.Sprintf("%s/accounts/%s/d1/database/%s/query", apiBase, AccountID, DBID)
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result apiResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || !result.Success {
		return nil, apiErrorMessage("D1 query", resp.StatusCode, result.Errors)
	}

	var results []queryResult
	if err := json.Unmarshal(result.Result, &results); err != nil {
		return nil, fmt.Errorf("db: decode D1 query result: %w", err)
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("db: D1 returned no query result")
	}
	if !results[0].Success {
		return nil, apiErrorMessage("D1 query", resp.StatusCode, results[0].Errors)
	}

	return results[0].Rows, nil
}

func buildColumnsAndValues(data map[string]any) ([]string, []any, error) {
	columns := make([]string, 0, len(data))
	for column := range data {
		if err := validateIdentifier(column); err != nil {
			return nil, nil, err
		}
		columns = append(columns, column)
	}
	sort.Strings(columns)

	values := make([]any, len(columns))
	for i, column := range columns {
		values[i] = data[column]
	}
	return columns, values, nil
}

func buildSet(data map[string]any) (string, []any, error) {
	columns, values, err := buildColumnsAndValues(data)
	if err != nil {
		return "", nil, err
	}

	parts := make([]string, len(columns))
	for i, column := range columns {
		parts[i] = column + " = ?"
	}
	return strings.Join(parts, ", "), values, nil
}

func buildWhere(where Where) (string, []any, error) {
	if len(where) == 0 {
		return "", nil, fmt.Errorf("db: Where is empty")
	}

	columns := make([]string, 0, len(where))
	for column := range where {
		if err := validateIdentifier(column); err != nil {
			return "", nil, err
		}
		columns = append(columns, column)
	}
	sort.Strings(columns)

	parts := make([]string, len(columns))
	values := make([]any, len(columns))
	for i, column := range columns {
		parts[i] = column + " = ?"
		values[i] = where[column]
	}
	return strings.Join(parts, " AND "), values, nil
}

func validateIdentifier(value string) error {
	if !identifierPattern.MatchString(value) {
		return fmt.Errorf("db: invalid identifier %q", value)
	}
	return nil
}

func placeholders(count int) string {
	parts := make([]string, count)
	for i := range parts {
		parts[i] = "?"
	}
	return strings.Join(parts, ", ")
}

func affectedRows(rows []map[string]any) int64 {
	if len(rows) == 0 {
		return 0
	}
	for _, key := range []string{"rows_written", "changes"} {
		if value, ok := rows[0][key]; ok {
			switch n := value.(type) {
			case float64:
				return int64(n)
			case int64:
				return n
			case int:
				return int64(n)
			}
		}
	}
	return 0
}

func get(path, cfToken string, out any) error {
	req, err := http.NewRequest(http.MethodGet, apiBase+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+cfToken)

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var result apiResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || !result.Success {
		return apiErrorMessage(path, resp.StatusCode, result.Errors)
	}
	if err := json.Unmarshal(result.Result, out); err != nil {
		return fmt.Errorf("db: decode %s: %w", path, err)
	}
	return nil
}

func apiErrorMessage(operation string, status int, errors []apiError) error {
	message := fmt.Sprintf("HTTP %d", status)
	if len(errors) > 0 && errors[0].Message != "" {
		message = errors[0].Message
	}
	return fmt.Errorf("db: %s failed: %s", operation, message)
}
