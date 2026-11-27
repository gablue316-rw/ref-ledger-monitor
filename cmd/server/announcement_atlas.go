package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/writeconcern"
)

const announcementAtlasPath = announcementMonitorPath + "/atlas"

type announcementCommandRepository struct {
	client      *mongo.Client
	collection  *mongo.Collection
	environment string
}

func configureAnnouncementCommands(cfg Config) (*announcementCommandRepository, error) {
	mode := os.Getenv("ANNOUNCEMENTS_COMMAND_MODE")
	if mode == "" || mode == "http" {
		return nil, nil
	}
	raw, database := os.Getenv("ANNOUNCEMENTS_COMMAND_URI"), os.Getenv("ANNOUNCEMENTS_COMMAND_DATABASE")
	if mode != "atlas" || raw == "" || !logsName.MatchString(database) || !logsName.MatchString(cfg.Environment) {
		return nil, errors.New("invalid announcement command configuration")
	}
	if err := validateLogsURI(raw, strings.EqualFold(cfg.Environment, "development")); err != nil {
		return nil, errors.New("invalid announcement command connection configuration")
	}
	u, err := url.Parse(raw)
	if err != nil || u.User == nil || u.User.Username() == "" {
		return nil, errors.New("announcement commands require a dedicated database user")
	}
	for _, other := range []string{cfg.MongoURI, os.Getenv("LOGS_READER_URI"), os.Getenv("LOGS_AUDIT_URI"), os.Getenv("LOGS_COMMAND_URI")} {
		if other == "" {
			continue
		}
		otherURL, err := url.Parse(other)
		if err != nil || otherURL.User != nil && otherURL.User.Username() == u.User.Username() {
			return nil, errors.New("announcement commands require a dedicated database user")
		}
	}
	client, err := mongo.Connect(context.Background(), options.Client().ApplyURI(raw).
		SetAppName("ref-ledger-monitor-announcement-commands").SetServerSelectionTimeout(5*time.Second).
		SetConnectTimeout(5*time.Second).SetWriteConcern(writeconcern.Majority()))
	if err != nil {
		log.Print("announcements atlas category=client_initialization")
		return nil, errors.New("announcement command client initialization failed")
	}
	return &announcementCommandRepository{client: client, collection: client.Database(database).Collection("refLedgerAnnouncementCommands"), environment: cfg.Environment}, nil
}

func (repo *announcementCommandRepository) close() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if repo.client.Disconnect(ctx) != nil {
		log.Print("announcements atlas category=disconnect")
	}
}

type atlasAnnouncementRequest struct {
	Message         string `json:"message" bson:"message"`
	Audience        string `json:"audience" bson:"audience"`
	TenantID        string `json:"tenantId,omitempty" bson:"tenantId,omitempty"`
	UserID          string `json:"userId,omitempty" bson:"userId,omitempty"`
	Username        string `json:"username,omitempty" bson:"username,omitempty"`
	SessionTargetID string `json:"sessionTargetId,omitempty" bson:"sessionTargetId,omitempty"`
	StartsAt        string `json:"startsAt,omitempty" bson:"startsAt,omitempty"`
	EndsAt          string `json:"endsAt,omitempty" bson:"endsAt,omitempty"`
	Timezone        string `json:"timezone,omitempty" bson:"timezone,omitempty"`
	Version         *int   `json:"version,omitempty" bson:"version,omitempty"`
}

type AtlasAnnouncementInput struct {
	RequestID      string                    `json:"requestId" bson:"requestId"`
	Type           string                    `json:"type" bson:"type"`
	After          string                    `json:"after,omitempty" bson:"after,omitempty"`
	AnnouncementID string                    `json:"announcementId,omitempty" bson:"announcementId,omitempty"`
	Username       string                    `json:"username,omitempty" bson:"username,omitempty"`
	TenantID       string                    `json:"tenantId,omitempty" bson:"tenantId,omitempty"`
	UserID         string                    `json:"userId,omitempty" bson:"userId,omitempty"`
	Request        *atlasAnnouncementRequest `json:"request,omitempty" bson:"request,omitempty"`
}

type atlasAnnouncementCommand struct {
	ID                     string `bson:"_id"`
	AtlasAnnouncementInput `bson:",inline"`
	Version                int       `bson:"version"`
	State                  string    `bson:"state"`
	Actor                  string    `bson:"actor"`
	Environment            string    `bson:"environment"`
	WorkerEnvironment      string    `bson:"workerEnvironment"`
	CreatedAt              time.Time `bson:"createdAt"`
	ExpiresAt              time.Time `bson:"expiresAt"`
	Outcome                string    `bson:"outcome,omitempty"`
	Reason                 string    `bson:"reason,omitempty"`
	Result                 bson.Raw  `bson:"result,omitempty"`
}

// Reject duplicate JSON keys at every depth before decoding the typed allowlist.
func uniqueJSONValue(d *json.Decoder) error {
	token, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return err
			}
			k, ok := key.(string)
			if !ok || seen[k] {
				return errors.New("duplicate key")
			}
			seen[k] = true
			if err := uniqueJSONValue(d); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err := uniqueJSONValue(d); err != nil {
				return err
			}
		}
	default:
		return errors.New("invalid value")
	}
	_, err = d.Token()
	return err
}

func validAtlasInput(in AtlasAnnouncementInput) bool {
	if !logsCommandID.MatchString(in.RequestID) {
		return false
	}
	objectID := func(s string) bool {
		_, err := primitive.ObjectIDFromHex(s)
		return err == nil && strings.ToLower(s) == s
	}
	tenantID := func(s string) bool {
		return s != "" && len(s) <= 256 && strings.TrimSpace(s) == s && !strings.ContainsAny(s, "\x00\r\n")
	}
	switch in.Type {
	case "list", "history":
		if in.After != "" && !logsCommandID.MatchString(in.After) {
			return false
		}
	case "detail", "clear", "edit", "delete":
		if !logsCommandID.MatchString(in.AnnouncementID) {
			return false
		}
	case "accounts":
		if len(in.Username) > 254 || in.After != "" && !objectID(in.After) {
			return false
		}
	case "sessions":
		if !tenantID(in.TenantID) || !objectID(in.UserID) || in.After != "" && !objectID(in.After) {
			return false
		}
	case "publish":
	default:
		return false
	}
	if in.Type != "list" && in.Type != "history" && in.Type != "accounts" && in.Type != "sessions" && in.After != "" ||
		in.Type != "detail" && in.Type != "clear" && in.Type != "edit" && in.Type != "delete" && in.AnnouncementID != "" ||
		in.Type != "accounts" && in.Username != "" ||
		in.Type != "sessions" && (in.TenantID != "" || in.UserID != "") ||
		(in.Type == "publish" || in.Type == "edit") != (in.Request != nil) {
		return false
	}
	if in.Request == nil {
		return true
	}
	r := in.Request
	if !utf8.ValidString(r.Message) || utf8.RuneCountInString(r.Message) < 1 || utf8.RuneCountInString(r.Message) > 4000 ||
		len(r.Username) > 254 || len(r.SessionTargetID) > 128 || len(r.Timezone) > 128 || len(r.StartsAt) > 128 || len(r.EndsAt) > 128 {
		return false
	}
	for _, c := range r.Message {
		if unicode.IsControl(c) && c != '\n' && c != '\t' {
			return false
		}
	}
	if in.Type == "edit" && (r.Version == nil || *r.Version < 1) || in.Type == "publish" && r.Version != nil {
		return false
	}
	switch r.Audience {
	case "system":
		if r.TenantID != "" || r.UserID != "" || r.Username != "" || r.SessionTargetID != "" {
			return false
		}
	case "user", "session":
		if !tenantID(r.TenantID) || !objectID(r.UserID) || r.Audience == "session" && r.SessionTargetID == "" || r.Audience == "user" && r.SessionTargetID != "" {
			return false
		}
	default:
		return false
	}
	return (r.StartsAt == "" && r.EndsAt == "") || r.Timezone != ""
}

func (app *Application) announcementAtlasHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	actor, ok := r.Context().Value(adminActorKey{}).(string)
	if !ok || actor == "" {
		adminUsersError(w, http.StatusUnauthorized, "Administrator sign-in required.")
		return
	}
	actor = logsThresholdActor(actor)
	if r.URL.RawQuery != "" {
		adminUsersError(w, 400, "Unexpected query parameters.")
		return
	}
	if r.URL.Path == announcementAtlasPath {
		if !adminUsersMethod(w, r, http.MethodGet) {
			return
		}
		mode, environment := "http", app.config.Environment
		if environment == "" {
			environment = "development"
		}
		if app.announcementCommands != nil {
			mode, environment = "atlas", app.announcementCommands.environment
		}
		writeJSON(w, 200, map[string]string{"mode": mode, "actor": actor, "environment": environment})
		return
	}
	submit := r.URL.Path == announcementAtlasPath+"/commands"
	if submit {
		if !adminUsersMethod(w, r, http.MethodPost) {
			return
		}
	} else {
		if !strings.HasPrefix(r.URL.Path, announcementAtlasPath+"/commands/") {
			http.NotFound(w, r)
			return
		}
		if !adminUsersMethod(w, r, http.MethodGet) {
			return
		}
	}
	repo := app.announcementCommands
	if repo == nil || repo.collection == nil {
		adminUsersError(w, 503, "Atlas commands are unavailable.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if submit {
		data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
		if err != nil {
			adminUsersError(w, 400, "Invalid JSON request.")
			return
		}
		check := json.NewDecoder(bytes.NewReader(data))
		if uniqueJSONValue(check) != nil {
			adminUsersError(w, 400, "Invalid JSON request.")
			return
		}
		var input *AtlasAnnouncementInput
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.DisallowUnknownFields()
		if dec.Decode(&input) != nil || input == nil || dec.Decode(&struct{}{}) != io.EOF || !validAtlasInput(*input) || !validAtlasFields(data, input.Type) {
			adminUsersError(w, 400, "Invalid announcement command.")
			return
		}
		now := time.Now().UTC().Truncate(time.Millisecond)
		command := atlasAnnouncementCommand{ID: input.RequestID, AtlasAnnouncementInput: *input, Version: 1, State: "pending",
			Actor: actor, Environment: repo.environment, WorkerEnvironment: repo.environment, CreatedAt: now, ExpiresAt: now.Add(15 * time.Minute)}
		encoded, err := bson.Marshal(command)
		if err != nil || len(encoded) > 64<<10 {
			adminUsersError(w, 400, "Invalid announcement command.")
			return
		}
		_, err = repo.collection.InsertOne(ctx, command)
		if mongo.IsDuplicateKeyError(err) {
			var existing atlasAnnouncementCommand
			err = repo.collection.FindOne(ctx, bson.M{"_id": command.ID, "environment": repo.environment, "workerEnvironment": repo.environment, "actor": actor}).Decode(&existing)
			if errors.Is(err, mongo.ErrNoDocuments) {
				adminUsersError(w, 409, "Command ID is already in use.")
				return
			}
			if err == nil && (existing.Version != 1 || !reflect.DeepEqual(existing.AtlasAnnouncementInput, command.AtlasAnnouncementInput)) {
				adminUsersError(w, 409, "Command ID already belongs to a different request.")
				return
			}
		}
		if err != nil {
			log.Print("announcements atlas category=submission_unconfirmed")
			adminUsersError(w, 503, "Command submission could not be confirmed. Check or retry the same request ID.")
			return
		}
		writeJSON(w, 202, map[string]string{"requestId": input.RequestID})
		return
	}
	id := strings.TrimPrefix(r.URL.Path, announcementAtlasPath+"/commands/")
	if !logsCommandID.MatchString(id) {
		adminUsersError(w, 400, "Invalid command ID.")
		return
	}
	var command atlasAnnouncementCommand
	err := repo.collection.FindOne(ctx, bson.M{"_id": id, "requestId": id, "environment": repo.environment, "workerEnvironment": repo.environment, "actor": actor}).Decode(&command)
	if errors.Is(err, mongo.ErrNoDocuments) {
		adminUsersError(w, 404, "Command not found. Retry only the original request.")
		return
	}
	if err != nil {
		log.Print("announcements atlas category=status_unavailable")
		adminUsersError(w, 503, "Command status is unavailable.")
		return
	}
	status, err := safeAnnouncementStatus(command)
	if err != nil {
		log.Print("announcements atlas category=invalid_status")
		adminUsersError(w, 503, "Command status is unavailable.")
		return
	}
	writeJSON(w, 200, status)
}

func validAtlasFields(data []byte, kind string) bool {
	var object map[string]json.RawMessage
	if json.Unmarshal(data, &object) != nil {
		return false
	}
	allowed := map[string]bool{"requestId": true, "type": true}
	switch kind {
	case "list", "history":
		allowed["after"] = true
	case "accounts":
		allowed["after"], allowed["username"] = true, true
	case "sessions":
		allowed["after"], allowed["tenantId"], allowed["userId"] = true, true, true
	case "detail", "clear", "delete":
		allowed["announcementId"] = true
	case "publish":
		allowed["request"] = true
	case "edit":
		allowed["announcementId"], allowed["request"] = true, true
	}
	for k, v := range object {
		if !allowed[k] || bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return false
		}
	}
	if raw, ok := object["request"]; ok {
		var req map[string]json.RawMessage
		if json.Unmarshal(raw, &req) != nil || req == nil {
			return false
		}
		allowedRequest := map[string]bool{"message": true, "audience": true, "tenantId": true, "userId": true,
			"username": true, "sessionTargetId": true, "startsAt": true, "endsAt": true, "timezone": true}
		if kind == "edit" {
			allowedRequest["version"] = true
		}
		for k, v := range req {
			if !allowedRequest[k] || bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
				return false
			}
		}
	}
	return true
}

var atlasReasons = map[string]bool{
	"malformed": true, "invalid_request_id": true, "invalid_version": true, "invalid_environment": true,
	"invalid_actor": true, "expired_or_invalid_lifetime": true, "invalid_type": true, "invalid_announcement_id": true,
	"invalid_account": true, "invalid_cursor": true, "version_required": true, "invalid_request": true,
	"not_found": true, "invalid_session": true, "conflict": true, "retry_exhausted": true,
}

func safeAnnouncementStatus(command atlasAnnouncementCommand) (map[string]any, error) {
	status := map[string]any{"requestId": command.ID, "state": command.State, "outcome": "", "reason": "", "result": nil}
	if command.State == "pending" || command.State == "processing" {
		return status, nil
	}
	if command.State != "completed" || command.Outcome != "success" && command.Outcome != "rejected" && command.Outcome != "failed" {
		return nil, errors.New("invalid state")
	}
	status["outcome"] = command.Outcome
	if command.Reason != "" && atlasReasons[command.Reason] {
		status["reason"] = command.Reason
	}
	if command.Outcome != "success" {
		return status, nil
	}
	result, err := safeAnnouncementResult(command.Type, command.Result)
	if err != nil {
		return nil, err
	}
	status["result"] = result
	return status, nil
}

func safeAnnouncementResult(kind string, raw bson.Raw) (any, error) {
	var doc bson.M
	if len(raw) == 0 || bson.Unmarshal(raw, &doc) != nil {
		return nil, errors.New("invalid result")
	}
	announcementFields := []string{"id", "message", "audience", "tenantId", "userId", "username", "sessionTargetId", "startsAt", "endsAt", "timezone", "cleared", "clearedAt", "createdBy", "createdAt", "updatedAt", "version"}
	clean := func(value any, fields []string, announcement bool) (map[string]any, error) {
		m, ok := value.(bson.M)
		if !ok {
			return nil, errors.New("invalid result item")
		}
		out := map[string]any{}
		if id, exists := m["_id"]; announcement && exists {
			if s, ok := id.(string); ok {
				out["id"] = s
			} else {
				return nil, errors.New("invalid id")
			}
		}
		for _, k := range fields {
			v, exists := m[k]
			if !exists || v == nil {
				continue
			}
			switch t := v.(type) {
			case string, bool, int32, int64, float64:
				out[k] = t
			case primitive.DateTime:
				out[k] = t.Time().UTC().Format(time.RFC3339Nano)
			default:
				return nil, errors.New("invalid result field")
			}
		}
		return out, nil
	}
	switch kind {
	case "detail", "publish", "edit", "clear", "delete":
		return clean(doc, announcementFields, true)
	case "list", "history", "accounts", "sessions":
		key, fields := "announcements", announcementFields
		if kind == "accounts" {
			key, fields = "accounts", []string{"tenantId", "userId", "username"}
		}
		if kind == "sessions" {
			key, fields = "sessions", []string{"sessionTargetId", "tenantId", "userId", "username", "platform", "createdAt", "expiresAt"}
		}
		items, ok := doc[key].(primitive.A)
		if !ok || len(items) > 100 {
			return nil, errors.New("invalid page")
		}
		cursor, ok := doc["nextCursor"].(string)
		if !ok || len(cursor) > 128 {
			return nil, errors.New("invalid cursor")
		}
		result := make([]map[string]any, 0, len(items))
		for _, item := range items {
			safe, err := clean(item, fields, kind == "list" || kind == "history")
			if err != nil {
				return nil, err
			}
			result = append(result, safe)
		}
		return map[string]any{key: result, "nextCursor": cursor}, nil
	default:
		return nil, errors.New("invalid type")
	}
}
