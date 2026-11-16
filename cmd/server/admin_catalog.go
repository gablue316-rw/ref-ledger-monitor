package main

import (
	"context"
	"errors"
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
	Version string `bson:"-" json:"version"`
	raw     bson.Raw
	ID      string `bson:"id" json:"id"`
	Name    string `bson:"name" json:"name"`
	Active  bool   `bson:"active" json:"active"`
}

type adminOfficialRole struct {
	RoleID       string `bson:"roleId" json:"roleId"`
	Title        string `bson:"title" json:"title"`
	DisplayOrder int    `bson:"displayOrder" json:"displayOrder"`
	Active       bool   `bson:"active" json:"active"`
}

type adminOfficialRoles struct {
	Version string              `bson:"-" json:"version"`
	Sport   string              `bson:"sport" json:"sport"`
	Roles   []adminOfficialRole `bson:"roles" json:"roles"`
}

// Sports and official roles are global catalogs; customer records remain scoped.
func globalCatalogFilter() bson.M { return bson.M{"tenantId": bson.M{"$exists": false}} }
func (app *Application) adminSportsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
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
		version, ok := adminRequireVersion(w, r)
		if !ok {
			return
		}
		roleVersion := r.Header.Get("X-Ref-Ledger-Roles-Version")
		if roleVersion == "" {
			adminConflict(w, errAdminConflict)
			return
		}
		if err := app.adminDeleteSport(ctx, id, version, roleVersion); err != nil {
			if errors.Is(err, errAdminSportUsed) {
				adminUsersError(w, 409, err.Error())
				return
			}
			if adminConflict(w, err) {
				return
			}
			adminUsersError(w, 500, "Unable to delete sport and roles. Nothing was committed.")
			return
		}
		writeJSON(w, 200, bson.M{"message": "Sport and role settings deleted."})

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
	version, ok := adminRequireVersion(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	filter := globalCatalogFilter()
	filter["id"] = request.ID
	var sport adminSport
	if err := app.db.Collection("sports").FindOne(ctx, filter).Decode(&sport); err != nil {
		if adminConflict(w, err) {
			return
		}
		adminUsersError(w, 500, "Unable to load sport.")
		return
	}
	if sport.Version != version {
		adminConflict(w, errAdminConflict)
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
	result, err := app.db.Collection("sports").UpdateOne(ctx, adminSnapshotFilter(sport.raw), bson.M{"$set": bson.M{"active": active}})
	if err != nil {
		adminUsersError(w, 500, "Unable to update sport status.")
		return
	}
	if result.MatchedCount == 0 {
		adminConflict(w, errAdminConflict)
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
	w.Header().Set("Cache-Control", "no-store")
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
		raw, err := app.adminRoleSnapshot(ctx, sport)
		if err != nil {
			adminUsersError(w, 500, "Unable to load official roles.")
			return
		}
		response, err := adminRoleResponse(raw, sport)
		if err != nil {
			adminUsersError(w, 500, "Unable to read official roles.")
			return
		}
		writeJSON(w, 200, response)
	case http.MethodPut, http.MethodDelete:
		var request adminOfficialRoles
		if r.Method == http.MethodPut {
			if !decodeAdminJSON(w, r, &request) {
				return
			}
			if err := validateOfficialRoles(&request); err != nil {
				adminUsersError(w, 400, err.Error())
				return
			}
		} else {
			request.Sport = strings.TrimSpace(r.URL.Query().Get("sport"))
			if request.Sport == "" {
				adminUsersError(w, 400, "Sport is required.")
				return
			}
		}
		version, ok := adminRequireVersion(w, r)
		if !ok {
			return
		}
		if r.Method == http.MethodPut {
			sportFilter := globalCatalogFilter()
			sportFilter["name"] = request.Sport
			if err := app.db.Collection("sports").FindOne(ctx, sportFilter).Err(); err != nil {
				catalogLookupError(w, err, "Sport")
				return
			}
		}
		raw, err := app.adminRoleSnapshot(ctx, request.Sport)
		if err != nil {
			adminUsersError(w, 500, "Unable to verify official roles.")
			return
		}
		if adminRoleVersion(raw) != version {
			adminConflict(w, errAdminConflict)
			return
		}
		if raw == nil {
			if r.Method == http.MethodDelete {
				writeJSON(w, 200, bson.M{"message": "Official roles already absent.", "version": adminMissingVersion})
				return
			}
			// A deterministic singleton ID makes simultaneous first-time Monitor saves
			// collide rather than insert two independent global role containers.
			doc := bson.D{{Key: "_id", Value: "monitor-global-official-roles"}, {Key: "officialRoles", Value: bson.A{bson.M{"sport": request.Sport, "roles": request.Roles}}}}
			created, _ := bson.Marshal(doc)
			if _, err := coll.InsertOne(ctx, bson.Raw(created)); err != nil {
				if mongo.IsDuplicateKeyError(err) {
					adminConflict(w, errAdminConflict)
				} else {
					adminUsersError(w, 500, "Unable to save official roles.")
				}
				return
			}
			response, _ := adminRoleResponse(created, request.Sport)
			writeJSON(w, 200, response)
			return
		}
		var update interface{} = bson.M{"$pull": bson.M{"officialRoles": bson.M{"sport": request.Sport}}}
		if r.Method == http.MethodPut {
			replacement := bson.M{"sport": request.Sport, "roles": request.Roles}
			update = mongo.Pipeline{bson.D{{Key: "$set", Value: bson.M{"officialRoles": bson.M{"$concatArrays": bson.A{
				bson.M{"$filter": bson.M{"input": bson.M{"$ifNull": bson.A{"$officialRoles", bson.A{}}}, "as": "entry", "cond": bson.M{"$ne": bson.A{"$$entry.sport", bson.M{"$literal": request.Sport}}}}},
				bson.M{"$literal": bson.A{replacement}},
			}}}}}}
		}
		next, err := coll.FindOneAndUpdate(ctx, adminSnapshotFilter(raw), update, options.FindOneAndUpdate().SetReturnDocument(options.After)).Raw()
		if err != nil {
			if adminConflict(w, err) {
				return
			}
			adminUsersError(w, 500, "Unable to save official roles.")
			return
		}
		response, err := adminRoleResponse(next, request.Sport)
		if err != nil {
			adminUsersError(w, 500, "Official roles were saved but their result could not be read. Reload settings.")
			return
		}
		writeJSON(w, 200, response)
	default:
		w.Header().Set("Allow", "GET, PUT, DELETE")
		adminUsersError(w, 405, "Method not allowed.")
	}
}

// Prefer the sport's existing container. For a new sport, use the first global
// container deterministically. No customer-owned roles document can be selected.
func (app *Application) adminRoleSnapshot(ctx context.Context, sport string) (bson.Raw, error) {
	coll := app.db.Collection("roles")
	filter := globalCatalogFilter()
	filter["officialRoles.sport"] = sport
	raw, err := coll.FindOne(ctx, filter, options.FindOne().SetSort(bson.D{{Key: "_id", Value: 1}})).Raw()
	if errors.Is(err, mongo.ErrNoDocuments) {
		raw, err = coll.FindOne(ctx, globalCatalogFilter(), options.FindOne().SetSort(bson.D{{Key: "_id", Value: 1}})).Raw()
	}
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	return raw, err
}
func adminRoleVersion(raw bson.Raw) string {
	if raw == nil {
		return adminMissingVersion
	}
	return adminRecordVersion(raw)
}
func adminRoleResponse(raw bson.Raw, sport string) (adminOfficialRoles, error) {
	response := adminOfficialRoles{Sport: sport, Roles: []adminOfficialRole{}, Version: adminRoleVersion(raw)}
	if raw != nil {
		var doc struct {
			OfficialRoles []adminOfficialRoles `bson:"officialRoles"`
		}
		if err := bson.Unmarshal(raw, &doc); err != nil {
			return response, err
		}
		for _, entry := range doc.OfficialRoles {
			if entry.Sport == sport && entry.Roles != nil {
				response.Roles = entry.Roles
				break
			}
		}
	}
	sort.SliceStable(response.Roles, func(i, j int) bool { return response.Roles[i].DisplayOrder < response.Roles[j].DisplayOrder })
	return response, nil
}

// Delete the sport and its role definitions as one transaction. A concurrent
// role edit must abort both changes rather than leave a half-deleted catalog.
var errAdminSportUsed = errors.New("cannot delete this sport because it is assigned to games")

func (app *Application) adminDeleteSport(ctx context.Context, id, version, roleVersion string) error {
	session, err := app.db.Client().StartSession()
	if err != nil {
		return err
	}
	defer session.EndSession(ctx)
	_, err = session.WithTransaction(ctx, func(tx mongo.SessionContext) (interface{}, error) {
		filter := globalCatalogFilter()
		filter["id"] = id
		var sport adminSport
		if err := app.db.Collection("sports").FindOne(tx, filter).Decode(&sport); err != nil {
			return nil, err
		}
		if sport.Version != version {
			return nil, errAdminConflict
		}
		raw, err := app.adminRoleSnapshot(tx, sport.Name)
		if err != nil {
			return nil, err
		}
		if adminRoleVersion(raw) != roleVersion {
			return nil, errAdminConflict
		}
		used, err := app.sportUsed(tx, "games", "sport", sport)
		if err != nil {
			return nil, err
		}
		if used {
			return nil, errAdminSportUsed
		}
		result, err := app.db.Collection("sports").DeleteOne(tx, adminSnapshotFilter(sport.raw))
		if err != nil {
			return nil, err
		}
		if result.DeletedCount != 1 {
			return nil, errAdminConflict
		}
		if raw != nil {
			result, err := app.db.Collection("roles").UpdateOne(tx, adminSnapshotFilter(raw), bson.M{"$pull": bson.M{"officialRoles": bson.M{"sport": sport.Name}}})
			if err != nil {
				return nil, err
			}
			if result.MatchedCount != 1 {
				return nil, errAdminConflict
			}
		}
		return nil, nil
	})
	return err
}
