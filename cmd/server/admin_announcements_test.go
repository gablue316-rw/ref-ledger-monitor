package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo/integration/mtest"
)

func TestAnnouncementConfiguration(t *testing.T) {
	token := strings.Repeat("x", 32)
	for _, tc := range []struct {
		name, url, token, env string
		valid, enabled        bool
	}{
		{"disabled", "", "", "production", true, false},
		{"https", "https://ledger.example.test", token, "production", true, true},
		{"loopback", "http://127.0.0.1:8080", token, "development", true, true},
		{"ipv6 loopback", "http://[::1]:8080", token, "development", true, true},
		{"partial", "", token, "production", false, false},
		{"short token", "https://ledger.example.test", "short", "production", false, false},
		{"invalid token", "https://ledger.example.test", token + "\n", "production", false, false},
		{"remote http", "http://ledger.example.test", token, "development", false, false},
		{"production http", "http://localhost", token, "production", false, false},
		{"userinfo", "https://user@" + "ledger.example.test", token, "production", false, false},
		{"path", "https://ledger.example.test/path", token, "production", false, false},
		{"query", "https://ledger.example.test/?", token, "production", false, false},
		{"fragment", "https://ledger.example.test/#", token, "production", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ANNOUNCEMENTS_ADMIN_URL", tc.url)
			t.Setenv("ANNOUNCEMENTS_ADMIN_TOKEN", tc.token)
			proxy, err := configureAnnouncementProxy(tc.env)
			if (err == nil) != tc.valid || (proxy != nil) != tc.enabled {
				t.Fatalf("proxy enabled=%v error=%v", proxy != nil, err)
			}
		})
	}
}

func announcementTestApp(t *testing.T, handler http.HandlerFunc) (*Application, string) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	token := strings.Repeat("s", 32)
	t.Setenv("ANNOUNCEMENTS_ADMIN_URL", server.URL)
	t.Setenv("ANNOUNCEMENTS_ADMIN_TOKEN", token)
	proxy, err := configureAnnouncementProxy("development")
	if err != nil {
		t.Fatal(err)
	}
	return &Application{announcements: proxy}, token
}

func announcementRequest(app *Application, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Ref-Ledger-Admin", "1")
	r = r.WithContext(context.WithValue(r.Context(), adminActorKey{}, "monitor"))
	w := httptest.NewRecorder()
	app.adminWrite(app.adminAnnouncementsHandler)(w, r)
	return w
}

func TestAnnouncementContract(t *testing.T) {
	const userID = "507f1f77bcf86cd799439011"
	var gotPath, gotQuery, gotBody string
	var response string
	var status int
	app, token := announcementTestApp(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		if r.Header.Get("Authorization") != "Bearer "+strings.Repeat("s", 32) ||
			r.Header.Get("X-Ref-Ledger-Admin-Actor") != logsThresholdActor("monitor") ||
			r.Header.Get("Cookie") != "" || r.Header.Get("X-Ref-Ledger-Logs-Version") != "" {
			t.Error("unsafe or missing upstream headers")
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, response)
	})
	status, response = 200, `{"announcements":[]}`
	w := announcementRequest(app, "GET", announcementMonitorPath, "")
	if w.Code != 200 || gotPath != announcementUpstreamPath || !strings.Contains(w.Body.String(), `"announcements":[]`) {
		t.Fatalf("list: %d %s path %s", w.Code, w.Body.String(), gotPath)
	}
	status, response = 201, `{"id":"notice-1","message":"hi","audience":"user","tenantId":"tenant-b","userId":"`+userID+`","version":1}`
	w = announcementRequest(app, "POST", announcementMonitorPath, `{"message":"hi","audience":"user","tenantId":"tenant-b","userId":"`+userID+`","username":"duplicate@example.test"}`)
	if w.Code != 201 || !strings.Contains(gotBody, `"tenantId":"tenant-b"`) || !strings.Contains(gotBody, `"userId":"`+userID+`"`) || strings.Contains(w.Body.String(), token) {
		t.Fatalf("user publish: %d %s upstream %s", w.Code, w.Body.String(), gotBody)
	}
	status, response = 200, `{"tenantId":"tenant-b","userId":"`+userID+`","username":"duplicate@example.test","sessions":[{"sessionTargetId":"web-target","sessionId":"secret-cookie","tenantId":"tenant-b","userId":"`+userID+`","username":"duplicate@example.test","platform":"web"},{"sessionTargetId":"android-target","sessionId":"secret-mobile","tenantId":"tenant-b","userId":"`+userID+`","username":"duplicate@example.test","platform":"android"}]}`
	w = announcementRequest(app, "GET", announcementMonitorPath+"/sessions?username=duplicate%40example.test", "")
	if w.Code != 200 || gotPath != announcementUpstreamPath+"/sessions" || gotQuery != "username=duplicate%40example.test" ||
		!strings.Contains(w.Body.String(), "web-target") || !strings.Contains(w.Body.String(), "android-target") || strings.Contains(w.Body.String(), "secret-") || strings.Contains(w.Body.String(), "sessionId") {
		t.Fatalf("sessions: %d %s", w.Code, w.Body.String())
	}
	for _, target := range []string{"web-target", "android-target"} {
		status, response = 200, `{"id":"notice-1","message":"hi","audience":"session","sessionTargetId":"`+target+`","version":2}`
		w = announcementRequest(app, "PUT", announcementMonitorPath+"/notice-1", `{"message":"hi","audience":"session","tenantId":"tenant-b","userId":"`+userID+`","sessionTargetId":"`+target+`","version":1}`)
		if w.Code != 200 || gotPath != announcementUpstreamPath+"/notice-1" || !strings.Contains(gotBody, `"sessionTargetId":"`+target+`"`) || !strings.Contains(gotBody, `"version":1`) {
			t.Fatalf("session update: %d %s", w.Code, gotBody)
		}
	}
	status, response = 200, `{"id":"notice-1","cleared":true,"version":3}`
	w = announcementRequest(app, "POST", announcementMonitorPath+"/notice-1/clear", `{}`)
	if w.Code != 200 || gotPath != announcementUpstreamPath+"/notice-1/clear" || !strings.Contains(w.Body.String(), `"cleared":true`) {
		t.Fatalf("clear: %d %s", w.Code, w.Body.String())
	}
	status, response = 200, `{"tenantId":"tenant-b","userId":"`+userID+`","username":"duplicate@example.test","sessions":[{"sessionTargetId":"x","tenantId":"tenant-a","userId":"`+userID+`","username":"duplicate@example.test"}]}`
	w = announcementRequest(app, "GET", announcementMonitorPath+"/sessions?username=duplicate", "")
	if w.Code != 502 {
		t.Fatalf("mismatched session must fail closed: %d", w.Code)
	}
}

func TestAnnouncementFailuresAndSecurity(t *testing.T) {
	status, response := 200, `{"announcements":[]}`
	calls := 0
	app, token := announcementTestApp(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if status == 302 {
			w.Header().Set("Location", "/redirect-target")
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, response)
	})
	for _, tc := range []struct {
		status int
		text   string
	}{{400, "Invalid announcement"}, {404, "not found"}, {409, "changed"}, {422, "Select a new session"}} {
		status, response = tc.status, `{"error":"`+token+`"}`
		w := announcementRequest(app, "GET", announcementMonitorPath, "")
		if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.text) || strings.Contains(w.Body.String(), token) {
			t.Fatalf("upstream %d: %d %s", tc.status, w.Code, w.Body.String())
		}
	}
	for _, tc := range []struct {
		status   int
		response string
	}{{401, token}, {500, token}, {302, ""}, {200, `{`}, {200, strings.Repeat("x", announcementListResponseLimit+1)}, {200, `{"announcements":[{"message":"` + token + `"}]}`}} {
		status, response = tc.status, tc.response
		before := calls
		w := announcementRequest(app, "GET", announcementMonitorPath, "")
		if w.Code != 502 || strings.Contains(w.Body.String(), token) || calls != before+1 || !strings.Contains(w.Body.String(), "Refresh before retrying") {
			t.Fatalf("unsafe upstream %d: %d %s", status, w.Code, w.Body.String())
		}
	}
	status, response = 200, strings.Repeat("x", announcementResponseLimit+1)
	w := announcementRequest(app, "GET", announcementMonitorPath+"/sessions?username=user", "")
	if w.Code != 502 {
		t.Fatalf("oversized session response: %d", w.Code)
	}
	status, response = 200, `{"announcements":[{"id":"large","message":"`+strings.Repeat("x", announcementResponseLimit)+`"}]}`
	w = announcementRequest(app, "GET", announcementMonitorPath, "")
	if w.Code != 200 {
		t.Fatalf("list over 1 MiB: %d", w.Code)
	}
	for _, tc := range []struct {
		method, path, body string
		want               int
	}{
		{"POST", announcementMonitorPath, `{"sessionId":"secret"}`, 400},
		{"POST", announcementMonitorPath, `null`, 400},
		{"POST", announcementMonitorPath, `{} {}`, 400},
		{"POST", announcementMonitorPath, `{"message":"` + strings.Repeat("x", 65536) + `"}`, 400},
		{"POST", announcementMonitorPath + "/x/clear", `{"extra":1}`, 400},
		{"GET", announcementMonitorPath + "/sessions?username=a&username=b", "", 400},
		{"GET", announcementMonitorPath + "/x", "", 405},
		{"DELETE", announcementMonitorPath + "/x", "", 405},
		{"PUT", announcementMonitorPath + "/x/other", `{}`, 404},
	} {
		before := calls
		w := announcementRequest(app, tc.method, tc.path, tc.body)
		if w.Code != tc.want || calls != before {
			t.Fatalf("%s %s: %d %s", tc.method, tc.path, w.Code, w.Body.String())
		}
	}
	w = announcementRequest(&Application{}, "GET", announcementMonitorPath, "")
	if w.Code != 503 {
		t.Fatalf("disabled: %d", w.Code)
	}
	req := httptest.NewRequest("GET", announcementMonitorPath, nil)
	w = httptest.NewRecorder()
	app.routes().ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatalf("authentication: %d", w.Code)
	}
	for _, header := range []string{"missing", "origin", "site", "content"} {
		req = httptest.NewRequest("POST", announcementMonitorPath, strings.NewReader(`{}`))
		req.Header.Set("X-Ref-Ledger-Admin", "1")
		req.Header.Set("Content-Type", "application/json")
		switch header {
		case "missing":
			req.Header.Del("X-Ref-Ledger-Admin")
		case "origin":
			req.Header.Set("Origin", "https://evil.example")
		case "site":
			req.Header.Set("Sec-Fetch-Site", "cross-site")
		case "content":
			req.Header.Set("Content-Type", "text/plain")
		}
		req = req.WithContext(context.WithValue(req.Context(), adminActorKey{}, "monitor"))
		w = httptest.NewRecorder()
		app.adminWrite(app.adminAnnouncementsHandler)(w, req)
		if w.Code != 403 && !(header == "content" && w.Code == 415) {
			t.Fatalf("%s: %d", header, w.Code)
		}
	}
}

func TestAnnouncementTimeout(t *testing.T) {
	app, _ := announcementTestApp(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
	})
	app.announcements.client.Timeout = 10 * time.Millisecond
	w := announcementRequest(app, "POST", announcementMonitorPath, `{"message":"x","audience":"system"}`)
	if w.Code != 502 || !strings.Contains(w.Body.String(), "Refresh before retrying") {
		t.Fatalf("timeout: %d %s", w.Code, w.Body.String())
	}
}

func TestAnnouncementUserSearchProjectionAndDuplicates(t *testing.T) {
	mt := mtest.New(t, mtest.NewOptions().ClientType(mtest.Mock))
	mt.Run("safe search", func(mt *mtest.T) {
		app := testApp()
		app.db = mt.DB
		id1, _ := primitive.ObjectIDFromHex("507f1f77bcf86cd799439011")
		id2, _ := primitive.ObjectIDFromHex("507f1f77bcf86cd799439012")
		mt.AddMockResponses(adminSessionMock(mt), mtest.CreateCursorResponse(0, mt.DB.Name()+".users", mtest.FirstBatch,
			bson.D{{Key: "_id", Value: id1}, {Key: "username", Value: "same@example.test"}, {Key: "tenantId", Value: "tenant-a"}, {Key: "sessionId", Value: "credential"}},
			bson.D{{Key: "_id", Value: id2}, {Key: "username", Value: "same@example.test"}, {Key: "tenantId", Value: "tenant-b"}}))
		w := httptest.NewRecorder()
		app.routes().ServeHTTP(w, adminRequest("GET", "/admin/api/admin/announcements/users?username=same&offset=0", ""))
		if w.Code != 200 || strings.Contains(w.Body.String(), "credential") || strings.Contains(w.Body.String(), "sessionId") ||
			!strings.Contains(w.Body.String(), id1.Hex()) || !strings.Contains(w.Body.String(), id2.Hex()) ||
			!strings.Contains(w.Body.String(), "tenant-a") || !strings.Contains(w.Body.String(), "tenant-b") {
			mt.Fatalf("response: %d %s", w.Code, w.Body.String())
		}
		var payload struct {
			Users []announcementUser `json:"users"`
		}
		if json.Unmarshal(w.Body.Bytes(), &payload) != nil || len(payload.Users) != 2 {
			mt.Fatal("incorrect users")
		}
		events := mt.GetAllStartedEvents()
		var found bool
		for _, e := range events {
			if e.CommandName == "find" && e.Command.Lookup("find").StringValue() == "users" {
				found = true
				projection := e.Command.Lookup("projection").Document()
				for _, field := range []string{"_id", "username", "name", "tenantId"} {
					if projection.Lookup(field).Int32() != 1 {
						mt.Fatalf("missing projection %s: %s", field, projection)
					}
				}
				elements, err := projection.Elements()
				if err != nil || len(elements) != 4 {
					mt.Fatalf("unsafe projection: %s", projection)
				}
			}
		}
		if !found {
			mt.Fatal(fmt.Sprint("user search query missing"))
		}
	})
	mt.Run("pagination", func(mt *mtest.T) {
		app := testApp()
		app.db = mt.DB
		docs := make([]bson.D, 51)
		for i := range docs {
			docs[i] = bson.D{{Key: "_id", Value: primitive.NewObjectID()}, {Key: "username", Value: "same@example.test"}, {Key: "tenantId", Value: "tenant"}}
		}
		mt.AddMockResponses(adminSessionMock(mt), mtest.CreateCursorResponse(0, mt.DB.Name()+".users", mtest.FirstBatch, docs...))
		w := httptest.NewRecorder()
		app.routes().ServeHTTP(w, adminRequest("GET", "/api/admin/announcements/users?name=a.b&offset=50", ""))
		var result struct {
			Users    []announcementUser `json:"users"`
			HasMore  bool               `json:"hasMore"`
			Offset   int                `json:"offset"`
			PageSize int                `json:"pageSize"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || len(result.Users) != 50 || !result.HasMore || result.Offset != 50 || result.PageSize != 50 {
			mt.Fatalf("invalid page: %d %s", w.Code, w.Body.String())
		}
	})
}
