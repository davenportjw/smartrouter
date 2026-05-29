package dashboard

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"geminirouter/backend/api"
	store "geminirouter/backend/config"
	"geminirouter/backend/proxy"
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
	apiController := api.NewAPIController(dbStore, proxy.NewRequestScheduler(1000, 100), nil, sharedSecret)

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
		reqKeySave.Header.Set("HX-Request", "true")
		rrKeySave := httptest.NewRecorder()
		dash.SaveKeyDetails(rrKeySave, reqKeySave)

		if rrKeySave.Code != http.StatusOK {
			t.Errorf("expected status 200 for Key Save, got %d", rrKeySave.Code)
		}
		if rrKeySave.Header().Get("HX-Redirect") != "/admin/keys" {
			t.Errorf("expected HX-Redirect header to be /admin/keys, got %s", rrKeySave.Header().Get("HX-Redirect"))
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

	// --- TEST COMPREHENSIVE CRUD AND HTMX REDIRECT FLOWS ---
	t.Run("UI Comprehensive CRUD and Modals Flow", func(t *testing.T) {
		// Seed dynamic location models in the store first
		_ = dbStore.SaveModel(ctx, config.ModelConfig{ID: "gemini-2.5-flash", DisplayName: "Gemini 2.5 Flash", Active: true, Location: "us-central1", Type: "foundation"})
		_ = dbStore.SaveModel(ctx, config.ModelConfig{ID: "gemini-2.5-pro", DisplayName: "Gemini 2.5 Pro", Active: true, Location: "us-central1", Type: "foundation"})
		_ = dbStore.SaveModel(ctx, config.ModelConfig{ID: "gemini-3.1-flash-lite", DisplayName: "Gemini 3.1 Flash Lite", Active: true, Location: "us-central1", Type: "foundation"})

		// 1. CLIENT ORGANIZATIONS CRUD
		t.Run("Client Organizations CRUD", func(t *testing.T) {
			// A. Serve New Client Modal
			reqNew := httptest.NewRequest("GET", "/admin/clients/new", nil)
			rrNew := httptest.NewRecorder()
			dash.ServeClientsNewModal(rrNew, reqNew)
			if rrNew.Code != http.StatusOK {
				t.Errorf("expected client new modal to return 200, got %d", rrNew.Code)
			}

			// B. Create Client - Standard Redirect Flow
			form := url.Values{}
			form.Add("client_id", "client-crud-test")
			form.Add("client_name", "CRUD Test Organization")
			form.Add("tier", "standard")
			form.Add("priority", "medium")
			form.Add("rpm", "150")
			form.Add("tpm", "80000")

			reqCreate := httptest.NewRequest("POST", "/admin/clients/create", strings.NewReader(form.Encode()))
			reqCreate.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			rrCreate := httptest.NewRecorder()
			dash.CreateClient(rrCreate, reqCreate)
			if rrCreate.Code != http.StatusSeeOther {
				t.Errorf("expected standard client create to return 303 redirect, got %d", rrCreate.Code)
			}

			// C. Create/Edit Client - HTMX HX-Redirect Flow
			formEdit := url.Values{}
			formEdit.Add("client_id", "client-crud-test")
			formEdit.Add("client_name", "CRUD Test Organization Edited")
			formEdit.Add("tier", "premium")
			formEdit.Add("priority", "high")
			formEdit.Add("rpm", "250")
			formEdit.Add("tpm", "120000")

			reqEdit := httptest.NewRequest("POST", "/admin/clients/create", strings.NewReader(formEdit.Encode()))
			reqEdit.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			reqEdit.Header.Set("HX-Request", "true")
			rrEdit := httptest.NewRecorder()
			dash.CreateClient(rrEdit, reqEdit)

			if rrEdit.Code != http.StatusOK {
				t.Errorf("expected HTMX client edit to return 200, got %d", rrEdit.Code)
			}
			if rrEdit.Header().Get("HX-Redirect") != "/admin/clients" {
				t.Errorf("expected HX-Redirect to /admin/clients, got %s", rrEdit.Header().Get("HX-Redirect"))
			}

			// Verify updated db state
			client, ok := dbStore.LookupClient("client-crud-test")
			if !ok {
				t.Fatalf("client should exist in database")
			}
			if client.Name != "CRUD Test Organization Edited" || client.Tier != "premium" || client.RPM != 250 {
				t.Errorf("client fields not saved properly: %+v", client)
			}

			// D. Serve Edit Client Modal
			reqEditModal := httptest.NewRequest("GET", "/admin/clients/edit?id=client-crud-test", nil)
			rrEditModal := httptest.NewRecorder()
			dash.ServeClientsEditModal(rrEditModal, reqEditModal)
			if rrEditModal.Code != http.StatusOK {
				t.Errorf("expected client edit modal to return 200, got %d", rrEditModal.Code)
			}

			// E. Delete Client
			reqDelete := httptest.NewRequest("DELETE", "/admin/clients/delete?id=client-crud-test", nil)
			rrDelete := httptest.NewRecorder()
			dash.DeleteClient(rrDelete, reqDelete)
			if rrDelete.Code != http.StatusOK {
				t.Errorf("expected delete client to return 200, got %d", rrDelete.Code)
			}

			_, ok = dbStore.LookupClient("client-crud-test")
			if ok {
				t.Errorf("client should have been deleted from database")
			}
		})

		// Seed a client first for App tests
		_ = dbStore.SaveClient(ctx, config.Client{ID: "client-for-apps", Name: "Apps Parent Client", Tier: "standard"})

		// 2. APPLICATIONS CRUD
		t.Run("Applications CRUD", func(t *testing.T) {
			// A. Serve New App Modal
			reqNew := httptest.NewRequest("GET", "/admin/apps/new", nil)
			rrNew := httptest.NewRecorder()
			dash.ServeAppsNewModal(rrNew, reqNew)
			if rrNew.Code != http.StatusOK {
				t.Errorf("expected apps new modal to return 200, got %d", rrNew.Code)
			}

			// B. Create App - Standard Redirect Flow
			form := url.Values{}
			form.Add("app_id", "app-crud-test")
			form.Add("app_name", "CRUD Test App")
			form.Add("client_id", "client-for-apps")
			form.Add("rpm", "80")
			form.Add("tpm", "35000")
			form.Add("priority", "low")

			reqCreate := httptest.NewRequest("POST", "/admin/apps/create", strings.NewReader(form.Encode()))
			reqCreate.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			rrCreate := httptest.NewRecorder()
			dash.CreateApp(rrCreate, reqCreate)
			if rrCreate.Code != http.StatusSeeOther {
				t.Errorf("expected standard app create to return 303 redirect, got %d", rrCreate.Code)
			}

			// C. Create/Edit App - HTMX HX-Redirect Flow
			formEdit := url.Values{}
			formEdit.Add("app_id", "app-crud-test")
			formEdit.Add("app_name", "CRUD Test App Edited")
			formEdit.Add("client_id", "client-for-apps")
			formEdit.Add("rpm", "120")
			formEdit.Add("tpm", "55000")
			formEdit.Add("priority", "high")

			reqEdit := httptest.NewRequest("POST", "/admin/apps/create", strings.NewReader(formEdit.Encode()))
			reqEdit.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			reqEdit.Header.Set("HX-Request", "true")
			rrEdit := httptest.NewRecorder()
			dash.CreateApp(rrEdit, reqEdit)

			if rrEdit.Code != http.StatusOK {
				t.Errorf("expected HTMX app edit to return 200, got %d", rrEdit.Code)
			}
			if rrEdit.Header().Get("HX-Redirect") != "/admin/apps" {
				t.Errorf("expected HX-Redirect to /admin/apps, got %s", rrEdit.Header().Get("HX-Redirect"))
			}

			// Verify updated db state
			app, ok := dbStore.LookupApp("app-crud-test")
			if !ok {
				t.Fatalf("app should exist in database")
			}
			if app.Name != "CRUD Test App Edited" || app.RPM != 120 || app.Priority != "high" {
				t.Errorf("app fields not saved properly: %+v", app)
			}

			// D. Serve Edit App Modal
			reqEditModal := httptest.NewRequest("GET", "/admin/apps/edit?id=app-crud-test", nil)
			rrEditModal := httptest.NewRecorder()
			dash.ServeAppsEditModal(rrEditModal, reqEditModal)
			if rrEditModal.Code != http.StatusOK {
				t.Errorf("expected app edit modal to return 200, got %d", rrEditModal.Code)
			}

			// E. Delete App
			reqDelete := httptest.NewRequest("DELETE", "/admin/apps/delete?id=app-crud-test", nil)
			rrDelete := httptest.NewRecorder()
			dash.DeleteApp(rrDelete, reqDelete)
			if rrDelete.Code != http.StatusOK {
				t.Errorf("expected delete app to return 200, got %d", rrDelete.Code)
			}

			_, ok = dbStore.LookupApp("app-crud-test")
			if ok {
				t.Errorf("app should have been deleted from database")
			}
		})

		// Seed a client and app for subsequent tests
		_ = dbStore.SaveClient(ctx, config.Client{ID: "client-seed", Name: "Seeded Client", Tier: "premium"})
		_ = dbStore.SaveApp(ctx, config.App{ID: "app-seed", ClientID: "client-seed", Name: "Seeded App", RPM: 100, TPM: 50000, Priority: "high"})

		// 3. DYNAMIC ROUTING RULES CRUD
		t.Run("Dynamic Routing Rules CRUD", func(t *testing.T) {
			// A. Serve New Rule Modal
			reqNew := httptest.NewRequest("GET", "/admin/rules/new", nil)
			rrNew := httptest.NewRecorder()
			dash.ServeRulesNewModal(rrNew, reqNew)
			if rrNew.Code != http.StatusOK {
				t.Errorf("expected rules new modal to return 200, got %d", rrNew.Code)
			}

			// B. Create Rule - Standard Redirect Flow
			form := url.Values{}
			form.Add("model_pattern", "gemini-*")
			form.Add("app_id", "app-seed")
			form.Add("client_tier", "all")
			form.Add("target_model", "gemini-2.5-flash")
			form.Add("target_location", "us-central1")
			form.Add("priority_weight", "2")

			reqCreate := httptest.NewRequest("POST", "/admin/rules/create", strings.NewReader(form.Encode()))
			reqCreate.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			rrCreate := httptest.NewRecorder()
			dash.CreateRule(rrCreate, reqCreate)
			if rrCreate.Code != http.StatusSeeOther {
				t.Errorf("expected standard rule create to return 303 redirect, got %d", rrCreate.Code)
			}

			// B2. Create Rule - HTMX Validation Error Swap Flow
			formErr := url.Values{}
			formErr.Add("model_pattern", "") // Invalid empty pattern!
			formErr.Add("app_id", "app-seed")
			formErr.Add("target_model", "gemini-2.5-pro")
			formErr.Add("target_location", "us-central1")

			reqErr := httptest.NewRequest("POST", "/admin/rules/create", strings.NewReader(formErr.Encode()))
			reqErr.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			reqErr.Header.Set("HX-Request", "true")
			rrErr := httptest.NewRecorder()
			dash.CreateRule(rrErr, reqErr)

			if rrErr.Code != http.StatusOK {
				t.Errorf("expected HTMX validation error to return 200 status for swap, got %d", rrErr.Code)
			}
			if rrErr.Header().Get("HX-Retarget") != "#error-alert-container" {
				t.Errorf("expected HX-Retarget to be '#error-alert-container', got %q", rrErr.Header().Get("HX-Retarget"))
			}
			if rrErr.Header().Get("HX-Reswap") != "innerHTML" {
				t.Errorf("expected HX-Reswap to be 'innerHTML', got %q", rrErr.Header().Get("HX-Reswap"))
			}
			bodyErr := rrErr.Body.String()
			if !strings.Contains(bodyErr, "Operation Failed") || !strings.Contains(bodyErr, "Requested Model Pattern cannot be empty") {
				t.Errorf("expected error message in body, got: %s", bodyErr)
			}

			rules, _ := dbStore.GetAllRules(ctx)
			var ruleID string
			for _, r := range rules {
				if r.ModelPattern == "gemini-*" && r.AppID == "app-seed" {
					ruleID = r.ID
					break
				}
			}
			if ruleID == "" {
				t.Fatalf("failed to retrieve rule ID for newly created rule")
			}

			// C. Create/Edit Rule - HTMX HX-Redirect Flow
			formEdit := url.Values{}
			formEdit.Add("id", ruleID)
			formEdit.Add("model_pattern", "gemini-*")
			formEdit.Add("app_id", "app-seed")
			formEdit.Add("client_tier", "premium")
			formEdit.Add("target_model", "gemini-2.5-pro")
			formEdit.Add("target_location", "us-central1")
			formEdit.Add("priority_weight", "7")

			reqEdit := httptest.NewRequest("POST", "/admin/rules/create", strings.NewReader(formEdit.Encode()))
			reqEdit.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			reqEdit.Header.Set("HX-Request", "true")
			rrEdit := httptest.NewRecorder()
			dash.CreateRule(rrEdit, reqEdit)

			if rrEdit.Code != http.StatusOK {
				t.Errorf("expected HTMX rule edit to return 200, got %d", rrEdit.Code)
			}
			if rrEdit.Header().Get("HX-Redirect") != "/admin/rules" {
				t.Errorf("expected HX-Redirect to /admin/rules, got %s", rrEdit.Header().Get("HX-Redirect"))
			}

			// Verify updated db state
			rulesUpdated, _ := dbStore.GetAllRules(ctx)
			var updatedRule *config.RoutingRule
			for _, r := range rulesUpdated {
				if r.ID == ruleID {
					updatedRule = &r
					break
				}
			}
			if updatedRule == nil {
				t.Fatalf("rule should exist in database")
			}
			if updatedRule.ClientTier != "premium" || updatedRule.TargetModel != "gemini-2.5-pro" || updatedRule.PriorityWeight != 7 {
				t.Errorf("rule fields not saved properly: %+v", updatedRule)
			}

			// D. Serve Edit Rule Modal
			reqEditModal := httptest.NewRequest("GET", "/admin/rules/edit?id="+ruleID, nil)
			rrEditModal := httptest.NewRecorder()
			dash.ServeRulesEditModal(rrEditModal, reqEditModal)
			if rrEditModal.Code != http.StatusOK {
				t.Errorf("expected rule edit modal to return 200, got %d", rrEditModal.Code)
			}

			// E. Delete Rule
			reqDelete := httptest.NewRequest("DELETE", "/admin/rules/delete?id="+ruleID, nil)
			rrDelete := httptest.NewRecorder()
			dash.DeleteRule(rrDelete, reqDelete)
			if rrDelete.Code != http.StatusOK {
				t.Errorf("expected delete rule to return 200, got %d", rrDelete.Code)
			}

			rulesPostDelete, _ := dbStore.GetAllRules(ctx)
			found := false
			for _, r := range rulesPostDelete {
				if r.ID == ruleID {
					found = true
				}
			}
			if found {
				t.Errorf("rule should have been deleted from database")
			}
		})

		// 4. CUSTOM HEADERS CRUD
		t.Run("Custom Headers CRUD", func(t *testing.T) {
			// A. Serve New Header Modal
			reqNew := httptest.NewRequest("GET", "/admin/headers/new", nil)
			rrNew := httptest.NewRecorder()
			dash.ServeHeadersNewModal(rrNew, reqNew)
			if rrNew.Code != http.StatusOK {
				t.Errorf("expected headers new modal to return 200, got %d", rrNew.Code)
			}

			// B. Create Header - Standard Redirect Flow
			form := url.Values{}
			form.Add("name", "X-Header-Test")
			form.Add("app_id", "app-seed")
			form.Add("description", "Test Custom Header Description")
			form.Add("required", "true")
			form.Add("validation", "non-empty")

			reqCreate := httptest.NewRequest("POST", "/admin/headers/create", strings.NewReader(form.Encode()))
			reqCreate.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			rrCreate := httptest.NewRecorder()
			dash.CreateHeader(rrCreate, reqCreate)
			if rrCreate.Code != http.StatusSeeOther {
				t.Errorf("expected standard header create to return 303 redirect, got %d", rrCreate.Code)
			}

			headers, _ := dbStore.GetAllHeaders(ctx)
			var headerID string
			for _, h := range headers {
				if h.Name == "X-Header-Test" && h.AppID == "app-seed" {
					headerID = h.ID
					break
				}
			}
			if headerID == "" {
				t.Fatalf("failed to retrieve header ID for newly created header")
			}

			// C. Create/Edit Header - HTMX HX-Redirect Flow
			formEdit := url.Values{}
			formEdit.Add("id", headerID)
			formEdit.Add("name", "X-Header-Test")
			formEdit.Add("app_id", "app-seed")
			formEdit.Add("description", "Test Custom Header Description Edited")
			formEdit.Add("required", "false")
			formEdit.Add("validation", "regex")
			formEdit.Add("value_pattern", "^[A-Za-z]+$")

			reqEdit := httptest.NewRequest("POST", "/admin/headers/create", strings.NewReader(formEdit.Encode()))
			reqEdit.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			reqEdit.Header.Set("HX-Request", "true")
			rrEdit := httptest.NewRecorder()
			dash.CreateHeader(rrEdit, reqEdit)

			if rrEdit.Code != http.StatusOK {
				t.Errorf("expected HTMX header edit to return 200, got %d", rrEdit.Code)
			}
			if rrEdit.Header().Get("HX-Redirect") != "/admin/headers" {
				t.Errorf("expected HX-Redirect to /admin/headers, got %s", rrEdit.Header().Get("HX-Redirect"))
			}

			// Verify updated db state
			headersUpdated, _ := dbStore.GetAllHeaders(ctx)
			var updatedHeader *config.CustomHeader
			for _, h := range headersUpdated {
				if h.ID == headerID {
					updatedHeader = &h
					break
				}
			}
			if updatedHeader == nil {
				t.Fatalf("header should exist in database")
			}
			if updatedHeader.Description != "Test Custom Header Description Edited" || updatedHeader.Required || updatedHeader.Validation != "regex" {
				t.Errorf("header fields not saved properly: %+v", updatedHeader)
			}

			// D. Serve Edit Header Modal
			reqEditModal := httptest.NewRequest("GET", "/admin/headers/edit?id="+headerID, nil)
			rrEditModal := httptest.NewRecorder()
			dash.ServeHeadersEditModal(rrEditModal, reqEditModal)
			if rrEditModal.Code != http.StatusOK {
				t.Errorf("expected header edit modal to return 200, got %d", rrEditModal.Code)
			}

			// E. Delete Header
			reqDelete := httptest.NewRequest("DELETE", "/admin/headers/delete?id="+headerID, nil)
			rrDelete := httptest.NewRecorder()
			dash.DeleteHeader(rrDelete, reqDelete)
			if rrDelete.Code != http.StatusOK {
				t.Errorf("expected delete header to return 200, got %d", rrDelete.Code)
			}

			headersPostDelete, _ := dbStore.GetAllHeaders(ctx)
			found := false
			for _, h := range headersPostDelete {
				if h.ID == headerID {
					found = true
				}
			}
			if found {
				t.Errorf("header should have been deleted from database")
			}
		})

		// 5. API KEYS CRUD
		t.Run("API Keys CRUD", func(t *testing.T) {
			// A. Serve New Key Modal
			reqNew := httptest.NewRequest("GET", "/admin/keys/new", nil)
			rrNew := httptest.NewRecorder()
			dash.ServeKeysNewModal(rrNew, reqNew)
			if rrNew.Code != http.StatusOK {
				t.Errorf("expected keys new modal to return 200, got %d", rrNew.Code)
			}

			// B. Generate a New Key
			formCreate := url.Values{}
			formCreate.Add("app_id", "app-seed")

			reqCreate := httptest.NewRequest("POST", "/admin/keys/create", strings.NewReader(formCreate.Encode()))
			reqCreate.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			rrCreate := httptest.NewRecorder()
			dash.CreateKey(rrCreate, reqCreate)
			if rrCreate.Code != http.StatusOK {
				t.Errorf("expected key creation to return 200, got %d", rrCreate.Code)
			}

			keys, _ := dbStore.GetAllKeys(ctx)
			var keyHash string
			for _, k := range keys {
				if k.AppID == "app-seed" && k.Status == "active" {
					keyHash = k.KeyHash
					break
				}
			}
			if keyHash == "" {
				t.Fatalf("failed to locate newly generated key in database")
			}

			// C. Serve Edit Key Modal
			reqEditModal := httptest.NewRequest("GET", "/admin/keys/edit?hash="+keyHash, nil)
			rrEditModal := httptest.NewRecorder()
			dash.ServeKeysEditModal(rrEditModal, reqEditModal)
			if rrEditModal.Code != http.StatusOK {
				t.Errorf("expected key edit modal to return 200, got %d", rrEditModal.Code)
			}

			// D. Save Key Details - Standard Redirect Flow
			formSave := url.Values{}
			formSave.Add("hash", keyHash)
			formSave.Add("app_id", "app-seed")
			formSave.Add("status", "revoked")

			reqSave := httptest.NewRequest("POST", "/admin/keys/save", strings.NewReader(formSave.Encode()))
			reqSave.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			rrSave := httptest.NewRecorder()
			dash.SaveKeyDetails(rrSave, reqSave)
			if rrSave.Code != http.StatusSeeOther {
				t.Errorf("expected standard key details save to return 303 redirect, got %d", rrSave.Code)
			}

			// E. Save Key Details - HTMX HX-Redirect Flow
			formSaveEdit := url.Values{}
			formSaveEdit.Add("hash", keyHash)
			formSaveEdit.Add("app_id", "app-seed")
			formSaveEdit.Add("status", "active")

			reqSaveEdit := httptest.NewRequest("POST", "/admin/keys/save", strings.NewReader(formSaveEdit.Encode()))
			reqSaveEdit.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			reqSaveEdit.Header.Set("HX-Request", "true")
			rrSaveEdit := httptest.NewRecorder()
			dash.SaveKeyDetails(rrSaveEdit, reqSaveEdit)

			if rrSaveEdit.Code != http.StatusOK {
				t.Errorf("expected HTMX key details save to return 200, got %d", rrSaveEdit.Code)
			}
			if rrSaveEdit.Header().Get("HX-Redirect") != "/admin/keys" {
				t.Errorf("expected HX-Redirect to /admin/keys, got %s", rrSaveEdit.Header().Get("HX-Redirect"))
			}

			// Verify key state
			keysCheck, _ := dbStore.GetAllKeys(ctx)
			var checkKey *config.APIKey
			for _, k := range keysCheck {
				if k.KeyHash == keyHash {
					checkKey = &k
					break
				}
			}
			if checkKey == nil {
				t.Fatalf("key should exist")
			}
			if checkKey.Status != "active" {
				t.Errorf("key status should be active")
			}

			// F. Revoke Key (HTMX Delete Helper)
			reqRevoke := httptest.NewRequest("POST", "/admin/keys/revoke?hash="+keyHash, nil)
			rrRevoke := httptest.NewRecorder()
			dash.RevokeKey(rrRevoke, reqRevoke)
			if rrRevoke.Code != http.StatusOK {
				t.Errorf("expected revoke key to return 200, got %d", rrRevoke.Code)
			}

			keysPostRevoke, _ := dbStore.GetAllKeys(ctx)
			var revokedKey *config.APIKey
			for _, k := range keysPostRevoke {
				if k.KeyHash == keyHash {
					revokedKey = &k
					break
				}
			}
			if revokedKey == nil || revokedKey.Status != "revoked" {
				t.Errorf("key should have been marked as revoked, got: %+v", revokedKey)
			}
		})

		// 6. DYNAMIC COMPLEXITY ROUTING SETTINGS
		t.Run("Dynamic Complexity Routing CRUD", func(t *testing.T) {
			// A. Serve Complexity Settings Edit Modal
			reqEditModal := httptest.NewRequest("GET", "/admin/complexity/edit?app_id=app-seed", nil)
			rrEditModal := httptest.NewRecorder()
			dash.ServeComplexityEditModal(rrEditModal, reqEditModal)
			if rrEditModal.Code != http.StatusOK {
				t.Errorf("expected complexity settings edit modal to return 200, got %d", rrEditModal.Code)
			}

			// B. Save Complexity Settings - Standard Redirect Flow
			form := url.Values{}
			form.Add("app_id", "app-seed")
			form.Add("enabled", "true")
			form.Add("always_override", "true")
			form.Add("simple_model", "gemini-3.1-flash-lite")
			form.Add("medium_model", "gemini-2.5-flash")
			form.Add("complex_model", "gemini-2.5-pro")
			form.Add("simple_char_limit", "100")
			form.Add("medium_char_limit", "500")
			form.Add("force_complex_multimodal", "true")
			form.Add("force_complex_tools", "true")
			form.Add("use_llm_classifier", "false")

			reqSave := httptest.NewRequest("POST", "/admin/complexity/save", strings.NewReader(form.Encode()))
			reqSave.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			rrSave := httptest.NewRecorder()
			dash.SaveComplexitySettings(rrSave, reqSave)
			if rrSave.Code != http.StatusSeeOther {
				t.Errorf("expected standard complexity save to return 303 redirect, got %d", rrSave.Code)
			}

			// C. Save Complexity Settings - HTMX HX-Redirect Flow
			formEdit := url.Values{}
			formEdit.Add("app_id", "app-seed")
			formEdit.Add("enabled", "true")
			formEdit.Add("always_override", "false")
			formEdit.Add("simple_model", "gemini-3.1-flash-lite")
			formEdit.Add("medium_model", "gemini-2.5-flash")
			formEdit.Add("complex_model", "gemini-2.5-pro")
			formEdit.Add("simple_char_limit", "200")
			formEdit.Add("medium_char_limit", "800")
			formEdit.Add("force_complex_multimodal", "false")
			formEdit.Add("force_complex_tools", "false")
			formEdit.Add("use_llm_classifier", "true")
			formEdit.Add("classifier_model", "gemini-3.1-flash-lite")

			reqEdit := httptest.NewRequest("POST", "/admin/complexity/save", strings.NewReader(formEdit.Encode()))
			reqEdit.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			reqEdit.Header.Set("HX-Request", "true")
			rrEdit := httptest.NewRecorder()
			dash.SaveComplexitySettings(rrEdit, reqEdit)

			if rrEdit.Code != http.StatusOK {
				t.Errorf("expected HTMX complexity edit to return 200, got %d", rrEdit.Code)
			}
			if rrEdit.Header().Get("HX-Redirect") != "/admin/complexity" {
				t.Errorf("expected HX-Redirect to /admin/complexity, got %s", rrEdit.Header().Get("HX-Redirect"))
			}

			// Verify updated app complexity state
			app, ok := dbStore.LookupApp("app-seed")
			if !ok {
				t.Fatalf("app-seed should exist in database")
			}
			comp := app.Complexity
			if !comp.Enabled || comp.AlwaysOverride || comp.SimpleCharLimit != 200 || comp.MediumCharLimit != 800 || !comp.UseLLMClassifier {
				t.Errorf("complexity fields not saved properly: %+v", comp)
			}
		})
	})

	os.RemoveAll("data/local_db.json")
}

func TestLocalLogsTelemetryAndDashboards(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")

	// 1. Clean up previous local logs if any
	os.RemoveAll("data/local_logs.jsonl")
	defer os.RemoveAll("data/local_logs.jsonl")

	// Create data folder if not exists
	_ = os.MkdirAll("data", 0755)

	// Write mock log entries to data/local_logs.jsonl using dynamic timestamps within the 24h window
	now := time.Now().UTC()
	t1 := now.Add(-10 * time.Minute).Format(time.RFC3339)
	t2 := now.Add(-5 * time.Minute).Format(time.RFC3339)
	t3 := now.Add(-1 * time.Minute).Format(time.RFC3339)

	logsContent := fmt.Sprintf(`{"severity":"INFO","time":"%s","method":"POST","path":"/v1beta/models/gemini-2.5-flash:generateContent","client_id":"test-client","app_id":"test-app","tier":"premium","model_requested":"gemini-2.5-flash","model_routed":"gemini-2.5-flash","status":200,"latency_ms":120,"bytes_sent":600}
{"severity":"INFO","time":"%s","method":"POST","path":"/v1beta/models/gemini-2.5-pro:generateContent","client_id":"test-client","app_id":"test-app","tier":"premium","model_requested":"gemini-2.5-pro","model_routed":"gemini-2.5-pro","status":200,"latency_ms":550,"bytes_sent":1500}
{"severity":"WARNING","time":"%s","method":"POST","path":"/v1beta/models/gemini-2.5-flash:generateContent","client_id":"other-client","app_id":"other-app","tier":"standard","model_requested":"gemini-2.5-flash","model_routed":"gemini-2.5-flash","status":429,"latency_ms":10,"bytes_sent":0}
`, t1, t2, t3)

	err := os.WriteFile("data/local_logs.jsonl", []byte(logsContent), 0644)
	if err != nil {
		t.Fatalf("failed to write mock local logs: %v", err)
	}

	// Initialize Dashboard Controller backed by a dummy store
	apiConfigStore := config.NewAPIConfigStore("http://localhost:8080", "bypass")
	dash := NewDashboardController(apiConfigStore, "dev-project", "us-central1")

	// 2. Test fetchLocalLogsCosts
	t.Run("Local Logs Costs Integration", func(t *testing.T) {
		costsVM, err := dash.fetchLocalLogsCosts()
		if err != nil {
			t.Fatalf("fetchLocalLogsCosts failed: %v", err)
		}

		// We should have 3 sessions
		if len(costsVM.RecentSessions) != 3 {
			t.Errorf("expected 3 sessions, got %d", len(costsVM.RecentSessions))
		}

		// Verification of cost estimation
		if costsVM.TotalSpend <= 0 {
			t.Errorf("expected positive cumulative spend, got %f", costsVM.TotalSpend)
		}

		// Verify client spend mapping
		foundTestClient := false
		for _, breakdown := range costsVM.ClientBreakdowns {
			if breakdown.ClientID == "test-client" {
				foundTestClient = true
				if breakdown.Cost <= 0 {
					t.Errorf("expected positive cost breakdown for test-client")
				}
			}
		}
		if !foundTestClient {
			t.Errorf("expected to find test-client cost breakdown")
		}

		// Regression test for Donut SVG segment offset calculation
		if costsVM.ModelCostSVG == "" {
			t.Errorf("expected non-empty ModelCostSVG")
		}
		if strings.Contains(costsVM.ModelCostSVG, `stroke-dashoffset="408.41"`) {
			t.Errorf("detected legacy incorrect positive dashoffset wrapping in SVG donut chart")
		}
		if !strings.Contains(costsVM.ModelCostSVG, `stroke-dashoffset="0.00"`) && !strings.Contains(costsVM.ModelCostSVG, `stroke-dashoffset="-0.00"`) {
			t.Errorf("expected initial SVG donut segment to start at 0.00 offset")
		}
	})

	// 3. Test fetchLocalLogsMetrics
	t.Run("Local Logs Metrics Integration", func(t *testing.T) {
		metricsVM, err := dash.fetchLocalLogsMetrics()
		if err != nil {
			t.Fatalf("fetchLocalLogsMetrics failed: %v", err)
		}

		// Total requests should be 3
		if metricsVM.TotalRequests != 3 {
			t.Errorf("expected 3 total requests, got %d", metricsVM.TotalRequests)
		}

		// 1 error out of 3 -> ~33.33% error rate
		expectedErrRate := (1.0 / 3.0) * 100.0
		if metricsVM.ErrorRate < expectedErrRate-1.0 || metricsVM.ErrorRate > expectedErrRate+1.0 {
			t.Errorf("expected error rate around %f, got %f", expectedErrRate, metricsVM.ErrorRate)
		}

		// Volume SVG charts should be non-empty strings
		if len(metricsVM.VolumeChartSVG) == 0 || len(metricsVM.LatencyChartSVG) == 0 {
			t.Errorf("expected generated SVG charts to be non-empty")
		}
	})

	// 4. Test UI dashboard router invocation
	t.Run("Dashboard ServeCosts and ServeMetrics routes", func(t *testing.T) {
		reqCosts := httptest.NewRequest("GET", "/admin/costs", nil)
		rrCosts := httptest.NewRecorder()
		dash.ServeCosts(rrCosts, reqCosts)
		if rrCosts.Code != http.StatusOK {
			t.Errorf("expected costs page to render 200, got %d", rrCosts.Code)
		}

		reqMetrics := httptest.NewRequest("GET", "/admin/metrics", nil)
		rrMetrics := httptest.NewRecorder()
		dash.ServeMetrics(rrMetrics, reqMetrics)
		if rrMetrics.Code != http.StatusOK {
			t.Errorf("expected metrics page to render 200, got %d", rrMetrics.Code)
		}
	})

	// 5. Test Serving Local Markdown Documentation (Dynamic Reader)
	t.Run("Docs page rendering and directory traversal protection", func(t *testing.T) {
		// Test default README docs page
		reqDefault := httptest.NewRequest("GET", "/admin/docs", nil)
		rrDefault := httptest.NewRecorder()
		dash.ServeDocs(rrDefault, reqDefault)
		if rrDefault.Code != http.StatusOK {
			t.Errorf("expected default docs page to render 200, got %d", rrDefault.Code)
		}
		bodyDefault := rrDefault.Body.String()
		if !strings.Contains(bodyDefault, "Smart Router") || !strings.Contains(bodyDefault, "Getting Started") {
			t.Errorf("default docs page did not render correctly or was missing contents")
		}

		// Test specific category path
		reqSpecific := httptest.NewRequest("GET", "/admin/docs?path=admin/client-organizations.md", nil)
		rrSpecific := httptest.NewRecorder()
		dash.ServeDocs(rrSpecific, reqSpecific)
		if rrSpecific.Code != http.StatusOK {
			t.Errorf("expected admin/client-organizations docs to render 200, got %d", rrSpecific.Code)
		}
		bodySpecific := rrSpecific.Body.String()
		if !strings.Contains(bodySpecific, "Client Organizations") {
			t.Errorf("specific docs page did not render target title or headers")
		}

		// Test invalid traversal injection - absolute path
		reqTraversalAbs := httptest.NewRequest("GET", "/admin/docs?path=/etc/passwd", nil)
		rrTraversalAbs := httptest.NewRecorder()
		dash.ServeDocs(rrTraversalAbs, reqTraversalAbs)
		if rrTraversalAbs.Code != http.StatusForbidden {
			t.Errorf("expected absolute path request to return 403 Forbidden, got %d", rrTraversalAbs.Code)
		}

		// Test invalid traversal injection - relative parent path
		reqTraversalRel := httptest.NewRequest("GET", "/admin/docs?path=../../go.mod", nil)
		rrTraversalRel := httptest.NewRecorder()
		dash.ServeDocs(rrTraversalRel, reqTraversalRel)
		if rrTraversalRel.Code != http.StatusForbidden {
			t.Errorf("expected relative parent traversal request to return 403 Forbidden, got %d", rrTraversalRel.Code)
		}
	})
}

