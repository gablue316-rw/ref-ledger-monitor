package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type logsRepository struct {
	reader      *mongo.Client
	auditor     *mongo.Client
	database    string
	environment string
	tenants     []string
	allTenants  bool
	settings    bool
}

var logsName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

const (
	logsAdminMonitorPrefix = "/api/admin/logs"
	logsAdminThresholdPath = logsAdminMonitorPrefix + "/threshold"
)

func validateLogsURI(raw string, development bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Fragment != "" {
		return errors.New("invalid logs MongoDB connection configuration")
	}
	if u.Scheme != "mongodb+srv" {
		host, _, splitErr := net.SplitHostPort(u.Host)
		ip := net.ParseIP(host)
		if !development || u.Scheme != "mongodb" || splitErr != nil || !(strings.EqualFold(host, "localhost") || ip != nil && ip.IsLoopback()) {
			return errors.New("logs MongoDB connections require Atlas TLS")
		}
	}
	for key, values := range u.Query() {
		switch strings.ToLower(key) {
		case "authsource", "appname", "replicaset", "directconnection", "retryreads":
		case "tls", "ssl":
			if len(values) != 1 || !strings.EqualFold(values[0], "true") {
				return errors.New("logs MongoDB TLS cannot be disabled")
			}
		default:
			return errors.New("unsupported logs MongoDB connection option")
		}
	}
	return nil
}

func configureLogsRepository() (*logsRepository, error) {
	readerURI, auditURI := os.Getenv("LOGS_READER_URI"), os.Getenv("LOGS_AUDIT_URI")
	db, environment, scope := strings.TrimSpace(os.Getenv("LOGS_DATABASE")), strings.TrimSpace(os.Getenv("LOGS_ENVIRONMENT")), strings.TrimSpace(os.Getenv("LOGS_TENANTS"))
	if readerURI == "" && auditURI == "" && db == "" && environment == "" && scope == "" {
		return nil, nil
	}
	if readerURI == "" || auditURI == "" || !logsName.MatchString(db) || !logsName.MatchString(environment) || scope == "" {
		return nil, errors.New("logs repository requires reader, audit, database, environment and tenant scope configuration")
	}
	development := strings.EqualFold(strings.TrimSpace(os.Getenv("APP_ENV")), "development")
	if err := validateLogsURI(readerURI, development); err != nil {
		return nil, err
	}
	if err := validateLogsURI(auditURI, development); err != nil {
		return nil, err
	}
	if !development {
		readerURL, _ := url.Parse(readerURI)
		auditURL, _ := url.Parse(auditURI)
		if readerURL.User == nil || auditURL.User == nil || readerURL.User.Username() == "" || auditURL.User.Username() == "" || readerURL.User.Username() == auditURL.User.Username() {
			return nil, errors.New("logs reader and audit require separate database users")
		}
	}
	repo := &logsRepository{database: db, environment: environment, allTenants: scope == "*", settings: os.Getenv("LOGS_READ_SETTINGS") == "true"}
	if !repo.allTenants {
		seen := map[string]bool{}
		for _, tenant := range strings.Split(scope, ",") {
			tenant = strings.TrimSpace(tenant)
			if tenant == "" || tenant == "*" || len(tenant) > 200 || strings.ContainsAny(tenant, "\r\n\x00") || seen[tenant] {
				return nil, errors.New("invalid logs tenant scope")
			}
			seen[tenant] = true
			repo.tenants = append(repo.tenants, tenant)
		}
	}
	newClient := func(uri string) (*mongo.Client, error) {
		client, err := mongo.Connect(context.Background(), options.Client().ApplyURI(uri).SetAppName("ref-ledger-monitor-logs").SetServerSelectionTimeout(5*time.Second).SetConnectTimeout(5*time.Second))
		if err != nil {
			return nil, errors.New("logs MongoDB client initialization failed")
		}
		return client, nil
	}
	var err error
	repo.reader, err = newClient(readerURI)
	if err != nil {
		return nil, err
	}
	repo.auditor, err = newClient(auditURI)
	if err != nil {
		repo.close()
		return nil, err
	}
	return repo, nil
}

func (repo *logsRepository) close() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, client := range []*mongo.Client{repo.reader, repo.auditor} {
		if client != nil {
			if err := client.Disconnect(ctx); err != nil {
				log.Print("logs MongoDB disconnect failed")
			}
		}
	}
}

type logRecord struct {
	ID            primitive.ObjectID `bson:"_id" json:"-"`
	SchemaVersion int                `bson:"schemaVersion" json:"schemaVersion"`
	Timestamp     time.Time          `bson:"timestamp" json:"-"`
	Environment   string             `bson:"environment" json:"environment"`
	Level         int                `bson:"level" json:"level"`
	Event         string             `bson:"event" json:"event"`
	Message       string             `bson:"message" json:"message"`
	TenantID      *string            `bson:"tenantId" json:"tenantId"`
	UserID        *string            `bson:"userId" json:"userId"`
	SessionID     *string            `bson:"sessionId" json:"sessionId"`
	TransactionID *string            `bson:"transactionId" json:"transactionId"`
	Collection    *string            `bson:"collection" json:"collection"`
	Operation     *string            `bson:"operation" json:"operation"`
	Outcome       *string            `bson:"outcome" json:"outcome"`
	ElapsedMS     *int64             `bson:"elapsedMs" json:"elapsedMs"`
	MongoID       *string            `bson:"mongoId" json:"mongoId"`
	ErrorCategory *string            `bson:"errorCategory" json:"errorCategory"`
	DriverCode    *int64             `bson:"driverCode" json:"driverCode"`
	Attributes    map[string]any     `bson:"attributes" json:"attributes"`
}

func (entry logRecord) MarshalJSON() ([]byte, error) {
	type recordJSON logRecord
	return json.Marshal(struct {
		recordJSON
		Timestamp string `json:"timestamp"`
	}{recordJSON(entry), entry.Timestamp.UTC().Format("2006-01-02T15:04:05.000Z")})
}

type logQuery struct {
	DateFrom, DateThrough, TimeFrom, TimeThrough, TimeAt, TimeZone string
	Level                                                          int
	Collection, TenantID, SessionID, TransactionID, UserID         string
	Offset, Limit                                                  int64
}

func (repo *logsRepository) filter(query logQuery, cutoff time.Time) (bson.M, error) {
	filter := bson.M{"environment": repo.environment, "timestamp": bson.M{"$lte": cutoff}}
	if !repo.allTenants {
		filter["tenantId"] = bson.M{"$in": repo.tenants}
	}
	if query.TenantID != "" && !repo.allTenants {
		allowed := false
		for _, tenant := range repo.tenants {
			allowed = allowed || tenant == query.TenantID
		}
		if !allowed {
			return nil, errors.New("tenant is outside authorized scope")
		}
	}
	if query.Level < 0 || query.Level > 4 {
		return nil, errors.New("invalid level")
	}
	if query.Level != 0 {
		filter["level"] = query.Level
	}
	for key, value := range map[string]string{"tenantId": query.TenantID, "userId": query.UserID, "sessionId": query.SessionID, "transactionId": query.TransactionID, "collection": query.Collection} {
		if value != "" {
			if len(value) > 200 || strings.ContainsAny(value, "\r\n\x00") {
				return nil, errors.New("invalid identifier")
			}
			filter[key] = value
		}
	}
	zone := query.TimeZone
	if zone == "" {
		zone = "UTC"
	}
	location, err := time.LoadLocation(zone)
	if err != nil {
		return nil, errors.New("invalid time zone")
	}
	if query.DateFrom != "" || query.DateThrough != "" {
		var start, end time.Time
		if query.DateFrom != "" {
			start, err = time.ParseInLocation("2006-01-02", query.DateFrom, location)
			if err != nil {
				return nil, errors.New("invalid start date")
			}
		}
		if query.DateThrough != "" {
			end, err = time.ParseInLocation("2006-01-02", query.DateThrough, location)
			if err != nil {
				return nil, errors.New("invalid end date")
			}
			if start.IsZero() {
				start = end
			}
		} else {
			end = start
		}
		end = end.AddDate(0, 0, 1)
		if !start.Before(end) {
			return nil, errors.New("invalid date range")
		}
		filter["timestamp"] = bson.M{"$gte": start.UTC(), "$lt": end.UTC(), "$lte": cutoff}
	}
	expression := bson.M{"$dateToString": bson.M{"date": "$timestamp", "format": "%H:%M", "timezone": location.String()}}
	if query.TimeAt != "" && (query.TimeFrom != "" || query.TimeThrough != "") {
		return nil, errors.New("choose one time filter")
	}
	for _, value := range []string{query.TimeAt, query.TimeFrom, query.TimeThrough} {
		if value != "" {
			parsed, err := time.Parse("15:04", value)
			if err != nil || parsed.Format("15:04") != value {
				return nil, errors.New("invalid time")
			}
		}
	}
	if query.TimeAt != "" {
		filter["$expr"] = bson.M{"$eq": bson.A{expression, query.TimeAt}}
	} else if query.TimeFrom != "" || query.TimeThrough != "" {
		comparisons := bson.A{}
		if query.TimeFrom != "" {
			comparisons = append(comparisons, bson.M{"$gte": bson.A{expression, query.TimeFrom}})
		}
		if query.TimeThrough != "" {
			comparisons = append(comparisons, bson.M{"$lte": bson.A{expression, query.TimeThrough}})
		}
		if len(comparisons) == 1 {
			filter["$expr"] = comparisons[0]
		} else if query.TimeFrom > query.TimeThrough {
			filter["$expr"] = bson.M{"$or": comparisons}
		} else {
			filter["$expr"] = bson.M{"$and": comparisons}
		}
	}
	return filter, nil
}

func parseAtlasQuery(values url.Values) (logQuery, error) {
	q := logQuery{DateFrom: values.Get("dateFrom"), DateThrough: values.Get("dateThrough"), TimeAt: values.Get("timeAt"), TimeFrom: values.Get("timeFrom"), TimeThrough: values.Get("timeThrough"), TimeZone: values.Get("timeZone"), Collection: values.Get("collection"), TenantID: values.Get("tenantId"), SessionID: values.Get("sessionId"), TransactionID: values.Get("transactionId"), UserID: values.Get("userId"), Limit: 50}
	var err error
	if v := values.Get("level"); v != "" {
		q.Level, err = strconv.Atoi(v)
		if err != nil || q.Level < 1 || q.Level > 4 {
			return q, errors.New("invalid level")
		}
	}
	if v := values.Get("offset"); v != "" {
		q.Offset, err = strconv.ParseInt(v, 10, 64)
		if err != nil || q.Offset < 0 || q.Offset > 100000 {
			return q, errors.New("invalid offset")
		}
	}
	if v := values.Get("limit"); v != "" {
		q.Limit, err = strconv.ParseInt(v, 10, 64)
		if err != nil || q.Limit < 1 || q.Limit > 200 {
			return q, errors.New("invalid limit")
		}
	}
	return q, nil
}

func logsCutoff(value string) (time.Time, error) {
	if value == "" {
		return time.Now().UTC().Truncate(time.Millisecond).Add(-time.Millisecond), nil
	}
	cutoff, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || cutoff.After(time.Now()) {
		return time.Time{}, errors.New("invalid cutoff")
	}
	return cutoff.UTC(), nil
}

func (app *Application) logsAtlasHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if (r.URL.Path == logsAdminThresholdPath && r.Method != http.MethodGet && r.Method != http.MethodPut) || r.URL.Path == logsAdminMonitorPrefix+"/clear" {
		adminUsersError(w, http.StatusMethodNotAllowed, "Log changes are unavailable during Phase 1.")
		return
	}
	if app.logs == nil {
		adminUsersError(w, http.StatusServiceUnavailable, "Ref Ledger logs are not configured.")
		return
	}
	switch {
	case r.URL.Path == logsAdminThresholdPath && r.Method == http.MethodGet:
		app.logsThresholdAtlas(w, r)
	case r.URL.Path == logsAdminThresholdPath && r.Method == http.MethodPut:
		app.logsThresholdUpdate(w, r)
	case r.URL.Path == logsAdminMonitorPrefix+"/records" && r.Method == http.MethodGet:
		app.logsRecordsAtlas(w, r)
	case r.URL.Path == logsAdminMonitorPrefix+"/export" && r.Method == http.MethodPost:
		app.logsExportAtlas(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (app *Application) logsThresholdAtlas(w http.ResponseWriter, r *http.Request) {
	threshold, err := app.logs.captureThreshold(r.Context())
	if err != nil {
		adminUsersError(w, http.StatusServiceUnavailable, "Capture threshold is unavailable.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"threshold": threshold, "environment": app.logs.environment, "updateEnabled": app.logsUpdater != nil})
}

func (repo *logsRepository) captureThreshold(parent context.Context) (int, error) {
	if !repo.settings {
		return 0, errors.New("settings unavailable")
	}
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	var setting struct {
		Threshold int `bson:"threshold"`
	}
	err := repo.reader.Database(repo.database).Collection("refLedgerLogSettings").FindOne(ctx, bson.M{"_id": "capture:" + repo.environment}).Decode(&setting)
	if errors.Is(err, mongo.ErrNoDocuments) {
		setting.Threshold = 3
		err = nil
	}
	if err != nil || setting.Threshold < 1 || setting.Threshold > 4 {
		return 0, errors.New("settings unavailable")
	}
	return setting.Threshold, nil
}

func (app *Application) logsRecordsAtlas(w http.ResponseWriter, r *http.Request) {
	query, err := parseAtlasQuery(r.URL.Query())
	if err != nil {
		adminUsersError(w, http.StatusBadRequest, "Invalid log filters or pagination.")
		return
	}
	cutoff, err := logsCutoff(r.URL.Query().Get("cutoff"))
	if err != nil {
		adminUsersError(w, http.StatusBadRequest, "Invalid log cutoff.")
		return
	}
	filter, err := app.logs.filter(query, cutoff)
	if err != nil {
		adminUsersError(w, http.StatusBadRequest, "Invalid or unauthorized log filters.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	cursor, err := app.logs.reader.Database(app.logs.database).Collection("refLedgerLogs").Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "timestamp", Value: -1}, {Key: "_id", Value: -1}}).SetSkip(query.Offset).SetLimit(query.Limit+1).SetMaxTime(15*time.Second))
	if err != nil {
		adminUsersError(w, http.StatusServiceUnavailable, "Unable to query Ref Ledger logs.")
		return
	}
	defer cursor.Close(ctx)
	items := make([]logRecord, 0, query.Limit+1)
	if err := cursor.All(ctx, &items); err != nil {
		adminUsersError(w, http.StatusServiceUnavailable, "Unable to query Ref Ledger logs.")
		return
	}
	more := int64(len(items)) > query.Limit
	if more {
		items = items[:query.Limit]
	}
	zone := query.TimeZone
	if zone == "" {
		zone = "UTC"
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "hasMore": more, "cutoff": cutoff.Format(time.RFC3339Nano), "environment": app.logs.environment, "timeZone": zone})
}

func (repo *logsRepository) audit(actor, id, scope, outcome string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	hash := sha256.Sum256([]byte(actor))
	_, err := repo.auditor.Database(repo.database).Collection("monitorLogExportAudit").InsertOne(ctx, bson.M{
		"timestamp": time.Now().UTC(), "environment": repo.environment, "actor": "sha256:" + hex.EncodeToString(hash[:]), "exportId": id, "action": "export", "scope": scope, "authorizedTenants": repo.tenants, "allTenants": repo.allTenants, "outcome": outcome,
	})
	return err
}

func (app *Application) logsExportAtlas(w http.ResponseWriter, r *http.Request) {
	var request struct {
		All     bool     `json:"all"`
		Filters logQuery `json:"filters"`
	}
	if !decodeAdminJSON(w, r, &request) {
		return
	}
	if request.All {
		request.Filters = logQuery{}
	}
	cutoff, _ := logsCutoff("")
	filter, err := app.logs.filter(request.Filters, cutoff)
	if err != nil {
		adminUsersError(w, http.StatusBadRequest, "Invalid or unauthorized log filters.")
		return
	}
	scope := "filtered"
	if request.All {
		scope = "all"
	}
	id := primitive.NewObjectID().Hex()
	if err := app.logs.audit(app.config.Username, id, scope, "started"); err != nil {
		log.Printf("log export audit write failed: export_id=%s stage=started", id)
		adminUsersError(w, http.StatusServiceUnavailable, "Unable to audit log export.")
		return
	}
	outcome := "failed"
	defer func() {
		if err := app.logs.audit(app.config.Username, id, scope, outcome); err != nil {
			log.Printf("log export audit write failed: export_id=%s stage=final outcome=%s", id, outcome)
			w.Header().Set("X-Log-Export-Audit", "failed")
		} else {
			w.Header().Set("X-Log-Export-Audit", "recorded")
		}
	}()
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	collection := app.logs.reader.Database(app.logs.database).Collection("refLedgerLogs")
	count, err := collection.CountDocuments(ctx, filter, options.Count().SetMaxTime(30*time.Second))
	if err != nil {
		adminUsersError(w, http.StatusServiceUnavailable, "Unable to count Ref Ledger logs.")
		return
	}
	cursor, err := collection.Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "timestamp", Value: -1}, {Key: "_id", Value: -1}}).SetMaxTime(30*time.Second))
	if err != nil {
		adminUsersError(w, http.StatusServiceUnavailable, "Unable to export Ref Ledger logs.")
		return
	}
	defer cursor.Close(ctx)
	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="ref-ledger-logs.jsonl"`)
	w.Header().Set("X-Log-Export-Cutoff", cutoff.Format(time.RFC3339Nano))
	w.Header().Set("X-Log-Export-Scope", scope)
	w.Header().Set("X-Log-Export-Count", strconv.FormatInt(count, 10))
	w.Header().Add("Trailer", "X-Log-Export-Status")
	w.Header().Add("Trailer", "X-Log-Export-Audit")
	encoder := json.NewEncoder(w)
	var exported int64
	for cursor.Next(ctx) {
		var entry logRecord
		if cursor.Decode(&entry) != nil || encoder.Encode(entry) != nil {
			w.Header().Set("X-Log-Export-Status", "incomplete")
			return
		}
		exported++
	}
	if cursor.Err() != nil || ctx.Err() != nil || exported != count {
		w.Header().Set("X-Log-Export-Status", "incomplete")
		return
	}
	outcome = "success"
	w.Header().Set("X-Log-Export-Status", "complete")
}
