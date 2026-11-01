package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"golang.org/x/crypto/bcrypt"
)

// Ported from the supplied Ref Ledger web server. Authentication is enforced
// by app.routes using the monitor's independent Basic Auth credentials.
func (app *Application) adminRolesHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")

	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	collection := app.db.Collection("userroles")

	cursor, err := collection.Find(
		ctx,
		bson.M{"active": true},
		options.Find().SetSort(
			bson.D{{Key: "name", Value: 1}},
		),
	)
	if err != nil {
		http.Error(w, "Unable to load roles", http.StatusInternalServerError)
		return
	}
	defer cursor.Close(ctx)

	roles := make([]string, 0)
	seen := make(map[string]bool)

	for cursor.Next(ctx) {
		var record struct {
			Name string `bson:"name"`
		}

		if err := cursor.Decode(&record); err != nil {
			http.Error(w, "Unable to read role", http.StatusInternalServerError)
			return
		}

		if record.Name == "" ||
			record.Name != strings.TrimSpace(record.Name) ||
			seen[record.Name] {
			continue
		}

		seen[record.Name] = true
		roles = append(roles, record.Name)
	}

	if err := cursor.Err(); err != nil {
		http.Error(w, "Unable to read roles", http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"roles": roles,
	})
}

func (app *Application) adminRoleHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")

	writeError := func(status int, message string) {
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(map[string]string{
			"message": message,
		})
	}

	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeError(http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	if r.Header.Get("X-Ref-Ledger-Admin") != "1" {
		writeError(http.StatusForbidden, "Invalid administration request")
		return
	}

	var request struct {
		UserID string `json:"userId"`
		Role   string `json:"role"`
	}

	r.Body = http.MaxBytesReader(w, r.Body, 4096)

	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&request); err != nil {
		writeError(http.StatusBadRequest, "Invalid role request")
		return
	}

	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeError(http.StatusBadRequest, "Provide one JSON object")
		return
	}

	userID, err := primitive.ObjectIDFromHex(request.UserID)
	if err != nil {
		writeError(http.StatusBadRequest, "Invalid user ID")
		return
	}

	if request.Role == "" ||
		request.Role != strings.TrimSpace(request.Role) {
		writeError(http.StatusBadRequest, "Select a valid role")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()

	db := app.db

	transaction, err := app.db.Client().StartSession()
	if err != nil {
		writeError(http.StatusInternalServerError, "Unable to start role update")
		return
	}
	defer transaction.EndSession(ctx)

	invalidRole := errors.New("invalid role")
	userNotFound := errors.New("user not found")

	currentSessionCancelled := false

	_, err = transaction.WithTransaction(
		ctx,
		func(tx mongo.SessionContext) (interface{}, error) {
			// Only allow roles configured in the backend collection.
			err := db.Collection("userroles").FindOne(
				tx,
				bson.M{
					"name":   request.Role,
					"active": true,
				},
			).Err()

			if errors.Is(err, mongo.ErrNoDocuments) {
				return nil, invalidRole
			}
			if err != nil {
				return nil, err
			}

			var user struct {
				Username string `bson:"username"`
				TenantID string `bson:"tenantId"`
			}

			err = db.Collection("users").FindOne(
				tx,
				bson.M{"_id": userID},
			).Decode(&user)

			if errors.Is(err, mongo.ErrNoDocuments) {
				return nil, userNotFound
			}
			if err != nil {
				return nil, err
			}

			_, err = db.Collection("users").UpdateOne(
				tx,
				bson.M{"_id": userID},
				bson.M{
					"$set": bson.M{
						"role": request.Role,
					},
				},
			)
			if err != nil {
				return nil, err
			}

			// Existing sessions contain the old role.
			_, err = db.Collection("sessions").DeleteMany(
				tx,
				bson.M{
					"username": user.Username,
					"tenantId": user.TenantID,
				},
			)
			if err != nil {
				return nil, err
			}

			currentSessionCancelled = false
			return nil, nil
		},
	)

	switch {
	case errors.Is(err, invalidRole):
		writeError(http.StatusBadRequest, "Selected role is not active or does not exist")
		return

	case errors.Is(err, userNotFound):
		writeError(http.StatusNotFound, "User not found")
		return

	case err != nil:
		log.Printf("Update user role failed: %v", err)
		writeError(
			http.StatusInternalServerError,
			"Role update could not be confirmed. Refresh the user directory before retrying.",
		)
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":                 true,
		"message":                 "Role updated. The user must sign in again.",
		"currentSessionCancelled": currentSessionCancelled,
	})
}

func (app *Application) adminPasswordHandler(
	w http.ResponseWriter,
	r *http.Request,
) {
	w.Header().Set("Cache-Control", "no-store")

	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(
			w,
			"Method not allowed",
			http.StatusMethodNotAllowed,
		)
		return
	}

	writeError := func(status int, message string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"message": message,
		})
	}

	// Matches the custom header sent by administration.html.
	if r.Header.Get("X-Ref-Ledger-Admin") != "1" {
		writeError(http.StatusForbidden, "Invalid admin request.")
		return
	}

	var payload struct {
		UserID      string `json:"userId"`
		NewPassword string `json:"newPassword"`
	}

	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	defer r.Body.Close()

	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&payload); err != nil {
		writeError(
			http.StatusBadRequest,
			"Invalid password update request.",
		)
		return
	}

	// Reject additional JSON values after the request object.
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeError(
			http.StatusBadRequest,
			"Request must contain a single JSON object.",
		)
		return
	}

	userID, err := primitive.ObjectIDFromHex(
		strings.TrimSpace(payload.UserID),
	)
	if err != nil {
		writeError(http.StatusBadRequest, "Invalid user ID.")
		return
	}

	// Validate without trimming the actual password:
	// leading and trailing spaces may be intentional.
	if strings.TrimSpace(payload.NewPassword) == "" {
		writeError(
			http.StatusBadRequest,
			"New password cannot be blank.",
		)
		return
	}

	// bcrypt accepts a maximum of 72 bytes.
	if len([]byte(payload.NewPassword)) > 72 {
		writeError(
			http.StatusBadRequest,
			"New password cannot exceed 72 bytes.",
		)
		return
	}

	passwordHash, err := bcrypt.GenerateFromPassword(
		[]byte(payload.NewPassword),
		bcrypt.DefaultCost,
	)
	if err != nil {
		writeError(
			http.StatusInternalServerError,
			"Unable to secure the new password.",
		)
		return
	}

	ctx, cancel := context.WithTimeout(
		r.Context(),
		10*time.Second,
	)
	defer cancel()

	result, err := app.db.Collection("users").UpdateOne(
		ctx,
		bson.M{
			"_id": userID,
		},
		bson.M{
			"$set": bson.M{
				"passwordHash": string(passwordHash),
			},
		},
	)
	if err != nil {
		// Never log the request body, password, or hash.
		writeError(
			http.StatusInternalServerError,
			"Unable to update the user's password.",
		)
		return
	}

	if result.MatchedCount == 0 {
		writeError(http.StatusNotFound, "User not found.")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"message":                 "Password changed successfully.",
		"currentSessionCancelled": false,
	})
}
