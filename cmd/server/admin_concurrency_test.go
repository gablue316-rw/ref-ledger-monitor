package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/integration/mtest"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func TestAdminSnapshotMetadataDoesNotExposeCredentials(t *testing.T) {
	stored := bson.D{{Key: "_id", Value: primitive.NewObjectID()}, {Key: "username", Value: "test@example.com"}, {Key: "passwordHash", Value: "private-hash"}, {Key: "mobileIdentity", Value: bson.D{{Key: "issuer", Value: "private-issuer"}, {Key: "subject", Value: "private-subject"}}}, {Key: "futureField", Value: "preserve-me"}}
	raw, _ := bson.Marshal(stored)
	var user adminUserRecord
	if err := bson.Unmarshal(raw, &user); err != nil {
		t.Fatal(err)
	}
	if user.Version != adminRecordVersion(raw) || !bytes.Equal(user.raw, raw) {
		t.Fatal("incomplete snapshot")
	}
	encoded, _ := json.Marshal(adminUserView{adminUserRecord: user})
	for _, secret := range []string{"private-hash", "passwordHash", "private-issuer", "private-subject", "preserve-me"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatal("private field exposed")
		}
	}
	storedAgain, _ := bson.Marshal(user)
	if bson.Raw(storedAgain).Lookup("version").Type != 0 || bson.Raw(storedAgain).Lookup("raw").Type != 0 {
		t.Fatal("read metadata persisted")
	}
}

func TestMaintenanceTokenIgnoresOnlyActiveWriters(t *testing.T) {
	first, _ := bson.Marshal(bson.D{{Key: "_id", Value: "maintenance"}, {Key: "enabled", Value: false}, {Key: "activeWrites", Value: int64(0)}})
	busy, _ := bson.Marshal(bson.D{{Key: "_id", Value: "maintenance"}, {Key: "enabled", Value: false}, {Key: "activeWrites", Value: int64(42)}})
	changed, _ := bson.Marshal(bson.D{{Key: "_id", Value: "maintenance"}, {Key: "enabled", Value: true}, {Key: "activeWrites", Value: int64(0)}})
	dated, _ := bson.Marshal(bson.D{{Key: "_id", Value: "maintenance"}, {Key: "enabled", Value: false}, {Key: "activeWrites", Value: int64(0)}, {Key: "updatedAt", Value: time.Now()}})
	if adminMaintenanceVersion(first) != adminMaintenanceVersion(busy) {
		t.Fatal("active writers invalidate settings")
	}
	if adminMaintenanceVersion(first) == adminMaintenanceVersion(changed) || adminMaintenanceVersion(first) == adminMaintenanceVersion(dated) {
		t.Fatal("settings change ignored")
	}
}

func TestAdminMissingVersionsFailClosed(t *testing.T) {
	id := "000000000000000000000001"
	for _, c := range []struct {
		name, method, path, body string
		handler                  func(*Application, http.ResponseWriter, *http.Request)
	}{
		{"role", "POST", "/api/admin/users/role", `{"userId":"` + id + `","role":"user"}`, (*Application).adminRoleHandler},
		{"password", "POST", "/api/admin/users/password", `{"userId":"` + id + `","newPassword":"sample"}`, (*Application).adminPasswordHandler},
		{"delete user", "POST", "/api/admin/users/delete", `{"userId":"` + id + `","confirmationUsername":"test@example.com"}`, (*Application).adminUsersDelete},
		{"sessions", "POST", "/api/admin/users/sessions/cancel", `{"userId":"` + id + `","allSessions":true}`, (*Application).adminUsersCancelSessions},
		{"sport", "POST", "/api/sport-active", `{"id":"Softball"}`, (*Application).adminSportActiveHandler},
		{"delete sport", "DELETE", "/api/sports?id=Softball", "", (*Application).adminSportsHandler},
		{"roles", "PUT", "/api/settings/official-roles", `{"sport":"Softball","roles":[]}`, (*Application).adminOfficialRolesHandler},
		{"delete roles", "DELETE", "/api/settings/official-roles?sport=Softball", "", (*Application).adminOfficialRolesHandler},
		{"maintenance", "POST", "/api/admin/maintenance", `{"enabled":true}`, (*Application).adminMaintenanceHandler},
	} {
		t.Run(c.name, func(t *testing.T) {
			// Some handlers resolve the collection before decoding; a mock DB provides a
			// client without allowing a missing-version request to issue any commands.
			mt := mtest.New(t, mtest.NewOptions().ClientType(mtest.Mock))
			mt.Run("rejected", func(mt *mtest.T) {
				app := testApp()
				app.db = mt.DB
				w := httptest.NewRecorder()
				c.handler(app, w, adminRequest(c.method, c.path, c.body))
				if w.Code != 409 {
					mt.Fatalf("%d %s", w.Code, w.Body)
				}
				if len(mt.GetAllStartedEvents()) != 0 {
					mt.Fatal("missing version issued a database command")
				}
			})
		})
	}
}

func TestStaleAccountDeleteDoesNotContactAuth0(t *testing.T) {
	mt := mtest.New(t, mtest.NewOptions().ClientType(mtest.Mock))
	mt.Run("before external side effects", func(mt *mtest.T) {
		issuer, calls := mockAdminAuth0(mt.T, http.StatusNoContent)
		id := primitive.NewObjectID()
		stored := bson.D{{Key: "_id", Value: id}, {Key: "username", Value: "target@test.com"}, {Key: "tenantId", Value: "a"}, {Key: "role", Value: "readonly"}, {Key: "mobileIdentity", Value: bson.D{{Key: "issuer", Value: issuer}, {Key: "subject", Value: "auth0|123"}}}}
		mt.AddMockResponses(cursor(mt.DB.Name()+".$cmd.listCollections"), cursor(mt.DB.Name()+".users", stored))
		app := testApp()
		app.db = mt.DB
		req := adminRequest("POST", "/api/admin/users/delete", `{"userId":"`+id.Hex()+`","confirmationUsername":"target@test.com"}`)
		req.Header.Set("If-Match", "stale")
		w := httptest.NewRecorder()
		app.adminUsersDelete(w, req)
		if w.Code != 409 || *calls != 0 {
			mt.Fatalf("status=%d Auth0 calls=%d", w.Code, *calls)
		}
		for _, event := range mt.GetAllStartedEvents() {
			if event.CommandName == "delete" {
				mt.Fatal("stale request deleted data")
			}
		}
	})
}

func TestStaleRoleTransactionDoesNotCancelSessions(t *testing.T) {
	mt := mtest.New(t, mtest.NewOptions().ClientType(mtest.Mock))
	mt.Run("reject before writes", func(mt *mtest.T) {
		id := primitive.NewObjectID()
		mt.AddMockResponses(cursor(mt.DB.Name()+".userroles", bson.D{{Key: "name", Value: "readonly"}, {Key: "active", Value: true}}), cursor(mt.DB.Name()+".users", bson.D{{Key: "_id", Value: id}, {Key: "role", Value: "admin"}}), mtest.CreateSuccessResponse())
		app := testApp()
		app.db = mt.DB
		req := adminRequest("POST", "/api/admin/users/role", `{"userId":"`+id.Hex()+`","role":"readonly"}`)
		req.Header.Set("If-Match", "stale")
		w := httptest.NewRecorder()
		app.adminRoleHandler(w, req)
		if w.Code != 409 {
			mt.Fatalf("%d %s", w.Code, w.Body)
		}
		for _, event := range mt.GetAllStartedEvents() {
			if event.CommandName == "update" || event.CommandName == "delete" {
				mt.Fatal("stale role changed data")
			}
		}
	})
}

// This test opts into a disposable replica set. It creates and drops a unique
// monitor_concurrency_test_* database; it never opens the application database.
func TestMonitorConcurrencyIntegration(t *testing.T) {
	uri := os.Getenv("MONITOR_CONCURRENCY_TEST_URI")
	if uri == "" {
		t.Skip("set MONITOR_CONCURRENCY_TEST_URI to a disposable replica set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Disconnect(context.Background())
	db := client.Database(fmt.Sprintf("monitor_concurrency_test_%d", time.Now().UnixNano()))
	defer db.Drop(context.Background())
	app := testApp()
	app.db = db
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	insert := func(coll string, doc interface{}) {
		t.Helper()
		_, err := db.Collection(coll).InsertOne(ctx, doc)
		must(err)
	}
	call := func(method, path, body, version string, handler http.HandlerFunc) *httptest.ResponseRecorder {
		t.Helper()
		req := adminRequest(method, path, body)
		if version != "" {
			req.Header.Set("If-Match", version)
		}
		w := httptest.NewRecorder()
		handler(w, req)
		return w
	}
	token := func(coll string, filter bson.M) string {
		raw, err := db.Collection(coll).FindOne(ctx, filter).Raw()
		must(err)
		return adminRecordVersion(raw)
	}
	t.Run("competing exact snapshots and added fields", func(t *testing.T) {
		coll := db.Collection("snapshot_tests")
		insert("snapshot_tests", bson.M{"_id": "one", "value": 0})
		raw, err := coll.FindOne(ctx, bson.M{"_id": "one"}).Raw()
		must(err)
		var wg sync.WaitGroup
		counts := make(chan int64, 2)
		errs := make(chan error, 2)
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func(value int) {
				defer wg.Done()
				r, e := coll.UpdateOne(ctx, adminSnapshotFilter(raw), bson.M{"$set": bson.M{"value": value}})
				if e != nil {
					errs <- e
					return
				}
				counts <- r.MatchedCount
			}(i + 1)
		}
		wg.Wait()
		close(counts)
		close(errs)
		for e := range errs {
			must(e)
		}
		var total int64
		for n := range counts {
			total += n
		}
		if total != 1 {
			t.Fatal(total)
		}
		raw, err = coll.FindOne(ctx, bson.M{"_id": "one"}).Raw()
		must(err)
		_, err = coll.UpdateOne(ctx, bson.M{"_id": "one"}, bson.M{"$set": bson.M{"newField": "$data"}})
		must(err)
		result, err := coll.DeleteOne(ctx, adminSnapshotFilter(raw))
		must(err)
		if result.DeletedCount != 0 {
			t.Fatal("new field escaped concurrency check")
		}
	})
	t.Run("user roles and password edits invalidate old snapshots", func(t *testing.T) {
		id := primitive.NewObjectID()
		insert("users", bson.M{"_id": id, "username": "test@example.com", "tenantId": "a", "role": "user", "passwordHash": "old-secret", "futureField": "private"})
		insert("userroles", bson.M{"name": "readonly", "active": true})
		insert("userroles", bson.M{"name": "admin", "active": true})
		insert("sessions", bson.M{"username": "test@example.com", "tenantId": "a", "sessionId": "s1"})
		version := token("users", bson.M{"_id": id})
		var wg sync.WaitGroup
		statuses := make(chan int, 2)
		for _, role := range []string{"readonly", "admin"} {
			wg.Add(1)
			go func(role string) {
				defer wg.Done()
				w := call("POST", "/api/admin/users/role", `{"userId":"`+id.Hex()+`","role":"`+role+`"}`, version, app.adminRoleHandler)
				statuses <- w.Code
			}(role)
		}
		wg.Wait()
		close(statuses)
		success, conflict := 0, 0
		for code := range statuses {
			if code == 200 {
				success++
			} else if code == 409 {
				conflict++
			} else {
				t.Fatal(code)
			}
		}
		if success != 1 || conflict != 1 {
			t.Fatal(success, conflict)
		}
		count, err := db.Collection("sessions").CountDocuments(ctx, bson.M{"tenantId": "a"})
		must(err)
		if count != 0 {
			t.Fatal("role change did not invalidate sessions")
		}
		stale := call("POST", "/api/admin/users/password", `{"userId":"`+id.Hex()+`","newPassword":"different"}`, version, app.adminPasswordHandler)
		if stale.Code != 409 {
			t.Fatal(stale.Code, stale.Body)
		}
		current := token("users", bson.M{"_id": id})
		saved := call("POST", "/api/admin/users/password", `{"userId":"`+id.Hex()+`","newPassword":"different"}`, current, app.adminPasswordHandler)
		if saved.Code != 200 {
			t.Fatal(saved.Code, saved.Body)
		}
		search := call("GET", "/api/admin/users?username=test@example.com", "", "", app.adminUsersSearch)
		if search.Code != 200 || strings.Contains(search.Body.String(), "passwordHash") || strings.Contains(search.Body.String(), "old-secret") || strings.Contains(search.Body.String(), "futureField") {
			t.Fatal(search.Code, search.Body)
		}
		var response struct {
			Users []adminUserView `json:"users"`
		}
		must(json.Unmarshal(search.Body.Bytes(), &response))
		if len(response.Users) != 1 || response.Users[0].Version != token("users", bson.M{"_id": id}) {
			t.Fatal("directory token not from full user document")
		}
	})
	t.Run("roles and sport deletion share conditional transaction", func(t *testing.T) {
		insert("sports", bson.M{"id": "Softball", "name": "Softball", "active": false})
		// First creation supplies a repeatable token, even with literal dollar data.
		created := call("PUT", "/api/settings/official-roles", `{"sport":"Softball","roles":[{"roleId":"plate","title":"$Plate","active":true}]}`, adminMissingVersion, app.adminOfficialRolesHandler)
		if created.Code != 200 {
			t.Fatal(created.Code, created.Body)
		}
		var roleState adminOfficialRoles
		must(json.Unmarshal(created.Body.Bytes(), &roleState))
		raw, err := app.adminRoleSnapshot(ctx, "Softball")
		must(err)
		if roleState.Version != adminRoleVersion(raw) {
			t.Fatal("new-role token does not match stored BSON")
		}
		old := roleState.Version
		saved := call("PUT", "/api/settings/official-roles", `{"sport":"Softball","roles":[{"roleId":"plate","title":"Plate changed","active":true}]}`, old, app.adminOfficialRolesHandler)
		if saved.Code != 200 {
			t.Fatal(saved.Code, saved.Body)
		}
		stale := call("PUT", "/api/settings/official-roles", `{"sport":"Softball","roles":[]}`, old, app.adminOfficialRolesHandler)
		if stale.Code != 409 {
			t.Fatal(stale.Code, stale.Body)
		}
		sportToken := token("sports", bson.M{"id": "Softball"})
		if err := app.adminDeleteSport(ctx, "Softball", sportToken, old); !errors.Is(err, errAdminConflict) {
			t.Fatal(err)
		}
		n, err := db.Collection("sports").CountDocuments(ctx, bson.M{"id": "Softball"})
		must(err)
		if n != 1 {
			t.Fatal("stale role deletion removed sport")
		}
		raw, err = app.adminRoleSnapshot(ctx, "Softball")
		must(err)
		must(app.adminDeleteSport(ctx, "Softball", sportToken, adminRoleVersion(raw)))
		n, err = db.Collection("sports").CountDocuments(ctx, bson.M{"id": "Softball"})
		must(err)
		if n != 0 {
			t.Fatal("sport not deleted")
		}
		state, err := app.adminRoleSnapshot(ctx, "Softball")
		must(err)
		response, err := adminRoleResponse(state, "Softball")
		must(err)
		if len(response.Roles) != 0 {
			t.Fatal("role cleanup not committed")
		}
	})
	t.Run("maintenance ignores counter changes but rejects old settings", func(t *testing.T) {
		get := call("GET", "/api/admin/maintenance", "", "", app.adminMaintenanceHandler)
		var state struct {
			Version string `json:"version"`
		}
		must(json.Unmarshal(get.Body.Bytes(), &state))
		if state.Version != adminMissingVersion {
			t.Fatal(state)
		}
		init := call("POST", "/api/admin/maintenance", `{"enabled":false}`, state.Version, app.adminMaintenanceHandler)
		if init.Code != 200 {
			t.Fatal(init.Code, init.Body)
		}
		must(json.Unmarshal(init.Body.Bytes(), &state))
		old := state.Version
		_, err := db.Collection("system_control").UpdateOne(ctx, bson.M{"_id": "maintenance"}, bson.M{"$inc": bson.M{"activeWrites": int64(2)}})
		must(err)
		enable := call("POST", "/api/admin/maintenance", `{"enabled":true}`, old, app.adminMaintenanceHandler)
		if enable.Code != 200 || !strings.Contains(enable.Body.String(), `"activeWrites":2`) {
			t.Fatal(enable.Code, enable.Body)
		}
		stale := call("POST", "/api/admin/maintenance", `{"enabled":false}`, old, app.adminMaintenanceHandler)
		if stale.Code != 409 {
			t.Fatal(stale.Code, stale.Body)
		}
	})
}
