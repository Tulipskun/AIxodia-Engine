package db

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// The providers — and the API keys that exist nowhere else — live in Workers
// KV, not in D1. The namespace is looked up by title once per boot and kept in
// kvNamespace; every KV call fails until VerifyKV has found it.
const kvNamespaceTitle = "aixodia-providers"

var kvNamespace string

type kvNamespaceResult struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// VerifyKV finds the providers namespace, creating it when the token may. It is
// separate from Verify because a boot that cannot reach KV still serves the
// built-in provider, and the config reloader retries it.
func VerifyKV() error {
	if token == "" || AccountID == "" {
		return fmt.Errorf("db: Verify must succeed first")
	}

	var namespaces []kvNamespaceResult
	if err := get("/accounts/"+AccountID+"/storage/kv/namespaces?per_page=100", token, &namespaces); err != nil {
		return err
	}
	for _, namespace := range namespaces {
		if namespace.Title == kvNamespaceTitle {
			kvNamespace = namespace.ID
			return nil
		}
	}

	created, err := createKVNamespace(kvNamespaceTitle)
	if err != nil {
		return fmt.Errorf("db: KV namespace %q not found and could not be created: %w", kvNamespaceTitle, err)
	}
	kvNamespace = created.ID
	return nil
}

// KVReady reports whether VerifyKV has found the namespace, so the boot can
// retry it on the config tick instead of on every call.
func KVReady() bool { return kvNamespace != "" }

func createKVNamespace(title string) (kvNamespaceResult, error) {
	body, err := json.Marshal(map[string]string{"title": title})
	if err != nil {
		return kvNamespaceResult{}, err
	}
	endpoint := apiBase + "/accounts/" + AccountID + "/storage/kv/namespaces"
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return kvNamespaceResult{}, err
	}
	applyAuth(req, token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return kvNamespaceResult{}, err
	}
	defer resp.Body.Close()

	var result apiResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return kvNamespaceResult{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || !result.Success {
		return kvNamespaceResult{}, apiErrorMessage("KV create namespace", resp.StatusCode, result.Errors)
	}
	var created kvNamespaceResult
	if err := json.Unmarshal(result.Result, &created); err != nil {
		return kvNamespaceResult{}, fmt.Errorf("db: decode KV namespace: %w", err)
	}
	return created, nil
}

func kvURL(key string) string {
	return apiBase + "/accounts/" + AccountID + "/storage/kv/namespaces/" + kvNamespace + "/values/" + url.PathEscape(key)
}

// KVGet reads one value. found is false for a key that was never written,
// which is not an error: an empty providers key is a fresh install.
func KVGet(key string) (string, bool, error) {
	if kvNamespace == "" {
		return "", false, fmt.Errorf("db: VerifyKV must succeed first")
	}
	req, err := http.NewRequest(http.MethodGet, kvURL(key), nil)
	if err != nil {
		return "", false, err
	}
	applyAuth(req, token)

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return "", false, nil
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", false, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", false, kvError("KV read", resp.StatusCode, raw)
	}
	return string(raw), true, nil
}

// KVPut writes one value, replacing any earlier one.
func KVPut(key, value string) error {
	if kvNamespace == "" {
		return fmt.Errorf("db: VerifyKV must succeed first")
	}
	req, err := http.NewRequest(http.MethodPut, kvURL(key), strings.NewReader(value))
	if err != nil {
		return err
	}
	applyAuth(req, token)
	req.Header.Set("Content-Type", "text/plain")

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return kvError("KV write", resp.StatusCode, raw)
	}
	return nil
}

// KVDelete removes one key. Removing a key that is already gone is a success.
func KVDelete(key string) error {
	if kvNamespace == "" {
		return fmt.Errorf("db: VerifyKV must succeed first")
	}
	req, err := http.NewRequest(http.MethodDelete, kvURL(key), nil)
	if err != nil {
		return err
	}
	applyAuth(req, token)

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return kvError("KV delete", resp.StatusCode, raw)
	}
	return nil
}

// kvError reads the API's JSON error body when there is one, and falls back to
// the status line for the HTML failure pages Cloudflare sometimes returns.
func kvError(operation string, status int, raw []byte) error {
	var body apiResponse
	if err := json.Unmarshal(raw, &body); err == nil && len(body.Errors) > 0 {
		return apiErrorMessage(operation, status, body.Errors)
	}
	return fmt.Errorf("db: %s failed: HTTP %d", operation, status)
}

// KV is the store config speaks to. Keeping it a type with methods (rather than
// the package functions) lets config take an interface and tests a map.
type KV struct{}

func (KV) Get(key string) (string, bool, error) { return KVGet(key) }
func (KV) Put(key, value string) error          { return KVPut(key, value) }
