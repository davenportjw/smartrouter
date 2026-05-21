package config

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// APIConfigStore implements AdminStore by making REST API calls to the Backend API Service.
type APIConfigStore struct {
	BackendURL   string
	SharedSecret string
	HTTPClient   *http.Client
}

// NewAPIConfigStore initializes a new API client config store.
func NewAPIConfigStore(backendURL, sharedSecret string) *APIConfigStore {
	if backendURL == "" {
		backendURL = "http://localhost:8080"
	}
	// Trim trailing slash for consistency
	backendURL = strings.TrimSuffix(backendURL, "/")

	return &APIConfigStore{
		BackendURL:   backendURL,
		SharedSecret: sharedSecret,
		HTTPClient:   &http.Client{Timeout: 10 * time.Second},
	}
}

// fetchOIDCToken queries the local GCP metadata server to retrieve an identity OIDC token for service-to-service call.
func (ac *APIConfigStore) fetchOIDCToken(ctx context.Context) (string, error) {
	client := &http.Client{Timeout: 1 * time.Second}
	tokenURL := fmt.Sprintf("http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/identity?audience=%s", url.QueryEscape(ac.BackendURL))
	req, err := http.NewRequestWithContext(ctx, "GET", tokenURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("metadata server returned status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(body)), nil
}

// makeRequest is a private helper to execute authenticated HTTP requests against the Backend API.
func (ac *APIConfigStore) makeRequest(ctx context.Context, method, path string, body interface{}, result interface{}) error {
	var bodyReader io.Reader
	if body != nil {
		jsonBytes, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("failed to marshal body: %w", err)
		}
		bodyReader = bytes.NewReader(jsonBytes)
	}

	targetURL := fmt.Sprintf("%s%s", ac.BackendURL, path)
	req, err := http.NewRequestWithContext(ctx, method, targetURL, bodyReader)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	// 1. Attach authorization token (prefer SharedSecret if configured)
	if ac.SharedSecret != "" {
		req.Header.Set("Authorization", "Bearer "+ac.SharedSecret)
	} else if os.Getenv("LOCAL_DEV") != "true" {
		token, err := ac.fetchOIDCToken(ctx)
		if err == nil && token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
	}

	resp, err := ac.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		bodyErr, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API returned status %s: %s", resp.Status, string(bodyErr))
	}

	if result != nil {
		if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
			return fmt.Errorf("failed to decode response: %w", err)
		}
	}

	return nil
}

// AdminStore Interface Implementation

func (ac *APIConfigStore) GetAllKeys(ctx context.Context) ([]APIKey, error) {
	var keys []APIKey
	err := ac.makeRequest(ctx, "GET", "/api/keys", nil, &keys)
	return keys, err
}

func (ac *APIConfigStore) GetAllClients(ctx context.Context) ([]Client, error) {
	var clients []Client
	err := ac.makeRequest(ctx, "GET", "/api/clients", nil, &clients)
	return clients, err
}

func (ac *APIConfigStore) GetAllApps(ctx context.Context) ([]App, error) {
	var apps []App
	err := ac.makeRequest(ctx, "GET", "/api/apps", nil, &apps)
	return apps, err
}

func (ac *APIConfigStore) LookupApp(appID string) (App, bool) {
	// Since LookupApp is called synchronous/cache-like in Go templates without context,
	// we perform a fast REST call utilizing a background context.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var app App
	err := ac.makeRequest(ctx, "GET", fmt.Sprintf("/api/apps/lookup?id=%s", url.QueryEscape(appID)), nil, &app)
	if err != nil {
		log.Printf("[APIConfigStore] LookupApp failed for id %s: %v", appID, err)
		return App{}, false
	}
	return app, true
}

func (ac *APIConfigStore) SaveKey(ctx context.Context, key APIKey) error {
	return ac.makeRequest(ctx, "POST", "/api/keys", key, nil)
}

func (ac *APIConfigStore) RevokeKey(ctx context.Context, hash string) error {
	path := fmt.Sprintf("/api/keys/revoke?hash=%s", url.QueryEscape(hash))
	return ac.makeRequest(ctx, "POST", path, nil, nil)
}

func (ac *APIConfigStore) GetAllRules(ctx context.Context) ([]RoutingRule, error) {
	var rules []RoutingRule
	err := ac.makeRequest(ctx, "GET", "/api/rules", nil, &rules)
	return rules, err
}

func (ac *APIConfigStore) GetAllModels(ctx context.Context) ([]ModelConfig, error) {
	var models []ModelConfig
	err := ac.makeRequest(ctx, "GET", "/api/models", nil, &models)
	return models, err
}

func (ac *APIConfigStore) SaveRule(ctx context.Context, rule RoutingRule) error {
	return ac.makeRequest(ctx, "POST", "/api/rules", rule, nil)
}

func (ac *APIConfigStore) DeleteRule(ctx context.Context, id string) error {
	path := fmt.Sprintf("/api/rules?id=%s", url.QueryEscape(id))
	return ac.makeRequest(ctx, "DELETE", path, nil, nil)
}

func (ac *APIConfigStore) GetAllHeaders(ctx context.Context) ([]CustomHeader, error) {
	var headers []CustomHeader
	err := ac.makeRequest(ctx, "GET", "/api/headers", nil, &headers)
	return headers, err
}

func (ac *APIConfigStore) SaveHeader(ctx context.Context, header CustomHeader) error {
	return ac.makeRequest(ctx, "POST", "/api/headers", header, nil)
}

func (ac *APIConfigStore) DeleteHeader(ctx context.Context, id string) error {
	path := fmt.Sprintf("/api/headers?id=%s", url.QueryEscape(id))
	return ac.makeRequest(ctx, "DELETE", path, nil, nil)
}

func (ac *APIConfigStore) SaveModel(ctx context.Context, model ModelConfig) error {
	return ac.makeRequest(ctx, "POST", "/api/models", model, nil)
}

func (ac *APIConfigStore) DeleteModel(ctx context.Context, id string) error {
	path := fmt.Sprintf("/api/models?id=%s", url.QueryEscape(id))
	return ac.makeRequest(ctx, "DELETE", path, nil, nil)
}

func (ac *APIConfigStore) SaveApp(ctx context.Context, app App) error {
	return ac.makeRequest(ctx, "POST", "/api/apps", app, nil)
}

func (ac *APIConfigStore) DeleteApp(ctx context.Context, id string) error {
	path := fmt.Sprintf("/api/apps?id=%s", url.QueryEscape(id))
	return ac.makeRequest(ctx, "DELETE", path, nil, nil)
}

func (ac *APIConfigStore) SaveClient(ctx context.Context, client Client) error {
	return ac.makeRequest(ctx, "POST", "/api/clients", client, nil)
}

func (ac *APIConfigStore) DeleteClient(ctx context.Context, id string) error {
	path := fmt.Sprintf("/api/clients?id=%s", url.QueryEscape(id))
	return ac.makeRequest(ctx, "DELETE", path, nil, nil)
}

func (ac *APIConfigStore) GetQueueStatus(ctx context.Context) ([]QueueSnapshotItem, error) {
	var status []QueueSnapshotItem
	err := ac.makeRequest(ctx, "GET", "/api/queue", nil, &status)
	return status, err
}
