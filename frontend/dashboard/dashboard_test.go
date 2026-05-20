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

	// --- TEST NEW EDIT FLOWS ---
	t.Run("UI Edit and Update Flows", func(t *testing.T) {
		// Seed premium client first for application bindings
		_ = dbStore.SaveClient(ctx, config.Client{ID: "client-ui-1", Tier: "premium"})

		// 1. Edit App Flow
		appForm := url.Values{}
		appForm.Add("app_id", "app-ui-dynamic")
		appForm.Add("app_name", "Updated Application Title")
		appForm.Add("client_id", "client-ui-1")
		appForm.Add("rpm", "500")
		appForm.Add("tpm", "300000")
		appForm.Add("priority", "low")

		reqApp := httptest.NewRequest("POST", "/admin/apps/create", strings.NewReader(appForm.Encode()))
		reqApp.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rrApp := httptest.NewRecorder()
		dash.CreateApp(rrApp, reqApp)

		if rrApp.Code != http.StatusSeeOther {
			t.Errorf("expected redirect 303 for App edit, got %d", rrApp.Code)
		}

		updatedApp, ok := dbStore.LookupApp("app-ui-dynamic")
		if !ok {
			t.Fatalf("App should exist")
		}
		if updatedApp.Name != "Updated Application Title" || updatedApp.RPM != 500 || updatedApp.Priority != "low" {
			t.Errorf("App properties not updated: %+v", updatedApp)
		}

		// 2. Edit Rule Flow
		ruleForm := url.Values{}
		ruleForm.Add("model_pattern", "gemini-2.5-pro")
		ruleForm.Add("app_id", "app-ui-dynamic")
		ruleForm.Add("client_tier", "premium")
		ruleForm.Add("target_model", "gemini-2.5-pro")
		ruleForm.Add("target_location", "us-east1")
		ruleForm.Add("priority_weight", "4")

		reqRule := httptest.NewRequest("POST", "/admin/rules/create", strings.NewReader(ruleForm.Encode()))
		reqRule.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rrRule := httptest.NewRecorder()
		dash.CreateRule(rrRule, reqRule)

		if rrRule.Code != http.StatusSeeOther {
			t.Errorf("expected redirect 303 for Rule creation, got %d", rrRule.Code)
		}

		rules, _ := dbStore.GetAllRules(ctx)
		var targetRule *config.RoutingRule
		for _, r := range rules {
			if r.ModelPattern == "gemini-2.5-pro" && r.TargetLocation == "us-east1" {
				targetRule = &r
				break
			}
		}
		if targetRule == nil {
			t.Fatalf("expected to find created rule")
		}
		ruleID := targetRule.ID

		// Now update the rule
		ruleFormEdit := url.Values{}
		ruleFormEdit.Add("id", ruleID)
		ruleFormEdit.Add("model_pattern", "gemini-2.5-pro")
		ruleFormEdit.Add("app_id", "app-ui-dynamic")
		ruleFormEdit.Add("client_tier", "premium")
		ruleFormEdit.Add("target_model", "gemini-2.5-pro")
		ruleFormEdit.Add("target_location", "europe-west3") // edited
		ruleFormEdit.Add("priority_weight", "8") // edited

		reqRuleEdit := httptest.NewRequest("POST", "/admin/rules/create", strings.NewReader(ruleFormEdit.Encode()))
		reqRuleEdit.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rrRuleEdit := httptest.NewRecorder()
		dash.CreateRule(rrRuleEdit, reqRuleEdit)

		rulesUpdated, _ := dbStore.GetAllRules(ctx)
		var updatedRule *config.RoutingRule
		for _, r := range rulesUpdated {
			if r.ID == ruleID {
				updatedRule = &r
				break
			}
		}
		if updatedRule == nil {
			t.Fatalf("should still find rule with ID: %s", ruleID)
		}
		if updatedRule.TargetLocation != "europe-west3" || updatedRule.PriorityWeight != 8 {
			t.Errorf("rule did not update correctly: %+v", updatedRule)
		}

		// 3. Edit Header Flow
		headerForm := url.Values{}
		headerForm.Add("name", "X-Custom-Header")
		headerForm.Add("app_id", "app-ui-dynamic")
		headerForm.Add("description", "Original description")
		headerForm.Add("required", "true")
		headerForm.Add("validation", "non-empty")

		reqHeader := httptest.NewRequest("POST", "/admin/headers/create", strings.NewReader(headerForm.Encode()))
		reqHeader.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rrHeader := httptest.NewRecorder()
		dash.CreateHeader(rrHeader, reqHeader)

		headers, _ := dbStore.GetAllHeaders(ctx)
		var targetHeader *config.CustomHeader
		for _, h := range headers {
			if h.Name == "X-Custom-Header" && h.Description == "Original description" {
				targetHeader = &h
				break
			}
		}
		if targetHeader == nil {
			t.Fatalf("expected to find created header")
		}
		headerID := targetHeader.ID

		// Update header
		headerFormEdit := url.Values{}
		headerFormEdit.Add("id", headerID)
		headerFormEdit.Add("name", "X-Custom-Header")
		headerFormEdit.Add("app_id", "app-ui-dynamic")
		headerFormEdit.Add("description", "Updated description") // edited
		headerFormEdit.Add("required", "false") // edited
		headerFormEdit.Add("validation", "regex") // edited
		headerFormEdit.Add("value_pattern", "^val-") // edited

		reqHeaderEdit := httptest.NewRequest("POST", "/admin/headers/create", strings.NewReader(headerFormEdit.Encode()))
		reqHeaderEdit.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rrHeaderEdit := httptest.NewRecorder()
		dash.CreateHeader(rrHeaderEdit, reqHeaderEdit)

		headersUpdated, _ := dbStore.GetAllHeaders(ctx)
		var updatedHeader *config.CustomHeader
		for _, h := range headersUpdated {
			if h.ID == headerID {
				updatedHeader = &h
				break
			}
		}
		if updatedHeader == nil {
			t.Fatalf("should still find header with ID: %s", headerID)
		}
		if updatedHeader.Description != "Updated description" || updatedHeader.Required || updatedHeader.Validation != "regex" {
			t.Errorf("header did not update correctly: %+v", updatedHeader)
		}

		// 4. Edit Key Details Flow
		keys, _ := dbStore.GetAllKeys(ctx)
		if len(keys) == 0 {
			t.Fatalf("expected at least 1 key")
		}
		keyHash := keys[0].KeyHash

		// Register another app to bind the key to
		appForm2 := url.Values{}
		appForm2.Add("app_id", "app-ui-dynamic-2")
		appForm2.Add("app_name", "Second Application")
		appForm2.Add("client_id", "client-ui-1")
		appForm2.Add("rpm", "100")
		appForm2.Add("tpm", "100000")
		appForm2.Add("priority", "low")

		reqApp2 := httptest.NewRequest("POST", "/admin/apps/create", strings.NewReader(appForm2.Encode()))
		reqApp2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rrApp2 := httptest.NewRecorder()
		dash.CreateApp(rrApp2, reqApp2)

		// Now save key details
		keySaveForm := url.Values{}
		keySaveForm.Add("hash", keyHash)
		keySaveForm.Add("app_id", "app-ui-dynamic-2")
		keySaveForm.Add("status", "revoked")

		reqKeySave := httptest.NewRequest("POST", "/admin/keys/save", strings.NewReader(keySaveForm.Encode()))
		reqKeySave.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rrKeySave := httptest.NewRecorder()
		dash.SaveKeyDetails(rrKeySave, reqKeySave)

		if rrKeySave.Code != http.StatusOK {
			t.Errorf("expected status 200 for Key Save, got %d", rrKeySave.Code)
		}

		keysUpdated, _ := dbStore.GetAllKeys(ctx)
		foundKey := false
		for _, k := range keysUpdated {
			if k.KeyHash == keyHash {
				foundKey = true
				if k.AppID != "app-ui-dynamic-2" || k.Status != "revoked" {
					t.Errorf("key properties did not update: %+v", k)
				}
			}
		}
		if !foundKey {
			t.Errorf("edited key not found in database")
		}
	})

	os.RemoveAll("data/local_db.json")
}
