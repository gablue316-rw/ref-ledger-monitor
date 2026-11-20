package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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

func TestLogsAdminProxyRejectsMissingConfiguration(t *testing.T) {
	response := httptest.NewRecorder()
	(&Application{}).logsAdminProxy(response, httptest.NewRequest(http.MethodGet, logsAdminMonitorPrefix+"/threshold", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503", response.Code)
	}
	if strings.Contains(response.Body.String(), "token") {
		t.Fatal("proxy failure disclosed service credentials")
	}
}
