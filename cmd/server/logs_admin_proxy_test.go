package main

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func captureLogsAdminProxyDiagnostic(t *testing.T, run func()) string {
	t.Helper()
	oldWriter := log.Writer()
	var output bytes.Buffer
	log.SetOutput(&output)
	defer log.SetOutput(oldWriter)
	run()
	return output.String()
}

func TestValidateLogsAdminEndpoint(t *testing.T) {
	tests := []struct {
		name, endpoint, environment string
		valid                       bool
	}{
		{"https", "https://ledger.example.test", "production", true},
		{"development loopback", "http://127.0.0.1:8080", "development", true},
		{"production http", "http://127.0.0.1:8080", "production", false},
		{"external http", "http://ledger.example.test", "development", false},
		{"path rejected", "https://ledger.example.test/logs", "production", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateLogsAdminEndpoint(test.endpoint, strings.Repeat("x", 32), test.environment)
			if (err == nil) != test.valid {
				t.Fatalf("validation error = %v, valid = %v", err, test.valid)
			}
		})
	}
	if err := validateLogsAdminEndpoint("https://ledger.example.test", "short", "production"); err == nil {
		t.Fatal("short service token was accepted")
	}
}

func TestLogsAdminThresholdHandlerLogsMonitorRejectionBeforeProxy(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, logsAdminThresholdPath+"?private-query=value", nil)
	request.AddCookie(&http.Cookie{Name: adminCookie, Value: "private-cookie"})
	response := httptest.NewRecorder()
	diagnostic := captureLogsAdminProxyDiagnostic(t, func() {
		(&Application{}).routes().ServeHTTP(response, request)
	})
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want %d", response.Code, http.StatusUnauthorized)
	}
	if !strings.Contains(diagnostic, "disposition=monitor_rejected stage=before_proxy") {
		t.Fatalf("missing pre-proxy rejection diagnostic: %s", diagnostic)
	}
	for _, privateValue := range []string{"private-query", "private-cookie"} {
		if strings.Contains(diagnostic, privateValue) {
			t.Fatalf("diagnostic leaked %q: %s", privateValue, diagnostic)
		}
	}
}

func TestLogsAdminProxyForwardsVersionedRequestAndArchive(t *testing.T) {
	const secret = "0123456789abcdef0123456789abcdef"
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != logsAdminBackendPrefix+"/export" || r.URL.Query().Get("scope") != "all" {
			t.Errorf("unexpected backend path/query: %s", r.URL.String())
		}
		if r.Header.Get("Authorization") != "Bearer "+secret ||
			r.Header.Get("X-Ref-Ledger-Logs-Version") != "1" ||
			r.Header.Get("X-Ref-Ledger-Admin-Actor") != "operator" {
			t.Error("versioned administrator credentials were not forwarded")
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"all":true}` {
			t.Errorf("unexpected request body: %s", body)
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("Content-Disposition", `attachment; filename="ref-ledger-logs.jsonl"`)
		w.Header().Set("X-Log-Export-Count", "1")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{\"schemaVersion\":1}\n"))
	}))
	defer backend.Close()

	app := &Application{config: Config{
		Username: "operator", LogsAdminURL: backend.URL, LogsAdminToken: secret,
	}}
	request := httptest.NewRequest(http.MethodPost, logsAdminMonitorPrefix+"/export?scope=all", strings.NewReader(`{"all":true}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	app.logsAdminProxy(response, request)
	if response.Code != http.StatusOK || response.Body.String() != "{\"schemaVersion\":1}\n" {
		t.Fatalf("unexpected proxy response: %d %q", response.Code, response.Body.String())
	}
	if response.Header().Get("X-Log-Export-Count") != "1" || !strings.Contains(response.Header().Get("Content-Disposition"), "jsonl") {
		t.Fatal("export metadata was not forwarded")
	}
}

func TestLogsAdminThresholdProxyLogsUpstreamResponseWithoutRequestData(t *testing.T) {
	const secret = "0123456789abcdef0123456789abcdef"
	const responseBody = "private-response-body"
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != logsAdminBackendPrefix+"/threshold" || r.URL.Query().Get("private-query") != "value" {
			t.Errorf("unexpected backend path/query: %s", r.URL.String())
		}
		_, _ = w.Write([]byte(responseBody))
	}))
	defer backend.Close()

	app := &Application{config: Config{LogsAdminURL: backend.URL, LogsAdminToken: secret}}
	request := httptest.NewRequest(http.MethodGet, logsAdminThresholdPath+"?private-query=value", nil)
	request.AddCookie(&http.Cookie{Name: adminCookie, Value: "private-cookie"})
	request.Header.Set("X-Private-Header", "private-header-secret")
	response := httptest.NewRecorder()
	diagnostic := captureLogsAdminProxyDiagnostic(t, func() {
		app.logsAdminProxy(response, request)
	})
	if response.Code != http.StatusOK || response.Body.String() != responseBody {
		t.Fatalf("unexpected response: status=%d body=%q", response.Code, response.Body.String())
	}
	for _, expected := range []string{
		"disposition=upstream_attempt",
		"upstream_host=127.0.0.1",
		"upstream_path=" + logsAdminBackendPrefix + "/threshold",
		"outcome=response status=200",
	} {
		if !strings.Contains(diagnostic, expected) {
			t.Fatalf("diagnostic %q does not contain %q", diagnostic, expected)
		}
	}
	for _, privateValue := range []string{secret, "private-query", "private-cookie", "private-header-secret", responseBody} {
		if strings.Contains(diagnostic, privateValue) {
			t.Fatalf("diagnostic leaked %q: %s", privateValue, diagnostic)
		}
	}
}

func TestLogsAdminThresholdProxyLogsFixedUpstreamFailureCategory(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	backendURL := backend.URL
	backend.Close()

	app := &Application{config: Config{
		LogsAdminURL: backendURL, LogsAdminToken: strings.Repeat("x", 32),
	}}
	request := httptest.NewRequest(http.MethodGet, logsAdminThresholdPath+"?private-query=value", nil)
	response := httptest.NewRecorder()
	diagnostic := captureLogsAdminProxyDiagnostic(t, func() {
		app.logsAdminProxy(response, request)
	})
	if response.Code != http.StatusBadGateway {
		t.Fatalf("got %d, want %d", response.Code, http.StatusBadGateway)
	}
	for _, expected := range []string{
		"disposition=upstream_attempt",
		"upstream_host=127.0.0.1",
		"upstream_path=" + logsAdminBackendPrefix + "/threshold",
		"outcome=request_failed error_category=request_failed",
	} {
		if !strings.Contains(diagnostic, expected) {
			t.Fatalf("diagnostic %q does not contain %q", diagnostic, expected)
		}
	}
	if strings.Contains(diagnostic, "private-query") || strings.Contains(diagnostic, backendURL) {
		t.Fatalf("diagnostic leaked request details: %s", diagnostic)
	}
}

func TestLogsAdminThresholdProxySanitizesRedirectDiagnostic(t *testing.T) {
	const secret = "0123456789abcdef0123456789abcdef"
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "https://redirect-user:redirect-password@redirect.example.test/private-path?redirect-query-secret=value#private-fragment")
		w.WriteHeader(http.StatusFound)
		_, _ = w.Write([]byte("private-redirect-body"))
	}))
	defer backend.Close()

	app := &Application{config: Config{LogsAdminURL: backend.URL, LogsAdminToken: secret}}
	request := httptest.NewRequest(http.MethodGet, logsAdminThresholdPath+"?private-query=value", nil)
	request.AddCookie(&http.Cookie{Name: adminCookie, Value: "private-cookie"})
	request.Header.Set("X-Private-Header", "private-header-secret")
	response := httptest.NewRecorder()
	diagnostic := captureLogsAdminProxyDiagnostic(t, func() {
		app.logsAdminProxy(response, request)
	})
	if response.Code != http.StatusBadGateway {
		t.Fatalf("got %d, want %d", response.Code, http.StatusBadGateway)
	}
	for _, expected := range []string{
		"outcome=response status=302",
		"outcome=redirect status=302 redirect_host=redirect.example.test redirect_path=/private-path",
	} {
		if !strings.Contains(diagnostic, expected) {
			t.Fatalf("diagnostic %q does not contain %q", diagnostic, expected)
		}
	}
	for _, privateValue := range []string{
		secret, "redirect-user", "redirect-password", "redirect-query-secret",
		"private-fragment", "private-query", "private-cookie", "private-header-secret", "private-redirect-body",
	} {
		if strings.Contains(diagnostic, privateValue) {
			t.Fatalf("diagnostic leaked %q: %s", privateValue, diagnostic)
		}
	}
}

func TestLogsAdminProxyRejectsMissingConfiguration(t *testing.T) {
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, logsAdminThresholdPath+"?private-query=value", nil)
	diagnostic := captureLogsAdminProxyDiagnostic(t, func() {
		(&Application{}).logsAdminProxy(response, request)
	})
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503", response.Code)
	}
	if strings.Contains(response.Body.String(), "token") {
		t.Fatal("proxy failure disclosed service credentials")
	}
	if !strings.Contains(diagnostic, "disposition=monitor_rejected stage=before_proxy reason=not_configured") {
		t.Fatalf("missing pre-proxy rejection diagnostic: %s", diagnostic)
	}
	if strings.Contains(diagnostic, "private-query") {
		t.Fatalf("diagnostic leaked request query: %s", diagnostic)
	}
}
