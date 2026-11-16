package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/integration/mtest"
)

func TestMaintenanceAccess(t *testing.T) {
	app := testApp()
	for _, path := range []string{"/api/admin/maintenance", "/admin/api/admin/maintenance"} {
		w := httptest.NewRecorder()
		app.routes().ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 401 {
			t.Fatalf("unprotected maintenance control: %d", w.Code)
		}
	}
}

func TestMaintenanceStateAndToggle(t *testing.T) {
	mt := mtest.New(t, mtest.NewOptions().ClientType(mtest.Mock))
	for _, active := range []int64{0, 2} {
		mt.Run("enable and drain", func(mt *mtest.T) {
			app := testApp()
			app.db = mt.DB
			old := bson.D{{Key: "_id", Value: "maintenance"}, {Key: "enabled", Value: false}, {Key: "activeWrites", Value: active}}
			next := bson.D{{Key: "_id", Value: "maintenance"}, {Key: "enabled", Value: true}, {Key: "activeWrites", Value: active}, {Key: "updatedAt", Value: time.Now().UTC()}}
			mt.AddMockResponses(adminSessionMock(mt), cursor(mt.DB.Name()+".system_control", old), mtest.CreateSuccessResponse(bson.E{Key: "value", Value: next}))
			raw, _ := bson.Marshal(old)
			req := adminRequest("POST", "/api/admin/maintenance", `{"enabled":true}`)
			req.Header.Set("If-Match", adminMaintenanceVersion(raw))
			w := httptest.NewRecorder()
			app.routes().ServeHTTP(w, req)
			want := `"drained":false`
			if active == 0 {
				want = `"drained":true`
			}
			if w.Code != 200 || !strings.Contains(w.Body.String(), want) {
				mt.Fatalf("%d %s", w.Code, w.Body)
			}
			command := mt.GetAllStartedEvents()[2].Command
			update := command.Lookup("update").Document().Lookup("$set").Document()
			if update.Lookup("activeWrites").Type != 0 {
				mt.Fatal("toggle overwrites active writer count")
			}
			predicate := command.Lookup("query").Document()
			if predicate.Lookup("$expr").Type == 0 || strings.Contains(predicate.String(), "activeWrites") {
				mt.Fatal("missing settings preconditions")
			}
		})
	}
	mt.Run("disable works while locked", func(mt *mtest.T) {
		app := testApp()
		app.db = mt.DB
		old := bson.D{{Key: "_id", Value: "maintenance"}, {Key: "enabled", Value: true}, {Key: "activeWrites", Value: int64(0)}}
		next := bson.D{{Key: "_id", Value: "maintenance"}, {Key: "enabled", Value: false}, {Key: "activeWrites", Value: int64(0)}, {Key: "updatedAt", Value: time.Now().UTC()}}
		mt.AddMockResponses(adminSessionMock(mt), cursor(mt.DB.Name()+".system_control", old), mtest.CreateSuccessResponse(bson.E{Key: "value", Value: next}))
		raw, _ := bson.Marshal(old)
		req := adminRequest("POST", "/api/admin/maintenance", `{"enabled":false}`)
		req.Header.Set("If-Match", adminMaintenanceVersion(raw))
		w := httptest.NewRecorder()
		app.routes().ServeHTTP(w, req)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"enabled":false`) {
			mt.Fatalf("%d %s", w.Code, w.Body)
		}
	})
	mt.Run("origin protection", func(mt *mtest.T) {
		app := testApp()
		app.db = mt.DB
		mt.AddMockResponses(adminSessionMock(mt))
		req := adminRequest("POST", "/api/admin/maintenance", `{"enabled":true}`)
		req.Header.Set("Origin", "https://attacker.example")
		w := httptest.NewRecorder()
		app.routes().ServeHTTP(w, req)
		if w.Code != 403 {
			mt.Fatal(w.Code)
		}
	})
}

func TestSharedMaintenanceGate(t *testing.T) {
	mt := mtest.New(t, mtest.NewOptions().ClientType(mtest.Mock))
	mt.Run("enabled blocks admin mutations", func(mt *mtest.T) {
		app := testApp()
		app.db = mt.DB
		mt.AddMockResponses(adminSessionMock(mt), mtest.CreateSuccessResponse(), mtest.CreateSuccessResponse(bson.E{Key: "value", Value: nil}))
		w := httptest.NewRecorder()
		app.routes().ServeHTTP(w, adminRequest("POST", "/api/admin/users/delete", `{}`))
		if w.Code != 503 {
			mt.Fatalf("write escaped gate: %d", w.Code)
		}
		var cmd bson.M
		bson.Unmarshal(mt.GetAllStartedEvents()[2].Command, &cmd)
		if cmd["query"].(bson.M)["enabled"] != false {
			mt.Fatal("admission is not conditional on maintenance being off")
		}
	})
	mt.Run("counter surrounds admitted handler", func(mt *mtest.T) {
		app := testApp()
		app.db = mt.DB
		mt.AddMockResponses(mtest.CreateSuccessResponse(), mtest.CreateSuccessResponse(bson.E{Key: "value", Value: bson.D{{Key: "activeWrites", Value: int64(1)}}}), mtest.CreateSuccessResponse(bson.E{Key: "n", Value: 1}))
		called := false
		w := httptest.NewRecorder()
		app.maintenanceWrite(func(w http.ResponseWriter, r *http.Request) { called = true; w.WriteHeader(204) })(w, adminRequest("POST", "/api/sports", `{}`))
		if !called || w.Code != 204 || len(mt.GetAllStartedEvents()) != 3 {
			mt.Fatal("write was not admitted and released")
		}
	})
	mt.Run("database failure blocks writes", func(mt *mtest.T) {
		app := testApp()
		app.db = mt.DB
		mt.AddMockResponses(mtest.CreateCommandErrorResponse(mtest.CommandError{Code: 13, Message: "not authorized"}))
		app.maintenanceWrite(func(http.ResponseWriter, *http.Request) { mt.Fatal("write ran without verified status") })(httptest.NewRecorder(), adminRequest("POST", "/api/sports", `{}`))
	})
}
