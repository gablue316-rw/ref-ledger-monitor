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
		_, err := coll.UpdateOne(ctx, bson.M{"_id": "maintenance"}, bson.M{
			"$set":         bson.M{"enabled": *request.Enabled, "updatedAt": time.Now().UTC()},
			"$setOnInsert": bson.M{"activeWrites": int64(0)},
		}, options.Update().SetUpsert(true))
		if err != nil {
			adminUsersError(w, 503, "Unable to change maintenance mode. Refresh to verify its state.")
			return
		}
	}
	var state struct {
		Enabled   bool       `bson:"enabled" json:"enabled"`
		Active    int64      `bson:"activeWrites" json:"activeWrites"`
		UpdatedAt *time.Time `bson:"updatedAt,omitempty" json:"updatedAt,omitempty"`
	}
	err := coll.FindOne(ctx, bson.M{"_id": "maintenance"}).Decode(&state)
	if err != nil && !errors.Is(err, mongo.ErrNoDocuments) {
		adminUsersError(w, 503, "Unable to verify maintenance mode.")
		return
	}
	_ = json.NewEncoder(w).Encode(struct {
		Enabled  bool   `json:"enabled"`
		Active   int64  `json:"activeWrites"`
		Drained  bool   `json:"drained"`
		Database string `json:"database"`
	}{state.Enabled, state.Active, state.Enabled && state.Active == 0, app.db.Name()})
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
