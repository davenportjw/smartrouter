package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
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
	t.Setenv("LOCAL_DATA_DIR", t.TempDir())

	ctx := context.Background()
	dbStore, err := store.NewConfigStore(ctx, "test-project")
	if err != nil {
		t.Fatalf("Failed to initialize backend store: %v", err)
	}

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

}

func TestLocalLogsTelemetryAndDashboards(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")

	// 1. Clean up previous local logs if any
	os.RemoveAll("data")
	defer os.RemoveAll("data")

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

func TestDashboardComplexityInspect(t *testing.T) {
	dash := NewDashboardController(nil, "test-project", "us-central1")

	t.Run("Inspect HTML Jev Rule Classification", func(t *testing.T) {
		form := url.Values{}
		form.Add("prompt", "```go\npackage queue\ntype RingBuffer struct {\n    mu sync.RWMutex\n}\nfunc (r *RingBuffer) Pop() interface{} {\n    return nil\n}\n```\nOptimize concurrency and algorithm proof.")
		form.Add("engine", "jev")

		req := httptest.NewRequest("POST", "/admin/complexity/inspect", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rr := httptest.NewRecorder()

		dash.HandleComplexityInspect(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d", rr.Code)
		}
		body := rr.Body.String()
		if !strings.Contains(body, "gemini-3.1-pro") {
			t.Errorf("expected prompt with complex code to route to gemini-3.1-pro, got: %s", body)
		}
		if !strings.Contains(body, "Decision Rationale") {
			t.Errorf("expected explainable rationale section in HTML, got: %s", body)
		}
	})

	t.Run("Inspect JSON CLM Contrastive Classification", func(t *testing.T) {
		form := url.Values{}
		form.Add("prompt", "What is 2+2?")
		form.Add("engine", "clm")

		req := httptest.NewRequest("POST", "/admin/complexity/inspect", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Accept", "application/json")
		rr := httptest.NewRecorder()

		dash.HandleComplexityInspect(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d", rr.Code)
		}
		if !strings.Contains(rr.Header().Get("Content-Type"), "application/json") {
			t.Errorf("expected application/json content type, got: %s", rr.Header().Get("Content-Type"))
		}
		body := rr.Body.String()
		if !strings.Contains(body, "gemini-3.5-flash-lite") {
			t.Errorf("expected simple prompt to route to gemini-3.5-flash-lite, got: %s", body)
		}
	})

	t.Run("Inspect Agent Task Context Routing", func(t *testing.T) {
		form := url.Values{}
		form.Add("prompt", "Inspect and fix this memory leak")
		form.Add("task", "debugging")

		req := httptest.NewRequest("POST", "/admin/complexity/inspect", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Accept", "application/json")
		rr := httptest.NewRecorder()

		dash.HandleComplexityInspect(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d", rr.Code)
		}
		body := rr.Body.String()
		if !strings.Contains(body, "task_mapping") || !strings.Contains(body, "gemini-3.1-pro") {
			t.Errorf("expected debugging task to map to complex tier gemini-3.1-pro, got: %s", body)
		}
	})
}

func TestDashboardRemediationsAndUIArchitecture(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")
	t.Setenv("LOCAL_DATA_DIR", t.TempDir())
	t.Setenv("PROXY_PUBLIC_URL", "https://custom-proxy.smartrouter.internal")

	ctx := context.Background()
	dbStore, err := store.NewConfigStore(ctx, "test-project")
	if err != nil {
		t.Fatalf("Failed to initialize backend store: %v", err)
	}

	dash := NewDashboardController(dbStore, "test-project", "us-central1")

	t.Run("Crisp Slate & Cobalt 3-Panel Layout and Dynamic Proxy URL", func(t *testing.T) {
		for _, page := range []struct {
			path    string
			handler http.HandlerFunc
		}{
			{"/admin/apps", dash.ServeApps},
			{"/admin/clusters", dash.ServeClusters},
			{"/admin/complexity", dash.ServeComplexity},
		} {
			req := httptest.NewRequest("GET", page.path, nil)
			rr := httptest.NewRecorder()
			page.handler(rr, req)

			if rr.Code != http.StatusOK {
				t.Fatalf("expected 200 OK on %s, got %d", page.path, rr.Code)
			}
			body := rr.Body.String()
			for _, expected := range []string{
				`id="left-sidebar"`,
				`id="main-workspace"`,
				`id="context-drawer" class="hidden `,
				`id="context-drawer-toggle"`,
				`aria-expanded="false"`,
				`aria-controls="context-drawer"`,
				`id="context-drawer-chevron"`,
				`<span>Inspector</span>`,
				`https://custom-proxy.smartrouter.internal`,
				`<span>Client Orgs</span>`,
				`<span>Applications</span>`,
				`<span>API Keys</span>`,
				`<span>Custom Headers</span>`,
				`<span>Complexity Routing</span>`,
				`<span>Routing Rules</span>`,
				`<span>Models Registry</span>`,
				`<span>Compute Clusters</span>`,
				`<span>Active Queue</span>`,
				`<span>Telemetry</span>`,
				`<span>Cost Analytics</span>`,
				`<span>Documentation</span>`,
			} {
				if !strings.Contains(body, expected) {
					t.Errorf("expected %s 3-panel layout to contain %q", page.path, expected)
				}
			}
			for _, forbidden := range []string{
				`Gemini 2.5+ / 3.8 Flash / Gemma 4`,
				`App-Scoped RPM/TPM`,
				`Route Inspector &amp; SDK Ref`,
				`1. Client Orgs`,
				`2. Applications`,
				`3. API Keys`,
				`4. Custom Headers`,
				`5. Complexity Routing`,
				`6. Routing Rules`,
				`7. Models Registry`,
				`8. Compute Clusters`,
				`9. Active Queue`,
				`10. Telemetry`,
				`11. Cost Analytics`,
			} {
				if strings.Contains(body, forbidden) {
					t.Errorf("expected %s layout NOT to contain %q", page.path, forbidden)
				}
			}
		}
	})

	t.Run("Unified Left Sidebar and Adjacent Docs Sidebar Typography and Non-Pill Styling", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/admin/docs", nil)
		rr := httptest.NewRecorder()
		dash.ServeDocs(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200 OK on /admin/docs, got %d", rr.Code)
		}
		body := rr.Body.String()

		for _, expected := range []string{
			`id="left-sidebar"`,
			`id="docs-sidebar" class="w-64 shrink-0 bg-slate-100 border-r border-slate-200 flex flex-col overflow-y-auto select-none p-4 space-y-6"`,
			`id="main-workspace" class="flex-1 min-h-0 flex overflow-hidden"`,
			`px-2.5 pb-1 text-[11px] font-semibold uppercase tracking-wider text-slate-500`,
			`px-2.5 py-1.5 rounded-r-md text-[13px] leading-5 transition-colors`,
			`bg-blue-50 text-blue-700 font-semibold border-l-2 border-blue-600`,
			`font-size: 0.9375rem;`,
			`font-size: 1.625rem;`,
		} {
			if !strings.Contains(body, expected) {
				t.Errorf("expected /admin/docs to contain %q", expected)
			}
		}

		for _, forbidden := range []string{
			`rounded-2xl`,
			`text-[10px] font-bold uppercase tracking-widest`,
			`Back to Dashboard`,
		} {
			if strings.Contains(body, forbidden) {
				t.Errorf("expected /admin/docs NOT to contain %q", forbidden)
			}
		}
	})

	t.Run("CreateApp Inherits RPM, TPM, and Priority from Parent Client", func(t *testing.T) {
		_ = dbStore.SaveClient(ctx, config.Client{
			ID:       "client-parent-inherit",
			Name:     "Parent Inherit Org",
			Tier:     "enterprise",
			RPM:      750,
			TPM:      500000,
			Priority: "high",
		})

		form := url.Values{}
		form.Add("app_id", "app-inherited-quotas")
		form.Add("app_name", "Inherited Quotas App")
		form.Add("client_id", "client-parent-inherit")
		// Leave rpm, tpm, and priority blank

		req := httptest.NewRequest("POST", "/admin/apps/create", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rr := httptest.NewRecorder()
		dash.CreateApp(rr, req)

		if rr.Code != http.StatusSeeOther {
			t.Fatalf("expected 303 SeeOther, got %d: %s", rr.Code, rr.Body.String())
		}

		app, ok := dbStore.LookupApp("app-inherited-quotas")
		if !ok {
			t.Fatalf("expected app-inherited-quotas to be saved")
		}
		if app.RPM != 750 || app.TPM != 500000 || app.Priority != "high" {
			t.Errorf("expected inherited RPM=750, TPM=500000, Priority=high; got RPM=%d, TPM=%d, Priority=%s", app.RPM, app.TPM, app.Priority)
		}
		if app.Complexity.SimpleModel != "gemini-3.5-flash-lite" || app.Complexity.MediumModel != "gemini-3.8-flash" || app.Complexity.ComplexModel != "gemini-3.1-pro" {
			t.Errorf("expected default real complexity models, got %+v", app.Complexity)
		}
	})

	t.Run("SaveComplexitySettings Persists TaskMapping", func(t *testing.T) {
		form := url.Values{}
		form.Add("app_id", "app-inherited-quotas")
		form.Add("enabled", "true")
		form.Add("engine", "jev")
		form.Add("simple_char_limit", "200")
		form.Add("medium_char_limit", "1000")
		form.Add("task_code", "gemini-3.1-pro")
		form.Add("task_triage", "gemini-3.5-flash-lite")
		form.Add("task_summarization", "gemini-3.8-flash")

		req := httptest.NewRequest("POST", "/admin/complexity/save", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rr := httptest.NewRecorder()
		dash.SaveComplexitySettings(rr, req)

		if rr.Code != http.StatusSeeOther {
			t.Fatalf("expected 303 SeeOther, got %d: %s", rr.Code, rr.Body.String())
		}

		app, ok := dbStore.LookupApp("app-inherited-quotas")
		if !ok {
			t.Fatalf("expected app-inherited-quotas to exist")
		}
		if app.Complexity.TaskMapping["code"] != "gemini-3.1-pro" ||
			app.Complexity.TaskMapping["triage"] != "gemini-3.5-flash-lite" ||
			app.Complexity.TaskMapping["summarization"] != "gemini-3.8-flash" {
			t.Errorf("expected TaskMapping overrides to persist, got: %+v", app.Complexity.TaskMapping)
		}
	})

	t.Run("Provider Cluster and Endpoint CRUD and Missing Runner 404", func(t *testing.T) {
		// 1. Create Local Cluster
		clusterForm := url.Values{}
		clusterForm.Add("type", "cluster")
		clusterForm.Add("id", "cluster-onprem-gpu")
		clusterForm.Add("name", "On-Premises L4 GPU Pool")
		clusterForm.Add("location", "us-central1-edge")
		clusterForm.Add("models", "gemma-4-27b-it, gemma-4-9b-it")

		reqCluster := httptest.NewRequest("POST", "/admin/clusters/providers/create", strings.NewReader(clusterForm.Encode()))
		reqCluster.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		reqCluster.Header.Set("HX-Request", "true")
		rrCluster := httptest.NewRecorder()
		dash.CreateProviderCluster(rrCluster, reqCluster)

		if rrCluster.Code != http.StatusOK || rrCluster.Header().Get("HX-Redirect") != "/admin/clusters" {
			t.Fatalf("expected 200 with HX-Redirect=/admin/clusters, got %d", rrCluster.Code)
		}

		// 2. Create Direct Endpoint
		epForm := url.Values{}
		epForm.Add("type", "endpoint")
		epForm.Add("id", "ep-vllm-prod")
		epForm.Add("name", "vLLM Production Gateway")
		epForm.Add("url", "http://10.0.10.50:8000")
		epForm.Add("model", "gemma-4-27b-it")

		reqEp := httptest.NewRequest("POST", "/admin/clusters/providers/create", strings.NewReader(epForm.Encode()))
		reqEp.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rrEp := httptest.NewRecorder()
		dash.CreateProviderCluster(rrEp, reqEp)

		if rrEp.Code != http.StatusSeeOther {
			t.Fatalf("expected 303 SeeOther for endpoint creation, got %d", rrEp.Code)
		}

		// 3. Render Clusters page and verify both appear and modal-container is outside poller
		reqView := httptest.NewRequest("GET", "/admin/clusters", nil)
		rrView := httptest.NewRecorder()
		dash.ServeClusters(rrView, reqView)
		viewBody := rrView.Body.String()
		if !strings.Contains(viewBody, "cluster-onprem-gpu") || !strings.Contains(viewBody, "ep-vllm-prod") {
			t.Errorf("expected clusters page to render created cluster and endpoint, got: %s", viewBody)
		}

		// Verify PollClustersActive does not include modal-container
		reqPoll := httptest.NewRequest("GET", "/admin/clusters/active", nil)
		rrPoll := httptest.NewRecorder()
		dash.PollClustersActive(rrPoll, reqPoll)
		if strings.Contains(rrPoll.Body.String(), `id="modal-container"`) {
			t.Errorf("expected PollClustersActive partial not to contain #modal-container so open modals are not destroyed during polling")
		}

		// 4. Delete cluster and endpoint
		reqDelCluster := httptest.NewRequest("DELETE", "/admin/clusters/providers/delete?type=cluster&id=cluster-onprem-gpu", nil)
		reqDelCluster.Header.Set("HX-Request", "true")
		rrDelCluster := httptest.NewRecorder()
		dash.DeleteProviderCluster(rrDelCluster, reqDelCluster)
		if rrDelCluster.Code != http.StatusOK || rrDelCluster.Header().Get("HX-Refresh") != "true" {
			t.Errorf("expected 200 OK with HX-Refresh=true on cluster delete, got %d", rrDelCluster.Code)
		}

		// 5. Missing runner node returns 404 NotFound (no synthetic fake node)
		reqMissingRunner := httptest.NewRequest("GET", "/admin/clusters/mappings-modal?id=nonexistent-runner", nil)
		rrMissingRunner := httptest.NewRecorder()
		dash.ServeClusterMappingsModal(rrMissingRunner, reqMissingRunner)
		if rrMissingRunner.Code != http.StatusNotFound {
			t.Errorf("expected 404 NotFound for nonexistent runner node, got %d", rrMissingRunner.Code)
		}
	})

	t.Run("RevokeKey Sets HX-Refresh Header", func(t *testing.T) {
		hash := "hash-revoke-hx-refresh-test-001"
		if err := dbStore.SaveKey(ctx, config.APIKey{
			KeyHash: hash,
			AppID:   "app-inherited-quotas",
			Status:  "active",
		}); err != nil {
			t.Fatalf("failed to save key: %v", err)
		}
		reqRevoke := httptest.NewRequest("DELETE", "/admin/keys/revoke?hash="+hash, nil)
		rrRevoke := httptest.NewRecorder()
		dash.RevokeKey(rrRevoke, reqRevoke)

		if rrRevoke.Code != http.StatusOK || rrRevoke.Header().Get("HX-Refresh") != "true" {
			t.Errorf("expected 200 OK and HX-Refresh=true on RevokeKey, got code=%d header=%q", rrRevoke.Code, rrRevoke.Header().Get("HX-Refresh"))
		}
	})

	t.Run("ServeLogin Clears Invalid Session Cookie When SessionVerifier Fails", func(t *testing.T) {
		dash.SessionVerifier = func(r *http.Request) bool {
			return false
		}
		reqLogin := httptest.NewRequest("GET", "/login", nil)
		reqLogin.AddCookie(&http.Cookie{Name: "session", Value: "expired-or-forged-token"})
		rrLogin := httptest.NewRecorder()

		dash.ServeLogin(rrLogin, reqLogin)

		if rrLogin.Code != http.StatusOK {
			t.Errorf("expected 200 OK rendering login page when session cookie is invalid, got %d", rrLogin.Code)
		}
		var cleared bool
		for _, c := range rrLogin.Result().Cookies() {
			if c.Name == "session" && c.MaxAge < 0 {
				cleared = true
			}
		}
		if !cleared {
			t.Errorf("expected invalid session cookie to be cleared with MaxAge < 0 on /login")
		}
	})

	t.Run("ServeLogin Resolves Firebase Web API Key Fallback and Project Defaults", func(t *testing.T) {
		t.Setenv("FIREBASE_API_KEY", "")
		t.Setenv("FIREBASE_WEB_API_KEY", "AIzaSyTestFallbackWebKey123")
		t.Setenv("FIREBASE_AUTH_DOMAIN", "")
		t.Setenv("FIREBASE_PROJECT_ID", "")
		t.Setenv("FIREBASE_STORAGE_BUCKET", "")
		t.Setenv("FIREBASE_APP_ID", "1:834476222725:web:testappid")

		loginDash := NewDashboardController(dbStore, "davenport-boutique", "us-central1")
		reqLogin := httptest.NewRequest("GET", "/login", nil)
		rrLogin := httptest.NewRecorder()

		loginDash.ServeLogin(rrLogin, reqLogin)

		if rrLogin.Code != http.StatusOK {
			t.Fatalf("expected 200 OK on /login, got %d", rrLogin.Code)
		}
		body := rrLogin.Body.String()
		if !strings.Contains(body, `data-api-key="AIzaSyTestFallbackWebKey123"`) {
			t.Errorf("expected login HTML to contain fallback FIREBASE_WEB_API_KEY, got: %s", body)
		}
		if !strings.Contains(body, `data-auth-domain="davenport-boutique.firebaseapp.com"`) {
			t.Errorf("expected login HTML to contain default auth domain davenport-boutique.firebaseapp.com")
		}
		if !strings.Contains(body, `data-project-id="davenport-boutique"`) {
			t.Errorf("expected login HTML to contain default project id davenport-boutique")
		}
	})
}

func TestDashboardFastinoContrastiveLMAndSecretKeys(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")
	t.Setenv("LOCAL_DATA_DIR", t.TempDir())
	ctx := context.Background()
	dbStore, err := store.NewConfigStore(ctx, "test-project")
	if err != nil {
		t.Fatalf("Failed to initialize backend store: %v", err)
	}

	_ = dbStore.SaveClient(ctx, config.Client{ID: "client-dec", Name: "Decision Org", Tier: "premium"})
	_ = dbStore.SaveApp(ctx, config.App{ID: "app-dec", ClientID: "client-dec", Name: "Decision App", RPM: 100, TPM: 100000})

	dash := NewDashboardController(dbStore, "test-project", "us-central1")

	t.Run("Save and Delete Engine Keys via /admin/complexity/keys", func(t *testing.T) {
		form := url.Values{}
		form.Add("engine", "fastino")
		form.Add("api_key", "sk-fastino-admin-vault-key-8888")

		req := httptest.NewRequest("POST", "/admin/complexity/keys", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("HX-Request", "true")
		rr := httptest.NewRecorder()
		dash.HandleComplexityKeys(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200 OK on POST /admin/complexity/keys, got %d: %s", rr.Code, rr.Body.String())
		}

		// Render /admin/complexity and verify masked preview and gcloud instructions appear, but NOT raw key
		reqView := httptest.NewRequest("GET", "/admin/complexity", nil)
		rrView := httptest.NewRecorder()
		dash.ServeComplexity(rrView, reqView)
		body := rrView.Body.String()
		if strings.Contains(body, "sk-fastino-admin-vault-key-8888") {
			t.Errorf("SECURITY VIOLATION: raw API key was rendered in /admin/complexity HTML!")
		}
		if !strings.Contains(body, "sk-...8888") {
			t.Errorf("expected masked key preview sk-...8888 in /admin/complexity HTML")
		}
		if !strings.Contains(body, "gcloud secrets create") {
			t.Errorf("expected gcloud Secret Manager CLI instructions on /admin/complexity page")
		}
	})

	t.Run("SaveComplexitySettings Persists Endpoints, Criteria, and MinConfidence", func(t *testing.T) {
		form := url.Values{}
		form.Add("app_id", "app-dec")
		form.Add("enabled", "true")
		form.Add("engine", "fastino")
		form.Add("jev_endpoint", "https://jev.internal/v1/systemone")
		form.Add("fastino_endpoint", "https://fastino.internal/v1/systemone")
		form.Add("clm_endpoint", "https://clm.internal/rank")
		form.Add("secret_ref", "FASTINO_SECRET_REF")
		form.Add("min_confidence", "0.75")
		form.Add("simple_criteria", "short factual queries")
		form.Add("medium_criteria", "general summarization")
		form.Add("complex_criteria", "distributed systems proofs")
		form.Add("simple_model", "gemini-3.5-flash-lite")
		form.Add("medium_model", "gemini-3.8-flash")
		form.Add("complex_model", "gemini-3.1-pro")

		req := httptest.NewRequest("POST", "/admin/complexity/save", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rr := httptest.NewRecorder()
		dash.SaveComplexitySettings(rr, req)

		if rr.Code != http.StatusSeeOther {
			t.Fatalf("expected 303 SeeOther, got %d: %s", rr.Code, rr.Body.String())
		}

		app, ok := dbStore.LookupApp("app-dec")
		if !ok {
			t.Fatalf("expected app-dec to exist")
		}
		c := app.Complexity
		if c.Engine != "fastino" || c.FastinoEndpoint != "https://fastino.internal/v1/systemone" || c.CLMEndpoint != "https://clm.internal/rank" || c.MinConfidence != 0.75 || c.ComplexCriteria != "distributed systems proofs" {
			t.Errorf("unexpected saved complexity settings: %+v", c)
		}
	})

	t.Run("Inspect Fastino and Contrastive-LM Engines", func(t *testing.T) {
		for _, eng := range []string{"fastino", "contrastive-lm"} {
			form := url.Values{}
			form.Add("prompt", "```go\nfunc Paxos() {}\n```\nProve consensus safety in O(N^2).")
			form.Add("engine", eng)

			req := httptest.NewRequest("POST", "/admin/complexity/inspect", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("Accept", "application/json")
			rr := httptest.NewRecorder()
			dash.HandleComplexityInspect(rr, req)

			if rr.Code != http.StatusOK {
				t.Fatalf("inspect %s failed with %d", eng, rr.Code)
			}
			if !strings.Contains(rr.Body.String(), "gemini-3.1-pro") {
				t.Errorf("expected %s inspect to route complex code prompt to gemini-3.1-pro, got: %s", eng, rr.Body.String())
			}
		}
	})

	t.Run("CreateRule Persists Classifier Conditions", func(t *testing.T) {
		form := url.Values{}
		form.Add("model_pattern", "*")
		form.Add("app_id", "app-dec")
		form.Add("client_tier", "all")
		form.Add("complexity_tier", "complex")
		form.Add("decision_engine", "contrastive-lm")
		form.Add("min_confidence", "0.85")
		form.Add("task_kind", "code_generation")
		form.Add("target_model", "gemini-3.1-pro")
		form.Add("target_location", "us-central1")
		form.Add("priority_weight", "15")

		req := httptest.NewRequest("POST", "/admin/rules/save", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rr := httptest.NewRecorder()
		dash.SaveRule(rr, req)

		if rr.Code != http.StatusSeeOther {
			t.Fatalf("expected 303 SeeOther on SaveRule, got %d: %s", rr.Code, rr.Body.String())
		}

		rules, _ := dbStore.GetAllRules(ctx)
		var found *config.RoutingRule
		for _, r := range rules {
			if r.AppID == "app-dec" && r.DecisionEngine == "contrastive-lm" {
				rCopy := r
				found = &rCopy
				break
			}
		}
		if found == nil {
			t.Fatalf("expected to find created rule with decision_engine=contrastive-lm")
		}
		if found.ComplexityTier != "complex" || found.MinConfidence != 0.85 || found.TaskKind != "code_generation" {
			t.Errorf("rule classifier conditions not persisted properly: %+v", found)
		}
	})

	t.Run("Configure Contrastive-LM Cluster Runner and Probe Live Endpoint", func(t *testing.T) {
		clmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"tier":"complex","confidence":0.95,"rationale":"Cluster CLM ranker matched complex tier","scores":{"simple":0.02,"medium":0.03,"complex":0.95}}`))
		}))
		defer clmServer.Close()

		form := url.Values{}
		form.Add("engine", "contrastive-lm")
		form.Add("connection_mode", "cluster")
		form.Add("cluster_id", "clm-cluster-default")
		form.Add("endpoint", clmServer.URL+"/rank")

		req := httptest.NewRequest("POST", "/admin/complexity/keys", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("HX-Request", "true")
		rr := httptest.NewRecorder()
		dash.HandleComplexityKeys(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200 OK saving CLM cluster config, got %d: %s", rr.Code, rr.Body.String())
		}

		// Verify /admin/complexity renders Cluster Runner and endpoint
		reqView := httptest.NewRequest("GET", "/admin/complexity", nil)
		rrView := httptest.NewRecorder()
		dash.ServeComplexity(rrView, reqView)
		viewBody := rrView.Body.String()
		if !strings.Contains(viewBody, "Cluster Runner") || !strings.Contains(viewBody, clmServer.URL+"/rank") {
			t.Errorf("expected /admin/complexity to display Cluster Runner and CLM endpoint")
		}

		// Verify /admin/clusters renders Decision Engine (CLM) badge
		reqClusters := httptest.NewRequest("GET", "/admin/clusters", nil)
		rrClusters := httptest.NewRecorder()
		dash.ServeClusters(rrClusters, reqClusters)
		if !strings.Contains(rrClusters.Body.String(), "Decision Engine (CLM)") {
			t.Errorf("expected /admin/clusters to display Decision Engine (CLM) badge for Contrastive-LM endpoint")
		}

		// Probe via /admin/complexity/providers/probe (HTML)
		probeForm := url.Values{}
		probeForm.Add("engine", "contrastive-lm")
		reqProbe := httptest.NewRequest("POST", "/admin/complexity/providers/probe", strings.NewReader(probeForm.Encode()))
		reqProbe.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rrProbe := httptest.NewRecorder()
		dash.HandleComplexityProviderProbe(rrProbe, reqProbe)
		if rrProbe.Code != http.StatusOK || !strings.Contains(rrProbe.Body.String(), "Healthy") {
			t.Errorf("expected Healthy probe response, got %d: %s", rrProbe.Code, rrProbe.Body.String())
		}
	})

	t.Run("Quick API and SDK Reference Includes OpenAI Spec and Common Packages", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/admin/apps", nil)
		rr := httptest.NewRecorder()
		dash.ServeApps(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200 OK on /admin/apps, got %d", rr.Code)
		}
		body := rr.Body.String()
		requiredSnippets := []string{
			"from openai import OpenAI",
			"/v1/chat/completions",
			"chat.completions.create",
			"Authorization: Bearer YOUR_ROUTER_KEY",
			"langchain_openai",
			"litellm",
			"from google import genai",
		}
		for _, snippet := range requiredSnippets {
			if !strings.Contains(body, snippet) {
				t.Errorf("expected Quick API & SDK Reference drawer to contain %q, but it was missing", snippet)
			}
		}
	})

	t.Run("Code Snippet Boxes Include Copy-to-Clipboard Icon Buttons", func(t *testing.T) {
		// 1. Check Layout & Right Contextual Drawer (including cURL Gemini Native :generateContent)
		reqApps := httptest.NewRequest("GET", "/admin/apps", nil)
		rrApps := httptest.NewRecorder()
		dash.ServeApps(rrApps, reqApps)
		if rrApps.Code != http.StatusOK {
			t.Fatalf("expected 200 OK on /admin/apps, got %d", rrApps.Code)
		}
		appsBody := rrApps.Body.String()
		for _, expected := range []string{
			"copyCodeSnippet",
			"initMarkdownCopyButtons",
			"copy-snippet-btn",
			`aria-label="Copy to clipboard"`,
			"cURL (Gemini Native",
		} {
			if !strings.Contains(appsBody, expected) {
				t.Errorf("expected /admin/apps layout & drawer to contain %q", expected)
			}
		}
		if cnt := strings.Count(appsBody, "copy-snippet-btn"); cnt < 7 {
			t.Errorf("expected at least 7 copy-snippet-btn buttons in layout/drawer (Proxy Base URL + 6 code snippets), got %d", cnt)
		}

		// 2. Check Keys Page (Target Router Proxy URL + 5 SDK/cURL snippet boxes)
		reqKeys := httptest.NewRequest("GET", "/admin/keys", nil)
		rrKeys := httptest.NewRecorder()
		dash.ServeKeys(rrKeys, reqKeys)
		if rrKeys.Code != http.StatusOK {
			t.Fatalf("expected 200 OK on /admin/keys, got %d", rrKeys.Code)
		}
		keysBody := rrKeys.Body.String()
		if cnt := strings.Count(keysBody, "copy-snippet-btn"); cnt < 13 {
			t.Errorf("expected at least 13 copy-snippet-btn buttons on /admin/keys (drawer + keys page snippets), got %d", cnt)
		}

		// 3. Check Headers Page (4 SDK/cURL snippet boxes)
		reqHeaders := httptest.NewRequest("GET", "/admin/headers", nil)
		rrHeaders := httptest.NewRecorder()
		dash.ServeHeaders(rrHeaders, reqHeaders)
		if rrHeaders.Code != http.StatusOK {
			t.Fatalf("expected 200 OK on /admin/headers, got %d", rrHeaders.Code)
		}
		headersBody := rrHeaders.Body.String()
		if cnt := strings.Count(headersBody, "copy-snippet-btn"); cnt < 11 {
			t.Errorf("expected at least 11 copy-snippet-btn buttons on /admin/headers (drawer + 4 headers snippets), got %d", cnt)
		}

		// 4. Check Key Creation Alert Modal/Banner
		keyForm := url.Values{}
		keyForm.Add("app_id", "app-dec")
		reqCreateKey := httptest.NewRequest("POST", "/admin/keys/create", strings.NewReader(keyForm.Encode()))
		reqCreateKey.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rrCreateKey := httptest.NewRecorder()
		dash.CreateKey(rrCreateKey, reqCreateKey)
		if rrCreateKey.Code != http.StatusOK {
			t.Fatalf("expected 200 OK on /admin/keys/create, got %d", rrCreateKey.Code)
		}
		createKeyBody := rrCreateKey.Body.String()
		if !strings.Contains(createKeyBody, "copy-snippet-btn") || !strings.Contains(createKeyBody, "copyCodeSnippet") {
			t.Errorf("expected newly created key banner to include copy-snippet-btn and copyCodeSnippet")
		}
	})

	t.Run("Decision Engine Providers and Clusters Use Master-Detail Split View", func(t *testing.T) {
		reqComp := httptest.NewRequest("GET", "/admin/complexity", nil)
		rrComp := httptest.NewRecorder()
		dash.ServeComplexity(rrComp, reqComp)
		if rrComp.Code != http.StatusOK {
			t.Fatalf("expected 200 OK on /admin/complexity, got %d", rrComp.Code)
		}
		compBody := rrComp.Body.String()

		for _, required := range []string{
			`id="decision-providers-master-detail"`,
			`id="decision-provider-list"`,
			`id="provider-detail-inspector"`,
			`data-provider-row="jev"`,
			`data-provider-row="fastino"`,
			`data-provider-row="contrastive-lm"`,
			`selectDecisionProvider`,
			`id="provider-probe-result"`,
			`id="secret-engine"`,
			`id="clm-connection-mode"`,
			`clmForm.classList.toggle('hidden', engine !== 'contrastive-lm')`,
		} {
			if !strings.Contains(compBody, required) {
				t.Errorf("expected /admin/complexity master-detail view to contain %q", required)
			}
		}

		if !strings.Contains(compBody, `id="clm-cluster-setup-form"`) || !strings.Contains(compBody, "hidden") {
			t.Errorf("expected #clm-cluster-setup-form to start hidden until contrastive-lm is selected")
		}

		if strings.Contains(compBody, "Provider Status Cards (Jev, Fastino, Contrastive-LM)") {
			t.Errorf("expected legacy 3-column Provider Status Cards grid to be replaced by Master-Detail list")
		}

		reqClusters := httptest.NewRequest("GET", "/admin/clusters", nil)
		rrClusters := httptest.NewRecorder()
		dash.ServeClusters(rrClusters, reqClusters)
		if rrClusters.Code != http.StatusOK {
			t.Fatalf("expected 200 OK on /admin/clusters, got %d", rrClusters.Code)
		}
		clustersBody := rrClusters.Body.String()
		for _, required := range []string{
			`1. On-Premises Clusters (Pull-Queue Pools)`,
			`2. Cluster Nodes (Managed Runners)`,
			`3. Custom / Direct HTTP Endpoints (Unmanaged Hardware)`,
		} {
			if !strings.Contains(clustersBody, required) {
				t.Errorf("expected /admin/clusters 3-table CRUDL view to contain %q", required)
			}
		}

		// Seed a test key for ServeKeysEditModal check
		_ = dbStore.SaveKey(ctx, config.APIKey{
			KeyHash: "hash-master-detail-key-01",
			AppID:   "app-dec",
			Status:  "active",
		})

		// Verify all 6 configuration pages render a side-by-side Master-Detail Split View with a sticky right-hand #modal-container inspector pane
		splitPages := []struct {
			name    string
			path    string
			handler http.HandlerFunc
		}{
			{"KeysPage", "/admin/keys", dash.ServeKeys},
			{"AppsPage", "/admin/apps", dash.ServeApps},
			{"ClientsPage", "/admin/clients", dash.ServeClients},
			{"RulesPage", "/admin/rules", dash.ServeRules},
			{"HeadersPage", "/admin/headers", dash.ServeHeaders},
			{"ComplexityPage", "/admin/complexity", dash.ServeComplexity},
		}
		for _, sp := range splitPages {
			req := httptest.NewRequest("GET", sp.path, nil)
			rr := httptest.NewRecorder()
			sp.handler(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("expected 200 OK for %s (%s), got %d", sp.name, sp.path, rr.Code)
			}
			pageHTML := rr.Body.String()
			for _, token := range []string{
				`data-layout="master-detail-split"`,
				`flex-col lg:flex-row gap-6 items-start`,
				`id="modal-container"`,
				`lg:sticky lg:top-6`,
				`hidden [&:has(*)]:block`,
			} {
				if !strings.Contains(pageHTML, token) {
					t.Errorf("expected %s (%s) to contain master-detail split token %q", sp.name, sp.path, token)
				}
			}
		}

		// Specifically on /admin/complexity, verify #modal-container appears BEFORE #decision-providers-master-detail and Smart Routing Inspector (right beside the Applications table)
		modalIdx := strings.Index(compBody, `id="modal-container"`)
		providersIdx := strings.Index(compBody, `id="decision-providers-master-detail"`)
		simulatorIdx := strings.Index(compBody, `Smart Routing Inspector`)
		if modalIdx == -1 || providersIdx == -1 || simulatorIdx == -1 {
			t.Fatalf("expected /admin/complexity to contain #modal-container, #decision-providers-master-detail, and Smart Routing Inspector")
		}
		if modalIdx > providersIdx || modalIdx > simulatorIdx {
			t.Errorf("expected #modal-container on /admin/complexity to be positioned beside the top Applications table BEFORE #decision-providers-master-detail and Smart Routing Inspector (modalIdx=%d, providersIdx=%d, simulatorIdx=%d)", modalIdx, providersIdx, simulatorIdx)
		}

		// Verify all configuration forms render as right-hand inspector panels with Slate header bar and scrollable body rather than fixed inset-0 overlays
		inlineChecks := []struct {
			name    string
			handler http.HandlerFunc
			path    string
		}{
			{"ComplexityModal", dash.ServeComplexityEditModal, "/admin/complexity/edit?app_id=app-dec"},
			{"AppModal", dash.ServeAppsNewModal, "/admin/apps/new"},
			{"ClientModal", dash.ServeClientsNewModal, "/admin/clients/new"},
			{"RuleModal", dash.ServeRulesNewModal, "/admin/rules/new"},
			{"HeaderModal", dash.ServeHeadersNewModal, "/admin/headers/new"},
			{"KeyNewModal", dash.ServeKeysNewModal, "/admin/keys/new"},
			{"KeyEditModal", dash.ServeKeysEditModal, "/admin/keys/edit?hash=hash-master-detail-key-01"},
		}
		for _, check := range inlineChecks {
			req := httptest.NewRequest("GET", check.path, nil)
			rr := httptest.NewRecorder()
			check.handler(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("expected 200 OK for %s (%s), got %d", check.name, check.path, rr.Code)
			}
			modalHTML := rr.Body.String()
			if strings.Contains(modalHTML, "fixed inset-0") {
				t.Errorf("expected %s (%s) to render as a right-hand inspector panel without 'fixed inset-0' pop-up overlay", check.name, check.path)
			}
			for _, token := range []string{
				`max-h-[calc(100vh-5rem)]`,
				`overflow-y-auto`,
				`border-b border-slate-200`,
				`bg-slate-100/`,
			} {
				if !strings.Contains(modalHTML, token) {
					t.Errorf("expected %s (%s) right-hand inspector pane to contain %q", check.name, check.path, token)
				}
			}
		}
	})

	t.Run("Model Pricing Tier Table and Gemini 4 Argon Display & Stats Matching", func(t *testing.T) {
		pricingTests := []struct {
			modelID string
			wantIn  float64
			wantOut float64
		}{
			{"gemini-4-argon", 2.00, 10.00},
			{"gemini-4-argon@global", 2.00, 10.00},
			{"gemini-4-pro", 2.00, 10.00},
			{"gemini-3.8-flash", 0.15, 0.60},
			{"gemini-3.8-flash@us", 0.15, 0.60},
			{"gemini-3.5-flash-lite", 0.05, 0.20},
			{"gemini-3.1-flash-lite", 0.05, 0.20},
			{"gemini-3.5-flash", 0.10, 0.40},
			{"gemini-3.1-pro", 1.50, 6.00},
			{"gemini-2.5-flash-lite", 0.0375, 0.15},
			{"gemini-2.5-flash", 0.075, 0.30},
			{"gemini-2.5-pro", 1.25, 5.00},
			{"gemma-4-26b@local", 0.00, 0.00},
			{"gemma-4-26b@us-central1", 0.15, 0.60},
			{"text-embedding-005", 0.025, 0.00},
			{"gemini-embedding-2-preview", 0.025, 0.00},
		}
		for _, tc := range pricingTests {
			// Run multiple times to ensure deterministic ordering (no Go map iteration randomness)
			for i := 0; i < 20; i++ {
				inCost, outCost := GetModelPricing(tc.modelID)
				if math.Abs(inCost-tc.wantIn) > 1e-6 || math.Abs(outCost-tc.wantOut) > 1e-6 {
					t.Fatalf("GetModelPricing(%q) = (%v, %v), want (%v, %v)", tc.modelID, inCost, outCost, tc.wantIn, tc.wantOut)
				}
			}
		}

		// Verify estimateTokensAndCost uses GetModelPricing accurately for gemini-4-argon
		inTok, outTok, argonCost := estimateTokensAndCost("gemini-4-argon", 3000)
		if inTok != 1500 || outTok != 1000 {
			t.Errorf("expected 1500 in / 1000 out tokens for 3000 bytes_sent, got %d / %d", inTok, outTok)
		}
		// 1500 in @ $2.00/1M = $0.003; 1000 out @ $10.00/1M = $0.010; total = $0.013
		if math.Abs(argonCost-0.013) > 1e-6 {
			t.Errorf("expected gemini-4-argon cost 0.013, got %f", argonCost)
		}

		// Save gemini-4-argon@global with empty DisplayName to test fallback display name + stripped location stats matching
		_ = dbStore.SaveModel(ctx, config.ModelConfig{
			ID:          "gemini-4-argon@global",
			DisplayName: "",
			Location:    "global",
			Type:        "foundation",
			Active:      true,
		})
		_ = dbStore.SaveModel(ctx, config.ModelConfig{
			ID:          "gemma-4-26b@local",
			DisplayName: "gemma-4-26b",
			Location:    "local",
			Type:        "local",
			Active:      true,
		})

		// Write local log with model_routed="gemini-4-argon" (without @global suffix)
		_ = os.MkdirAll("data", 0755)
		defer os.RemoveAll("data")
		logLine := fmt.Sprintf(`{"severity":"INFO","time":"%s","method":"POST","path":"/v1beta/models/gemini-4-argon:generateContent","client_id":"c-dec","app_id":"app-dec","tier":"standard","model_requested":"gemini-4-argon","model_routed":"gemini-4-argon","status":200,"latency_ms":340,"bytes_sent":3000}`+"\n", time.Now().UTC().Format(time.RFC3339))
		if err := os.WriteFile("data/local_logs.jsonl", []byte(logLine), 0644); err != nil {
			t.Fatalf("failed to write local_logs.jsonl: %v", err)
		}

		reqModels := httptest.NewRequest("GET", "/admin/models", nil)
		rrModels := httptest.NewRecorder()
		dash.ServeModels(rrModels, reqModels)
		if rrModels.Code != http.StatusOK {
			t.Fatalf("expected 200 OK on /admin/models, got %d", rrModels.Code)
		}
		modelsBody := rrModels.Body.String()
		for _, expected := range []string{
			"Unit Cost (1M In / Out)",
			"$2.00 / $10.00",
			"$0.00 (Local)",
			"gemini-4-argon",
			"$0.01300",
			"340 ms",
		} {
			if !strings.Contains(modelsBody, expected) {
				t.Errorf("expected /admin/models to contain %q, but it was missing", expected)
			}
		}
	})

	t.Run("Async Background Model Refresh and Live Status Progress Endpoint", func(t *testing.T) {
		mockGCP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if strings.Contains(r.URL.Path, "/publishers/google/models") && !strings.Contains(r.URL.Path, "generateContent") {
				_, _ = w.Write([]byte(`{"publisherModels":[{"name":"publishers/google/models/gemini-4-argon","displayName":""},{"name":"publishers/google/models/gemini-3.8-flash","displayName":"Gemini 3.8 Flash"}]}`))
				return
			}
			_, _ = w.Write([]byte(`{"candidates":[]}`))
		}))
		defer mockGCP.Close()

		mockGCPURL, _ := url.Parse(mockGCP.URL)
		origClient := dash.HTTPClient
		dash.HTTPClient = &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				cloned := req.Clone(req.Context())
				cloned.URL.Scheme = mockGCPURL.Scheme
				cloned.URL.Host = mockGCPURL.Host
				return http.DefaultTransport.RoundTrip(cloned)
			}),
		}
		defer func() { dash.HTTPClient = origClient }()

		// 1. Check initial status endpoint
		reqStatus := httptest.NewRequest("GET", "/admin/models/refresh/status", nil)
		rrStatus := httptest.NewRecorder()
		dash.RefreshModelsStatus(rrStatus, reqStatus)
		if rrStatus.Code != http.StatusOK {
			t.Fatalf("expected 200 OK on /admin/models/refresh/status, got %d", rrStatus.Code)
		}
		var initialStatus ModelRefreshStatus
		if err := json.Unmarshal(rrStatus.Body.Bytes(), &initialStatus); err != nil {
			t.Fatalf("failed to unmarshal ModelRefreshStatus JSON: %v", err)
		}

		// 2. Trigger POST /admin/models/refresh with Accept: application/json -> 202 Accepted
		reqRefreshJSON := httptest.NewRequest("POST", "/admin/models/refresh", nil)
		reqRefreshJSON.Header.Set("Accept", "application/json")
		rrRefreshJSON := httptest.NewRecorder()
		dash.RefreshModels(rrRefreshJSON, reqRefreshJSON)
		if rrRefreshJSON.Code != http.StatusAccepted {
			t.Fatalf("expected 202 Accepted for JSON POST /admin/models/refresh, got %d: %s", rrRefreshJSON.Code, rrRefreshJSON.Body.String())
		}
		var startedStatus ModelRefreshStatus
		if err := json.Unmarshal(rrRefreshJSON.Body.Bytes(), &startedStatus); err != nil {
			t.Fatalf("failed to decode 202 JSON response: %v", err)
		}
		if startedStatus.Phase == "" {
			t.Errorf("expected non-empty phase in 202 response, got %+v", startedStatus)
		}

		// 3. Trigger POST /admin/models/refresh as standard browser form -> 303 See Other to /admin/models?refresh=started
		reqRefreshForm := httptest.NewRequest("POST", "/admin/models/refresh", nil)
		rrRefreshForm := httptest.NewRecorder()
		dash.RefreshModels(rrRefreshForm, reqRefreshForm)
		if rrRefreshForm.Code != http.StatusSeeOther {
			t.Fatalf("expected 303 See Other for form POST /admin/models/refresh, got %d", rrRefreshForm.Code)
		}
		if loc := rrRefreshForm.Header().Get("Location"); loc != "/admin/models?refresh=started" {
			t.Errorf("expected redirect Location '/admin/models?refresh=started', got %q", loc)
		}

		// 4. Verify /admin/models renders the background progress banner markup and polling JS
		reqPage := httptest.NewRequest("GET", "/admin/models?refresh=started", nil)
		rrPage := httptest.NewRecorder()
		dash.ServeModels(rrPage, reqPage)
		if rrPage.Code != http.StatusOK {
			t.Fatalf("expected 200 OK on /admin/models?refresh=started, got %d", rrPage.Code)
		}
		pageBody := rrPage.Body.String()
		for _, expected := range []string{
			`id="model-refresh-banner"`,
			`id="model-refresh-progress"`,
			`Runs asynchronously in the background — you can safely leave or close this page at any time.`,
			`/admin/models/refresh/status`,
		} {
			if !strings.Contains(pageBody, expected) {
				t.Errorf("expected /admin/models HTML to contain %q", expected)
			}
		}

		// Wait for background refresh goroutine to finish before test server closes
		if !dash.WaitForRefresh(15 * time.Second) {
			t.Fatalf("timed out waiting for background model refresh to complete")
		}
		finalStatus := dash.GetRefreshStatus()
		if finalStatus.Phase != "completed" || finalStatus.Percent != 100 {
			t.Errorf("expected final status phase='completed' percent=100, got %+v", finalStatus)
		}
	})
}

type roundTripFunc func(req *http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestClustersModalsPollingAndFullModificationWorkflow(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")
	t.Setenv("LOCAL_DATA_DIR", t.TempDir())
	ctx := context.Background()
	dbStore, err := store.NewConfigStore(ctx, "test-project")
	if err != nil {
		t.Fatalf("Failed to initialize backend store: %v", err)
	}

	dash := NewDashboardController(dbStore, "test-project", "us-central1")

	// Build a mux mirroring frontend/main.go cluster routes to test route wiring end-to-end
	mux := http.NewServeMux()
	mux.HandleFunc("/admin/clusters", dash.ServeClusters)
	mux.HandleFunc("/admin/clusters/poll", dash.PollClustersActive)
	mux.HandleFunc("/admin/clusters/active", dash.PollClustersActive)
	mux.HandleFunc("/admin/clusters/mappings", dash.ServeClusterMappingsModal)
	mux.HandleFunc("/admin/clusters/mappings-modal", dash.ServeClusterMappingsModal)
	mux.HandleFunc("/admin/clusters/node-modal", dash.ServeClusterNodeModal)
	mux.HandleFunc("/admin/clusters/nodes/save", dash.SaveClusterNode)
	mux.HandleFunc("/admin/clusters/attach", dash.AttachRunnerToClusters)
	mux.HandleFunc("/admin/clusters/mappings/save", dash.SaveClusterMappings)
	mux.HandleFunc("/admin/clusters/edit-modal", dash.ServeClusterEditModal)
	mux.HandleFunc("/admin/clusters/create", dash.CreateProviderCluster)
	mux.HandleFunc("/admin/clusters/providers/create", dash.CreateProviderCluster)
	mux.HandleFunc("/admin/clusters/providers/delete", dash.DeleteProviderCluster)
	mux.HandleFunc("/admin/clusters/delete", dash.DeRegisterClusterNode)

	// Seed an initial LocalCluster with Nodes and a DirectEndpoint
	initialNodes := map[string]config.Node{
		"seeded-node-1": {ID: "seeded-node-1", Name: "Seeded Studio", Status: "online", MemoryAllocatedGB: 64},
	}
	localProv := config.ProviderConfig{
		ID:          config.ProviderLocal,
		DisplayName: "Local Infrastructure",
		Enabled:     true,
		Regions: map[string]config.RegionConfig{
			"local-cluster": {
				Code:   "local-cluster",
				Active: true,
				LocalConfig: &config.LocalConfig{
					Clusters: []config.LocalCluster{
						{
							ID:          "mac-studio-pool",
							Name:        "Mac Studio M3 Ultra Pool",
							Description: "Primary Apple Silicon cluster",
							QueueName:   "queue-mac-studio",
							MaxQueueAge: 90,
							Nodes:       initialNodes,
						},
					},
					DirectEndpoints: []config.DirectEndpoint{
						{
							ID:        "ep-gemma4-vllm",
							Name:      "Gemma 4 26B vLLM Node",
							ModelName: "gemma-4-26b",
							URL:       "http://10.0.1.25:8000/v1",
							Engine:    "vllm",
							Role:      "inference",
							ClusterID: "mac-studio-pool",
							Active:    true,
						},
					},
				},
			},
		},
	}
	if err := dbStore.SaveProvider(ctx, localProv); err != nil {
		t.Fatalf("failed to seed local provider: %v", err)
	}

	// Seed a registered runner node
	if err := dbStore.SaveRegisteredRunner(ctx, config.RunnerResponse{
		Node: config.Node{
			ID:                "runner-m3-ultra-01",
			Name:              "studio-ultra.local",
			Status:            "online",
			MemoryAllocatedGB: 128,
			ComputeGPUCores:   76,
			SupportedModels:   []string{"gemma-4-26b", "gemma-4-4b"},
			MaxModelSizeGB:    64,
			MaxConcurrent:     4,
			LastHeartbeat:     time.Now(),
		},
		AssignedClusters: []string{"mac-studio-pool"},
	}); err != nil {
		t.Fatalf("failed to seed registered runner: %v", err)
	}

	t.Run("Both /admin/clusters/mappings and /admin/clusters/mappings-modal Render Overlay Modal With Capacity Inputs", func(t *testing.T) {
		for _, path := range []string{
			"/admin/clusters/mappings-modal?id=runner-m3-ultra-01",
			"/admin/clusters/mappings?id=runner-m3-ultra-01",
		} {
			req := httptest.NewRequest("GET", path, nil)
			rr := httptest.NewRecorder()
			mux.ServeHTTP(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("expected 200 OK on %s, got %d: %s", path, rr.Code, rr.Body.String())
			}
			body := rr.Body.String()
			for _, expected := range []string{
				`fixed inset-0`,
				`name="node_id"`,
				`value="runner-m3-ultra-01"`,
				`name="cluster_ids"`,
				`value="mac-studio-pool"`,
				`name="max_model_size_gb"`,
				`value="64"`,
				`name="max_concurrent"`,
				`value="4"`,
			} {
				if !strings.Contains(body, expected) {
					t.Errorf("expected %s modal HTML to contain %q, got: %s", path, expected, body)
				}
			}
		}
	})

	t.Run("SaveClusterMappings Updates AssignedClusters, MaxModelSizeGB, and MaxConcurrent", func(t *testing.T) {
		form := url.Values{}
		form.Set("node_id", "runner-m3-ultra-01")
		form.Add("cluster_ids", "mac-studio-pool")
		form.Set("max_model_size_gb", "96")
		form.Set("max_concurrent", "8")

		req := httptest.NewRequest("POST", "/admin/clusters/mappings/save", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("HX-Request", "true")
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200 OK on /admin/clusters/mappings/save, got %d: %s", rr.Code, rr.Body.String())
		}

		runners, err := dbStore.GetRegisteredRunners(ctx)
		if err != nil || len(runners) != 1 {
			t.Fatalf("expected 1 runner, got %d (err=%v)", len(runners), err)
		}
		if runners[0].Node.MaxModelSizeGB != 96 {
			t.Errorf("expected MaxModelSizeGB=96, got %d", runners[0].Node.MaxModelSizeGB)
		}
		if runners[0].Node.MaxConcurrent != 8 {
			t.Errorf("expected MaxConcurrent=8, got %d", runners[0].Node.MaxConcurrent)
		}
	})

	t.Run("Both /admin/clusters/active and /admin/clusters/poll Work for Live Polling", func(t *testing.T) {
		for _, path := range []string{"/admin/clusters/active", "/admin/clusters/poll"} {
			req := httptest.NewRequest("GET", path, nil)
			rr := httptest.NewRecorder()
			mux.ServeHTTP(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("expected 200 OK on %s, got %d", path, rr.Code)
			}
			body := rr.Body.String()
			if !strings.Contains(body, "runner-m3-ultra-01") || !strings.Contains(body, "gemma-4-26b") {
				t.Errorf("expected %s to render registered runner and active models, got: %s", path, body)
			}
		}
	})

	t.Run("ServeClusterEditModal Renders Populated Overlay Modal for Pull-Queue Cluster and Direct Endpoint", func(t *testing.T) {
		// 1. Edit existing LocalCluster
		reqCl := httptest.NewRequest("GET", "/admin/clusters/edit-modal?type=pull-queue&id=mac-studio-pool", nil)
		rrCl := httptest.NewRecorder()
		mux.ServeHTTP(rrCl, reqCl)
		if rrCl.Code != http.StatusOK {
			t.Fatalf("expected 200 OK for pull-queue edit modal, got %d: %s", rrCl.Code, rrCl.Body.String())
		}
		clBody := rrCl.Body.String()
		for _, expected := range []string{
			`fixed inset-0`,
			`value="mac-studio-pool"`,
			`value="Mac Studio M3 Ultra Pool"`,
			`value="queue-mac-studio"`,
			`Primary Apple Silicon cluster`,
			`value="90"`,
		} {
			if !strings.Contains(clBody, expected) {
				t.Errorf("expected cluster edit modal to contain %q, got: %s", expected, clBody)
			}
		}

		// 2. Edit existing DirectEndpoint
		reqEp := httptest.NewRequest("GET", "/admin/clusters/edit-modal?type=direct-http&id=ep-gemma4-vllm", nil)
		rrEp := httptest.NewRecorder()
		mux.ServeHTTP(rrEp, reqEp)
		if rrEp.Code != http.StatusOK {
			t.Fatalf("expected 200 OK for direct-http edit modal, got %d: %s", rrEp.Code, rrEp.Body.String())
		}
		epBody := rrEp.Body.String()
		for _, expected := range []string{
			`fixed inset-0`,
			`value="ep-gemma4-vllm"`,
			`value="Gemma 4 26B vLLM Node"`,
			`value="gemma-4-26b"`,
			`value="http://10.0.1.25:8000/v1"`,
			`name="engine"`,
			`name="role"`,
			`name="cluster_id"`,
			`name="active"`,
		} {
			if !strings.Contains(epBody, expected) {
				t.Errorf("expected endpoint edit modal to contain %q, got: %s", expected, epBody)
			}
		}
	})

	t.Run("Update Existing LocalCluster Preserves Nodes and Updates Description and MaxQueueAge", func(t *testing.T) {
		form := url.Values{}
		form.Set("type", "pull-queue")
		form.Set("id", "mac-studio-pool")
		form.Set("name", "Mac Studio M3 Ultra Pool (Updated)")
		form.Set("queue_name", "queue-mac-studio-v2")
		form.Set("description", "Updated high-memory Apple Silicon pool")
		form.Set("max_queue_age", "120")

		req := httptest.NewRequest("POST", "/admin/clusters/create", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("HX-Request", "true")
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200 OK updating cluster, got %d: %s", rr.Code, rr.Body.String())
		}

		providers, _ := dbStore.GetAllProviders(ctx)
		clusters := extractLocalClustersForTest(providers)
		if len(clusters) != 1 {
			t.Fatalf("expected 1 cluster, got %d", len(clusters))
		}
		cl := clusters[0]
		if cl.Name != "Mac Studio M3 Ultra Pool (Updated)" || cl.QueueName != "queue-mac-studio-v2" || cl.Description != "Updated high-memory Apple Silicon pool" || cl.MaxQueueAge != 120 {
			t.Errorf("unexpected updated cluster fields: %+v", cl)
		}
		if len(cl.Nodes) < 1 || cl.Nodes["seeded-node-1"].ID != "seeded-node-1" {
			t.Errorf("expected existing Nodes to be preserved on cluster update, got: %+v", cl.Nodes)
		}
	})

	t.Run("Update Existing DirectEndpoint Updates Name, ModelName, URL, Engine, Role, ClusterID, and Active", func(t *testing.T) {
		form := url.Values{}
		form.Set("type", "direct-http")
		form.Set("id", "ep-gemma4-vllm")
		form.Set("name", "Gemma 4 31B SGLang Gateway")
		form.Set("model", "gemma-4-31b")
		form.Set("url", "http://10.0.1.99:30000/v1")
		form.Set("engine", "sglang")
		form.Set("role", "inference")
		form.Set("cluster_id", "mac-studio-pool")
		form.Set("active", "false")

		req := httptest.NewRequest("POST", "/admin/clusters/create", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("HX-Request", "true")
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200 OK updating direct endpoint, got %d: %s", rr.Code, rr.Body.String())
		}

		providers, _ := dbStore.GetAllProviders(ctx)
		eps := extractDirectEndpointsForTest(providers)
		if len(eps) != 1 {
			t.Fatalf("expected 1 endpoint, got %d", len(eps))
		}
		ep := eps[0]
		if ep.Name != "Gemma 4 31B SGLang Gateway" || ep.ModelName != "gemma-4-31b" || ep.URL != "http://10.0.1.99:30000/v1" || ep.Engine != "sglang" || ep.Role != "inference" || ep.ClusterID != "mac-studio-pool" || ep.Active != false {
			t.Errorf("unexpected updated direct endpoint fields: %+v", ep)
		}
	})

	t.Run("Compact Intent Pills and Unpacked Infrastructure Table Without Bulky KPI Cards", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/admin/clusters", nil)
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200 OK on /admin/clusters, got %d", rr.Code)
		}
		body := rr.Body.String()

		// 1. Bulky top KPI cards and meaningless summed fleet hardware totals must be eliminated
		for _, forbidden := range []string{
			"Central Queue Depth",
			"Active Computing Agents",
			"Total Pool VRAM",
			"Pool VRAM",
			"GB Unified Memory",
			"GPU Cores ·",
			"Hardware &amp; Limits",
		} {
			if strings.Contains(body, forbidden) {
				t.Errorf("expected bulky KPI card / meaningless fleet hardware total %q to be removed from /admin/clusters", forbidden)
			}
		}

		// 2. Compact Telemetry & Intent Pill strip + 6-column infrastructure table + visual model cap bar + tfoot summary
		for _, expected := range []string{
			"Central Queue: 0 Active Requests",
			"1 Online Node",
			"Live polling · 5s",
			"Node &amp; Heartbeat",
			"Compute &amp; Concurrency",
			"Memory &amp; Model Cap",
			"Supported Models",
			"Cluster Pool",
			"Actions",
			"76 GPU Cores",
			"8x Max Concurrent",
			"128 GB",
			"Cap ≤ 96 GB",
			`title="runner-m3-ultra-01"`,
			"max-w-[220px]",
			"<tfoot",
			"Fleet Total: 1 Online Node",
			"Configure",
			"Remove",
		} {
			if !strings.Contains(body, expected) {
				t.Errorf("expected /admin/clusters to contain %q, but it was missing", expected)
			}
		}
	})

	t.Run("Three Full-Width CRUDL Tables Render On /admin/clusters With Clear Architecture Subtitles and Action Buttons", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/admin/clusters", nil)
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200 OK on /admin/clusters, got %d", rr.Code)
		}
		body := rr.Body.String()

		for _, expected := range []string{
			"1. On-Premises Clusters (Pull-Queue Pools)",
			"+ Add Cluster Pool",
			`/admin/clusters/edit-modal?type=cluster`,
			"Cluster Name &amp; ID",
			"Queue Name",
			"Served Models",
			"Max Queue Age (TTL)",
			"No failover (504 on timeout)",
			"Attached Nodes",
			"2. Cluster Nodes (Managed Runners)",
			"+ Register Cluster Node",
			`/admin/clusters/node-modal`,
			"3. Custom / Direct HTTP Endpoints (Unmanaged Hardware)",
			"+ Add Direct Endpoint",
			`/admin/clusters/edit-modal?type=endpoint`,
			"Endpoint Name &amp; ID",
			"Served Model",
			"Engine &amp; Role",
			"Target Base URL",
		} {
			if !strings.Contains(body, expected) {
				t.Errorf("expected /admin/clusters 3-table CRUDL layout to contain %q", expected)
			}
		}
	})

	t.Run("ServeClusterEditModal Supports Create Mode When ID Is Empty", func(t *testing.T) {
		// 1. Create mode for On-Premises Cluster Pool
		reqCl := httptest.NewRequest("GET", "/admin/clusters/edit-modal?type=cluster", nil)
		rrCl := httptest.NewRecorder()
		mux.ServeHTTP(rrCl, reqCl)
		if rrCl.Code != http.StatusOK {
			t.Fatalf("expected 200 OK for cluster create modal, got %d: %s", rrCl.Code, rrCl.Body.String())
		}
		clBody := rrCl.Body.String()
		for _, expected := range []string{
			"Add On-Premises Cluster Pool",
			`name="id"`,
			`name="queue_name"`,
			`name="max_queue_age"`,
			`value="90"`,
		} {
			if !strings.Contains(clBody, expected) {
				t.Errorf("expected cluster create modal to contain %q, got: %s", expected, clBody)
			}
		}

		// 2. Create mode for Direct HTTP Endpoint
		reqEp := httptest.NewRequest("GET", "/admin/clusters/edit-modal?type=endpoint", nil)
		rrEp := httptest.NewRecorder()
		mux.ServeHTTP(rrEp, reqEp)
		if rrEp.Code != http.StatusOK {
			t.Fatalf("expected 200 OK for endpoint create modal, got %d: %s", rrEp.Code, rrEp.Body.String())
		}
		epBody := rrEp.Body.String()
		for _, expected := range []string{
			"Add Direct HTTP Endpoint",
			`name="id"`,
			`name="model"`,
			`name="url"`,
			`name="engine"`,
		} {
			if !strings.Contains(epBody, expected) {
				t.Errorf("expected endpoint create modal to contain %q, got: %s", expected, epBody)
			}
		}
	})

	t.Run("ServeClusterNodeModal and SaveClusterNode Support Pre-Registering and Editing Managed Runner Nodes With cURL Join Command", func(t *testing.T) {
		// 1. Open Register Cluster Node modal (create mode)
		reqNew := httptest.NewRequest("GET", "/admin/clusters/node-modal", nil)
		rrNew := httptest.NewRecorder()
		mux.ServeHTTP(rrNew, reqNew)
		if rrNew.Code != http.StatusOK {
			t.Fatalf("expected 200 OK on /admin/clusters/node-modal, got %d: %s", rrNew.Code, rrNew.Body.String())
		}
		newBody := rrNew.Body.String()
		for _, expected := range []string{
			"Register Cluster Node",
			`name="node_id"`,
			`name="name"`,
			`name="memory_allocated_gb"`,
			`name="compute_gpu_cores"`,
			`name="max_model_size_gb"`,
			`name="max_concurrent"`,
			`name="supported_models"`,
			`name="cluster_ids"`,
			`/api/v1/cluster/runners/register`,
			`copyCodeSnippet`,
		} {
			if !strings.Contains(newBody, expected) {
				t.Errorf("expected new node modal to contain %q, got: %s", expected, newBody)
			}
		}

		// 2. Submit POST /admin/clusters/nodes/save to pre-register a new node with custom models and discovered cluster
		form := url.Values{}
		form.Set("node_id", "mac-studio-m4-02")
		form.Set("name", "Mac Studio M4 Max")
		form.Set("memory_allocated_gb", "64")
		form.Set("compute_gpu_cores", "40")
		form.Set("max_model_size_gb", "48")
		form.Set("max_concurrent", "4")
		form.Set("supported_models", "gemma-4-31b, gemma-4-4b")
		form.Add("cluster_ids", "mac-studio-pool")
		form.Add("cluster_ids", "local-cluster-auto")

		reqSave := httptest.NewRequest("POST", "/admin/clusters/nodes/save", strings.NewReader(form.Encode()))
		reqSave.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		reqSave.Header.Set("HX-Request", "true")
		rrSave := httptest.NewRecorder()
		mux.ServeHTTP(rrSave, reqSave)
		if rrSave.Code != http.StatusOK {
			t.Fatalf("expected 200 OK on /admin/clusters/nodes/save, got %d: %s", rrSave.Code, rrSave.Body.String())
		}

		runners, err := dbStore.GetRegisteredRunners(ctx)
		if err != nil || len(runners) != 2 {
			t.Fatalf("expected 2 registered runners after pre-registration, got %d (err=%v)", len(runners), err)
		}

		// Verify the pre-registered node can be opened in Edit mode, shows dynamic cURL supported_models, and renders discovered cluster checkbox
		reqEdit := httptest.NewRequest("GET", "/admin/clusters/node-modal?id=mac-studio-m4-02", nil)
		rrEdit := httptest.NewRecorder()
		mux.ServeHTTP(rrEdit, reqEdit)
		if rrEdit.Code != http.StatusOK {
			t.Fatalf("expected 200 OK editing mac-studio-m4-02, got %d: %s", rrEdit.Code, rrEdit.Body.String())
		}
		editBody := rrEdit.Body.String()
		for _, expected := range []string{
			`value="mac-studio-m4-02"`,
			`value="Mac Studio M4 Max"`,
			`value="64"`,
			`value="40"`,
			`value="48"`,
			`gemma-4-31b, gemma-4-4b`,
			`&#34;supported_models&#34;:[&#34;gemma-4-31b&#34;,&#34;gemma-4-4b&#34;]`,
			`value="local-cluster-auto" checked`,
		} {
			if !strings.Contains(editBody, expected) {
				t.Errorf("expected edit node modal to contain %q, got: %s", expected, editBody)
			}
		}

		// 3. Submit POST /admin/clusters/nodes/save with NO cluster_ids to detach all clusters
		formDetach := url.Values{}
		formDetach.Set("node_id", "mac-studio-m4-02")
		formDetach.Set("name", "Mac Studio M4 Max")
		formDetach.Set("memory_allocated_gb", "64")
		formDetach.Set("compute_gpu_cores", "40")
		formDetach.Set("max_model_size_gb", "48")
		formDetach.Set("max_concurrent", "4")
		formDetach.Set("supported_models", "gemma-4-31b, gemma-4-4b")

		reqDetach := httptest.NewRequest("POST", "/admin/clusters/nodes/save", strings.NewReader(formDetach.Encode()))
		reqDetach.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		reqDetach.Header.Set("HX-Request", "true")
		rrDetach := httptest.NewRecorder()
		mux.ServeHTTP(rrDetach, reqDetach)
		if rrDetach.Code != http.StatusOK {
			t.Fatalf("expected 200 OK detaching all clusters, got %d", rrDetach.Code)
		}

		runnersAfterDetach, _ := dbStore.GetRegisteredRunners(ctx)
		for _, r := range runnersAfterDetach {
			if r.Node.ID == "mac-studio-m4-02" && len(r.AssignedClusters) != 0 {
				t.Errorf("expected AssignedClusters to be empty after detaching all clusters, got %v", r.AssignedClusters)
			}
		}
	})

	t.Run("DeleteProviderCluster Scrubs Deleted Cluster ID From Registered Runners AssignedClusters", func(t *testing.T) {
		reqDel := httptest.NewRequest("DELETE", "/admin/clusters/providers/delete?type=cluster&id=mac-studio-pool", nil)
		reqDel.Header.Set("HX-Request", "true")
		rrDel := httptest.NewRecorder()
		mux.ServeHTTP(rrDel, reqDel)
		if rrDel.Code != http.StatusOK {
			t.Fatalf("expected 200 OK deleting cluster, got %d: %s", rrDel.Code, rrDel.Body.String())
		}

		runners, err := dbStore.GetRegisteredRunners(ctx)
		if err != nil {
			t.Fatalf("failed to fetch runners after cluster delete: %v", err)
		}
		for _, r := range runners {
			for _, cid := range r.AssignedClusters {
				if cid == "mac-studio-pool" {
					t.Errorf("expected deleted cluster 'mac-studio-pool' to be scrubbed from runner %s AssignedClusters, got %v", r.Node.ID, r.AssignedClusters)
				}
			}
		}
	})

	t.Run("CreateProviderCluster Persists User-Specified FallbackModel And FallbackLocation And Rejects Legacy Models", func(t *testing.T) {
		// 1. Reject legacy/invalid fallback model (e.g. gemini-1.5-flash)
		badForm := url.Values{}
		badForm.Set("type", "cluster")
		badForm.Set("id", "cluster-fallback-test")
		badForm.Set("name", "Cluster Fallback Test")
		badForm.Set("queue_name", "fb-queue")
		badForm.Set("max_queue_age", "60")
		badForm.Set("fallback_model", "gemini-1.5-flash")
		badForm.Set("fallback_location", "us-central1")

		reqBad := httptest.NewRequest("POST", "/admin/clusters/providers/create", strings.NewReader(badForm.Encode()))
		reqBad.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rrBad := httptest.NewRecorder()
		mux.ServeHTTP(rrBad, reqBad)
		if rrBad.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 Bad Request for legacy fallback_model gemini-1.5-flash, got %d", rrBad.Code)
		}

		// 2. When a cluster has NO attached nodes / main models, do NOT show the fallback badge in the table
		emptyClusterForm := url.Values{}
		emptyClusterForm.Set("type", "cluster")
		emptyClusterForm.Set("id", "cluster-fallback-test")
		emptyClusterForm.Set("name", "Cluster Fallback Test")
		emptyClusterForm.Set("queue_name", "fb-queue")
		emptyClusterForm.Set("max_queue_age", "60")
		emptyClusterForm.Set("fallback_model", "gemini-3.8-flash")
		emptyClusterForm.Set("fallback_location", "global")

		reqEmpty := httptest.NewRequest("POST", "/admin/clusters/providers/create", strings.NewReader(emptyClusterForm.Encode()))
		reqEmpty.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		reqEmpty.Header.Set("HX-Request", "true")
		rrEmpty := httptest.NewRecorder()
		mux.ServeHTTP(rrEmpty, reqEmpty)
		if rrEmpty.Code != http.StatusOK {
			t.Fatalf("expected 200 OK saving cluster without nodes, got %d: %s", rrEmpty.Code, rrEmpty.Body.String())
		}
		if strings.Contains(rrEmpty.Body.String(), "Failover → gemini-3.8-flash") {
			t.Errorf("expected fallback badge NOT to render when cluster has no main served models listed, got: %s", rrEmpty.Body.String())
		}

		// 3. Attach a node with main models (runner-m3-ultra-01 serving gemma-4-26b) and verify both main model and Failover badge render
		goodForm := url.Values{}
		goodForm.Set("type", "cluster")
		goodForm.Set("id", "cluster-fallback-test")
		goodForm.Set("name", "Cluster Fallback Test")
		goodForm.Set("queue_name", "fb-queue")
		goodForm.Set("max_queue_age", "60")
		goodForm.Set("fallback_model", "gemini-3.8-flash")
		goodForm.Set("fallback_location", "global")
		goodForm.Set("node_selection_present", "1")
		goodForm.Add("node_ids", "runner-m3-ultra-01")

		reqGood := httptest.NewRequest("POST", "/admin/clusters/providers/create", strings.NewReader(goodForm.Encode()))
		reqGood.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		reqGood.Header.Set("HX-Request", "true")
		rrGood := httptest.NewRecorder()
		mux.ServeHTTP(rrGood, reqGood)
		if rrGood.Code != http.StatusOK {
			t.Fatalf("expected 200 OK saving cluster with fallback_model, got %d: %s", rrGood.Code, rrGood.Body.String())
		}
		if !strings.Contains(rrGood.Body.String(), "Failover → gemini-3.8-flash (global)") {
			t.Errorf("expected rendered clusters tab to display 'Failover → gemini-3.8-flash (global)' when main models are present, got: %s", rrGood.Body.String())
		}

		// Verify edit modal pre-populates fallback_model and fallback_location
		reqModal := httptest.NewRequest("GET", "/admin/clusters/edit-modal?type=cluster&id=cluster-fallback-test", nil)
		rrModal := httptest.NewRecorder()
		mux.ServeHTTP(rrModal, reqModal)
		if !strings.Contains(rrModal.Body.String(), `value="gemini-3.8-flash"`) || !strings.Contains(rrModal.Body.String(), `value="global"`) {
			t.Errorf("expected cluster edit modal to contain fallback_model and fallback_location values, got: %s", rrModal.Body.String())
		}
	})

	t.Run("ClusterEditModal Lists Existing Nodes and CreateProviderCluster Attaches and Detaches Selected Nodes", func(t *testing.T) {
		// Seed a runner assigned to an auto-discovered cluster pool "gke-cpu-spot-pool"
		if err := dbStore.SaveRegisteredRunner(ctx, config.RunnerResponse{
			Node: config.Node{
				ID:                "gke-node-spot-01",
				Name:              "GKE CPU Spot Node 01",
				Status:            "online",
				LastHeartbeat:     time.Now(),
				MemoryAllocatedGB: 32,
				ComputeGPUCores:   16,
				SupportedModels:   []string{"gemma-4-26b"},
				MaxModelSizeGB:    24,
				MaxConcurrent:     4,
			},
			AssignedClusters: []string{"gke-cpu-spot-pool"},
		}); err != nil {
			t.Fatalf("failed to seed gke-node-spot-01: %v", err)
		}

		// 1. Verify /admin/clusters renders the 3-step visual workflow banner and auto-discovered "gke-cpu-spot-pool" in Table 1
		reqPage := httptest.NewRequest("GET", "/admin/clusters", nil)
		rrPage := httptest.NewRecorder()
		mux.ServeHTTP(rrPage, reqPage)
		if rrPage.Code != http.StatusOK {
			t.Fatalf("expected 200 OK on /admin/clusters, got %d", rrPage.Code)
		}
		pageBody := rrPage.Body.String()
		for _, expected := range []string{
			"1. Cluster Pool (Job Queue &amp; Policy)",
			"2. Cluster Nodes (Worker Daemons)",
			"3. Direct HTTP Endpoints (Push Servers)",
			"gke-cpu-spot-pool",
			"+ Attach / Manage Nodes",
		} {
			if !strings.Contains(pageBody, expected) {
				t.Errorf("expected /admin/clusters to contain %q", expected)
			}
		}

		// 2. Verify GET /admin/clusters/edit-modal?type=cluster lists existing nodes with checkboxes
		reqNewModal := httptest.NewRequest("GET", "/admin/clusters/edit-modal?type=cluster", nil)
		rrNewModal := httptest.NewRecorder()
		mux.ServeHTTP(rrNewModal, reqNewModal)
		if rrNewModal.Code != http.StatusOK {
			t.Fatalf("expected 200 OK for new cluster modal, got %d", rrNewModal.Code)
		}
		newModalBody := rrNewModal.Body.String()
		for _, expected := range []string{
			"Cluster Pool ID (Routing Target Location)",
			`name="node_selection_present"`,
			`name="node_ids"`,
			`value="gke-node-spot-01"`,
			"GKE CPU Spot Node 01",
		} {
			if !strings.Contains(newModalBody, expected) {
				t.Errorf("expected new cluster edit modal to contain %q, got: %s", expected, newModalBody)
			}
		}

		// 3. Create a new cluster pool and attach gke-node-spot-01 directly from the Cluster Pool modal
		createForm := url.Values{}
		createForm.Set("type", "cluster")
		createForm.Set("id", "gpu-pool-alpha")
		createForm.Set("name", "GPU Pool Alpha")
		createForm.Set("max_queue_age", "120")
		createForm.Set("fallback_model", "gemini-3.8-flash")
		createForm.Set("node_selection_present", "1")
		createForm.Add("node_ids", "gke-node-spot-01")

		reqCreate := httptest.NewRequest("POST", "/admin/clusters/providers/create", strings.NewReader(createForm.Encode()))
		reqCreate.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		reqCreate.Header.Set("HX-Request", "true")
		rrCreate := httptest.NewRecorder()
		mux.ServeHTTP(rrCreate, reqCreate)
		if rrCreate.Code != http.StatusOK {
			t.Fatalf("expected 200 OK creating gpu-pool-alpha with node_ids, got %d: %s", rrCreate.Code, rrCreate.Body.String())
		}

		// Verify gke-node-spot-01 is attached to gpu-pool-alpha in both RegisteredRunners and LocalCluster.Nodes
		runners, _ := dbStore.GetRegisteredRunners(ctx)
		var foundAttached bool
		for _, r := range runners {
			if r.Node.ID == "gke-node-spot-01" {
				for _, cid := range r.AssignedClusters {
					if cid == "gpu-pool-alpha" {
						foundAttached = true
					}
				}
			}
		}
		if !foundAttached {
			t.Errorf("expected gke-node-spot-01 AssignedClusters to contain gpu-pool-alpha")
		}

		providers, _ := dbStore.GetAllProviders(ctx)
		var foundNodeInCluster bool
		for _, cl := range extractLocalClustersForTest(providers) {
			if cl.ID == "gpu-pool-alpha" {
				if _, ok := cl.Nodes["gke-node-spot-01"]; ok {
					foundNodeInCluster = true
				}
			}
		}
		if !foundNodeInCluster {
			t.Errorf("expected gpu-pool-alpha LocalCluster.Nodes to contain gke-node-spot-01")
		}

		// 4. Uncheck gke-node-spot-01 when editing gpu-pool-alpha and verify detachment
		detachForm := url.Values{}
		detachForm.Set("type", "cluster")
		detachForm.Set("id", "gpu-pool-alpha")
		detachForm.Set("name", "GPU Pool Alpha")
		detachForm.Set("max_queue_age", "120")
		detachForm.Set("fallback_model", "gemini-3.8-flash")
		detachForm.Set("node_selection_present", "1")

		reqDetach := httptest.NewRequest("POST", "/admin/clusters/providers/create", strings.NewReader(detachForm.Encode()))
		reqDetach.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		reqDetach.Header.Set("HX-Request", "true")
		rrDetach := httptest.NewRecorder()
		mux.ServeHTTP(rrDetach, reqDetach)
		if rrDetach.Code != http.StatusOK {
			t.Fatalf("expected 200 OK detaching nodes from gpu-pool-alpha, got %d", rrDetach.Code)
		}

		runnersAfter, _ := dbStore.GetRegisteredRunners(ctx)
		for _, r := range runnersAfter {
			if r.Node.ID == "gke-node-spot-01" {
				for _, cid := range r.AssignedClusters {
					if cid == "gpu-pool-alpha" {
						t.Errorf("expected gpu-pool-alpha to be detached from gke-node-spot-01, got AssignedClusters=%v", r.AssignedClusters)
					}
				}
			}
		}
	})
}

func extractLocalClustersForTest(providers []config.ProviderConfig) []config.LocalCluster {
	var out []config.LocalCluster
	for _, p := range providers {
		if p.ID == config.ProviderLocal {
			for _, r := range p.Regions {
				if r.LocalConfig != nil {
					out = append(out, r.LocalConfig.Clusters...)
				}
			}
		}
	}
	return out
}

func extractDirectEndpointsForTest(providers []config.ProviderConfig) []config.DirectEndpoint {
	var out []config.DirectEndpoint
	for _, p := range providers {
		if p.ID == config.ProviderLocal {
			for _, r := range p.Regions {
				if r.LocalConfig != nil {
					out = append(out, r.LocalConfig.DirectEndpoints...)
				}
			}
		}
	}
	return out
}

func TestDashboardUnnecessaryPillsRemovedAcrossAllPages(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")
	t.Setenv("LOCAL_DATA_DIR", t.TempDir())

	ctx := context.Background()
	dbStore, err := store.NewConfigStore(ctx, "test-project")
	if err != nil {
		t.Fatalf("Failed to initialize backend store: %v", err)
	}

	// Seed a custom header (both Optional and Required) and a routing rule
	_ = dbStore.SaveHeader(ctx, config.CustomHeader{
		ID:          "hdr-opt-1",
		Name:        "X-Optional-Trace",
		Description: "Optional tracing header",
		Required:    false,
		AppID:       "",
	})
	_ = dbStore.SaveHeader(ctx, config.CustomHeader{
		ID:          "hdr-req-1",
		Name:        "X-Required-Tenant",
		Description: "Required tenant header",
		Required:    true,
		AppID:       "app-ui-1",
	})
	_ = dbStore.SaveRule(ctx, config.RoutingRule{
		ID:             "rule-pill-test",
		PriorityWeight: 1,
		ClientTier:     "premium",
		AppID:          "",
		ComplexityTier: "complex",
		DecisionEngine: "contrastive-lm",
		MinConfidence:  0.85,
		TaskKind:       "coding",
		TargetModel:    "gemini-3.8-flash",
		FallbackModel:  "gemini-3.5-flash",
		TargetLocation: "us-central1",
	})

	dash := NewDashboardController(dbStore, "test-project", "us-central1")

	routes := []struct {
		name           string
		path           string
		handler        http.HandlerFunc
		forbiddenPills []string
		expectedIntact []string
	}{
		{
			name:    "Apps Page (/admin/apps)",
			path:    "/admin/apps",
			handler: dash.ServeApps,
			forbiddenPills: []string{
				">Live HTMX</span>",
				`rounded-md text-xs font-medium bg-green-100 text-green-800">/v1/chat/completions</span>`,
			},
			expectedIntact: []string{
				"Applications",
			},
		},
		{
			name:    "Keys Page (/admin/keys)",
			path:    "/admin/keys",
			handler: dash.ServeKeys,
			forbiddenPills: []string{
				`bg-blue-100 text-blue-800">Header</span>`,
				`bg-purple-100 text-purple-800">Query</span>`,
				`bg-emerald-100 text-emerald-800">Bearer</span>`,
				`bg-indigo-100 text-indigo-800">OIDC JWT</span>`,
			},
			expectedIntact: []string{
				"Header:",
				"Query:",
				"Bearer:",
				"OIDC JWT:",
			},
		},
		{
			name:    "Headers Page (/admin/headers)",
			path:    "/admin/headers",
			handler: dash.ServeHeaders,
			forbiddenPills: []string{
				`bg-green-100 px-2.5 py-0.5 text-xs font-semibold text-green-800`,
				`No (Optional)`,
			},
			expectedIntact: []string{
				"Global",
				"Optional",
				"Yes",
			},
		},
		{
			name:    "Rules Page (/admin/rules)",
			path:    "/admin/rules",
			handler: dash.ServeRules,
			forbiddenPills: []string{
				`bg-purple-100 px-2.5 py-0.5 text-xs font-semibold text-purple-800`,
				`ring-indigo-600/20`,
				`ring-purple-600/20`,
				`ring-amber-600/20`,
				`ring-emerald-600/20`,
			},
			expectedIntact: []string{
				"tier:complex",
				"engine:contrastive-lm",
				"task:coding",
			},
		},
		{
			name:    "Clients Page (/admin/clients)",
			path:    "/admin/clients",
			handler: dash.ServeClients,
			forbiddenPills: []string{
				">Live HTMX</span>",
				`rounded-md text-xs font-medium bg-green-100 text-green-800">/v1/chat/completions</span>`,
			},
			expectedIntact: []string{
				"Clients",
			},
		},
		{
			name:    "Complexity Page (/admin/complexity)",
			path:    "/admin/complexity",
			handler: dash.ServeComplexity,
			forbiddenPills: []string{
				`>Cluster &amp; Provider Registry</span>`,
				`>.env → Secret Manager</span>`,
				`>Hot-Swap Vault</span>`,
				`>Live Classifier</span>`,
				`rounded-full bg-indigo-50 px-2 py-0.5 text-[10px] font-semibold text-indigo-700 ring-1 ring-inset ring-indigo-600/20">Cluster Runner</span>`,
				`rounded-full bg-blue-50 px-2 py-0.5 text-[10px] font-semibold text-blue-700 ring-1 ring-inset ring-blue-600/20">Remote API</span>`,
				`rounded-full bg-emerald-50 px-2 py-0.5 text-[10px] font-semibold text-emerald-700 ring-1 ring-inset ring-emerald-600/20">Local Embedded</span>`,
			},
			expectedIntact: []string{
				"Decision Engine Providers &amp; Cluster Setup",
				"Smart Routing Inspector",
			},
		},
		{
			name:    "Models Page (/admin/models)",
			path:    "/admin/models",
			handler: dash.ServeModels,
			forbiddenPills: []string{
				`border-amber-200 animate-pulse">Obsolete</span>`,
				`bg-gray-100 px-2 py-0.5 text-xs font-semibold text-gray-700">Foundation</span>`,
				`bg-amber-50 px-2 py-0.5 text-xs font-semibold text-amber-700 ring-1 ring-inset ring-amber-600/10">Tuned weights</span>`,
				`bg-emerald-50 px-2 py-0.5 text-xs font-semibold text-emerald-700 ring-1 ring-inset ring-emerald-600/15">Local Node</span>`,
				`bg-blue-50 px-2 py-0.5 text-xs font-semibold text-blue-700 ring-1 ring-inset ring-blue-700/10">Endpoint</span>`,
			},
			expectedIntact: []string{
				"Smart Router Models Registry",
			},
		},
		{
			name:    "Clusters Page (/admin/clusters)",
			path:    "/admin/clusters",
			handler: dash.ServeClusters,
			forbiddenPills: []string{
				`Pull-Queue &amp; Direct Inference Mesh`,
			},
			expectedIntact: []string{
				"Local Clusters &amp; Agents",
			},
		},
		{
			name:    "Queue Page (/admin/queue)",
			path:    "/admin/queue",
			handler: dash.ServeQueue,
			forbiddenPills: []string{
				`>Load Pending</span>`,
				`>Healthy / Idle</span>`,
			},
			expectedIntact: []string{
				"Active Request Routing Queue",
			},
		},
		{
			name:    "Metrics Page (/admin/metrics)",
			path:    "/admin/metrics",
			handler: dash.ServeMetrics,
			forbiddenPills: []string{
				`>Live Connection</span>`,
				`>Bar Chart</span>`,
			},
			expectedIntact: []string{
				"System Telemetry Monitoring",
				"Proxy Throughput Volume",
			},
		},
		{
			name:    "Costs Page (/admin/costs)",
			path:    "/admin/costs",
			handler: dash.ServeCosts,
			forbiddenPills: []string{
				`>Donut Chart</span>`,
				`>Relative Share</span>`,
			},
			expectedIntact: []string{
				"Spend by Model Type",
				"Spend by Client Application",
			},
		},
	}

	for _, tc := range routes {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", tc.path, nil)
			rr := httptest.NewRecorder()
			tc.handler(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("expected 200 OK on %s, got %d", tc.path, rr.Code)
			}
			body := rr.Body.String()
			for _, forbidden := range tc.forbiddenPills {
				if strings.Contains(body, forbidden) {
					t.Errorf("%s still contains unnecessary pill %q", tc.name, forbidden)
				}
			}
			for _, expected := range tc.expectedIntact {
				if !strings.Contains(body, expected) {
					t.Errorf("expected %s to preserve %q, but it was missing", tc.name, expected)
				}
			}
		})
	}
}

func TestTDD_MetricsCalculationAndOutlierResilience(t *testing.T) {
	t.Run("True 24h P95 and Hourly P50/P95 in fetchLocalLogsMetrics", func(t *testing.T) {
		tempDir := t.TempDir()
		t.Setenv("LOCAL_DEV", "true")
		t.Setenv("LOCAL_DATA_DIR", tempDir)

		now := time.Now().UTC()
		hourA := now.Add(-2 * time.Hour).Format(time.RFC3339)
		hourB := now.Add(-1 * time.Hour).Format(time.RFC3339)

		var lines []string
		// 99 fast requests (200ms) in hourA
		for i := 0; i < 99; i++ {
			lines = append(lines, fmt.Sprintf(`{"time":%q,"client_id":"c1","app_id":"a1","path":"/v1/models/gemini-2.5-flash:generateContent","model_routed":"gemini-2.5-flash","status":200,"latency_ms":200}`, hourA))
		}
		// 1 outlier timeout request (15000ms) in hourB
		lines = append(lines, fmt.Sprintf(`{"time":%q,"client_id":"c1","app_id":"a1","path":"/v1/models/gemini-2.5-flash:generateContent","model_routed":"gemini-2.5-flash","status":504,"latency_ms":15000}`, hourB))

		logFile := tempDir + "/local_logs.jsonl"
		if err := os.WriteFile(logFile, []byte(strings.Join(lines, "\n")+"\n"), 0644); err != nil {
			t.Fatalf("failed to write local_logs.jsonl: %v", err)
		}

		dc := &DashboardController{ProjectID: "test-proj", Location: "us-central1"}
		vm, err := dc.fetchLocalLogsMetrics()
		if err != nil {
			t.Fatalf("fetchLocalLogsMetrics returned error: %v", err)
		}

		if vm.TotalRequests != 100 {
			t.Fatalf("expected 100 TotalRequests, got %d", vm.TotalRequests)
		}
		if math.Abs(vm.ErrorRate-1.0) > 0.01 {
			t.Fatalf("expected 1.0%% ErrorRate, got %f", vm.ErrorRate)
		}
		// Across 100 requests (99 at 200ms, 1 at 15000ms), true 24h P95 is 200ms (index 94), NOT (200+15000)/2 = 7600ms!
		if vm.P95LatencyMs != 200 {
			t.Errorf("expected true 24h P95LatencyMs to be 200ms (resilient to single-request hour outlier), got %d", vm.P95LatencyMs)
		}
	})

	t.Run("parseGCPMetrics No 0.4 Mock Multiplier and 4xx/5xx Classification", func(t *testing.T) {
		dc := &DashboardController{ProjectID: "test-proj", Location: "us-central1"}
		now := time.Now().UTC()
		t1 := now.Add(-2 * time.Hour).Format(time.RFC3339)
		t2 := now.Add(-1 * time.Hour).Format(time.RFC3339)

		volJSON := fmt.Sprintf(`{
			"timeSeries": [
				{
					"metric": {"labels": {"response_code_class": "2xx"}},
					"points": [
						{"interval": {"endTime": %q}, "value": {"int64Value": "100"}}
					]
				},
				{
					"metric": {"labels": {"response_code_class": "4xx"}},
					"points": [
						{"interval": {"endTime": %q}, "value": {"int64Value": "5"}}
					]
				},
				{
					"metric": {"labels": {"response_code_class": "5xx"}},
					"points": [
						{"interval": {"endTime": %q}, "value": {"int64Value": "1"}}
					]
				}
			]
		}`, t1, t1, t2)

		// Include two series in hour t1 (high-volume 2xx at 250ms) and 1 request in t2 at 10000ms
		latJSON := fmt.Sprintf(`{
			"timeSeries": [
				{
					"metric": {"labels": {"response_code_class": "2xx"}},
					"points": [
						{"interval": {"endTime": %q}, "value": {"doubleValue": 250.0}}
					]
				},
				{
					"metric": {"labels": {"response_code_class": "5xx"}},
					"points": [
						{"interval": {"endTime": %q}, "value": {"doubleValue": 10000.0}}
					]
				}
			]
		}`, t1, t2)

		vm, err := dc.parseGCPMetrics([]byte(volJSON), []byte(latJSON))
		if err != nil {
			t.Fatalf("parseGCPMetrics failed: %v", err)
		}
		if vm.TotalRequests != 106 {
			t.Errorf("expected 106 total requests, got %d", vm.TotalRequests)
		}
		expectedErrRate := (6.0 / 106.0) * 100.0
		if math.Abs(vm.ErrorRate-expectedErrRate) > 0.05 {
			t.Errorf("expected error rate %.2f%% (6/106), got %.2f%%", expectedErrRate, vm.ErrorRate)
		}
		// Ensure no synthetic 0.4 multiplier is present in LatencyChartSVG (250 * 0.4 = 100)
		if strings.Contains(vm.LatencyChartSVG, "Median: 100 ms") {
			t.Errorf("LatencyChartSVG appears to use synthetic 0.4 multiplier (250 * 0.4 = 100)")
		}
		// Volume-weighted P95 across 105 requests at 250ms and 1 request at 10000ms should be ~342ms, not unweighted (250+10000)/2 = 5125ms
		if vm.P95LatencyMs > 1000 {
			t.Errorf("expected volume-weighted P95LatencyMs <= 1000ms, got %d", vm.P95LatencyMs)
		}
	})

	t.Run("resolveBackendServiceName and Production Cost Latency Aggregation", func(t *testing.T) {
		// Ensure local_logs.jsonl is not used
		t.Setenv("LOCAL_DEV", "false")
		t.Setenv("LOCAL_DATA_DIR", t.TempDir())
		t.Setenv("BACKEND_SERVICE_NAME", "")
		t.Setenv("K_SERVICE", "gemini-smart-router-ui")

		dc := &DashboardController{ProjectID: "test-proj", Location: "us-central1"}
		if got := dc.resolveBackendServiceName(); got != "gemini-smart-router" {
			t.Errorf("expected resolveBackendServiceName to strip '-ui' from K_SERVICE and return 'gemini-smart-router', got %q", got)
		}

		t.Setenv("BACKEND_SERVICE_NAME", "custom-router-backend")
		if got := dc.resolveBackendServiceName(); got != "custom-router-backend" {
			t.Errorf("expected resolveBackendServiceName to honor BACKEND_SERVICE_NAME, got %q", got)
		}

		nowStr := time.Now().UTC().Format(time.RFC3339)
		gcpCostLogs := fmt.Sprintf(`{
			"entries": [
				{
					"timestamp": %q,
					"insertId": "id-1",
					"jsonPayload": {
						"client_id": "c1",
						"app_id": "a1",
						"model_routed": "gemma-4-26b",
						"status": 200,
						"latency_ms": 420,
						"bytes_sent": 1200
					}
				},
				{
					"timestamp": %q,
					"insertId": "id-2",
					"jsonPayload": {
						"client_id": "c1",
						"app_id": "a1",
						"model_routed": "gemma-4-26b",
						"status": 200,
						"latency_ms": 580,
						"bytes_sent": 1500
					}
				}
			]
		}`, nowStr, nowStr)

		costsVM, err := dc.parseGCPCosts([]byte(gcpCostLogs))
		if err != nil {
			t.Fatalf("parseGCPCosts failed: %v", err)
		}
		if len(costsVM.RecentSessions) != 2 {
			t.Fatalf("expected 2 recent sessions from parseGCPCosts, got %d", len(costsVM.RecentSessions))
		}
		if costsVM.RecentSessions[0].LatencyMs == 0 {
			t.Errorf("expected CostTransaction.LatencyMs to be populated by parseGCPCosts, got 0")
		}
	})
}




