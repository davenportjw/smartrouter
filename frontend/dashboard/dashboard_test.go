package dashboard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"geminirouter/backend/api"
	store "geminirouter/backend/config"
	"geminirouter/pkg/config"
)

func TestDashboardUIAndRESTBackendIntegration(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")

	ctx := context.Background()
	dbStore, err := store.NewConfigStore(ctx, "test-project")
	if err != nil {
		t.Fatalf("Failed to initialize backend store: %v", err)
	}

	// Reset database from previous states
	os.Remove("data/local_db.json")
	dbStore, _ = store.NewConfigStore(ctx, "test-project")

	sharedSecret := "dashboard-secure-rest-token"
	apiController := api.NewAPIController(dbStore, sharedSecret)

	muxAPI := http.NewServeMux()
	apiController.RegisterRoutes(muxAPI)

	// 1. Start real Backend API Service in background
	backendServer := httptest.NewServer(muxAPI)
	defer backendServer.Close()

	// 2. Initialize REST Client Config Store
	apiConfigStore := config.NewAPIConfigStore(backendServer.URL, sharedSecret)

	// 3. Initialize Dashboard UI Controller backed by the REST client
	dash := NewDashboardController(apiConfigStore, "test-project", "us-central1")

	// --- TEST UI APPS RENDERING ---
	t.Run("UI Render Apps Page", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/admin/apps", nil)
		rr := httptest.NewRecorder()

		dash.ServeApps(rr, req)

		if rr.Code != http.StatusOK {
			t.Errorf("expected UI to render status 200, got %d", rr.Code)
		}

		body := rr.Body.String()
		if !strings.Contains(body, "<html") && !strings.Contains(body, "<body") {
			t.Errorf("expected body to render HTML layout, but got: %s", body)
		}

		if !strings.Contains(body, "Applications") {
			t.Errorf("expected body to contain 'Applications' title header")
		}
	})

	// --- TEST UI APP CREATION FLOW ---
	t.Run("UI Create App Form Submission Flow", func(t *testing.T) {
		// Perform UI Form Submission
		form := url.Values{}
		form.Add("app_id", "app-ui-dynamic")
		form.Add("app_name", "Form-Created Application")
		form.Add("client_id", "client-ui-1")
		form.Add("rpm", "300")
		form.Add("tpm", "200000")
		form.Add("priority", "high")

		req := httptest.NewRequest("POST", "/admin/apps/create", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rr := httptest.NewRecorder()

		dash.CreateApp(rr, req)

		// Verify UI redirects to full apps dashboard list upon creation
		if rr.Code != http.StatusSeeOther {
			t.Errorf("expected redirect 303, got %d", rr.Code)
		}

		// Verify that the App was successfully committed to the Backend Database via the REST API!
		app, ok := dbStore.LookupApp("app-ui-dynamic")
		if !ok {
			t.Fatalf("App was not committed to the database via the UI decoupled flow")
		}
		if app.Name != "Form-Created Application" || app.RPM != 300 || app.Priority != "high" {
			t.Errorf("app contents do not match form values: %+v", app)
		}
	})

	// --- TEST UI KEYS RENDERING AND GENERATION BANNER ---
	t.Run("UI Render Keys Page and Dynamic Key Generation", func(t *testing.T) {
		// Seed clients/apps so dropdown loads
		_ = dbStore.SaveClient(ctx, config.Client{ID: "client-ui-1", Tier: "premium"})

		req := httptest.NewRequest("GET", "/admin/keys", nil)
		rr := httptest.NewRecorder()

		dash.ServeKeys(rr, req)

		if rr.Code != http.StatusOK {
			t.Errorf("expected status 200, got %d", rr.Code)
		}

		// Perform UI key creation
		form := url.Values{}
		form.Add("app_id", "app-ui-dynamic")

		reqCreate := httptest.NewRequest("POST", "/admin/keys/create", strings.NewReader(form.Encode()))
		reqCreate.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rrCreate := httptest.NewRecorder()

		dash.CreateKey(rrCreate, reqCreate)

		if rrCreate.Code != http.StatusOK {
			t.Errorf("expected HTMX raw key banner to return status 200, got %d", rrCreate.Code)
		}

		body := rrCreate.Body.String()
		if !strings.Contains(body, "gr_key_") {
			t.Errorf("expected body to contain generated raw key value banner, got: %s", body)
		}

		if !strings.Contains(body, "Form-Created Application") {
			t.Errorf("expected banner to display target application name")
		}
	})

	os.RemoveAll("data/local_db.json")
}
