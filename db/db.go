package db

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
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

	// This endpoint only answers for user-owned tokens. An account-owned token
	// is a normal server credential and answers 401 here even when it is valid;
	// the account and database lookups below are the real proof. A user token
	// that was revoked or disabled still fails there, so a definite answer of
	// "not active" is respected.
	var verify tokenVerifyResult
	if err := get("/user/tokens/verify", cfToken, &verify); err == nil && verify.Status != "active" {
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

// PutState writes one system_state key. Settings and the handover note live
// here alongside the daemon keys, so a fresh engine finds them.
func PutState(key, value string) error {
	return upsertState(key, value)
}

// State is the key/value store the gateway speaks to. Keeping it a type with
// methods (rather than the package functions) lets the gateway take an
// interface and tests a map.
type State struct{}

func (State) GetState(key string) (string, bool, error) { return GetState(key) }
func (State) PutState(key, value string) error          { return PutState(key, value) }

// GetState reads one system_state key. found is false for a key that was never
// written, which is not an error: a fresh install asks for settings and is told
// the defaults.
func GetState(key string) (string, bool, error) {
	rows, err := Query("SELECT value FROM system_state WHERE key=?", key)
	if err != nil {
		return "", false, err
	}
	if len(rows) == 0 {
		return "", false, nil
	}
	value, _ := rows[0]["value"].(string)
	return value, true, nil
}

// upsertState writes one system_state key, replacing any earlier value.
func upsertState(key, value string) error {
	_, err := Query("INSERT INTO system_state (key, value, updated_at) VALUES (?, ?, unixepoch()) ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at", key, value)
	return err
}

// SaveDaemon writes this daemon's address, version (which carries the start
// time) and its first heartbeat into the system_state keys. Everything a
// client needs to find the daemon and read its uptime lives in these keys,
// so there is no separate tunnel table.
func SaveDaemon(url, version string, heartbeat int64) error {
	url = strings.TrimSpace(url)
	if url == "" {
		return fmt.Errorf("db: daemon url is empty")
	}

	for _, kv := range [][2]string{
		{"daemon/url", url},
		{"daemon/version", version},
		{"daemon/heartbeat", strconv.FormatInt(heartbeat, 10)},
	} {
		if err := upsertState(kv[0], kv[1]); err != nil {
			return err
		}
	}
	return nil
}

// TouchDaemon refreshes the daemon version and heartbeat keys.
func TouchDaemon(version string, heartbeat int64) error {
	for _, kv := range [][2]string{
		{"daemon/version", version},
		{"daemon/heartbeat", strconv.FormatInt(heartbeat, 10)},
	} {
		if err := upsertState(kv[0], kv[1]); err != nil {
			return err
		}
	}
	return nil
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
	applyAuth(req, token)
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

// applyAuth sets the Cloudflare credentials on a request. A value containing a
// colon is treated as an account global API key in "email:key" form and uses
// the X-Auth-* headers; anything else is a scoped API token sent as a Bearer.
func applyAuth(req *http.Request, cred string) {
	if i := strings.Index(cred, ":"); i >= 0 && i < len(cred)-1 {
		req.Header.Set("X-Auth-Email", cred[:i])
		req.Header.Set("X-Auth-Key", cred[i+1:])
	} else {
		req.Header.Set("Authorization", "Bearer "+cred)
	}
}

func get(path, cfToken string, out any) error {
	req, err := http.NewRequest(http.MethodGet, apiBase+path, nil)
	if err != nil {
		return err
	}
	applyAuth(req, cfToken)

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
