package main

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
)

// All administrator operations use the monitor's separate Basic Auth identity.
// The actor is not a Ref Ledger user; cancelling customer sessions cannot log it out.
func (app *Application) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthHandler)
	mux.HandleFunc("/readyz", app.requireAuth(app.readyHandler))
	mux.HandleFunc("/api/about", app.requireAuth(app.aboutHandler))
	mux.HandleFunc("/api/metrics/last-minute", app.requireAuth(app.metricsHandler))
	mux.HandleFunc("/api/admin/users", app.requireAuth(app.adminUsersSearch))
	mux.HandleFunc("/api/admin/roles", app.requireAuth(app.adminRolesHandler))
	mux.HandleFunc("/api/admin/users/role", app.requireAuth(app.adminWrite(app.adminRoleHandler)))
	mux.HandleFunc("/api/admin/users/password", app.requireAuth(app.adminWrite(app.adminPasswordHandler)))
	mux.HandleFunc("/api/admin/users/delete", app.requireAuth(app.adminWrite(app.adminUsersDelete)))
	mux.HandleFunc("/api/admin/users/sessions/cancel", app.requireAuth(app.adminWrite(app.adminUsersCancelSessions)))
	mux.HandleFunc("/api/sports", app.requireAuth(app.adminWrite(app.adminSportsHandler)))
	mux.HandleFunc("/api/sport-active", app.requireAuth(app.adminWrite(app.adminSportActiveHandler)))
	mux.HandleFunc("/api/sport-inactive", app.requireAuth(app.adminWrite(app.adminSportInactiveHandler)))
	mux.HandleFunc("/api/settings/official-roles", app.requireAuth(app.adminWrite(app.adminOfficialRolesHandler)))
	mux.HandleFunc("/admin.js", app.requireAuth(app.adminScriptHandler))
	mux.HandleFunc("/metrics", app.requireAuth(app.legacyMetricsHandler))
	mux.HandleFunc("/metrics/", app.requireAuth(app.legacyMetricsHandler))
	mux.HandleFunc("/", app.requireAuth(app.pageHandler))

	// Direct /admin access and a main-app proxy that strips /admin are both supported.
	// The prefixed handler forwards to the SAME mux using the stripped path.
	// Exact /admin avoids Go's implicit redirect before authentication.
	mux.HandleFunc("/admin", app.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		if !allowRead(w, r) {
			return
		}
		http.Redirect(w, r, "/admin/", http.StatusTemporaryRedirect)
	}))
	mux.Handle("/admin/", http.StripPrefix("/admin", mux))
	return mux
}

func (app *Application) adminScriptHandler(w http.ResponseWriter, r *http.Request) {
	if !allowRead(w, r) {
		return
	}
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	http.ServeFile(w, r, filepath.Join(filepath.Dir(app.config.IndexFile), "admin.js"))
}

func (app *Application) legacyMetricsHandler(w http.ResponseWriter, r *http.Request) {
	if !allowRead(w, r) {
		return
	}
	if r.URL.Path != "/metrics" && r.URL.Path != "/metrics/" {
		http.NotFound(w, r)
		return
	}
	// Always use the canonical administration mount, including direct monitor access.
	http.Redirect(w, r, "/admin/#metrics", http.StatusTemporaryRedirect)
}

// JSON-only writes and origin checks protect browser-cached Basic Auth credentials.
// Never trust X-Forwarded-Host from arbitrary direct callers. Public proxy origins
// are explicit because the main Ref Ledger proxy rewrites the monitor Host header.
func (app *Application) adminWrite(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			next(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/admin/") && r.Header.Get("X-Ref-Ledger-Admin") != "1" {
			adminUsersError(w, http.StatusForbidden, "Invalid administration request.")
			return
		}
		if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
			adminUsersError(w, http.StatusForbidden, "Cross-site administration writes are not allowed.")
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && !allowedAdminOrigin(origin, r.Host) {
			adminUsersError(w, http.StatusForbidden, "Administration request origin is not allowed.")
			return
		}
		if r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodPatch {
			mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err != nil || mediaType != "application/json" {
				adminUsersError(w, http.StatusUnsupportedMediaType, "JSON is required.")
				return
			}
		}
		next(w, r)
	}
}

func allowedAdminOrigin(origin, host string) bool {
	parsed, err := url.Parse(origin)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "" {
		return false
	}
	if strings.EqualFold(parsed.Host, host) {
		return true
	}
	origins := envOrDefault("ADMIN_ALLOWED_ORIGINS", "https://ref-ledger.com,https://www.ref-ledger.com")
	for _, allowed := range strings.Split(origins, ",") {
		if strings.EqualFold(strings.TrimSpace(allowed), origin) {
			return true
		}
	}
	return false
}

func decodeAdminJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		adminUsersError(w, http.StatusBadRequest, "Invalid JSON request.")
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		adminUsersError(w, http.StatusBadRequest, "Provide one JSON object.")
		return false
	}
	return true
}
