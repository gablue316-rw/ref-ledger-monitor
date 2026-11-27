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
	"go.mongodb.org/mongo-driver/mongo/integration/mtest"
)

const atlasTestID = "b2e39411-9629-4ebe-84b9-7c5a8171a962"

func TestAnnouncementAtlasConfiguration(t *testing.T) {
	cfg := Config{Environment: "production", MongoURI: "mongodb+srv://main@example.test"}
	t.Setenv("ANNOUNCEMENTS_COMMAND_MODE", "")
	if repo, err := configureAnnouncementCommands(cfg); err != nil || repo != nil {
		t.Fatalf("http default: %v", err)
	}
	t.Setenv("ANNOUNCEMENTS_COMMAND_MODE", "atlas")
	t.Setenv("ANNOUNCEMENTS_COMMAND_DATABASE", "ledger")
	for _, uri := range []string{"", "mongodb://remote:27017", "mongodb+srv://main@example.test", "mongodb+srv://other@example.test/?tls=false", "mongodb+srv://example.test"} {
		t.Setenv("ANNOUNCEMENTS_COMMAND_URI", uri)
		if repo, err := configureAnnouncementCommands(cfg); err == nil {
			if repo != nil {
				repo.close()
			}
			t.Fatalf("accepted unsafe config %q", uri)
		}
	}
	cfg.Environment = "development"
	t.Setenv("ANNOUNCEMENTS_COMMAND_URI", "mongodb://other@127.0.0.1:27017")
	repo, err := configureAnnouncementCommands(cfg)
	if err != nil {
		t.Fatal(err)
	}
	repo.close()
}

func TestAnnouncementAtlasEnvelopeAndReplay(t *testing.T) {
	mt := mtest.New(t, mtest.NewOptions().ClientType(mtest.Mock))
	mt.Run("envelope and replay", func(mt *mtest.T) {
		app := &Application{announcementCommands: &announcementCommandRepository{collection: mt.Coll, environment: "development"}}
		call := func(method, path, body string) *httptest.ResponseRecorder {
			r := httptest.NewRequest(method, path, strings.NewReader(body))
			r = r.WithContext(context.WithValue(r.Context(), adminActorKey{}, "operator"))
			w := httptest.NewRecorder()
			app.announcementAtlasHandler(w, r)
			return w
		}
		path := announcementAtlasPath + "/commands"
		body := `{"requestId":"` + atlasTestID + `","type":"publish","request":{"message":"hello","audience":"user","tenantId":"tenant-a","userId":"507f1f77bcf86cd799439011"}}`
		for _, invalid := range []string{
			`{"requestId":"` + atlasTestID + `","type":"list","after":"","tenantId":""}`,
			`{"requestId":"` + atlasTestID + `","type":"list","type":"clear"}`,
			`{"requestId":"` + atlasTestID + `","type":"publish","request":{"message":"a","message":"b","audience":"system"}}`,
			`{"requestId":"` + atlasTestID + `","type":"publish","request":{"message":"a","audience":"system","version":0}}`,
			`{"requestId":"` + atlasTestID + `","type":"list","after":null}`,
		} {
			if w := call("POST", path, invalid); w.Code != 400 {
				mt.Fatalf("accepted %s: %d", invalid, w.Code)
			}
		}
		mt.AddMockResponses(mtest.CreateSuccessResponse(bson.E{Key: "n", Value: 1}))
		if w := call("POST", path, body); w.Code != 202 {
			mt.Fatalf("submit: %d %s", w.Code, w.Body)
		}
		doc := mt.GetAllStartedEvents()[0].Command.Lookup("documents").Array().Index(0).Value().Document()
		expected := map[string]string{"_id": atlasTestID, "requestId": atlasTestID, "type": "publish", "actor": logsThresholdActor("operator"), "environment": "development", "workerEnvironment": "development", "state": "pending"}
		for k, want := range expected {
			if got := doc.Lookup(k).StringValue(); got != want {
				mt.Fatalf("%s=%q", k, got)
			}
		}
		elems, _ := doc.Elements()
		if len(elems) != 11 || doc.Lookup("version").Int32() != 1 || doc.Lookup("expiresAt").Time().Sub(doc.Lookup("createdAt").Time()) != 15*time.Minute {
			mt.Fatalf("invalid envelope: %v", doc)
		}
		request := doc.Lookup("request").Document()
		fields, _ := request.Elements()
		if len(fields) != 4 || request.Lookup("tenantId").StringValue() != "tenant-a" || request.Lookup("version").Type != 0 {
			mt.Fatalf("invalid nested request: %v", request)
		}
		var existing bson.D
		if err := bson.Unmarshal(doc, &existing); err != nil {
			mt.Fatal(err)
		}
		for i := range existing {
			if existing[i].Key == "state" {
				existing[i].Value = "completed"
				break
			}
		}
		existing = append(existing, bson.E{Key: "outcome", Value: "rejected"}, bson.E{Key: "result", Value: nil})
		for _, tc := range []struct {
			body string
			want int
		}{{body, 202}, {strings.Replace(body, "hello", "other", 1), 409}} {
			mt.AddMockResponses(mtest.CreateWriteErrorsResponse(mtest.WriteError{Code: 11000, Message: "duplicate"}), cursor(mt.DB.Name()+"."+mt.Coll.Name(), existing))
			if w := call("POST", path, tc.body); w.Code != tc.want {
				mt.Fatalf("replay: %d %s", w.Code, w.Body)
			}
		}
	})
}

func TestAnnouncementAtlasOperationsAndStatus(t *testing.T) {
	mt := mtest.New(t, mtest.NewOptions().ClientType(mtest.Mock))
	mt.Run("operations and scoped status", func(mt *mtest.T) {
		app := &Application{announcementCommands: &announcementCommandRepository{collection: mt.Coll, environment: "staging"}}
		call := func(method, path, body string) *httptest.ResponseRecorder {
			r := httptest.NewRequest(method, path, strings.NewReader(body))
			r = r.WithContext(context.WithValue(r.Context(), adminActorKey{}, "operator"))
			w := httptest.NewRecorder()
			app.announcementAtlasHandler(w, r)
			return w
		}
		base := announcementAtlasPath + "/commands"
		for _, tc := range []struct{ body, keys string }{
			{`"type":"list","after":"` + atlasTestID + `"`, "after"},
			{`"type":"detail","announcementId":"` + atlasTestID + `"`, "announcementId"},
			{`"type":"accounts","username":"user","after":"507f1f77bcf86cd799439011"`, "username,after"},
			{`"type":"sessions","tenantId":"tenant-a","userId":"507f1f77bcf86cd799439011"`, "tenantId,userId"},
			{`"type":"clear","announcementId":"` + atlasTestID + `"`, "announcementId"},
			{`"type":"edit","announcementId":"` + atlasTestID + `","request":{"message":"x","audience":"system","version":1}`, "announcementId,request"},
		} {
			mt.ClearEvents()
			mt.AddMockResponses(mtest.CreateSuccessResponse(bson.E{Key: "n", Value: 1}))
			body := `{"requestId":"` + atlasTestID + `",` + tc.body + `}`
			if w := call("POST", base, body); w.Code != 202 {
				mt.Fatalf("operation %s: %d %s", tc.body, w.Code, w.Body)
			}
			doc := mt.GetAllStartedEvents()[0].Command.Lookup("documents").Array().Index(0).Value().Document()
			fields, _ := doc.Elements()
			keys := strings.Split(tc.keys, ",")
			if len(fields) != 10+len(keys) {
				mt.Fatalf("unexpected fields: %v", doc)
			}
			for _, key := range keys {
				if doc.Lookup(key).Type == 0 {
					mt.Fatalf("missing %s: %v", key, doc)
				}
			}
		}
		for _, state := range []string{"pending", "processing", "completed"} {
			mt.ClearEvents()
			row := bson.D{{Key: "_id", Value: atlasTestID}, {Key: "requestId", Value: atlasTestID}, {Key: "state", Value: state}, {Key: "type", Value: "clear"}}
			if state == "completed" {
				row = append(row, bson.E{Key: "outcome", Value: "failed"}, bson.E{Key: "reason", Value: "retry_exhausted"}, bson.E{Key: "result", Value: nil})
			}
			mt.AddMockResponses(cursor(mt.DB.Name()+"."+mt.Coll.Name(), row))
			w := call("GET", base+"/"+atlasTestID, "")
			if w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"`+state+`"`) {
				mt.Fatalf("status: %d %s", w.Code, w.Body)
			}
			filter := mt.GetAllStartedEvents()[0].Command.Lookup("filter").Document()
			if filter.Lookup("actor").StringValue() != logsThresholdActor("operator") || filter.Lookup("environment").StringValue() != "staging" || filter.Lookup("workerEnvironment").StringValue() != "staging" {
				mt.Fatal("unscoped status")
			}
		}
		mt.AddMockResponses(mtest.CreateCommandErrorResponse(mtest.CommandError{Code: 91, Message: "credential-secret"}))
		w := call("GET", base+"/"+atlasTestID, "")
		if w.Code != 503 || strings.Contains(w.Body.String(), "secret") {
			mt.Fatalf("storage error: %d %s", w.Code, w.Body)
		}
		mt.AddMockResponses(mtest.CreateCommandErrorResponse(mtest.CommandError{Code: 91, Message: "credential-secret"}))
		w = call("POST", base, `{"requestId":"`+atlasTestID+`","type":"list"}`)
		if w.Code != 503 || !strings.Contains(w.Body.String(), "same request ID") || strings.Contains(w.Body.String(), "secret") {
			mt.Fatalf("ambiguous submit: %d %s", w.Code, w.Body)
		}
	})
}

func TestAnnouncementAtlasSafeStatus(t *testing.T) {
	date := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		kind     string
		result   bson.M
		expected string
	}{
		{"detail", bson.M{"_id": atlasTestID, "message": "notice", "createdAt": date, "password": "secret"}, `"id":"` + atlasTestID + `"`},
		{"list", bson.M{"announcements": bson.A{bson.M{"_id": atlasTestID, "startsAt": date, "token": "secret"}}, "nextCursor": ""}, `"startsAt":"2026-11-27T12:00:00Z"`},
		{"sessions", bson.M{"sessions": bson.A{bson.M{"sessionTargetId": "selector", "createdAt": date, "expiresAt": date, "sessionId": "secret"}}, "nextCursor": ""}, `"expiresAt":"2026-11-27T12:00:00Z"`},
		{"accounts", bson.M{"accounts": bson.A{bson.M{"tenantId": "tenant-a", "userId": "abc", "username": "user", "password": "secret"}}, "nextCursor": ""}, `"tenantId":"tenant-a"`},
	} {
		raw, _ := bson.Marshal(tc.result)
		value, err := safeAnnouncementStatus(atlasAnnouncementCommand{ID: atlasTestID, AtlasAnnouncementInput: AtlasAnnouncementInput{Type: tc.kind}, State: "completed", Outcome: "success", Result: raw})
		if err != nil {
			t.Fatalf("%s: %v", tc.kind, err)
		}
		data, _ := json.Marshal(value)
		if !strings.Contains(string(data), tc.expected) || strings.Contains(string(data), "secret") {
			t.Fatalf("%s: %s", tc.kind, data)
		}
	}
	for _, state := range []string{"pending", "processing"} {
		value, err := safeAnnouncementStatus(atlasAnnouncementCommand{ID: atlasTestID, State: state, Outcome: "success"})
		if err != nil || value["outcome"] != "" || value["result"] != nil {
			t.Fatal("premature result")
		}
	}
	var decoded atlasAnnouncementCommand
	raw, _ := bson.Marshal(bson.M{"_id": atlasTestID, "state": "completed", "outcome": "failed", "reason": "retry_exhausted", "result": nil})
	if err := bson.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("null result decode: %v", err)
	}
	value, err := safeAnnouncementStatus(decoded)
	if err != nil || value["reason"] != "retry_exhausted" {
		t.Fatal("failed status")
	}
}

func TestAnnouncementAtlasRoutes(t *testing.T) {
	app := &Application{config: Config{Environment: "development"}}
	req := httptest.NewRequest("GET", announcementAtlasPath, nil)
	w := httptest.NewRecorder()
	app.routes().ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("route auth: %d", w.Code)
	}
	req = req.WithContext(context.WithValue(req.Context(), adminActorKey{}, "operator"))
	w = httptest.NewRecorder()
	app.announcementAtlasHandler(w, req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"mode":"http"`) || !strings.Contains(w.Body.String(), logsThresholdActor("operator")) {
		t.Fatalf("mode: %d %s", w.Code, w.Body)
	}
}
