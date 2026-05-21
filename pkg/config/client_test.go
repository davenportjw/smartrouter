package config

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAPIConfigStore(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")

	var lastMethod string
	var lastPath string
	var lastAuthHeader string
	var receivedBody []byte

	// Setup mock HTTP server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastMethod = r.Method
		lastPath = r.URL.Path
		if r.URL.RawQuery != "" {
			lastPath = r.URL.Path + "?" + r.URL.RawQuery
		}
		lastAuthHeader = r.Header.Get("Authorization")

		if r.Body != nil {
			var err error
			receivedBody, err = io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("failed to read request body: %v", err)
			}
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)

		// Mock return payloads depending on path
		switch r.URL.Path {
		case "/api/keys":
			if r.Method == "GET" {
				w.Write([]byte(`[{"key_hash":"hash-123","client_id":"c-1","status":"active"}]`))
			}
		case "/api/clients":
			if r.Method == "GET" {
				w.Write([]byte(`[{"id":"c-1","name":"Client 1","tier":"premium"}]`))
			}
		case "/api/apps":
			if r.Method == "GET" {
				w.Write([]byte(`[{"id":"a-1","client_id":"c-1","name":"App 1"}]`))
			}
		case "/api/apps/lookup":
			w.Write([]byte(`{"id":"a-1","client_id":"c-1","name":"App 1"}`))
		case "/api/rules":
			if r.Method == "GET" {
				w.Write([]byte(`[{"id":"r-1","target_model":"gemini-2.5-pro"}]`))
			}
		case "/api/models":
			if r.Method == "GET" {
				w.Write([]byte(`[{"id":"gemini-2.5-flash","active":true}]`))
			}
		case "/api/headers":
			if r.Method == "GET" {
				w.Write([]byte(`[{"id":"h-1","name":"X-Header"}]`))
			}
		case "/api/queue":
			w.Write([]byte(`[]`))
		default:
			w.Write([]byte(`{"status":"ok"}`))
		}
	}))
	defer server.Close()

	sharedSecret := "mock-secret"
	store := NewAPIConfigStore(server.URL, sharedSecret)

	ctx := context.Background()

	t.Run("GetAllKeys", func(t *testing.T) {
		keys, err := store.GetAllKeys(ctx)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(keys) != 1 || keys[0].KeyHash != "hash-123" {
			t.Errorf("unexpected keys response: %+v", keys)
		}
		if lastMethod != "GET" || lastPath != "/api/keys" {
			t.Errorf("unexpected request: Method=%s, Path=%s", lastMethod, lastPath)
		}
		if lastAuthHeader != "Bearer "+sharedSecret {
			t.Errorf("unexpected auth header: %q", lastAuthHeader)
		}
	})

	t.Run("GetAllClients", func(t *testing.T) {
		clients, err := store.GetAllClients(ctx)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(clients) != 1 || clients[0].ID != "c-1" {
			t.Errorf("unexpected clients response: %+v", clients)
		}
		if lastMethod != "GET" || lastPath != "/api/clients" {
			t.Errorf("unexpected request: Method=%s, Path=%s", lastMethod, lastPath)
		}
	})

	t.Run("GetAllApps", func(t *testing.T) {
		apps, err := store.GetAllApps(ctx)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(apps) != 1 || apps[0].ID != "a-1" {
			t.Errorf("unexpected apps response: %+v", apps)
		}
	})

	t.Run("LookupApp", func(t *testing.T) {
		app, ok := store.LookupApp("a-1")
		if !ok {
			t.Fatalf("LookupApp failed")
		}
		if app.ID != "a-1" {
			t.Errorf("unexpected app: %+v", app)
		}
		if lastMethod != "GET" || lastPath != "/api/apps/lookup?id=a-1" {
			t.Errorf("unexpected request path: %q", lastPath)
		}
	})

	t.Run("SaveKey", func(t *testing.T) {
		key := APIKey{KeyHash: "new-hash", ClientID: "c-1", Status: "active"}
		err := store.SaveKey(ctx, key)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if lastMethod != "POST" || lastPath != "/api/keys" {
			t.Errorf("unexpected request: Method=%s, Path=%s", lastMethod, lastPath)
		}
		var savedKey APIKey
		if err := json.Unmarshal(receivedBody, &savedKey); err != nil {
			t.Fatalf("failed to unmarshal received body: %v", err)
		}
		if savedKey.KeyHash != "new-hash" {
			t.Errorf("unexpected saved key hash: %s", savedKey.KeyHash)
		}
	})

	t.Run("RevokeKey", func(t *testing.T) {
		err := store.RevokeKey(ctx, "hash-to-revoke")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if lastMethod != "POST" || lastPath != "/api/keys/revoke?hash=hash-to-revoke" {
			t.Errorf("unexpected request: Method=%s, Path=%s", lastMethod, lastPath)
		}
	})

	t.Run("GetAllRules", func(t *testing.T) {
		rules, err := store.GetAllRules(ctx)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(rules) != 1 || rules[0].ID != "r-1" {
			t.Errorf("unexpected rules response")
		}
	})

	t.Run("SaveRule", func(t *testing.T) {
		rule := RoutingRule{ID: "new-rule", TargetModel: "gemini-2.5-pro"}
		err := store.SaveRule(ctx, rule)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if lastMethod != "POST" || lastPath != "/api/rules" {
			t.Errorf("unexpected request: Method=%s, Path=%s", lastMethod, lastPath)
		}
	})

	t.Run("DeleteRule", func(t *testing.T) {
		err := store.DeleteRule(ctx, "rule-id")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if lastMethod != "DELETE" || lastPath != "/api/rules?id=rule-id" {
			t.Errorf("unexpected request: Method=%s, Path=%s", lastMethod, lastPath)
		}
	})

	t.Run("GetAllHeaders", func(t *testing.T) {
		headers, err := store.GetAllHeaders(ctx)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(headers) != 1 || headers[0].ID != "h-1" {
			t.Errorf("unexpected headers response")
		}
	})

	t.Run("SaveHeader", func(t *testing.T) {
		header := CustomHeader{ID: "new-header", Name: "X-Header"}
		err := store.SaveHeader(ctx, header)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if lastMethod != "POST" || lastPath != "/api/headers" {
			t.Errorf("unexpected request: Method=%s, Path=%s", lastMethod, lastPath)
		}
	})

	t.Run("DeleteHeader", func(t *testing.T) {
		err := store.DeleteHeader(ctx, "header-id")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if lastMethod != "DELETE" || lastPath != "/api/headers?id=header-id" {
			t.Errorf("unexpected request: Method=%s, Path=%s", lastMethod, lastPath)
		}
	})

	t.Run("GetAllModels", func(t *testing.T) {
		models, err := store.GetAllModels(ctx)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(models) != 1 || models[0].ID != "gemini-2.5-flash" {
			t.Errorf("unexpected models response")
		}
	})

	t.Run("SaveModel", func(t *testing.T) {
		model := ModelConfig{ID: "new-model"}
		err := store.SaveModel(ctx, model)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if lastMethod != "POST" || lastPath != "/api/models" {
			t.Errorf("unexpected request: Method=%s, Path=%s", lastMethod, lastPath)
		}
	})

	t.Run("DeleteModel", func(t *testing.T) {
		err := store.DeleteModel(ctx, "model-id")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if lastMethod != "DELETE" || lastPath != "/api/models?id=model-id" {
			t.Errorf("unexpected request: Method=%s, Path=%s", lastMethod, lastPath)
		}
	})

	t.Run("SaveApp", func(t *testing.T) {
		app := App{ID: "new-app"}
		err := store.SaveApp(ctx, app)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if lastMethod != "POST" || lastPath != "/api/apps" {
			t.Errorf("unexpected request: Method=%s, Path=%s", lastMethod, lastPath)
		}
	})

	t.Run("DeleteApp", func(t *testing.T) {
		err := store.DeleteApp(ctx, "app-id")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if lastMethod != "DELETE" || lastPath != "/api/apps?id=app-id" {
			t.Errorf("unexpected request: Method=%s, Path=%s", lastMethod, lastPath)
		}
	})

	t.Run("SaveClient", func(t *testing.T) {
		client := Client{ID: "new-client"}
		err := store.SaveClient(ctx, client)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if lastMethod != "POST" || lastPath != "/api/clients" {
			t.Errorf("unexpected request: Method=%s, Path=%s", lastMethod, lastPath)
		}
	})

	t.Run("DeleteClient", func(t *testing.T) {
		err := store.DeleteClient(ctx, "client-id")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if lastMethod != "DELETE" || lastPath != "/api/clients?id=client-id" {
			t.Errorf("unexpected request: Method=%s, Path=%s", lastMethod, lastPath)
		}
	})

	t.Run("GetQueueStatus", func(t *testing.T) {
		status, err := store.GetQueueStatus(ctx)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(status) != 0 {
			t.Errorf("unexpected queue status response")
		}
	})
}

// Helper function to read bodies in test scope
func ioReadAll(r io.Reader) ([]byte, error) {
	var buf bytes.Buffer
	_, err := buf.ReadFrom(r)
	return buf.Bytes(), err
}
