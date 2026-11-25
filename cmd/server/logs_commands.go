package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/writeconcern"
)

type logsCommandRepository struct {
	client *mongo.Client
}

type logsCommand struct {
	ID                string    `bson:"_id"`
	RequestID         string    `bson:"requestId" json:"requestId"`
	Version           int       `bson:"version"`
	WorkerEnvironment string    `bson:"workerEnvironment"`
	Environment       string    `bson:"environment" json:"environment"`
	Type              string    `bson:"type" json:"type"`
	Actor             string    `bson:"actor"`
	Scope             string    `bson:"scope" json:"scope"`
	TenantID          string    `bson:"tenantId" json:"tenantId"`
	Threshold         int       `bson:"threshold" json:"threshold"`
	Confirmation      string    `bson:"confirmation" json:"confirmation"`
	Cutoff            time.Time `bson:"cutoff" json:"cutoff"`
	State             string    `bson:"state" json:"state"`
	Outcome           string    `bson:"outcome" json:"outcome"`
	Reason            string    `bson:"reason" json:"reason"`
	DeletedCount      int64     `bson:"deletedCount" json:"deletedCount"`
}

var logsCommandID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var logsCommandTenant = regexp.MustCompile(`^[[:alnum:] _.,:@/+()#-]{1,200}$`)

func configureLogsCommands(repo *logsRepository, environment string) (*logsCommandRepository, error) {
	mode := os.Getenv("LOGS_COMMAND_MODE")
	if mode == "" || mode == "https" {
		return nil, nil
	}
	if mode != "atlas" || repo == nil {
		return nil, errors.New("invalid logs command mode or missing logs repository")
	}
	raw := os.Getenv("LOGS_COMMAND_URI")
	if raw == "" {
		return nil, errors.New("logs Atlas command credential is required")
	}
	development := strings.EqualFold(environment, "development")
	if err := validateLogsURI(raw, development); err != nil {
		return nil, err
	}
	if !development {
		commandURL, _ := url.Parse(raw)
		readerURL, _ := url.Parse(os.Getenv("LOGS_READER_URI"))
		auditURL, _ := url.Parse(os.Getenv("LOGS_AUDIT_URI"))
		if commandURL.User == nil || commandURL.User.Username() == "" || readerURL.User == nil || auditURL.User == nil ||
			commandURL.User.Username() == readerURL.User.Username() || commandURL.User.Username() == auditURL.User.Username() {
			return nil, errors.New("logs commands require a separate database user")
		}
	}
	client, err := mongo.Connect(context.Background(), options.Client().ApplyURI(raw).
		SetAppName("ref-ledger-monitor-commands").SetServerSelectionTimeout(5*time.Second).
		SetConnectTimeout(5*time.Second).SetWriteConcern(writeconcern.Majority()))
	if err != nil {
		return nil, errors.New("logs command MongoDB client initialization failed")
	}
	return &logsCommandRepository{client: client}, nil
}

func (repo *logsCommandRepository) close() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := repo.client.Disconnect(ctx); err != nil {
		log.Print("logs command MongoDB disconnect failed")
	}
}

func (repo *logsRepository) permitsCommandTenant(tenant string) bool {
	lower := strings.ToLower(tenant)
	if !logsCommandTenant.MatchString(tenant) || strings.TrimSpace(tenant) != tenant ||
		strings.Contains(lower, "bearer ") || strings.Contains(lower, "mongodb://") || strings.Contains(lower, "mongodb+srv://") {
		return false
	}
	if repo.allTenants {
		return true
	}
	for _, allowed := range repo.tenants {
		if allowed == tenant {
			return true
		}
	}
	return false
}

func (app *Application) logsClearScope(w http.ResponseWriter, r *http.Request) {
	if app.logsCommands == nil {
		adminUsersError(w, http.StatusServiceUnavailable, "Log clearing is unavailable.")
		return
	}
	cutoff, _ := logsCutoff("")
	writeJSON(w, http.StatusOK, map[string]any{"environment": app.logs.environment, "tenants": app.logs.tenants, "allTenants": app.logs.allTenants, "cutoff": cutoff.Format(time.RFC3339Nano)})
}

func (app *Application) logsCommandSubmit(w http.ResponseWriter, r *http.Request) {
	if app.logsCommands == nil {
		adminUsersError(w, http.StatusServiceUnavailable, "Atlas commands are unavailable.")
		return
	}
	actor, ok := r.Context().Value(adminActorKey{}).(string)
	if !ok || actor == "" {
		adminUsersError(w, http.StatusUnauthorized, "Administrator sign-in required.")
		return
	}
	var input struct {
		RequestID    string `json:"requestId"`
		Environment  string `json:"environment"`
		Type         string `json:"type"`
		Scope        string `json:"scope"`
		Threshold    int    `json:"threshold"`
		TenantID     string `json:"tenantId"`
		Confirmation string `json:"confirmation"`
		Cutoff       string `json:"cutoff"`
	}
	if !decodeAdminJSON(w, r, &input) {
		return
	}
	if !logsCommandID.MatchString(input.RequestID) || input.Environment != app.logs.environment {
		adminUsersError(w, http.StatusBadRequest, "Invalid command ID or environment.")
		return
	}
	command := logsCommand{ID: input.RequestID, RequestID: input.RequestID, Version: 1,
		Environment: app.logs.environment, WorkerEnvironment: app.logs.environment,
		Actor: logsThresholdActor(actor), Type: input.Type, State: "pending"}
	switch input.Type {
	case "set_capture_threshold":
		if !app.logs.settings || input.Threshold < 1 || input.Threshold > 4 || input.Scope != "" || input.TenantID != "" || input.Confirmation != "" || input.Cutoff != "" {
			adminUsersError(w, http.StatusBadRequest, "Invalid threshold command.")
			return
		}
		command.Scope, command.Threshold = "environment", input.Threshold
	case "clear_logs":
		cutoff, err := logsCutoff(input.Cutoff)
		if input.Scope == "environment" && !app.logs.allTenants {
			adminUsersError(w, http.StatusForbidden, "Environment-wide clearing is outside the authorized scope.")
			return
		}
		validScope := input.Scope == "environment" && input.TenantID == "" ||
			input.Scope == "tenant" && app.logs.permitsCommandTenant(input.TenantID)
		if input.Cutoff == "" || err != nil || !validScope || input.Confirmation != "CLEAR "+app.logs.environment || input.Threshold != 0 {
			adminUsersError(w, http.StatusBadRequest, "Invalid or unauthorized clear scope, cutoff, or confirmation.")
			return
		}
		command.Scope, command.TenantID, command.Confirmation, command.Cutoff = input.Scope, input.TenantID, input.Confirmation, cutoff.Truncate(time.Millisecond)
	default:
		adminUsersError(w, http.StatusBadRequest, "Invalid command type.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	collection := app.logsCommands.client.Database(app.logs.database).Collection("refLedgerLogCommands")
	_, err := collection.InsertOne(ctx, command)
	if mongo.IsDuplicateKeyError(err) {
		var existing logsCommand
		err = collection.FindOne(ctx, bson.M{"_id": command.ID, "environment": command.Environment, "actor": command.Actor}).Decode(&existing)
		if err == nil && (existing.Type != command.Type || existing.Scope != command.Scope || existing.TenantID != command.TenantID ||
			existing.Threshold != command.Threshold || existing.Confirmation != command.Confirmation || !existing.Cutoff.Equal(command.Cutoff)) {
			adminUsersError(w, http.StatusConflict, "Command ID already belongs to a different request.")
			return
		}
	}
	if err != nil {
		adminUsersError(w, http.StatusServiceUnavailable, "Command submission could not be confirmed. Check or retry the same request ID.")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"requestId": command.ID})
}

func (app *Application) logsCommandStatus(w http.ResponseWriter, r *http.Request) {
	if app.logsCommands == nil {
		adminUsersError(w, http.StatusServiceUnavailable, "Atlas commands are unavailable.")
		return
	}
	actor, ok := r.Context().Value(adminActorKey{}).(string)
	if !ok || actor == "" {
		adminUsersError(w, http.StatusUnauthorized, "Administrator sign-in required.")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, logsAdminMonitorPrefix+"/commands/")
	if !logsCommandID.MatchString(id) {
		adminUsersError(w, http.StatusBadRequest, "Invalid command ID.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	var command logsCommand
	err := app.logsCommands.client.Database(app.logs.database).Collection("refLedgerLogCommands").FindOne(ctx, bson.M{"_id": id, "environment": app.logs.environment, "workerEnvironment": app.logs.environment, "actor": logsThresholdActor(actor)}).Decode(&command)
	if errors.Is(err, mongo.ErrNoDocuments) {
		adminUsersError(w, http.StatusNotFound, "Command not found. Retry only the original request.")
		return
	}
	if err != nil {
		adminUsersError(w, http.StatusServiceUnavailable, "Command status is unavailable.")
		return
	}
	if command.State != "pending" && (command.State != "completed" || command.Outcome != "success" && command.Outcome != "rejected") {
		adminUsersError(w, http.StatusServiceUnavailable, "Command status is invalid.")
		return
	}
	writeJSON(w, http.StatusOK, command)
}
