package main

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	logsAdminMonitorPrefix = "/api/admin/logs"
	logsAdminBackendPrefix = "/api/internal/v1/admin/logs"
	logsAdminBodyLimit     = 64 << 10
)

func validateLogsAdminEndpoint(endpoint, token, appEnvironment string) error {
	if len(token) < 32 || len(token) > 4096 {
		return errors.New("LOGS_ADMIN_API_TOKEN must contain 32 to 4096 characters")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" ||
		parsed.Fragment != "" || parsed.Path != "" && parsed.Path != "/" {
		return errors.New("LOGS_ADMIN_API_URL must be an origin without path, credentials, query, or fragment")
	}
	if strings.EqualFold(parsed.Scheme, "https") {
		return nil
	}
	if strings.EqualFold(parsed.Scheme, "http") && strings.EqualFold(strings.TrimSpace(appEnvironment), "development") {
		host := parsed.Hostname()
		if strings.EqualFold(host, "localhost") {
			return nil
		}
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			return nil
		}
	}
	return errors.New("LOGS_ADMIN_API_URL must use HTTPS except for localhost or loopback HTTP in development")
}

func (app *Application) logsAdminProxy(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if app.config.LogsAdminURL == "" || app.config.LogsAdminToken == "" {
		adminUsersError(w, http.StatusServiceUnavailable, "Ref Ledger log administration is not configured.")
		return
	}
	if r.URL.Path != logsAdminMonitorPrefix && !strings.HasPrefix(r.URL.Path, logsAdminMonitorPrefix+"/") {
		http.NotFound(w, r)
		return
	}
	suffix := strings.TrimPrefix(r.URL.Path, logsAdminMonitorPrefix)
	target, err := url.Parse(app.config.LogsAdminURL + logsAdminBackendPrefix + suffix)
	if err != nil {
		adminUsersError(w, http.StatusBadGateway, "Ref Ledger log endpoint is invalid.")
		return
	}
	target.RawQuery = r.URL.RawQuery
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	var body io.Reader
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		raw, err := io.ReadAll(io.LimitReader(r.Body, logsAdminBodyLimit+1))
		if err != nil {
			adminUsersError(w, http.StatusBadRequest, "Ref Ledger log request could not be read.")
			return
		}
		if len(raw) > logsAdminBodyLimit {
			adminUsersError(w, http.StatusRequestEntityTooLarge, "Ref Ledger log request is too large.")
			return
		}
		body = strings.NewReader(string(raw))
	}
	request, err := http.NewRequestWithContext(ctx, r.Method, target.String(), body)
	if err != nil {
		adminUsersError(w, http.StatusBadGateway, "Ref Ledger log request could not be created.")
		return
	}
	request.Header.Set("Authorization", "Bearer "+app.config.LogsAdminToken)
	request.Header.Set("X-Ref-Ledger-Logs-Version", "1")
	request.Header.Set("X-Ref-Ledger-Admin-Actor", app.config.Username)
	if contentType := r.Header.Get("Content-Type"); contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	response, err := client.Do(request)
	if err != nil {
		adminUsersError(w, http.StatusBadGateway, "Ref Ledger logging service could not be reached.")
		return
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		adminUsersError(w, http.StatusBadGateway, "Ref Ledger logging service returned an unexpected redirect.")
		return
	}
	for _, name := range []string{
		"Content-Type", "Content-Disposition", "X-Log-Export-Cutoff",
		"X-Log-Export-Scope", "X-Log-Export-Count", "Trailer",
	} {
		if value := response.Header.Get(name); value != "" {
			w.Header().Set(name, value)
		}
	}
	w.WriteHeader(response.StatusCode)
	if _, err := io.Copy(w, response.Body); err != nil {
		log.Printf("Ref Ledger logs response forwarding failed: %v", err)
	}
	for _, name := range []string{"X-Log-Export-Status", "X-Log-Export-Audit"} {
		if value := response.Trailer.Get(name); value != "" {
			w.Header().Set(name, value)
		}
	}
}
