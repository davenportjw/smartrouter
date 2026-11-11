package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// LOCAL_DEV must never relax API authentication on Cloud Run (K_SERVICE set).
func TestAuthMiddlewareLocalDevIgnoredOnCloudRun(t *testing.T) {
	ac := &APIController{} // no shared secret configured
	called := false
	h := ac.AuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	t.Setenv("LOCAL_DEV", "true")
	t.Setenv("K_SERVICE", "smartrouter-backend")
	req := httptest.NewRequest("GET", "/api/apps", nil)
	req.Header.Set("X-User-Email", "admin@example.com")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized || called {
		t.Fatalf("expected 401 on Cloud Run despite LOCAL_DEV, got %d (handler called=%v)", rr.Code, called)
	}

	t.Setenv("K_SERVICE", "")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/api/apps", nil))
	if rr.Code != http.StatusOK || !called {
		t.Fatalf("expected local dev to pass off Cloud Run, got %d", rr.Code)
	}
}

func TestSecretEqualsConstantTime(t *testing.T) {
	if secretEquals("", "") || secretEquals("", "x") || secretEquals("abc", "abd") {
		t.Fatal("unexpected match")
	}
	if !secretEquals("same-value", "same-value") {
		t.Fatal("expected match")
	}
}
