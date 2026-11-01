package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gablue316/ref-ledger-monitor/internal/database"
	"github.com/gablue316/ref-ledger-monitor/internal/handlers"

	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"
)

// Save this file as cmd/server/main.go in ref-ledger-monitor.
// Install the same MongoDB driver major version used by Ref Ledger:
//
//   go mod init ref-ledger-monitor   (only if go.mod does not exist)
//   go get go.mongodb.org/mongo-driver@v1.17.10
//   go mod tidy
//   go run ./cmd/server
//
// Required environment variables:
//   MONGODB_ENV       local or atlas (default: local).
//   MONGODB_HOST      Local host:port or Atlas cluster hostname.
//   MONGODB_USERNAME  Required for Atlas.
//   MONGODB_PASSWORD  Required for Atlas.
//   MONGODB_NAME      Explicit database name; there is no fallback database.
//   MONITOR_USERNAME  Separate monitor HTTP Basic Auth username.
//   MONITOR_PASSWORD  Separate monitor HTTP Basic Auth password.
//
// Optional environment variables:
//   PORT              Default: 8081. Hosting providers can supply their own.
//   BIND_ADDRESS      Default: 127.0.0.1 locally; 0.0.0.0 in production.
//   APP_ENV           Default: development. Use production when deployed.
//   MONGODB_APP_NAME   Atlas appName; default: ref-ledger-cluster.
//   MONGODB_REPLICA_SET Default: refLedgerRS (local connections).
//   MONGODB_DIRECT_CONNECTION Set true only when needed for local MongoDB.
//   WEB_DIR           Default: ./web (relative to the working directory).
//   POD_NAME          Default: local-dev.
//   APP_RELEASE       Default: development.
//
// Set variables in PowerShell or in your provider's environment settings.
// This program does not automatically read a .env file.
// Use HTTPS at your hosting provider/reverse proxy for deployed Basic Auth.
// Administration uses the monitor database directly; no Ref Ledger imports are required.
// ADMIN_ALLOWED_ORIGINS optionally overrides allowed browser write origins (comma-separated).
// Defaults: https://ref-ledger.com,https://www.ref-ledger.com; direct request origin is also allowed.

type Config struct {
	MongoURI    string
	MongoDBName string
	Username    string
	Password    string
	Address     string
	IndexFile   string
	Environment string
	PodName     string
	Release     string
}

type Application struct {
	config Config
	db     *mongo.Database
}

func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.LUTC)

	if err := run(); err != nil {
		log.Printf("Ref Ledger Monitor stopped: %v", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	// A cancelled root context handles Ctrl+C locally and termination in a container.
	ctx, stop := signal.NotifyContext(
		context.Background(),
		os.Interrupt,
		syscall.SIGTERM,
	)
	defer stop()

	log.Printf(
		"Starting Ref Ledger Monitor: environment=%s database=%s pod=%s release=%s",
		cfg.Environment, cfg.MongoDBName, cfg.PodName, cfg.Release,
	)

	client, err := connectMongo(ctx, cfg.MongoURI)
	if err != nil {
		return err
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := client.Disconnect(closeCtx); err != nil {
			log.Print("MongoDB disconnect failed")
		}
	}()

	app := &Application{
		config: cfg,
		db:     client.Database(cfg.MongoDBName),
	}

	mux := app.routes()

	server := &http.Server{
		Addr:              cfg.Address,
		Handler:           logRequests(mux),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	serverErrors := make(chan error, 1)
	go func() {
		log.Printf("Ref Ledger Monitor listening on %s", cfg.Address)
		serverErrors <- server.ListenAndServe()
	}()

	select {
	case err := <-serverErrors:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("HTTP server: %w", err)

	case <-ctx.Done():
		log.Print("Shutting down Ref Ledger Monitor")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		_ = server.Close()
		return fmt.Errorf("HTTP shutdown: %w", err)
	}

	return nil
}

func loadConfig() (Config, error) {
	cfg := Config{
		MongoDBName: strings.TrimSpace(os.Getenv("MONGODB_NAME")),
		Username:    os.Getenv("MONITOR_USERNAME"),
		Password:    os.Getenv("MONITOR_PASSWORD"),
		Environment: envOrDefault("APP_ENV", "development"),
		PodName:     envOrDefault("POD_NAME", "local-dev"),
		Release:     envOrDefault("APP_RELEASE", "development"),
	}

	for _, required := range []struct {
		name  string
		value string
	}{
		{"MONGODB_NAME", cfg.MongoDBName},
		{"MONITOR_USERNAME", cfg.Username},
		{"MONITOR_PASSWORD", cfg.Password},
	} {
		if strings.TrimSpace(required.value) == "" {
			return Config{}, fmt.Errorf("required environment variable %s is empty", required.name)
		}
	}

	var err error
	cfg.MongoURI, err = buildMongoURI(cfg.MongoDBName)
	if err != nil {
		return Config{}, err
	}

	port := envOrDefault("PORT", "8081")
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return Config{}, fmt.Errorf("PORT must be a number between 1 and 65535")
	}

	host := "127.0.0.1"
	if strings.EqualFold(cfg.Environment, "production") {
		host = "0.0.0.0"
	}
	cfg.Address = net.JoinHostPort(envOrDefault("BIND_ADDRESS", host), port)

	cfg.IndexFile, err = filepath.Abs(filepath.Join(envOrDefault("WEB_DIR", "./web"), "index.html"))
	if err != nil {
		return Config{}, fmt.Errorf("resolve web directory: %w", err)
	}

	info, err := os.Stat(cfg.IndexFile)
	if err != nil || !info.Mode().IsRegular() {
		return Config{}, fmt.Errorf("web/index.html is missing or is not a regular file; check WEB_DIR")
	}

	return cfg, nil
}

func envOrDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

// Match Ref Ledger's connection settings. Require an explicit database name
// so selecting a host never silently selects a different database.
func buildMongoURI(databaseName string) (string, error) {
	mode := strings.ToLower(envOrDefault("MONGODB_ENV", "local"))
	connection := &url.URL{Path: "/" + databaseName}
	query := url.Values{}

	switch mode {
	case "atlas":
		username := os.Getenv("MONGODB_USERNAME")
		password := os.Getenv("MONGODB_PASSWORD")
		if strings.TrimSpace(username) == "" || strings.TrimSpace(password) == "" {
			return "", errors.New("MONGODB_USERNAME and MONGODB_PASSWORD are required for Atlas")
		}
		connection.Scheme = "mongodb+srv"
		connection.Host = envOrDefault("MONGODB_HOST", "ref-ledger-cluster.2hni1bk.mongodb.net")
		// Escapes special characters in usernames and passwords correctly.
		connection.User = url.UserPassword(username, password)
		query.Set("appName", envOrDefault("MONGODB_APP_NAME", "ref-ledger-cluster"))
		// Atlas database users normally authenticate against admin even when
		// the URI path selects the application's database.
		query.Set("authSource", "admin")

	case "local":
		connection.Scheme = "mongodb"
		connection.Host = envOrDefault("MONGODB_HOST", "localhost:27017")
		query.Set("replicaSet", envOrDefault("MONGODB_REPLICA_SET", "refLedgerRS"))
		if strings.EqualFold(os.Getenv("MONGODB_DIRECT_CONNECTION"), "true") {
			query.Set("directConnection", "true")
		}

	default:
		return "", errors.New("MONGODB_ENV must be local or atlas")
	}

	connection.RawQuery = query.Encode()
	return connection.String(), nil
}

func connectMongo(parent context.Context, uri string) (*mongo.Client, error) {
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()

	client, err := mongo.Connect(
		ctx,
		options.Client().
			ApplyURI(uri).
			SetAppName("ref-ledger-monitor").
			SetConnectTimeout(10*time.Second).
			SetServerSelectionTimeout(10*time.Second),
	)
	if err != nil {
		// Do not print the URI or raw connection errors, which may contain credentials.
		return nil, errors.New("MongoDB initialization failed; check the MongoDB environment variables")
	}

	if err := client.Ping(ctx, readpref.Primary()); err != nil {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		_ = client.Disconnect(closeCtx)

		return nil, errors.New("MongoDB ping failed; check the URI, credentials, network access, and replica set")
	}

	log.Print("MongoDB connection verified")
	return client, nil
}

func (app *Application) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")

		username, password, ok := r.BasicAuth()
		userHash := sha256.Sum256([]byte(username))
		passHash := sha256.Sum256([]byte(password))
		wantUser := sha256.Sum256([]byte(app.config.Username))
		wantPass := sha256.Sum256([]byte(app.config.Password))

		validUser := subtle.ConstantTimeCompare(userHash[:], wantUser[:])
		validPass := subtle.ConstantTimeCompare(passHash[:], wantPass[:])

		if !ok || validUser&validPass != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="Ref Ledger Administration", charset="UTF-8"`)
			http.Error(w, "Administrator sign-in required", http.StatusUnauthorized)
			return
		}

		next(w, r)
	}
}

func allowRead(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}

	w.Header().Set("Allow", "GET, HEAD")
	http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	return false
}

func (app *Application) pageHandler(w http.ResponseWriter, r *http.Request) {
	// Do not let the root route serve the HTML for misspelled API paths.
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if !allowRead(w, r) {
		return
	}

	http.ServeFile(w, r, app.config.IndexFile)
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	if !allowRead(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (app *Application) readyHandler(w http.ResponseWriter, r *http.Request) {
	if !allowRead(w, r) {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	if err := app.db.RunCommand(ctx, map[string]int{"ping": 1}).Err(); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not ready"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

// aboutHandler returns the running release, not a hard-coded page version.
func (app *Application) aboutHandler(w http.ResponseWriter, r *http.Request) {
	if !allowRead(w, r) {
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodHead {
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Application string `json:"application"`
		Version     string `json:"version"`
	}{Application: "Ref Ledger Administration", Version: app.config.Release})
}

func (app *Application) metricsHandler(w http.ResponseWriter, r *http.Request) {
	reader := &database.MetricsStore{DB: app.db}
	handlers.LastMinute(reader)(w, r)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Print("Unable to write JSON response")
	}
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Log paths, not cookies, authorization headers, or connection strings.
		log.Printf("Method=%s Path=%s RemoteAddr=%s", r.Method, r.URL.Path, r.RemoteAddr)
		next.ServeHTTP(w, r)
	})
}
