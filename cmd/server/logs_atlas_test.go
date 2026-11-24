package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// This test bootstraps users only on a fresh, disposable loopback mongod.
func TestLogsAtlasLiveLeastPrivilege(t *testing.T) {
	if os.Getenv("LOGS_PRIVILEGE_LIVE_TEST") != "1" {
		t.Skip("requires a fresh disposable authorization-enabled mongod")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const uri = "mongodb://127.0.0.1:28180/?directConnection=true"
	bootstrap, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatal("bootstrap client failed")
	}
	defer bootstrap.Disconnect(context.Background())
	password := primitive.NewObjectID().Hex() + primitive.NewObjectID().Hex()
	if err := bootstrap.Database("admin").RunCommand(ctx, bson.D{
		{Key: "createUser", Value: "verificationRoot"}, {Key: "pwd", Value: password},
		{Key: "roles", Value: bson.A{bson.M{"role": "root", "db": "admin"}}},
	}).Err(); err != nil {
		t.Fatal("fresh disposable MongoDB bootstrap failed")
	}
	connect := func(name string) *mongo.Client {
		client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri).SetAuth(options.Credential{AuthSource: "admin", Username: name, Password: password}))
		if err != nil {
			t.Fatal("test authentication client failed")
		}
		t.Cleanup(func() { _ = client.Disconnect(context.Background()) })
		return client
	}
	denied := func(err error) bool {
		var command mongo.CommandError
		return errors.As(err, &command) && command.Code == 13
	}
	root := connect("verificationRoot")
	const dbName = "refledger_privilege_verification"
	admin := root.Database("admin")
	createRole := func(name string, privileges bson.A) {
		err := admin.RunCommand(ctx, bson.D{{Key: "createRole", Value: name}, {Key: "privileges", Value: privileges}, {Key: "roles", Value: bson.A{}}}).Err()
		if err != nil {
			t.Fatal("unable to create test role")
		}
	}
	privilege := func(collection string, actions ...string) bson.M {
		return bson.M{"resource": bson.M{"db": dbName, "collection": collection}, "actions": actions}
	}
	createRole("verificationReader", bson.A{privilege("refLedgerLogs", "find"), privilege("refLedgerLogSettings", "find")})
	createRole("verificationAuditor", bson.A{privilege("monitorLogExportAudit", "insert")})
	for _, name := range []string{"verificationReader", "verificationAuditor"} {
		if err := admin.RunCommand(ctx, bson.D{{Key: "createUser", Value: name}, {Key: "pwd", Value: password}, {Key: "roles", Value: bson.A{bson.M{"role": name, "db": "admin"}}}}).Err(); err != nil {
			t.Fatal("unable to create test user")
		}
	}
	db := root.Database(dbName)
	if _, err := db.Collection("refLedgerLogs").InsertOne(ctx, bson.M{"environment": "development", "tenantId": "allowed", "timestamp": time.Now().Add(-time.Second), "schemaVersion": 1, "level": 3, "event": "privilege-verification", "message": "verification"}); err != nil {
		t.Fatal("fixture insert failed")
	}
	if _, err := db.Collection("refLedgerLogSettings").InsertOne(ctx, bson.M{"_id": "capture:development", "threshold": 3}); err != nil {
		t.Fatal("settings fixture insert failed")
	}
	reader, auditor := connect("verificationReader"), connect("verificationAuditor")
	if err := reader.Database(dbName).Collection("refLedgerLogs").FindOne(ctx, bson.M{}).Err(); err != nil {
		t.Fatal("reader could not read log collection")
	}
	if err := reader.Database(dbName).Collection("refLedgerLogSettings").FindOne(ctx, bson.M{}).Err(); err != nil {
		t.Fatal("reader could not read settings collection")
	}
	if err := reader.Database(dbName).Collection("unrelated").FindOne(ctx, bson.M{}).Err(); !denied(err) {
		t.Fatal("unrelated collection read was not denied")
	}
	if _, err := reader.Database(dbName).Collection("refLedgerLogs").InsertOne(ctx, bson.M{}); !denied(err) {
		t.Fatal("log insert was not denied")
	}
	if _, err := reader.Database(dbName).Collection("refLedgerLogs").DeleteMany(ctx, bson.M{}); !denied(err) {
		t.Fatal("log deletion was not denied")
	}
	if _, err := auditor.Database(dbName).Collection("monitorLogExportAudit").InsertOne(ctx, bson.M{"outcome": "test"}); err != nil {
		t.Fatal("auditor could not insert audit entry")
	}
	if err := auditor.Database(dbName).Collection("refLedgerLogs").FindOne(ctx, bson.M{}).Err(); !denied(err) {
		t.Fatal("auditor read was not denied")
	}
	const actor = "verification-admin"
	token := strings.Repeat("b", 64)
	if _, err := db.Collection("admin_sessions").InsertOne(ctx, bson.M{"_id": adminTokenHash(token), "username": actor, "expiresAt": time.Now().Add(time.Hour)}); err != nil {
		t.Fatal("test session insert failed")
	}
	app := &Application{config: Config{Username: actor}, db: db, logs: &logsRepository{reader: reader, auditor: auditor, database: dbName, environment: "development", tenants: []string{"allowed"}, settings: true}}
	request := httptest.NewRequest(http.MethodPost, logsAdminMonitorPrefix+"/export", strings.NewReader(`{"all":true,"filters":{}}`))
	request.AddCookie(&http.Cookie{Name: adminCookie, Value: token})
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Ref-Ledger-Admin", "1")
	response := httptest.NewRecorder()
	app.routes().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "privilege-verification") {
		t.Fatal("restricted clients could not perform audited export")
	}
}
func TestLogsAtlasScopeAndTimeFilters(t *testing.T) {
	repo := &logsRepository{environment: "development", tenants: []string{"tenant-a"}}
	cutoff := time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC)
	q := logQuery{DateFrom: "2026-03-08", TimeZone: "America/New_York", TimeFrom: "23:30", TimeThrough: "00:15", Level: 4, TenantID: "tenant-a", Collection: "games", SessionID: "session", TransactionID: "tx", UserID: "user"}
	filter, err := repo.filter(q, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	timestamps := filter["timestamp"].(bson.M)
	if !timestamps["$gte"].(time.Time).Equal(time.Date(2026, 3, 8, 5, 0, 0, 0, time.UTC)) ||
		!timestamps["$lt"].(time.Time).Equal(time.Date(2026, 3, 9, 4, 0, 0, 0, time.UTC)) {
		t.Fatal("DST date bounds are incorrect")
	}
	if _, ok := filter["$expr"].(bson.M)["$or"]; !ok {
		t.Fatal("midnight range must cross midnight")
	}
	for key, expected := range map[string]any{"environment": "development", "tenantId": "tenant-a", "level": 4, "collection": "games", "sessionId": "session", "transactionId": "tx", "userId": "user"} {
		if filter[key] != expected {
			t.Errorf("%s is not an exact filter", key)
		}
	}
	q.TenantID = "tenant-b"
	if _, err := repo.filter(q, cutoff); err == nil {
		t.Fatal("accepted unauthorized tenant")
	}
	q.TenantID = ""
	q.TimeAt = "12:00"
	if _, err := repo.filter(q, cutoff); err == nil {
		t.Fatal("accepted conflicting time filters")
	}
	q = logQuery{DateFrom: "2026-11-02", DateThrough: "2026-11-01"}
	if _, err := repo.filter(q, cutoff); err == nil {
		t.Fatal("accepted reversed dates")
	}
	filter, err = repo.filter(logQuery{}, cutoff)
	if err != nil || len(filter["tenantId"].(bson.M)["$in"].([]string)) != 1 {
		t.Fatal("unfiltered requests must retain authorized tenant scope")
	}
}

func TestLogsAtlasURIAndDisabledWrites(t *testing.T) {
	for _, raw := range []string{
		"mongodb+srv://example.test/?tls=false",
		"mongodb+srv://example.test/?tlsInsecure=true",
		"mongodb://example.test:27017/",
	} {
		if validateLogsURI(raw, false) == nil {
			t.Fatal("accepted unsafe MongoDB URI")
		}
	}
	if validateLogsURI("mongodb+srv://example.test/?tls=true", false) != nil {
		t.Fatal("rejected TLS Atlas URI")
	}
	for _, path := range []string{logsAdminThresholdPath, logsAdminMonitorPrefix + "/clear"} {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		resp := httptest.NewRecorder()
		(&Application{}).logsAtlasHandler(resp, req)
		if resp.Code != http.StatusMethodNotAllowed {
			t.Fatalf("write route returned %d", resp.Code)
		}
	}
	for _, raw := range []string{"offset=100001", "limit=201", "level=0"} {
		values, _ := url.ParseQuery(raw)
		if _, err := parseAtlasQuery(values); err == nil {
			t.Fatalf("accepted invalid pagination or level: %s", raw)
		}
	}
}

// Reads records produced by Ref Ledger's opt-in live writer test in the isolated replica set.
func TestLogsAtlasLiveWriterToViewerAndExport(t *testing.T) {
	const uri = "mongodb://127.0.0.1:28179/?replicaSet=loggingVerifyRS&directConnection=true"
	if os.Getenv("STRUCTURED_LOG_LIVE_TEST_URI") != uri {
		t.Skip("requires the disposable logging verification replica set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatal("test MongoDB connection failed")
	}
	defer client.Disconnect(context.Background())
	var hello struct {
		SetName string `bson:"setName"`
	}
	if err := client.Database("admin").RunCommand(ctx, bson.D{{Key: "hello", Value: 1}}).Decode(&hello); err != nil || hello.SetName != "loggingVerifyRS" {
		t.Fatal("not the disposable verification replica set")
	}
	db := client.Database("refledger_logging_verification")
	var source logRecord
	err = db.Collection("refLedgerLogs").FindOne(ctx, bson.M{"environment": "development", "operation": "game.insert", "event": "database.write.completed", "tenantId": bson.M{"$regex": "^live-test-"}}, options.FindOne().SetSort(bson.D{{Key: "timestamp", Value: -1}, {Key: "_id", Value: -1}})).Decode(&source)
	if err != nil || source.TenantID == nil || source.TransactionID == nil {
		t.Fatal("Ref Ledger writer record was not found")
	}
	const actor = "verification-admin"
	token := strings.Repeat("a", 64)
	_, err = db.Collection("admin_sessions").InsertOne(ctx, bson.M{"_id": adminTokenHash(token), "username": actor, "expiresAt": time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal("unable to create test administrator session")
	}
	defer db.Collection("admin_sessions").DeleteOne(context.Background(), bson.M{"_id": adminTokenHash(token)})
	repo := &logsRepository{reader: client, auditor: client, database: db.Name(), environment: "development", tenants: []string{*source.TenantID}, settings: true}
	app := &Application{config: Config{Username: actor}, db: db, logs: repo}
	do := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.AddCookie(&http.Cookie{Name: adminCookie, Value: token})
		if method == http.MethodPost {
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Ref-Ledger-Admin", "1")
		}
		rec := httptest.NewRecorder()
		app.routes().ServeHTTP(rec, req)
		return rec
	}
	missing := httptest.NewRecorder()
	app.routes().ServeHTTP(missing, httptest.NewRequest(http.MethodGet, logsAdminMonitorPrefix+"/records", nil))
	if missing.Code != http.StatusUnauthorized {
		t.Fatal("missing administrator session was not rejected")
	}
	if response := do(http.MethodPost, logsAdminMonitorPrefix+"/clear", `{}`); response.Code != http.StatusMethodNotAllowed {
		t.Fatal("clear route was not disabled")
	}
	marker := "fixture-" + primitive.NewObjectID().Hex()
	created := []any{}
	for _, row := range []struct {
		tenant, environment, event string
		level                      int
		timestamp                  time.Time
	}{
		{*source.TenantID, "development", "fixture-a", 4, time.Date(2026, 3, 8, 6, 59, 0, 0, time.UTC)},
		{*source.TenantID, "development", "fixture-b", 4, time.Date(2026, 3, 8, 6, 59, 0, 0, time.UTC)},
		{*source.TenantID, "development", "fixture-c", 3, time.Date(2026, 3, 8, 7, 0, 0, 0, time.UTC)},
		{*source.TenantID, "development", "fixture-fall-a", 4, time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC)},
		{*source.TenantID, "development", "fixture-fall-b", 4, time.Date(2026, 11, 1, 6, 30, 0, 0, time.UTC)},
		{"other-tenant", "development", "fixture-forbidden-tenant", 4, time.Date(2026, 3, 8, 6, 59, 0, 0, time.UTC)},
		{*source.TenantID, "other-environment", "fixture-forbidden-environment", 4, time.Date(2026, 3, 8, 6, 59, 0, 0, time.UTC)},
	} {
		created = append(created, bson.M{"schemaVersion": 1, "timestamp": row.timestamp, "environment": row.environment, "level": row.level, "event": row.event, "message": "fixture", "tenantId": row.tenant, "sessionId": marker, "userId": "fixture-user", "collection": "games", "transactionId": marker})
	}
	if _, err := db.Collection("refLedgerLogs").InsertMany(ctx, created); err != nil {
		t.Fatal("fixture insert failed")
	}
	defer db.Collection("refLedgerLogs").DeleteMany(context.Background(), bson.M{"sessionId": marker})
	params := url.Values{"sessionId": {marker}, "tenantId": {*source.TenantID}, "userId": {"fixture-user"}, "collection": {"games"}, "transactionId": {marker}, "dateFrom": {"2026-03-08"}, "timeZone": {"America/New_York"}, "timeAt": {"01:59"}, "level": {"4"}, "limit": {"1"}}
	first := do(http.MethodGet, logsAdminMonitorPrefix+"/records?"+params.Encode(), "")
	if first.Code != http.StatusOK {
		t.Fatal("historical filter failed")
	}
	var page struct {
		Items   []logRecord `json:"items"`
		HasMore bool        `json:"hasMore"`
		Cutoff  string      `json:"cutoff"`
	}
	if json.Unmarshal(first.Body.Bytes(), &page) != nil || len(page.Items) != 1 || !page.HasMore {
		t.Fatal("first stable pagination page invalid")
	}
	firstEvent := page.Items[0].Event
	params.Set("offset", "1")
	params.Set("cutoff", page.Cutoff)
	second := do(http.MethodGet, logsAdminMonitorPrefix+"/records?"+params.Encode(), "")
	if json.Unmarshal(second.Body.Bytes(), &page) != nil || len(page.Items) != 1 || page.HasMore || page.Items[0].Event == firstEvent {
		t.Fatal("second stable pagination page invalid")
	}
	params.Del("offset")
	params.Del("cutoff")
	params.Del("timeAt")
	params.Set("timeFrom", "23:00")
	params.Set("timeThrough", "03:00")
	params.Set("limit", "50")
	crossing := do(http.MethodGet, logsAdminMonitorPrefix+"/records?"+params.Encode(), "")
	var crossingPage struct {
		Items []logRecord `json:"items"`
	}
	if crossing.Code != http.StatusOK || json.Unmarshal(crossing.Body.Bytes(), &crossingPage) != nil || len(crossingPage.Items) != 2 || strings.Contains(crossing.Body.String(), "fixture-forbidden") {
		t.Fatal("midnight/DST filter or scope failed")
	}
	params.Del("timeFrom")
	params.Del("timeThrough")
	params.Set("dateFrom", "2026-11-01")
	params.Set("dateThrough", "2026-11-02")
	params.Set("timeAt", "01:30")
	fallback := do(http.MethodGet, logsAdminMonitorPrefix+"/records?"+params.Encode(), "")
	if fallback.Code != http.StatusOK || json.Unmarshal(fallback.Body.Bytes(), &crossingPage) != nil || len(crossingPage.Items) != 2 {
		t.Fatal("inclusive date range or repeated DST minute failed")
	}
	params.Del("dateFrom")
	params.Del("dateThrough")
	timeOnly := do(http.MethodGet, logsAdminMonitorPrefix+"/records?"+params.Encode(), "")
	if timeOnly.Code != http.StatusOK || json.Unmarshal(timeOnly.Body.Bytes(), &crossingPage) != nil || len(crossingPage.Items) != 2 {
		t.Fatal("time-only filter failed")
	}
	for _, field := range []string{"collection", "sessionId", "transactionId", "userId"} {
		values := url.Values{"sessionId": {marker}, "timeZone": {"UTC"}}
		values.Set(field, "not-a-match")
		noMatch := do(http.MethodGet, logsAdminMonitorPrefix+"/records?"+values.Encode(), "")
		if noMatch.Code != http.StatusOK || json.Unmarshal(noMatch.Body.Bytes(), &crossingPage) != nil || len(crossingPage.Items) != 0 {
			t.Fatalf("exact %s filter was not applied", field)
		}
	}
	resp := do(http.MethodGet, logsAdminMonitorPrefix+"/records?transactionId="+url.QueryEscape(*source.TransactionID), "")
	if resp.Code != http.StatusOK {
		t.Fatalf("viewer status = %d", resp.Code)
	}
	var result struct {
		Items []logRecord `json:"items"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &result); err != nil || len(result.Items) < 1 {
		t.Fatal("viewer did not return transaction records")
	}
	found := false
	for _, item := range result.Items {
		found = found || item.Event == source.Event && item.TransactionID != nil && *item.TransactionID == *source.TransactionID
	}
	if !found {
		t.Fatal("viewer did not return the Ref Ledger writer record")
	}
	resp = do(http.MethodGet, logsAdminMonitorPrefix+"/records?tenantId=other-tenant", "")
	if resp.Code != http.StatusBadRequest {
		t.Fatal("viewer accepted unauthorized tenant")
	}
	body, _ := json.Marshal(map[string]any{"all": false, "filters": map[string]any{"transactionId": *source.TransactionID}})
	resp = do(http.MethodPost, logsAdminMonitorPrefix+"/export", string(body))
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), *source.TransactionID) || !strings.Contains(resp.Body.String(), `"schemaVersion":1`) {
		t.Fatal("filtered export did not include the Ref Ledger record")
	}
	resp = do(http.MethodPost, logsAdminMonitorPrefix+"/export", `{"all":true,"filters":{}}`)
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), *source.TransactionID) || strings.Contains(resp.Body.String(), "fixture-forbidden") {
		t.Fatal("all-accessible export did not include the Ref Ledger record")
	}
	resp = do(http.MethodPost, logsAdminMonitorPrefix+"/export", `{"all":false,"filters":{"tenantId":"other-tenant"}}`)
	if resp.Code != http.StatusBadRequest {
		t.Fatal("export accepted unauthorized tenant")
	}
	auditCount, err := db.Collection("monitorLogExportAudit").CountDocuments(ctx, bson.M{"environment": "development", "outcome": "success"})
	if err != nil || auditCount < 2 {
		t.Fatal("successful exports were not durably audited")
	}
	failedWriter := &failedLogResponse{headers: make(http.Header)}
	failedRequest := httptest.NewRequest(http.MethodPost, logsAdminMonitorPrefix+"/export", strings.NewReader(string(body)))
	failedRequest.Header.Set("Content-Type", "application/json")
	failedRequest.Header.Set("X-Ref-Ledger-Admin", "1")
	failedRequest.AddCookie(&http.Cookie{Name: adminCookie, Value: token})
	app.routes().ServeHTTP(failedWriter, failedRequest)
	failedCount, err := db.Collection("monitorLogExportAudit").CountDocuments(ctx, bson.M{"environment": "development", "outcome": "failed"})
	if err != nil || failedCount == 0 {
		t.Fatal("aborted export was not audited as failed")
	}
	unavailable, err := mongo.Connect(ctx, options.Client().ApplyURI("mongodb://127.0.0.1:1").SetServerSelectionTimeout(time.Second))
	if err != nil {
		t.Fatal("unable to create disconnected test client")
	}
	defer unavailable.Disconnect(context.Background())
	repo.reader = unavailable
	resp = do(http.MethodGet, logsAdminMonitorPrefix+"/records", "")
	if resp.Code != http.StatusServiceUnavailable || strings.Contains(strings.ToLower(resp.Body.String()), "sign in") {
		t.Fatal("data source failure was not reported as a service failure")
	}
	repo.reader, repo.auditor = client, unavailable
	resp = do(http.MethodPost, logsAdminMonitorPrefix+"/export", `{"all":true,"filters":{}}`)
	if resp.Code != http.StatusServiceUnavailable {
		t.Fatal("export proceeded without an audit store")
	}
}

type failedLogResponse struct{ headers http.Header }

func (w *failedLogResponse) Header() http.Header { return w.headers }
func (w *failedLogResponse) WriteHeader(int)     {}
func (w *failedLogResponse) Write([]byte) (int, error) {
	return 0, errors.New("simulated disconnected client")
}

// Requires a disposable mongod started with enableTestCommands on port 28192.
func TestLogsAtlasLiveAuditWriteFailures(t *testing.T) {
	if os.Getenv("LOGS_AUDIT_LIVE_TEST") != "1" {
		t.Skip("requires the disposable browser verification MongoDB")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := mongo.Connect(ctx, options.Client().ApplyURI("mongodb://127.0.0.1:28192/?directConnection=true").SetRetryWrites(false))
	if err != nil {
		t.Fatal("test MongoDB client failed")
	}
	defer client.Disconnect(context.Background())
	db := client.Database("browser_verification")
	if err := db.Collection("refLedgerLogs").FindOne(ctx, bson.M{"event": "browser.fixture.0", "environment": "browser-test"}).Err(); err != nil {
		t.Fatal("disposable browser fixture not found")
	}
	failpoint := func(mode any) {
		t.Helper()
		if err := client.Database("admin").RunCommand(ctx, bson.D{
			{Key: "configureFailPoint", Value: "failCommand"}, {Key: "mode", Value: mode},
			{Key: "data", Value: bson.M{"failCommands": bson.A{"insert"}, "errorCode": 13}},
		}).Err(); err != nil {
			t.Fatal("unable to configure disposable MongoDB failpoint")
		}
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if err := client.Database("admin").RunCommand(cleanup, bson.D{{Key: "configureFailPoint", Value: "failCommand"}, {Key: "mode", Value: "off"}}).Err(); err != nil {
			t.Error("unable to disable disposable MongoDB failpoint")
		}
	}()
	app := &Application{config: Config{Username: "test-admin"}, logs: &logsRepository{
		reader: client, auditor: client, database: db.Name(), environment: "browser-test", tenants: []string{"tenant-a"},
	}}
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(previous)
	idPattern := regexp.MustCompile(`export_id=([0-9a-f]{24})`)
	runExport := func() (*httptest.ResponseRecorder, string) {
		t.Helper()
		output.Reset()
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, logsAdminMonitorPrefix+"/export", strings.NewReader(`{"all":false,"filters":{"transactionId":"browser-tx-linked"}}`))
		app.logsExportAtlas(response, request)
		match := idPattern.FindStringSubmatch(output.String())
		if len(match) != 2 {
			t.Fatal("audit failure diagnostic did not contain an export ID")
		}
		return response, match[1]
	}
	failpoint(bson.M{"times": 1})
	initial, initialID := runExport()
	if initial.Code != http.StatusServiceUnavailable || strings.Contains(initial.Body.String(), "browser.fixture.") || !strings.Contains(output.String(), "stage=started") {
		t.Fatal("initial audit failure did not prevent the export")
	}
	count, err := db.Collection("monitorLogExportAudit").CountDocuments(ctx, bson.M{"exportId": initialID})
	if err != nil || count != 0 {
		t.Fatal("initial audit failure unexpectedly created an audit record")
	}
	failpoint(bson.M{"skip": 1})
	final, finalID := runExport()
	failpoint("off")
	if final.Code != http.StatusOK || !strings.Contains(final.Body.String(), "browser.fixture.") || !strings.Contains(output.String(), "stage=final outcome=success") {
		t.Fatal("final audit failure was not diagnosed")
	}
	count, err = db.Collection("monitorLogExportAudit").CountDocuments(ctx, bson.M{"exportId": finalID})
	if err != nil || count != 1 {
		t.Fatal("final failure did not leave exactly one correlated attempt")
	}
	var audit struct {
		Outcome string `bson:"outcome"`
	}
	if err := db.Collection("monitorLogExportAudit").FindOne(ctx, bson.M{"exportId": finalID}).Decode(&audit); err != nil || audit.Outcome != "started" {
		t.Fatal("final failure did not preserve the started audit entry")
	}
}
