package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo/integration/mtest"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func TestLogsCommandConfiguration(t *testing.T) {
	repo := &logsRepository{}
	for _, mode := range []string{"", "https"} {
		t.Setenv("LOGS_COMMAND_MODE", mode)
		commands, err := configureLogsCommands(repo, "development")
		if err != nil || commands != nil {
			t.Fatalf("default HTTPS mode: %v", err)
		}
	}
	t.Setenv("LOGS_COMMAND_MODE", "atlas")
	t.Setenv("LOGS_COMMAND_URI", "")
	if _, err := configureLogsCommands(repo, "development"); err == nil {
		t.Fatal("accepted missing command credential")
	}
	t.Setenv("LOGS_COMMAND_URI", "mongodb://127.0.0.1:27017/")
	commands, err := configureLogsCommands(repo, "development")
	if err != nil {
		t.Fatal(err)
	}
	commands.close()
	t.Setenv("LOGS_COMMAND_URI", "mongodb+srv://reader:password@example.test/")
	t.Setenv("LOGS_READER_URI", "mongodb+srv://reader:password@example.test/")
	t.Setenv("LOGS_AUDIT_URI", "mongodb+srv://auditor:password@example.test/")
	if _, err := configureLogsCommands(repo, "production"); err == nil {
		t.Fatal("accepted shared reader credential")
	}
	t.Setenv("LOGS_COMMAND_MODE", "other")
	if _, err := configureLogsCommands(repo, "development"); err == nil {
		t.Fatal("accepted invalid command mode")
	}
}

func TestLogsClearAvailabilityIsConsistent(t *testing.T) {
	mt := mtest.New(t, mtest.NewOptions().ClientType(mtest.Mock))
	mt.Run("unavailable reason and scope endpoint", func(mt *mtest.T) {
		app := &Application{logs: &logsRepository{reader: mt.Client, database: mt.DB.Name(), environment: "development", allTenants: true}}
		mt.AddMockResponses(mtest.CreateCursorResponse(0, mt.DB.Name()+".refLedgerLogs", mtest.FirstBatch))
		records := httptest.NewRecorder()
		app.logsAtlasHandler(records, httptest.NewRequest(http.MethodGet, logsAdminMonitorPrefix+"/records", nil))
		var data struct {
			ClearEnabled           bool   `json:"clearEnabled"`
			ClearUnavailableReason string `json:"clearUnavailableReason"`
		}
		if records.Code != http.StatusOK || json.Unmarshal(records.Body.Bytes(), &data) != nil || data.ClearEnabled || data.ClearUnavailableReason != logsClearUnavailable {
			mt.Fatalf("invalid availability response: %s", records.Body.String())
		}
		scope := httptest.NewRecorder()
		app.logsAtlasHandler(scope, httptest.NewRequest(http.MethodGet, logsAdminMonitorPrefix+"/clear-scope", nil))
		if scope.Code != http.StatusServiceUnavailable || !strings.Contains(scope.Body.String(), data.ClearUnavailableReason) {
			mt.Fatalf("inconsistent scope response: %s", scope.Body.String())
		}
	})
}

func TestLogsCommandSubmissionAndStatus(t *testing.T) {
	const id = "b2e39411-9629-4ebe-84b9-7c5a8171a962"
	mt := mtest.New(t, mtest.NewOptions().ClientType(mtest.Mock))
	mt.Run("command format, scope and status", func(mt *mtest.T) {
		repo := &logsRepository{database: mt.DB.Name(), environment: "development", tenants: []string{"tenant-a"}, settings: true}
		app := &Application{logs: repo, logsCommands: &logsCommandRepository{client: mt.Client}}
		call := func(method, path, body string) *httptest.ResponseRecorder {
			req := httptest.NewRequest(method, path, strings.NewReader(body))
			req = req.WithContext(context.WithValue(req.Context(), adminActorKey{}, "admin"))
			resp := httptest.NewRecorder()
			app.logsAtlasHandler(resp, req)
			return resp
		}
		path := logsAdminMonitorPrefix + "/commands"
		for _, body := range []string{
			`{"requestId":"` + id + `","environment":"production","type":"set_capture_threshold","threshold":4}`,
			`{"requestId":"` + id + `","environment":"development","type":"clear_logs","tenantId":"tenant-a","confirmation":"CLEAR development","cutoff":"2026-01-01T00:00:00Z"}`,
			`{"requestId":"` + id + `","environment":"development","type":"clear_logs","scope":"tenant","tenantId":"","confirmation":"CLEAR development","cutoff":"2026-01-01T00:00:00Z"}`,
			`{"requestId":"` + id + `","environment":"development","type":"clear_logs","scope":"tenant","tenantId":"tenant-b","confirmation":"CLEAR development","cutoff":"2026-01-01T00:00:00Z"}`,
			`{"requestId":"` + id + `","environment":"development","type":"clear_logs","scope":"tenant","tenantId":"tenant-a","confirmation":"CLEAR development","cutoff":"2999-01-01T00:00:00Z"}`,
			`{"requestId":"` + id + `","environment":"development","type":"clear_logs","scope":"tenant","tenantId":"tenant-a","confirmation":"CLEAR production","cutoff":"2026-01-01T00:00:00Z"}`,
		} {
			if got := call(http.MethodPost, path, body); got.Code != http.StatusBadRequest {
				mt.Fatalf("invalid command accepted: %s (%d)", body, got.Code)
			}
		}
		if events := mt.GetAllStartedEvents(); len(events) != 0 {
			mt.Fatal("invalid command reached database")
		}
		unauthorized := `{"requestId":"` + id + `","environment":"development","type":"clear_logs","scope":"environment","tenantId":"","confirmation":"CLEAR development","cutoff":"2026-01-01T00:00:00Z"}`
		if got := call(http.MethodPost, path, unauthorized); got.Code != http.StatusForbidden {
			mt.Fatalf("environment-wide clear returned %d, want forbidden", got.Code)
		}
		if events := mt.GetAllStartedEvents(); len(events) != 0 {
			mt.Fatal("unauthorized environment-wide clear reached database")
		}
		for _, tc := range []struct {
			body, kind, scope, tenant string
			threshold                 int32
			allTenants                bool
		}{
			{`{"requestId":"` + id + `","environment":"development","type":"set_capture_threshold","threshold":4}`, "set_capture_threshold", "environment", "", 4, false},
			{`{"requestId":"` + id + `","environment":"development","type":"clear_logs","scope":"tenant","tenantId":"tenant-a","confirmation":"CLEAR development","cutoff":"2026-01-01T00:00:00Z"}`, "clear_logs", "tenant", "tenant-a", 0, false},
			{unauthorized, "clear_logs", "environment", "", 0, true},
		} {
			mt.ClearEvents()
			repo.allTenants = tc.allTenants
			mt.AddMockResponses(mtest.CreateSuccessResponse(bson.E{Key: "n", Value: 1}))
			if got := call(http.MethodPost, path, tc.body); got.Code != http.StatusAccepted {
				mt.Fatalf("submit: %d %s", got.Code, got.Body)
			}
			events := mt.GetAllStartedEvents()
			doc := events[0].Command.Lookup("documents").Array().Index(0).Value().Document()
			for key, want := range map[string]string{"_id": id, "requestId": id, "environment": "development", "workerEnvironment": "development", "actor": logsThresholdActor("admin"), "state": "pending", "type": tc.kind, "scope": tc.scope, "tenantId": tc.tenant} {
				if got := doc.Lookup(key).StringValue(); got != want {
					mt.Errorf("%s=%q want %q", key, got, want)
				}
			}
			if doc.Lookup("version").Int32() != 1 || doc.Lookup("threshold").Int32() != tc.threshold {
				mt.Fatal("wrong version or threshold")
			}
			if tc.kind == "clear_logs" && (doc.Lookup("confirmation").StringValue() != "CLEAR development" || doc.Lookup("cutoff").Time().UTC().Format(time.RFC3339) != "2026-01-01T00:00:00Z") {
				mt.Fatal("wrong clear confirmation or cutoff")
			}
		}
		repo.allTenants = true
		for _, body := range []string{
			`{"requestId":"` + id + `","environment":"development","type":"clear_logs","scope":"tenant","tenantId":"","confirmation":"CLEAR development","cutoff":"2026-01-01T00:00:00Z"}`,
			`{"requestId":"` + id + `","environment":"development","type":"clear_logs","scope":"environment","tenantId":"tenant-a","confirmation":"CLEAR development","cutoff":"2026-01-01T00:00:00Z"}`,
			`{"requestId":"` + id + `","environment":"development","type":"clear_logs","scope":"all","confirmation":"CLEAR development","cutoff":"2026-01-01T00:00:00Z"}`,
		} {
			mt.ClearEvents()
			if got := call(http.MethodPost, path, body); got.Code != http.StatusBadRequest || len(mt.GetAllStartedEvents()) != 0 {
				mt.Fatalf("invalid all-tenant command accepted: %s (%d)", body, got.Code)
			}
		}
		mt.ClearEvents()
		mt.AddMockResponses(cursor(mt.DB.Name()+".refLedgerLogCommands", bson.D{{Key: "_id", Value: id}, {Key: "requestId", Value: id}, {Key: "state", Value: "completed"}, {Key: "outcome", Value: "success"}, {Key: "deletedCount", Value: int64(7)}}))
		result := call(http.MethodGet, path+"/"+id, "")
		if result.Code != http.StatusOK || !strings.Contains(result.Body.String(), `"deletedCount":7`) {
			mt.Fatalf("status: %d %s", result.Code, result.Body)
		}
		filter := mt.GetAllStartedEvents()[0].Command.Lookup("filter").Document()
		if filter.Lookup("actor").StringValue() != logsThresholdActor("admin") || filter.Lookup("environment").StringValue() != "development" {
			mt.Fatal("status read was not actor and environment scoped")
		}
	})
}

func TestSavedLogDetailsDecode(t *testing.T) {
	mt := mtest.New(t, mtest.NewOptions().ClientType(mtest.Mock).ClientOptions(options.Client().SetBSONOptions(&options.BSONOptions{DefaultDocumentM: true})))
	mt.Run("nested attributes", func(mt *mtest.T) {
		rows := []bson.D{
			{{Key: "_id", Value: primitive.NewObjectID()}, {Key: "timestamp", Value: time.Now()}, {Key: "attributes", Value: bson.M{"method": "POST", "statusCode": 201}}},
			{{Key: "_id", Value: primitive.NewObjectID()}, {Key: "timestamp", Value: time.Now()}, {Key: "attributes", Value: bson.M{"savedValues": bson.M{"home": "A"}}}},
			{{Key: "_id", Value: primitive.NewObjectID()}, {Key: "timestamp", Value: time.Now()}, {Key: "attributes", Value: bson.M{"changedValues": bson.M{"home": bson.M{"old": "A", "new": "B"}}}}},
			{{Key: "_id", Value: primitive.NewObjectID()}, {Key: "timestamp", Value: time.Now()}},
		}
		mt.AddMockResponses(cursor(mt.DB.Name()+".refLedgerLogs", rows...))
		repo := &logsRepository{reader: mt.Client, database: mt.DB.Name(), environment: "development", allTenants: true}
		app := &Application{logs: repo}
		resp := httptest.NewRecorder()
		app.logsAtlasHandler(resp, httptest.NewRequest(http.MethodGet, logsAdminMonitorPrefix+"/records", nil))
		var result struct {
			Items []map[string]any `json:"items"`
		}
		if resp.Code != http.StatusOK || json.Unmarshal(resp.Body.Bytes(), &result) != nil || len(result.Items) != 4 {
			mt.Fatalf("records: %d %s", resp.Code, resp.Body)
		}
		attrs := result.Items[2]["attributes"].(map[string]any)
		changes, ok := attrs["changedValues"].(map[string]any)
		if !ok {
			mt.Fatalf("changed values are not an object: %v", attrs)
		}
		change := changes["home"].(map[string]any)
		if change["old"] != "A" || change["new"] != "B" {
			mt.Fatal("historical values lost")
		}
		http := result.Items[0]["attributes"].(map[string]any)
		saved := result.Items[1]["attributes"].(map[string]any)["savedValues"].(map[string]any)
		if http["method"] != "POST" || http["statusCode"] != float64(201) || saved["home"] != "A" {
			mt.Fatal("HTTP or saved values lost")
		}
		if result.Items[3]["attributes"] != nil {
			mt.Fatal("older row gained invented details")
		}
	})
}

func TestLogsCommandRetryUsesOriginalID(t *testing.T) {
	const id = "b2e39411-9629-4ebe-84b9-7c5a8171a962"
	mt := mtest.New(t, mtest.NewOptions().ClientType(mtest.Mock))
	mt.Run("same command succeeds, different command conflicts", func(mt *mtest.T) {
		app := &Application{logs: &logsRepository{database: mt.DB.Name(), environment: "development", settings: true}, logsCommands: &logsCommandRepository{client: mt.Client}}
		existing := bson.D{{Key: "_id", Value: id}, {Key: "requestId", Value: id}, {Key: "environment", Value: "development"}, {Key: "actor", Value: logsThresholdActor("admin")}, {Key: "type", Value: "set_capture_threshold"}, {Key: "scope", Value: "environment"}, {Key: "threshold", Value: 4}}
		for _, tc := range []struct {
			level string
			want  int
		}{{"4", http.StatusAccepted}, {"3", http.StatusConflict}} {
			mt.AddMockResponses(mtest.CreateWriteErrorsResponse(mtest.WriteError{Code: 11000, Message: "duplicate"}), cursor(mt.DB.Name()+".refLedgerLogCommands", existing))
			req := httptest.NewRequest(http.MethodPost, logsAdminMonitorPrefix+"/commands", strings.NewReader(`{"requestId":"`+id+`","environment":"development","type":"set_capture_threshold","threshold":`+tc.level+`}`))
			req = req.WithContext(context.WithValue(req.Context(), adminActorKey{}, "admin"))
			resp := httptest.NewRecorder()
			app.logsAtlasHandler(resp, req)
			if resp.Code != tc.want {
				mt.Fatalf("retry returned %d: %s", resp.Code, resp.Body)
			}
		}
	})
}

func TestClearExportScopeFilters(t *testing.T) {
	cutoff := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	repo := &logsRepository{environment: "development", allTenants: true}
	all, err := repo.filter(logQuery{}, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := all["tenantId"]; ok {
		t.Fatal("environment-wide export excludes tenantless server logs")
	}
	if all["environment"] != "development" || !all["timestamp"].(bson.M)["$lte"].(time.Time).Equal(cutoff) {
		t.Fatal("environment-wide export lost environment or cutoff")
	}
	tenant, err := repo.filter(logQuery{TenantID: "tenant-a"}, cutoff)
	if err != nil || tenant["tenantId"] != "tenant-a" || tenant["environment"] != "development" ||
		!tenant["timestamp"].(bson.M)["$lte"].(time.Time).Equal(cutoff) {
		t.Fatal("tenant export lost scope or cutoff")
	}
}
