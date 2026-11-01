package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type adminSport struct {
	ID     string `bson:"id" json:"id"`
	Name   string `bson:"name" json:"name"`
	Active bool   `bson:"active" json:"active"`
}

type adminOfficialRole struct {
	RoleID       string `bson:"roleId" json:"roleId"`
	Title        string `bson:"title" json:"title"`
	DisplayOrder int    `bson:"displayOrder" json:"displayOrder"`
	Active       bool   `bson:"active" json:"active"`
}

type adminOfficialRoles struct {
	Sport string              `bson:"sport" json:"sport"`
	Roles []adminOfficialRole `bson:"roles" json:"roles"`
}

// Sports and official roles are global catalogs; customer records remain scoped.
func globalCatalogFilter() bson.M { return bson.M{"tenantId": bson.M{"$exists": false}} }
func (app *Application) adminSportsHandler(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	coll := app.db.Collection("sports")
	switch r.Method {
	case http.MethodGet:
		filter := globalCatalogFilter()
		if r.URL.Query().Get("active") == "true" {
			filter["active"] = true
		}
		cur, err := coll.Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "name", Value: 1}}))
		if err != nil {
			adminUsersError(w, 500, "Unable to load sports.")
			return
		}
		defer cur.Close(ctx)
		sports := make([]adminSport, 0)
		if err = cur.All(ctx, &sports); err != nil {
			adminUsersError(w, 500, "Unable to read sports.")
			return
		}
		writeJSON(w, 200, sports)
	case http.MethodPost:
		var sport adminSport
		if !decodeAdminJSON(w, r, &sport) {
			return
		}
		sport.ID = strings.TrimSpace(sport.ID)
		sport.Name = strings.TrimSpace(sport.Name)
		if sport.ID == "" || sport.Name == "" || utf8.RuneCountInString(sport.Name) > 50 || len(sport.ID) > 200 {
			adminUsersError(w, 400, "Sport ID and name are required; name cannot exceed 50 characters.")
			return
		}
		duplicate := globalCatalogFilter()
		duplicate["$or"] = bson.A{bson.M{"id": sport.ID}, bson.M{"name": sport.Name}}
		err := coll.FindOne(ctx, duplicate).Err()
		if err == nil {
			adminUsersError(w, 409, "Sport already exists.")
			return
		}
		if !errors.Is(err, mongo.ErrNoDocuments) {
			adminUsersError(w, 500, "Unable to check sports.")
			return
		}
		// Match the administration form's setup sequence.
		sport.Active = false
		if _, err = coll.InsertOne(ctx, sport); err != nil {
			if mongo.IsDuplicateKeyError(err) {
				adminUsersError(w, 409, "Sport already exists.")
			} else {
				adminUsersError(w, 500, "Unable to add sport.")
			}
			return
		}
		writeJSON(w, 201, bson.M{"message": "Sport added as inactive. Configure its official roles before activation."})
	case http.MethodDelete:
		id := strings.TrimSpace(r.URL.Query().Get("id"))
		if id == "" {
			adminUsersError(w, 400, "Sport ID is required.")
			return
		}
		filter := globalCatalogFilter()
		filter["id"] = id
		var sport adminSport
		if err := coll.FindOne(ctx, filter).Decode(&sport); err != nil {
			catalogLookupError(w, err, "Sport")
			return
		}
		// Same all-account game reference check as Ref Ledger, accepting id or name.
		used, err := app.sportUsed(ctx, "games", "sport", sport)
		if err != nil {
			adminUsersError(w, 500, "Unable to check sport references.")
			return
		}
		if used {
			adminUsersError(w, 409, "Cannot delete this sport because it is assigned to games.")
			return
		}
		result, err := coll.DeleteOne(ctx, filter)
		if err != nil {
			adminUsersError(w, 500, "Unable to delete sport.")
			return
		}
		if result.DeletedCount == 0 {
			adminUsersError(w, 404, "Sport not found.")
			return
		}
		// The existing admin.js follows this with DELETE official-roles for the sport.
		writeJSON(w, 200, bson.M{"message": "Sport deleted successfully."})
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		adminUsersError(w, 405, "Method not allowed.")
	}
}

func (app *Application) sportUsed(ctx context.Context, collection, key string, sport adminSport) (bool, error) {
	err := app.db.Collection(collection).FindOne(ctx, bson.M{key: bson.M{"$in": bson.A{sport.ID, sport.Name}}}, options.FindOne().SetProjection(bson.M{"_id": 1})).Err()
	if errors.Is(err, mongo.ErrNoDocuments) {
		return false, nil
	}
	return err == nil, err
}
func catalogLookupError(w http.ResponseWriter, err error, what string) {
	if errors.Is(err, mongo.ErrNoDocuments) {
		adminUsersError(w, 404, what+" not found.")
	} else {
		adminUsersError(w, 500, "Unable to load "+strings.ToLower(what)+".")
	}
}
func (app *Application) adminSportActiveHandler(w http.ResponseWriter, r *http.Request) {
	app.adminSetSportActive(w, r, true)
}
func (app *Application) adminSportInactiveHandler(w http.ResponseWriter, r *http.Request) {
	app.adminSetSportActive(w, r, false)
}
func (app *Application) adminSetSportActive(w http.ResponseWriter, r *http.Request, active bool) {
	if !adminUsersMethod(w, r, http.MethodPost) {
		return
	}
	var request adminSport
	if !decodeAdminJSON(w, r, &request) {
		return
	}
	request.ID = strings.TrimSpace(request.ID)
	if request.ID == "" {
		adminUsersError(w, 400, "Sport ID is required.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	filter := globalCatalogFilter()
	filter["id"] = request.ID
	var sport adminSport
	if err := app.db.Collection("sports").FindOne(ctx, filter).Decode(&sport); err != nil {
		catalogLookupError(w, err, "Sport")
		return
	}
	if !active {
		for _, ref := range []struct{ collection, key string }{{"users", "sports"}, {"games", "sport"}} {
			used, err := app.sportUsed(ctx, ref.collection, ref.key, sport)
			if err != nil {
				adminUsersError(w, 500, "Unable to check sport references.")
				return
			}
			if used {
				adminUsersError(w, 409, "Cannot deactivate this sport because it is assigned to "+ref.collection+".")
				return
			}
		}
	}
	result, err := app.db.Collection("sports").UpdateOne(ctx, filter, bson.M{"$set": bson.M{"active": active}})
	if err != nil {
		adminUsersError(w, 500, "Unable to update sport status.")
		return
	}
	if result.MatchedCount == 0 {
		adminUsersError(w, 404, "Sport not found.")
		return
	}
	writeJSON(w, 200, bson.M{"success": true, "message": "Sport status updated."})
}

func validateOfficialRoles(request *adminOfficialRoles) error {
	request.Sport = strings.TrimSpace(request.Sport)
	if request.Sport == "" || utf8.RuneCountInString(request.Sport) > 50 {
		return errors.New("Select a valid sport.")
	}
	if len(request.Roles) > 100 {
		return errors.New("Configure at most 100 roles per sport.")
	}
	if request.Roles == nil {
		request.Roles = make([]adminOfficialRole, 0)
	}
	ids, titles := map[string]bool{}, map[string]bool{}
	for i := range request.Roles {
		role := &request.Roles[i]
		role.Title = strings.TrimSpace(role.Title)
		if role.RoleID == "" || role.RoleID != strings.TrimSpace(role.RoleID) || len(role.RoleID) > 200 {
			return errors.New("Every role requires a stable roleId.")
		}
		if role.Title == "" || utf8.RuneCountInString(role.Title) > 100 {
			return errors.New("Role titles must contain 1–100 characters.")
		}
		if ids[role.RoleID] || titles[strings.ToLower(role.Title)] {
			return errors.New("Role IDs and titles must be unique within the sport.")
		}
		ids[role.RoleID] = true
		titles[strings.ToLower(role.Title)] = true
		role.DisplayOrder = i + 1
	}
	return nil
}

func (app *Application) adminOfficialRolesHandler(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	coll := app.db.Collection("roles")
	switch r.Method {
	case http.MethodGet:
		sport := strings.TrimSpace(r.URL.Query().Get("sport"))
		if sport == "" {
			adminUsersError(w, 400, "Sport is required.")
			return
		}
		response := adminOfficialRoles{Sport: sport, Roles: make([]adminOfficialRole, 0)}
		filter := globalCatalogFilter()
		filter["officialRoles"] = bson.M{"$elemMatch": bson.M{"sport": sport}}
		var doc struct {
			OfficialRoles []adminOfficialRoles `bson:"officialRoles"`
		}
		err := coll.FindOne(ctx, filter, options.FindOne().SetProjection(bson.M{"officialRoles": bson.M{"$elemMatch": bson.M{"sport": sport}}})).Decode(&doc)
		if err != nil && !errors.Is(err, mongo.ErrNoDocuments) {
			adminUsersError(w, 500, "Unable to load official roles.")
			return
		}
		if len(doc.OfficialRoles) > 0 && doc.OfficialRoles[0].Roles != nil {
			response.Roles = doc.OfficialRoles[0].Roles
		}
		sort.SliceStable(response.Roles, func(i, j int) bool { return response.Roles[i].DisplayOrder < response.Roles[j].DisplayOrder })
		writeJSON(w, 200, response)
	case http.MethodPut:
		var request adminOfficialRoles
		if !decodeAdminJSON(w, r, &request) {
			return
		}
		if err := validateOfficialRoles(&request); err != nil {
			adminUsersError(w, 400, err.Error())
			return
		}
		sportFilter := globalCatalogFilter()
		sportFilter["name"] = request.Sport
		if err := app.db.Collection("sports").FindOne(ctx, sportFilter).Err(); err != nil {
			catalogLookupError(w, err, "Sport")
			return
		}
		// Match the existing roles.officialRoles array schema. Replace one sport atomically.
		replacement := bson.M{"sport": request.Sport, "roles": request.Roles}
		update := mongo.Pipeline{bson.D{{Key: "$set", Value: bson.M{"officialRoles": bson.M{"$concatArrays": bson.A{
			bson.M{"$filter": bson.M{"input": bson.M{"$ifNull": bson.A{"$officialRoles", bson.A{}}}, "as": "entry", "cond": bson.M{"$ne": bson.A{"$$entry.sport", bson.M{"$literal": request.Sport}}}}},
			bson.M{"$literal": bson.A{replacement}},
		}}}}}}
		if _, err := coll.UpdateOne(ctx, globalCatalogFilter(), update, options.Update().SetUpsert(true)); err != nil {
			adminUsersError(w, 500, "Unable to save official roles.")
			return
		}
		writeJSON(w, 200, bson.M{"message": fmt.Sprintf("Official roles saved for %s.", request.Sport)})
	case http.MethodDelete:
		sport := strings.TrimSpace(r.URL.Query().Get("sport"))
		if sport == "" {
			adminUsersError(w, 400, "Sport is required.")
			return
		}
		// Idempotent cleanup remains available after the sport itself was removed.
		if _, err := coll.UpdateOne(ctx, globalCatalogFilter(), bson.M{"$pull": bson.M{"officialRoles": bson.M{"sport": sport}}}); err != nil {
			adminUsersError(w, 500, "Unable to delete official roles.")
			return
		}
		writeJSON(w, 200, bson.M{"message": "Official roles deleted."})
	default:
		w.Header().Set("Allow", "GET, PUT, DELETE")
		adminUsersError(w, 405, "Method not allowed.")
	}
}
