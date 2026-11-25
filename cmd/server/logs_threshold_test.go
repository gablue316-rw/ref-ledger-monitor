package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLogsThresholdUpdaterConfiguration(t *testing.T) {
	repo := &logsRepository{environment: "development"}
	token := strings.Repeat("x", 32)
	for _, tc := range []struct {
		name, url, token, environment string
		repo                          *logsRepository
		valid, enabled                bool
	}{
		{"disabled", "", "", "production", repo, true, false},
		{"local development", "http://127.0.0.1:8080", token, "development", repo, true, true},
		{"https", "https://logs.example.test", token, "production", repo, true, true},
		{"missing repo", "https://logs.example.test", token, "production", nil, false, false},
		{"missing token", "https://logs.example.test", "", "production", repo, false, false},
		{"short token", "https://logs.example.test", "short", "production", repo, false, false},
		{"invalid token", "https://logs.example.test", token + " bad", "production", repo, false, false},
		{"remote http", "http://logs.example.test", token, "development", repo, false, false},
		{"production http", "http://localhost:8080", token, "production", repo, false, false},
		{"userinfo", "https://user@logs.example.test", token, "production", repo, false, false},
		{"path", "https://logs.example.test/other", token, "production", repo, false, false},
		{"query", "https://logs.example.test/?x=y", token, "production", repo, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("LOGS_ADMIN_URL", tc.url)
			t.Setenv("LOGS_ADMIN_TOKEN", tc.token)
			updater, err := configureLogsThresholdUpdater(tc.repo, tc.environment)
			if (err == nil) != tc.valid || (updater != nil) != tc.enabled {
				t.Fatalf("updater=%v error=%v", updater != nil, err)
			}
		})
	}
}

func TestLogsThresholdUpstream(t *testing.T) {
	for _, tc := range []struct {
		name, environment, response string
		getStatus, putStatus        int
		wantPut, wantError          bool
	}{
		{"success", "development", `{"environment":"development","threshold":3}`, 200, 200, true, false},
		{"environment mismatch", "production", `{"environment":"development","threshold":3}`, 200, 200, false, true},
		{"invalid upstream threshold", "development", `{"environment":"development","threshold":8}`, 200, 200, false, true},
		{"invalid JSON", "development", `{`, 200, 200, false, true},
		{"get failure", "development", `{"environment":"development","threshold":3}`, 503, 200, false, true},
		{"put failure", "development", `{"environment":"development","threshold":3}`, 200, 503, true, true},
		{"redirect", "development", `{"environment":"development","threshold":3}`, 302, 200, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			put := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != refLedgerThresholdPath || r.Header.Get("Authorization") != "Bearer "+strings.Repeat("x", 32) || r.Header.Get("X-Ref-Ledger-Logs-Version") != "1" || r.Header.Get("X-Ref-Ledger-Admin-Actor") != "session-admin" {
					t.Error("incorrect upstream request")
				}
				if r.Method == http.MethodGet {
					w.WriteHeader(tc.getStatus)
					_, _ = w.Write([]byte(tc.response))
					return
				}
				put = true
				var body struct {
					Threshold int `json:"threshold"`
				}
				if r.Method != http.MethodPut || r.Header.Get("Content-Type") != "application/json" || json.NewDecoder(r.Body).Decode(&body) != nil || body.Threshold != 4 {
					t.Error("incorrect update payload")
				}
				w.WriteHeader(tc.putStatus)
			}))
			defer server.Close()
			t.Setenv("LOGS_ADMIN_URL", server.URL)
			t.Setenv("LOGS_ADMIN_TOKEN", strings.Repeat("x", 32))
			updater, err := configureLogsThresholdUpdater(&logsRepository{}, "development")
			if err != nil {
				t.Fatal(err)
			}
			err = updater.update(context.Background(), tc.environment, "session-admin", 4)
			if (err != nil) != tc.wantError || put != tc.wantPut {
				t.Fatalf("error=%v put=%v", err, put)
			}
		})
	}
}

func TestLogsThresholdUpdateValidationAndAuthorization(t *testing.T) {
	app := &Application{logs: &logsRepository{environment: "development", settings: true}, logsUpdater: &logsThresholdUpdater{}}
	for _, tc := range []struct {
		name, body string
		actor      bool
		want       int
	}{
		{"zero", `{"threshold":0,"environment":"development"}`, true, 400},
		{"too high", `{"threshold":5,"environment":"development"}`, true, 400},
		{"wrong environment", `{"threshold":3,"environment":"production"}`, true, 400},
		{"unknown field", `{"threshold":3,"environment":"development","actor":"spoof"}`, true, 400},
		{"missing actor", `{"threshold":3,"environment":"development"}`, false, 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPut, logsAdminThresholdPath, strings.NewReader(tc.body))
			if tc.actor {
				req = req.WithContext(context.WithValue(req.Context(), adminActorKey{}, "session-admin"))
			}
			resp := httptest.NewRecorder()
			app.logsAtlasHandler(resp, req)
			if resp.Code != tc.want {
				t.Fatalf("got %d, want %d", resp.Code, tc.want)
			}
		})
	}
	req := httptest.NewRequest(http.MethodPut, logsAdminThresholdPath, strings.NewReader(`{"threshold":4,"environment":"development"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Ref-Ledger-Admin", "1")
	resp := httptest.NewRecorder()
	app.routes().ServeHTTP(resp, req)
	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated update returned %d", resp.Code)
	}
}

func TestLogsThresholdUpstreamCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("cancelled request reached upstream")
	}))
	defer server.Close()
	t.Setenv("LOGS_ADMIN_URL", server.URL)
	t.Setenv("LOGS_ADMIN_TOKEN", strings.Repeat("x", 32))
	updater, err := configureLogsThresholdUpdater(&logsRepository{}, "development")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := updater.update(ctx, "development", "admin", 4); err == nil {
		t.Fatal("cancelled request succeeded")
	}
}
