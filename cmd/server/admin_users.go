package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readconcern"
	"go.mongodb.org/mongo-driver/mongo/writeconcern"
)

type adminUserRecord struct {
	Version        string `bson:"-" json:"version"`
	raw            bson.Raw
	MobileIdentity adminAuth0Identity `bson:"mobileIdentity" json:"-"`
	ID             primitive.ObjectID `bson:"_id" json:"id"`
	Username       string             `bson:"username" json:"username"`
	Name           string             `bson:"name" json:"name"`
	TenantID       string             `bson:"tenantId" json:"tenantId"`
	Role           string             `bson:"role" json:"role"`
}

type adminUserSession struct {
	TargetID  string    `json:"sessionTargetId,omitempty"`
	SessionID string    `bson:"sessionId" json:"sessionId"`
	ExpiresAt time.Time `bson:"expiresAt" json:"expiresAt"`
	Client    string    `json:"client"`
}

type storedSession struct {
	TargetID  string    `bson:"targetId"`
	SessionID string    `bson:"sessionId"`
	ExpiresAt time.Time `bson:"expiresAt"`
	Username  string    `bson:"username"`
	TenantID  string    `bson:"tenantId"`
	Type      string    `bson:"type"`
	Platform  string    `bson:"platform"`
	Client    string    `bson:"client"`
}

func (s storedSession) clientLabel() string {
	switch {
	case s.Platform == "android" || (s.Type == "mobile" && s.Platform == ""):
		return "Android app"
	case s.Client == "web" || (s.Type == "" && s.Platform == "" && s.Client == "" && s.SessionID != ""):
		// Older web sessions have an ID but no type. Legacy mobile activity has no ID.
		return "Web browser"
	default:
		return "Unknown"
	}
}

type adminUserView struct {
	adminUserRecord `bson:",inline"`
	Sessions        []adminUserSession `json:"sessions"`
	IsCurrentUser   bool               `json:"isCurrentUser"`
}

type adminUserAction struct {
	UserID               string `json:"userId"`
	SessionID            string `json:"sessionId"`
	AllSessions          bool   `json:"allSessions"`
	ConfirmationUsername string `json:"confirmationUsername"`
}

func adminUsersJSON(w http.ResponseWriter, status int, body interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		log.Printf("admin users response: %v", err)
	}
}

func adminUsersError(w http.ResponseWriter, status int, message string) {
	adminUsersJSON(w, status, map[string]string{"message": message})
}

func adminUsersMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method == method {
		return true
	}
	w.Header().Set("Allow", method)
	adminUsersError(w, http.StatusMethodNotAllowed, "Method not allowed.")
	return false
}

func adminUsersDecode(w http.ResponseWriter, r *http.Request) (adminUserAction, bool) {
	var action adminUserAction
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || contentType != "application/json" {
		adminUsersError(w, http.StatusUnsupportedMediaType, "JSON is required.")
		return action, false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&action); err != nil {
		adminUsersError(w, http.StatusBadRequest, "Invalid administration request.")
		return action, false
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		adminUsersError(w, http.StatusBadRequest, "Only one JSON object is allowed.")
		return action, false
	}
	return action, true
}

func (app *Application) adminUsersSearch(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !adminUsersMethod(w, r, http.MethodGet) {
		return
	}
	query := r.URL.Query()
	username, name := strings.TrimSpace(query.Get("username")), strings.TrimSpace(query.Get("name"))
	if len(username) > 254 || len(name) > 200 {
		adminUsersError(w, http.StatusBadRequest, "Search text is too long.")
		return
	}
	var offset int64
	if raw := query.Get("offset"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 0 || parsed > 1000000 {
			adminUsersError(w, http.StatusBadRequest, "Invalid page offset.")
			return
		}
		offset = parsed
	}
	filter := bson.M{}
	if username != "" {
		filter["username"] = primitive.Regex{Pattern: regexp.QuoteMeta(username), Options: "i"}
	}
	if name != "" {
		filter["name"] = primitive.Regex{Pattern: regexp.QuoteMeta(name), Options: "i"}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	db := app.db
	const pageSize int64 = 50
	cursor, err := db.Collection("users").Find(ctx, filter, options.Find().
		SetSort(bson.D{{Key: "username", Value: 1}, {Key: "_id", Value: 1}}).
		SetSkip(offset).SetLimit(pageSize+1))
	if err != nil {
		log.Printf("admin users search: %v", err)
		adminUsersError(w, http.StatusInternalServerError, "Unable to load users.")
		return
	}
	defer cursor.Close(ctx)
	users := make([]adminUserRecord, 0)
	if err := cursor.All(ctx, &users); err != nil {
		adminUsersError(w, http.StatusInternalServerError, "Unable to read users.")
		return
	}
	hasMore := len(users) > int(pageSize)
	if hasMore {
		users = users[:pageSize]
	}
	// One session query per page, with explicit username + tenant ownership pairs.
	pairs := make(bson.A, 0, len(users))
	for _, user := range users {
		pairs = append(pairs, bson.M{"username": user.Username, "tenantId": user.TenantID})
	}
	sessionMap := make(map[string][]adminUserSession)
	if len(pairs) > 0 {
		sessionCursor, err := db.Collection("sessions").Find(ctx, bson.M{"$or": pairs}, options.Find().
			SetProjection(bson.M{"_id": 0, "sessionId": 1, "targetId": 1, "expiresAt": 1, "username": 1, "tenantId": 1, "type": 1, "platform": 1, "client": 1}).
			SetSort(bson.D{{Key: "expiresAt", Value: -1}, {Key: "sessionId", Value: 1}}))
		if err != nil {
			adminUsersError(w, http.StatusInternalServerError, "Unable to load sessions.")
			return
		}
		defer sessionCursor.Close(ctx)
		var stored []storedSession
		if err := sessionCursor.All(ctx, &stored); err != nil {
			adminUsersError(w, http.StatusInternalServerError, "Unable to read sessions.")
			return
		}
		for _, session := range stored {
			key := session.TenantID + "\x00" + session.Username
			sessionMap[key] = append(sessionMap[key], adminUserSession{
				SessionID: session.SessionID,
				TargetID:  session.TargetID,
				ExpiresAt: session.ExpiresAt,
				Client:    session.clientLabel(),
			})
		}
	}
	views := make([]adminUserView, 0, len(users))
	for _, user := range users {
		sessions := sessionMap[user.TenantID+"\x00"+user.Username]
		if sessions == nil {
			sessions = make([]adminUserSession, 0)
		}
		views = append(views, adminUserView{adminUserRecord: user, Sessions: sessions,
			IsCurrentUser: false})
	}
	adminUsersJSON(w, http.StatusOK, map[string]interface{}{
		"users": views, "hasMore": hasMore, "offset": offset, "pageSize": pageSize,
	})
}

func (app *Application) adminUsersTarget(ctx context.Context, id string) (adminUserRecord, error) {
	var user adminUserRecord
	objectID, err := primitive.ObjectIDFromHex(id)
	if err != nil {
		return user, mongo.ErrNoDocuments
	}
	err = app.db.Collection("users").FindOne(
		ctx, bson.M{"_id": objectID},
	).Decode(&user)
	return user, err
}

func (app *Application) adminUsersCancelSessions(w http.ResponseWriter, r *http.Request) {
	if !adminUsersMethod(w, r, http.MethodPost) {
		return
	}
	action, ok := adminUsersDecode(w, r)
	if !ok {
		return
	}
	// Require an explicit all-sessions flag OR one specific session ID.
	if action.AllSessions == (action.SessionID != "") {
		adminUsersError(w, http.StatusBadRequest, "Choose one session or explicitly choose all sessions.")
		return
	}
	version, ok := adminRequireVersion(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	user, err := app.adminUsersTarget(ctx, action.UserID)
	if errors.Is(err, mongo.ErrNoDocuments) {
		adminConflict(w, errAdminConflict)
		return
	}
	if err != nil {
		adminUsersError(w, http.StatusInternalServerError, "Unable to load the selected user.")
		return
	}
	if user.Version != version {
		adminConflict(w, errAdminConflict)
		return
	}
	filter := bson.M{"username": user.Username, "tenantId": user.TenantID}
	if !action.AllSessions {
		filter["sessionId"] = action.SessionID
	}
	result, err := app.db.Collection("sessions").DeleteMany(ctx, filter)
	if err != nil {
		log.Printf("admin cancel sessions: %v", err)
		adminUsersError(w, http.StatusInternalServerError, "Unable to cancel sessions.")
		return
	}
	// Monitor administrator sessions are independent of Ref Ledger sessions.
	currentCancelled := false
	adminUsersJSON(w, http.StatusOK, map[string]interface{}{
		"message":        fmt.Sprintf("Cancelled %d session(s). User data was preserved.", result.DeletedCount),
		"cancelledCount": result.DeletedCount, "currentSessionCancelled": currentCancelled,
	})
}

var errAdminSharedTenant = errors.New("multiple users share this tenant")
var errAdminDeleteConfirmation = errors.New("email confirmation does not match")
var errAdminInvalidTenant = errors.New("missing or invalid tenant ID")

func (app *Application) adminUsersDelete(w http.ResponseWriter, r *http.Request) {
	if !adminUsersMethod(w, r, http.MethodPost) {
		return
	}
	action, ok := adminUsersDecode(w, r)
	if !ok {
		return
	}
	version, ok := adminRequireVersion(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	db := app.db
	// Include every ordinary collection, including future tenant-owned collections.
	// Shared records without this tenantId (for example global roles/sports) remain.
	collections, err := db.ListCollectionNames(ctx, bson.M{"type": "collection"})
	if err != nil {
		adminUsersError(w, http.StatusInternalServerError, "Unable to enumerate account data. Nothing was deleted.")
		return
	}
	transaction, err := app.db.Client().StartSession()
	if err != nil {
		adminUsersError(w, http.StatusInternalServerError, "Unable to begin account deletion. Nothing was deleted.")
		return
	}
	defer transaction.EndSession(context.Background())
	// Validate the destructive target before contacting Auth0. Never select an
	// Auth0 identity by email: different connections can share an email address.
	target, err := app.adminUsersTarget(ctx, action.UserID)
	if err != nil {
		if adminConflict(w, err) {
			return
		}
		adminUsersError(w, http.StatusInternalServerError, "Unable to load user. Refresh before retrying.")
		return
	}
	if target.Version != version {
		adminConflict(w, errAdminConflict)
		return
	}
	if target.TenantID == "" || strings.TrimSpace(target.TenantID) != target.TenantID || target.TenantID == "na" || target.TenantID == "anonymous" || action.ConfirmationUsername != target.Username {
		adminUsersError(w, http.StatusBadRequest, "Deletion stopped: invalid tenant or email confirmation.")
		return
	}
	count, err := db.Collection("users").CountDocuments(ctx, bson.M{"tenantId": target.TenantID})
	if err != nil || count != 1 {
		adminUsersError(w, http.StatusConflict, "Deletion stopped: unable to confirm a single-user tenant.")
		return
	}
	// A stored identity must not be shared by another local account.
	identityCount, err := db.Collection("users").CountDocuments(ctx, bson.M{
		"mobileIdentity.issuer":  target.MobileIdentity.Issuer,
		"mobileIdentity.subject": target.MobileIdentity.Subject,
	})
	if err != nil || identityCount != 1 {
		adminUsersError(w, http.StatusConflict, "Deletion stopped: Auth0 identity is missing, shared, or could not be verified.")
		return
	}
	auth0, err := newAdminAuth0Client(target.MobileIdentity)
	if err != nil {
		adminUsersError(w, http.StatusServiceUnavailable, "Nothing was deleted: "+err.Error())
		return
	}
	if err := auth0.deleteIdentity(ctx, target.MobileIdentity.Subject); err != nil {
		adminUsersError(w, http.StatusBadGateway, "MongoDB account retained. "+err.Error())
		return
	}
	txnOptions := options.Transaction().SetReadConcern(readconcern.Snapshot()).SetWriteConcern(writeconcern.Majority())
	var deleted map[string]int64
	_, err = transaction.WithTransaction(ctx, func(sc mongo.SessionContext) (interface{}, error) {
		// Re-read within the transaction so ownership and target are checked together.
		user, err := app.adminUsersTarget(sc, action.UserID)
		if err != nil {
			return nil, err
		}
		if user.Version != version {
			return nil, errAdminConflict
		}
		if user.Username != target.Username || user.TenantID != target.TenantID || user.MobileIdentity != target.MobileIdentity {
			return nil, errors.New("account identity changed during deletion")
		}
		if user.TenantID == "" || strings.TrimSpace(user.TenantID) != user.TenantID || user.TenantID == "na" || user.TenantID == "anonymous" {
			return nil, errAdminInvalidTenant
		}
		if action.ConfirmationUsername != user.Username {
			return nil, errAdminDeleteConfirmation
		}
		count, err := db.Collection("users").CountDocuments(sc, bson.M{"tenantId": user.TenantID})
		if err != nil {
			return nil, err
		}
		if count != 1 {
			return nil, errAdminSharedTenant
		}
		deleted = make(map[string]int64)
		for _, collection := range collections {
			if collection == "users" || strings.HasPrefix(collection, "system.") {
				continue
			}
			filter := bson.M{"tenantId": user.TenantID}
			if collection == "sessions" {
				// Also remove older sessions missing tenantId for this unique username.
				filter = bson.M{"$or": bson.A{filter, bson.M{"username": user.Username, "tenantId": bson.M{"$exists": false}}}}
			}
			result, err := db.Collection(collection).DeleteMany(sc, filter)
			if err != nil {
				return nil, fmt.Errorf("delete tenant records in %s: %w", collection, err)
			}
			deleted[collection] = result.DeletedCount
		}
		result, err := db.Collection("users").DeleteOne(sc, adminSnapshotFilter(user.raw))
		if err != nil {
			return nil, err
		}
		if result.DeletedCount != 1 {
			return nil, mongo.ErrNoDocuments
		}
		deleted["users"] = result.DeletedCount
		return nil, nil
	}, txnOptions)
	if err != nil {
		log.Printf("admin account deletion: Auth0 removed; MongoDB cleanup failed: %v", err)
		adminUsersError(w, http.StatusInternalServerError,
			"Auth0 identity removed, but MongoDB cleanup could not be confirmed. Refresh and retry deletion. If the account or tenant changed, review it before retrying. MongoDB must support transactions (replica set or Atlas).")
		return
	}
	adminUsersJSON(w, http.StatusOK, map[string]interface{}{
		"message": "The Auth0 identity, MongoDB user, tenant records, and sessions were deleted.", "deletedCounts": deleted,
	})
}
