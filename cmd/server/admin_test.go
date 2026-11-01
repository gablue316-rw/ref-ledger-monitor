package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo/integration/mtest"
	"golang.org/x/crypto/bcrypt"
)

func testApp() *Application {
	return &Application{config: Config{Username: "monitor", Password: "test-secret", Release: "v1.test"}}
}
func adminRequest(method, path, body string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.SetBasicAuth("monitor", "test-secret")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Ref-Ledger-Admin", "1")
	return r
}
func TestAdminRoutesRequireAuth(t *testing.T) {
	app := testApp()
	mux := app.routes()
	for _, path := range []string{"/", "/admin", "/admin/", "/admin.js", "/admin/admin.js", "/api/admin/users", "/api/admin/roles", "/api/admin/users/role", "/api/admin/users/password", "/api/admin/users/delete", "/api/admin/users/sessions/cancel", "/api/sports", "/api/sport-active", "/api/sport-inactive", "/api/settings/official-roles", "/api/metrics/last-minute", "/admin/api/admin/users"} {
		t.Run(path, func(t *testing.T) {
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
			if w.Code != 401 {
				t.Fatalf("got %d, want 401", w.Code)
			}
		})
	}
}
func TestAdminPrefixAndStaticFiles(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("admin html"), 0600)
	os.WriteFile(filepath.Join(dir, "admin.js"), []byte("'use strict';"), 0600)
	app := testApp()
	app.config.IndexFile = filepath.Join(dir, "index.html")
	mux := app.routes()
	for _, path := range []string{"/", "/admin/", "/admin.js", "/admin/admin.js", "/api/about", "/admin/api/about"} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, adminRequest("GET", path, ""))
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
	}
	for _, path := range []string{"/api/typo", "/admin/api/typo", "/admin/anything.go"} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, adminRequest("GET", path, ""))
		if w.Code != 404 {
			t.Fatalf("%s: got %d", path, w.Code)
		}
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, adminRequest("GET", "/admin", ""))
	if w.Code != 307 || w.Header().Get("Location") != "/admin/" {
		t.Fatalf("redirect: %d %s", w.Code, w.Header().Get("Location"))
	}
}
func TestAdminWriteProtection(t *testing.T) {
	app := testApp()
	handler := app.requireAuth(app.adminWrite(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	cases := []struct {
		name, origin, site, content, header string
		code                                int
	}{
		{"local", "http://example.com", "same-origin", "application/json", "1", 204},
		{"main proxy", "https://ref-ledger.com", "same-origin", "application/json", "1", 204},
		{"other site", "https://evil.example", "same-origin", "application/json", "1", 403},
		{"cross site", "", "cross-site", "application/json", "1", 403},
		{"form", "", "", "application/x-www-form-urlencoded", "1", 415},
		{"missing header", "", "", "application/json", "", 403},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := adminRequest("POST", "/api/admin/users/delete", `{}`)
			r.Header.Set("Origin", c.origin)
			r.Header.Set("Sec-Fetch-Site", c.site)
			r.Header.Set("Content-Type", c.content)
			r.Header.Set("X-Ref-Ledger-Admin", c.header)
			w := httptest.NewRecorder()
			handler(w, r)
			if w.Code != c.code {
				t.Fatalf("got %d: %s", w.Code, w.Body.String())
			}
		})
	}
}
func TestAdminMethodAndInputRejection(t *testing.T) {
	mux := testApp().routes()
	for _, path := range []string{"/api/admin/users/role", "/api/admin/users/password", "/api/admin/users/delete", "/api/admin/users/sessions/cancel", "/api/sport-active", "/api/sport-inactive"} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, adminRequest("GET", path, ""))
		if w.Code != 405 {
			t.Fatalf("%s method: %d", path, w.Code)
		}
	}
	for _, body := range []string{`{"userId":"bad","newPassword":"x"}`, `{"userId":"000000000000000000000001","newPassword":" "}`, `{"userId":"000000000000000000000001","newPassword":"` + strings.Repeat("a", 73) + `"}`, `{} {}`, `{"unexpected":true}`} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, adminRequest("POST", "/api/admin/users/password", body))
		if w.Code != 400 {
			t.Fatalf("password input: got %d", w.Code)
		}
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, adminRequest("GET", "/api/admin/users?offset=-1", ""))
	if w.Code != 400 {
		t.Fatalf("offset: got %d", w.Code)
	}
}
func TestOfficialRoleValidation(t *testing.T) {
	valid := adminOfficialRoles{Sport: "Basketball", Roles: []adminOfficialRole{{RoleID: "r1", Title: "Referee", Active: true}, {RoleID: "r2", Title: "U1"}}}
	if err := validateOfficialRoles(&valid); err != nil {
		t.Fatal(err)
	}
	if valid.Roles[1].DisplayOrder != 2 {
		t.Fatal("display order not normalized")
	}
	invalid := valid
	invalid.Roles = append([]adminOfficialRole(nil), valid.Roles...)
	invalid.Roles[1].Title = "referee"
	if validateOfficialRoles(&invalid) == nil {
		t.Fatal("duplicate title accepted")
	}
	empty := adminOfficialRoles{Sport: "Softball"}
	if err := validateOfficialRoles(&empty); err != nil || empty.Roles == nil {
		t.Fatal("empty roles must be []")
	}
}
func cursor(ns string, docs ...bson.D) bson.D {
	return mtest.CreateCursorResponse(0, ns, mtest.FirstBatch, docs...)
}
func TestAdminSearchAcrossAccounts(t *testing.T) {
	mt := mtest.New(t, mtest.NewOptions().ClientType(mtest.Mock))
	mt.Run("search has no actor tenant filter", func(mt *mtest.T) {
		id := primitive.NewObjectID()
		mt.AddMockResponses(cursor(mt.DB.Name()+".users", bson.D{{Key: "_id", Value: id}, {Key: "username", Value: "someone@test.com"}, {Key: "name", Value: "Someone"}, {Key: "tenantId", Value: "other-account"}, {Key: "role", Value: "user"}}), cursor(mt.DB.Name()+".sessions", bson.D{{Key: "username", Value: "someone@test.com"}, {Key: "tenantId", Value: "other-account"}, {Key: "sessionId", Value: "s1"}, {Key: "expiresAt", Value: time.Now().Add(time.Hour)}}))
		app := testApp()
		app.db = mt.DB
		w := httptest.NewRecorder()
		app.routes().ServeHTTP(w, adminRequest("GET", "/api/admin/users?username=some.*", ""))
		if w.Code != 200 {
			mt.Fatalf("%d %s", w.Code, w.Body.String())
		}
		var data struct {
			Users []adminUserView `json:"users"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &data); err != nil {
			mt.Fatal(err)
		}
		if len(data.Users) != 1 || data.Users[0].TenantID != "other-account" || data.Users[0].IsCurrentUser || len(data.Users[0].Sessions) != 1 {
			mt.Fatalf("unexpected response: %s", w.Body.String())
		}
		events := mt.GetAllStartedEvents()
		var cmd bson.M
		bson.Unmarshal(events[0].Command, &cmd)
		filter := cmd["filter"].(bson.M)
		if _, exists := filter["tenantId"]; exists {
			mt.Fatal("search was tenant-scoped")
		}
		regex := filter["username"].(primitive.Regex)
		if regex.Pattern != `some\.\*` {
			mt.Fatalf("search regex was not escaped: %s", regex.Pattern)
		}
	})
}
func TestAdminCancelOneSession(t *testing.T) {
	mt := mtest.New(t, mtest.NewOptions().ClientType(mtest.Mock))
	mt.Run("ownership filter", func(mt *mtest.T) {
		id := primitive.NewObjectID()
		mt.AddMockResponses(cursor(mt.DB.Name()+".users", bson.D{{Key: "_id", Value: id}, {Key: "username", Value: "target@test.com"}, {Key: "tenantId", Value: "account-target"}}), mtest.CreateSuccessResponse(bson.E{Key: "n", Value: 1}))
		app := testApp()
		app.db = mt.DB
		w := httptest.NewRecorder()
		app.routes().ServeHTTP(w, adminRequest("POST", "/api/admin/users/sessions/cancel", `{"userId":"`+id.Hex()+`","sessionId":"s1","allSessions":false}`))
		if w.Code != 200 {
			mt.Fatalf("%d %s", w.Code, w.Body.String())
		}
		var cmd struct {
			Deletes []struct {
				Q bson.M `bson:"q"`
			} `bson:"deletes"`
		}
		bson.Unmarshal(mt.GetAllStartedEvents()[1].Command, &cmd)
		q := cmd.Deletes[0].Q
		if q["username"] != "target@test.com" || q["tenantId"] != "account-target" || q["sessionId"] != "s1" {
			mt.Fatalf("unsafe session filter: %v", q)
		}
	})
}
func TestAdminRoleTransaction(t *testing.T) {
	mt := mtest.New(t, mtest.NewOptions().ClientType(mtest.Mock))
	mt.Run("update and invalidate sessions", func(mt *mtest.T) {
		id := primitive.NewObjectID()
		mt.AddMockResponses(cursor(mt.DB.Name()+".userroles", bson.D{{Key: "name", Value: "readonly"}, {Key: "active", Value: true}}), cursor(mt.DB.Name()+".users", bson.D{{Key: "username", Value: "target@test.com"}, {Key: "tenantId", Value: "target"}}), mtest.CreateSuccessResponse(bson.E{Key: "n", Value: 1}, bson.E{Key: "nModified", Value: 1}), mtest.CreateSuccessResponse(bson.E{Key: "n", Value: 2}), mtest.CreateSuccessResponse())
		app := testApp()
		app.db = mt.DB
		w := httptest.NewRecorder()
		app.routes().ServeHTTP(w, adminRequest("POST", "/api/admin/users/role", `{"userId":"`+id.Hex()+`","role":"readonly"}`))
		if w.Code != 200 {
			mt.Fatalf("%d %s", w.Code, w.Body.String())
		}
		events := mt.GetAllStartedEvents()
		if len(events) != 5 || events[4].CommandName != "commitTransaction" {
			mt.Fatalf("missing transaction commit: %v", events)
		}
	})
}
func TestAdminPasswordBcrypt(t *testing.T) {
	mt := mtest.New(t, mtest.NewOptions().ClientType(mtest.Mock))
	mt.Run("bcrypt update", func(mt *mtest.T) {
		mt.AddMockResponses(mtest.CreateSuccessResponse(bson.E{Key: "n", Value: 1}, bson.E{Key: "nModified", Value: 1}))
		app := testApp()
		app.db = mt.DB
		w := httptest.NewRecorder()
		app.routes().ServeHTTP(w, adminRequest("POST", "/api/admin/users/password", `{"userId":"000000000000000000000001","newPassword":"Password test 123"}`))
		if w.Code != 200 {
			mt.Fatalf("%d %s", w.Code, w.Body.String())
		}
		var cmd struct {
			Updates []struct {
				U struct {
					Set struct {
						Hash string `bson:"passwordHash"`
					} `bson:"$set"`
				} `bson:"u"`
			} `bson:"updates"`
		}
		bson.Unmarshal(mt.GetAllStartedEvents()[0].Command, &cmd)
		if err := bcrypt.CompareHashAndPassword([]byte(cmd.Updates[0].U.Set.Hash), []byte("Password test 123")); err != nil {
			mt.Fatal("stored hash is not bcrypt")
		}
		if strings.Contains(w.Body.String(), "Password test 123") {
			mt.Fatal("response leaked password")
		}
	})
}
func TestAccountDeleteTransaction(t *testing.T) {
	mt := mtest.New(t, mtest.NewOptions().ClientType(mtest.Mock))
	mt.Run("delete all selected account data", func(mt *mtest.T) {
		id := primitive.NewObjectID()
		mt.AddMockResponses(cursor(mt.DB.Name()+".$cmd.listCollections", bson.D{{Key: "name", Value: "games"}, {Key: "type", Value: "collection"}}, bson.D{{Key: "name", Value: "sessions"}, {Key: "type", Value: "collection"}}, bson.D{{Key: "name", Value: "roles"}, {Key: "type", Value: "collection"}}, bson.D{{Key: "name", Value: "users"}, {Key: "type", Value: "collection"}}), cursor(mt.DB.Name()+".users", bson.D{{Key: "_id", Value: id}, {Key: "username", Value: "target@test.com"}, {Key: "tenantId", Value: "target-account"}}), cursor(mt.DB.Name()+".users", bson.D{{Key: "n", Value: int32(1)}}), mtest.CreateSuccessResponse(bson.E{Key: "n", Value: 4}), mtest.CreateSuccessResponse(bson.E{Key: "n", Value: 2}), mtest.CreateSuccessResponse(bson.E{Key: "n", Value: 0}), mtest.CreateSuccessResponse(bson.E{Key: "n", Value: 1}), mtest.CreateSuccessResponse())
		app := testApp()
		app.db = mt.DB
		w := httptest.NewRecorder()
		app.routes().ServeHTTP(w, adminRequest("POST", "/api/admin/users/delete", `{"userId":"`+id.Hex()+`","confirmationUsername":"target@test.com"}`))
		if w.Code != 200 {
			mt.Fatalf("%d %s", w.Code, w.Body.String())
		}
		events := mt.GetAllStartedEvents()
		for _, event := range events {
			if event.CommandName != "delete" {
				continue
			}
			var cmd struct {
				Deletes []struct {
					Q bson.M `bson:"q"`
				} `bson:"deletes"`
			}
			bson.Unmarshal(event.Command, &cmd)
			q := cmd.Deletes[0].Q
			if q["tenantId"] != "target-account" && q["$or"] == nil {
				mt.Fatalf("unscoped delete: %v", q)
			}
		}
		if events[len(events)-1].CommandName != "commitTransaction" {
			mt.Fatal("missing commit")
		}
	})
	mt.Run("wrong confirmation aborts", func(mt *mtest.T) {
		id := primitive.NewObjectID()
		mt.AddMockResponses(cursor(mt.DB.Name()+".$cmd.listCollections"), cursor(mt.DB.Name()+".users", bson.D{{Key: "_id", Value: id}, {Key: "username", Value: "target@test.com"}, {Key: "tenantId", Value: "target"}}), mtest.CreateSuccessResponse())
		app := testApp()
		app.db = mt.DB
		w := httptest.NewRecorder()
		app.routes().ServeHTTP(w, adminRequest("POST", "/api/admin/users/delete", `{"userId":"`+id.Hex()+`","confirmationUsername":"wrong"}`))
		if w.Code != 400 {
			mt.Fatalf("%d %s", w.Code, w.Body.String())
		}
		for _, event := range mt.GetAllStartedEvents() {
			if event.CommandName == "delete" {
				mt.Fatal("wrong confirmation deleted records")
			}
		}
	})
}

func TestLegacyMetricsRedirect(t *testing.T) {
	mux := testApp().routes()
	for _, path := range []string{"/metrics", "/metrics/", "/admin/metrics"} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, adminRequest("GET", path, ""))
		if w.Code != 307 || w.Header().Get("Location") != "/admin/#metrics" {
			t.Fatalf("%s: %d %s", path, w.Code, w.Header().Get("Location"))
		}
	}
}

func TestSportsCannotDeactivateWhenAssigned(t *testing.T) {
	mt := mtest.New(t, mtest.NewOptions().ClientType(mtest.Mock))
	mt.Run("assigned sport", func(mt *mtest.T) {
		mt.AddMockResponses(cursor(mt.DB.Name()+".sports", bson.D{{Key: "id", Value: "Basketball"}, {Key: "name", Value: "Basketball"}, {Key: "active", Value: true}}), cursor(mt.DB.Name()+".users", bson.D{{Key: "_id", Value: primitive.NewObjectID()}}))
		app := testApp()
		app.db = mt.DB
		w := httptest.NewRecorder()
		app.routes().ServeHTTP(w, adminRequest("POST", "/api/sport-inactive", `{"id":"Basketball","name":"Basketball"}`))
		if w.Code != 409 {
			mt.Fatalf("%d %s", w.Code, w.Body.String())
		}
		for _, event := range mt.GetAllStartedEvents() {
			if event.CommandName == "update" {
				mt.Fatal("assigned sport was deactivated")
			}
		}
	})
}

func TestOfficialRolesPreserveArraySchema(t *testing.T) {
	mt := mtest.New(t, mtest.NewOptions().ClientType(mtest.Mock))
	mt.Run("literal role data", func(mt *mtest.T) {
		mt.AddMockResponses(cursor(mt.DB.Name()+".sports", bson.D{{Key: "name", Value: "Basketball"}}), mtest.CreateSuccessResponse(bson.E{Key: "n", Value: 1}, bson.E{Key: "nModified", Value: 1}))
		app := testApp()
		app.db = mt.DB
		w := httptest.NewRecorder()
		app.routes().ServeHTTP(w, adminRequest("PUT", "/api/settings/official-roles", `{"sport":"Basketball","roles":[{"roleId":"stable-role","title":"$Referee","displayOrder":99,"active":true}]}`))
		if w.Code != 200 {
			mt.Fatalf("%d %s", w.Code, w.Body.String())
		}
		event := mt.GetAllStartedEvents()[1]
		var cmd bson.M
		bson.Unmarshal(event.Command, &cmd)
		updates := cmd["updates"].(bson.A)
		entry := updates[0].(bson.M)
		if entry["q"].(bson.M)["tenantId"] == nil {
			mt.Fatal("catalog write could target customer document")
		}
		encoded, _ := json.Marshal(entry["u"])
		for _, part := range []string{"$concatArrays", "$filter", "$literal", "officialRoles", "stable-role", "$Referee"} {
			if !bytes.Contains(encoded, []byte(part)) {
				mt.Fatalf("missing %s in update: %s", part, encoded)
			}
		}
	})
	mt.Run("missing sport roles return empty array", func(mt *mtest.T) {
		mt.AddMockResponses(cursor(mt.DB.Name() + ".roles"))
		app := testApp()
		app.db = mt.DB
		w := httptest.NewRecorder()
		app.routes().ServeHTTP(w, adminRequest("GET", "/api/settings/official-roles?sport=Basketball", ""))
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"roles":[]`) {
			mt.Fatalf("%d %s", w.Code, w.Body.String())
		}
	})
}

func TestSharedAccountDeletionIsBlocked(t *testing.T) {
	mt := mtest.New(t, mtest.NewOptions().ClientType(mtest.Mock))
	mt.Run("shared account", func(mt *mtest.T) {
		id := primitive.NewObjectID()
		mt.AddMockResponses(cursor(mt.DB.Name()+".$cmd.listCollections"), cursor(mt.DB.Name()+".users", bson.D{{Key: "_id", Value: id}, {Key: "username", Value: "target@test.com"}, {Key: "tenantId", Value: "shared-account"}}), cursor(mt.DB.Name()+".users", bson.D{{Key: "n", Value: int32(2)}}), mtest.CreateSuccessResponse())
		app := testApp()
		app.db = mt.DB
		w := httptest.NewRecorder()
		app.routes().ServeHTTP(w, adminRequest("POST", "/api/admin/users/delete", `{"userId":"`+id.Hex()+`","confirmationUsername":"target@test.com"}`))
		if w.Code != 409 {
			mt.Fatalf("%d %s", w.Code, w.Body.String())
		}
		for _, event := range mt.GetAllStartedEvents() {
			if event.CommandName == "delete" {
				mt.Fatal("shared account data was deleted")
			}
		}
	})
}
