package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type adminMaintenanceState struct {
	Enabled bool  `bson:"enabled" json:"enabled"`
	Active  int64 `bson:"activeWrites" json:"activeWrites"`
}

// Only administrator-controlled settings participate. Admission counters change
// throughout normal operation and must not make a loaded toggle immediately stale.
func adminMaintenanceFilter(raw bson.Raw) bson.M {
	expressions := bson.A{}
	for _, key := range []string{"enabled", "updatedAt"} {
		field := "$" + key
		present := bson.M{"$ne": bson.A{bson.M{"$type": field}, "missing"}}
		value := raw.Lookup(key)
		if value.Type == 0 {
			expressions = append(expressions, bson.M{"$eq": bson.A{bson.M{"$type": field}, "missing"}})
		} else {
			expressions = append(expressions, present, bson.M{"$eq": bson.A{field, bson.M{"$literal": value}}})
		}
	}
	return bson.M{"_id": "maintenance", "$expr": bson.M{"$and": expressions}}
}

func adminMaintenanceVersion(raw bson.Raw) string {
	if raw == nil {
		return adminMissingVersion
	}
	fields := bson.D{}
	for _, key := range []string{"enabled", "updatedAt"} {
		value := raw.Lookup(key)
		fields = append(fields, bson.E{Key: key + "Present", Value: value.Type != 0})
		if value.Type != 0 {
			fields = append(fields, bson.E{Key: key, Value: value})
		}
	}
	data, _ := bson.Marshal(fields)
	return adminRecordVersion(data)
}
func (app *Application) adminMaintenanceHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		adminUsersError(w, 405, "Method not allowed.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	coll := maintenanceCollection(app.db)
	var enabled *bool
	version := ""
	if r.Method == http.MethodPost {
		var request struct {
			Enabled *bool `json:"enabled"`
		}
		if !decodeAdminJSON(w, r, &request) {
			return
		}
		if request.Enabled == nil {
			adminUsersError(w, 400, "enabled must be true or false.")
			return
		}
		var ok bool
		version, ok = adminRequireVersion(w, r)
		if !ok {
			return
		}
		enabled = request.Enabled
	}
	raw, err := coll.FindOne(ctx, bson.M{"_id": "maintenance"}).Raw()
	if errors.Is(err, mongo.ErrNoDocuments) {
		raw = nil
		err = nil
	}
	if err != nil {
		adminUsersError(w, 503, "Unable to verify maintenance mode.")
		return
	}
	if r.Method == http.MethodPost {
		if adminMaintenanceVersion(raw) != version {
			adminConflict(w, errAdminConflict)
			return
		}
		now := time.Now().UTC()
		if raw == nil {
			doc := bson.D{{Key: "_id", Value: "maintenance"}, {Key: "enabled", Value: *enabled}, {Key: "activeWrites", Value: int64(0)}, {Key: "updatedAt", Value: now}}
			data, _ := bson.Marshal(doc)
			if _, err = coll.InsertOne(ctx, bson.Raw(data)); err == nil {
				raw = data
			}
			if mongo.IsDuplicateKeyError(err) {
				adminConflict(w, errAdminConflict)
				return
			}
		} else {
			raw, err = coll.FindOneAndUpdate(ctx, adminMaintenanceFilter(raw), bson.M{"$set": bson.M{"enabled": *enabled, "updatedAt": now}}, options.FindOneAndUpdate().SetReturnDocument(options.After)).Raw()
		}
		if err != nil {
			if adminConflict(w, err) {
				return
			}
			adminUsersError(w, 503, "Unable to change maintenance mode. Refresh to verify its state.")
			return
		}
	}
	var state adminMaintenanceState
	if raw != nil {
		if err := bson.Unmarshal(raw, &state); err != nil {
			adminUsersError(w, 503, "Unable to read maintenance state.")
			return
		}
	}
	_ = json.NewEncoder(w).Encode(struct {
		Enabled  bool   `json:"enabled"`
		Active   int64  `json:"activeWrites"`
		Drained  bool   `json:"drained"`
		Database string `json:"database"`
		Version  string `json:"version"`
	}{state.Enabled, state.Active, state.Enabled && state.Active == 0, app.db.Name(), adminMaintenanceVersion(raw)})
}

// Invoked inside the existing authentication and CSRF protections.
func (app *Application) maintenanceWrite(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
			next(w, r)
			return
		}
		release, err := beginMaintenanceWrite(app.db)
		if err != nil {
			adminUsersError(w, 503, "Changes are unavailable during maintenance, or maintenance status could not be verified.")
			return
		}
		defer release()
		next(w, r)
	}
}
