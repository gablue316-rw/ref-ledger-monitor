package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBasicAuthCannotBypassAdminSession(t *testing.T) {
	app := &Application{config: Config{Username: "admin", Password: "secret"}}
	request := httptest.NewRequest("GET", "/api/about", nil)
	request.SetBasicAuth("admin", "secret")
	response := httptest.NewRecorder()
	app.requireAuth(func(http.ResponseWriter, *http.Request) { t.Fatal("Basic Auth bypassed session") })(response, request)
	if response.Code != 401 {
		t.Fatalf("got %d, want 401", response.Code)
	}
	if response.Header().Get("WWW-Authenticate") != "" {
		t.Fatal("must not prompt for Basic Auth")
	}
}

func TestUnauthenticatedAdminPageRedirect(t *testing.T) {
	app := &Application{}
	response := httptest.NewRecorder()
	app.requireAuth(func(http.ResponseWriter, *http.Request) { t.Fatal("unprotected page") })(response, httptest.NewRequest("GET", "/", nil))
	if response.Code != 303 || response.Header().Get("Location") != "/admin/login" {
		t.Fatal("missing login redirect")
	}
}

func TestLogoutExpiresCookieWithoutSession(t *testing.T) {
	app := &Application{config: Config{Environment: "production"}}
	response := httptest.NewRecorder()
	app.adminLogoutHandler(response, httptest.NewRequest("POST", "/api/admin/logout", strings.NewReader("{}")))
	if response.Code != 200 {
		t.Fatalf("got %d", response.Code)
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != adminCookie || cookies[0].MaxAge != -1 || !cookies[0].HttpOnly || !cookies[0].Secure || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatal("invalid logout cookie")
	}
}

func TestAdminCookieLifetimeAndFlags(t *testing.T) {
	app := &Application{}
	response := httptest.NewRecorder()
	app.setAdminCookie(response, httptest.NewRequest("POST", "/", nil), strings.Repeat("a", 64), time.Now().Add(adminSessionTTL))
	c := response.Result().Cookies()[0]
	if c.MaxAge != 1800 || c.Path != "/" || !c.HttpOnly || c.Secure {
		t.Fatal("invalid local cookie")
	}
}

func TestInvalidLoginDoesNotCreateSession(t *testing.T) {
	app := &Application{config: Config{Username: "admin", Password: "secret"}}
	response := httptest.NewRecorder()
	app.adminLoginHandler(response, httptest.NewRequest("POST", "/api/admin/login", strings.NewReader(`{"username":"admin","password":"wrong"}`)))
	if response.Code != 401 || len(response.Result().Cookies()) != 0 {
		t.Fatal("invalid credentials accepted")
	}
}
