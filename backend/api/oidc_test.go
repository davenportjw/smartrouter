package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"google.golang.org/api/idtoken"
)

const (
	testAudA = "https://backend-a.example.com"
	testAudB = "https://backend-b.example.com"
	testUI   = "ui-sa@project.example.com"
	testGen  = "gen-sa@project.example.com"
)

// stubValidator accepts token "tok" only for audience aud and returns the given claims.
func stubValidator(t *testing.T, aud string, claims map[string]interface{}) {
	t.Helper()
	orig := validateIDToken
	t.Cleanup(func() { validateIDToken = orig })
	validateIDToken = func(_ context.Context, token, audience string) (*idtoken.Payload, error) {
		if token != "tok" || audience != aud {
			return nil, errors.New("audience mismatch")
		}
		return &idtoken.Payload{Audience: audience, Claims: claims}, nil
	}
}

func TestParseCSV(t *testing.T) {
	got := parseCSV(" a , ,b,, c ")
	if want := []string{"a", "b", "c"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("parseCSV = %v, want %v", got, want)
	}
	if got := parseCSV(""); len(got) != 0 {
		t.Fatalf("parseCSV(\"\") = %v, want empty", got)
	}
}

func TestOIDCPolicyEnabledRequiresBothLists(t *testing.T) {
	cases := []struct {
		aud, callers string
		want         bool
	}{
		{"", "", false},
		{testAudA, "", false},
		{"", testUI, false},
		{" , ", testUI, false},
		{testAudA, testUI, true},
	}
	for _, c := range cases {
		if got := newOIDCPolicy(c.aud, c.callers).enabled(); got != c.want {
			t.Errorf("enabled(%q,%q) = %v, want %v", c.aud, c.callers, got, c.want)
		}
	}
}

func TestOIDCPolicyCheckCaller(t *testing.T) {
	p := newOIDCPolicy(testAudA, testUI+", "+testGen)
	cases := []struct {
		name   string
		claims map[string]interface{}
		want   error
	}{
		{"allowed", map[string]interface{}{"email": testUI, "email_verified": true}, nil},
		{"allowed case-insensitive", map[string]interface{}{"email": "GEN-SA@project.example.com", "email_verified": "true"}, nil},
		{"unverified", map[string]interface{}{"email": testUI, "email_verified": false}, errEmailNotVerified},
		{"missing verified", map[string]interface{}{"email": testUI}, errEmailNotVerified},
		{"not allowed", map[string]interface{}{"email": "other@example.com", "email_verified": true}, errCallerNotAllowed},
		{"missing email", map[string]interface{}{"email_verified": true}, errCallerNotAllowed},
	}
	for _, c := range cases {
		err := p.checkCaller(c.claims)
		if c.want == nil && err != nil {
			t.Errorf("%s: unexpected error %v", c.name, err)
		}
		if c.want != nil && !errors.Is(err, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, err, c.want)
		}
	}
}

func TestOIDCPolicyAuthorizeTriesEachAudience(t *testing.T) {
	stubValidator(t, testAudB, map[string]interface{}{"email": testUI, "email_verified": true})
	p := newOIDCPolicy(testAudA+","+testAudB, testUI)
	if err := p.authorize(context.Background(), "tok"); err != nil {
		t.Fatalf("expected token valid for second audience, got %v", err)
	}
	if err := newOIDCPolicy(testAudA, testUI).authorize(context.Background(), "tok"); !errors.Is(err, errOIDCAudience) {
		t.Fatalf("expected audience error, got %v", err)
	}
}

func TestOIDCPolicyAuthorizeFailsClosed(t *testing.T) {
	stubValidator(t, testAudA, map[string]interface{}{"email": testUI, "email_verified": true})
	for _, p := range []oidcPolicy{newOIDCPolicy("", testUI), newOIDCPolicy(testAudA, "")} {
		if err := p.authorize(context.Background(), "tok"); !errors.Is(err, errOIDCDisabled) {
			t.Fatalf("expected fail-closed error, got %v", err)
		}
	}
}

func TestAuthMiddlewareOIDCPinning(t *testing.T) {
	t.Setenv("LOCAL_DEV", "")
	stubValidator(t, testAudA, map[string]interface{}{"email": testUI, "email_verified": true})

	run := func(sharedSecret string, header map[string]string) int {
		ac := &APIController{SharedSecret: sharedSecret}
		h := ac.AuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		req := httptest.NewRequest("GET", "/api/apps", nil)
		for k, v := range header {
			req.Header.Set(k, v)
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr.Code
	}
	bearer := map[string]string{"Authorization": "Bearer tok"}

	// Fail closed: env unset -> OIDC rejected.
	t.Setenv(EnvOIDCAudiences, "")
	t.Setenv(EnvAllowedCallers, "")
	if code := run("", bearer); code != http.StatusUnauthorized {
		t.Fatalf("env unset: got %d, want 401", code)
	}

	// Allowed caller + audience.
	t.Setenv(EnvOIDCAudiences, testAudA)
	t.Setenv(EnvAllowedCallers, testUI)
	if code := run("", bearer); code != http.StatusOK {
		t.Fatalf("allowed caller: got %d, want 200", code)
	}

	// Caller not in allowlist.
	t.Setenv(EnvAllowedCallers, testGen)
	if code := run("", bearer); code != http.StatusUnauthorized {
		t.Fatalf("disallowed caller: got %d, want 401", code)
	}

	// Wrong audience.
	t.Setenv(EnvOIDCAudiences, testAudB)
	t.Setenv(EnvAllowedCallers, testUI)
	if code := run("", bearer); code != http.StatusUnauthorized {
		t.Fatalf("wrong audience: got %d, want 401", code)
	}

	// Shared-secret path is unchanged even with OIDC disabled.
	t.Setenv(EnvOIDCAudiences, "")
	secret := "test-" + t.Name()
	if code := run(secret, map[string]string{"X-Shared-Secret": secret}); code != http.StatusOK {
		t.Fatalf("shared secret: got %d, want 200", code)
	}
}
