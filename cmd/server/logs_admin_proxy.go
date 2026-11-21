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
	logsAdminThresholdPath = logsAdminMonitorPrefix + "/threshold"
	logsAdminBodyLimit     = 64 << 10
)

func (app *Application) logsAdminThresholdHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		app.requireAuth(app.adminWrite(app.logsAdminProxy))(w, r)
		return
	}

	proxyReached := false
	proxy := func(w http.ResponseWriter, r *http.Request) {
		proxyReached = true
		app.logsAdminProxy(w, r)
	}
	app.requireAuth(app.adminWrite(proxy))(w, r)
	if !proxyReached {
		log.Printf("Ref Ledger threshold diagnostic: disposition=monitor_rejected stage=before_proxy")
	}
}

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
		logLogsAdminThresholdPreProxyRejection(r, "not_configured")
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
		logLogsAdminThresholdPreProxyRejection(r, "invalid_target")
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
		logLogsAdminThresholdPreProxyRejection(r, "request_creation_failed")
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
		logLogsAdminThresholdUpstreamFailure(r, target, logsAdminProxyErrorCategory(err))
		adminUsersError(w, http.StatusBadGateway, "Ref Ledger logging service could not be reached.")
		return
	}
	defer response.Body.Close()
	logLogsAdminThresholdUpstreamResponse(r, target, response.StatusCode)
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		redirectTarget := logsAdminRedirectTarget(target, response)
		redirectHost, redirectPath := logsAdminDestinationParts(redirectTarget)
		logLogsAdminThresholdRedirect(r, response.StatusCode, redirectHost, redirectPath)
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
		log.Printf("Ref Ledger logs response forwarding failed: error_category=response_forward_failed")
	}
	for _, name := range []string{"X-Log-Export-Status", "X-Log-Export-Audit"} {
		if value := response.Trailer.Get(name); value != "" {
			w.Header().Set(name, value)
		}
	}
}

func isLogsAdminThresholdRead(r *http.Request) bool {
	return r.Method == http.MethodGet && r.URL.Path == logsAdminThresholdPath
}

func logLogsAdminThresholdPreProxyRejection(r *http.Request, reason string) {
	if isLogsAdminThresholdRead(r) {
		log.Printf("Ref Ledger threshold diagnostic: disposition=monitor_rejected stage=before_proxy reason=%s", reason)
	}
}

func logLogsAdminThresholdUpstreamFailure(r *http.Request, target *url.URL, category string) {
	if !isLogsAdminThresholdRead(r) {
		return
	}
	host, path := logsAdminDestinationParts(target)
	log.Printf("Ref Ledger threshold diagnostic: disposition=upstream_attempt upstream_host=%s upstream_path=%s outcome=request_failed error_category=%s",
		host, path, category)
}

func logLogsAdminThresholdUpstreamResponse(r *http.Request, target *url.URL, status int) {
	if !isLogsAdminThresholdRead(r) {
		return
	}
	host, path := logsAdminDestinationParts(target)
	log.Printf("Ref Ledger threshold diagnostic: disposition=upstream_attempt upstream_host=%s upstream_path=%s outcome=response status=%d",
		host, path, status)
}

func logLogsAdminThresholdRedirect(r *http.Request, status int, host, path string) {
	if isLogsAdminThresholdRead(r) {
		log.Printf("Ref Ledger threshold diagnostic: outcome=redirect status=%d redirect_host=%s redirect_path=%s",
			status, host, path)
	}
}

func logsAdminProxyErrorCategory(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return "timeout"
	}
	return "request_failed"
}

func logsAdminRedirectTarget(target *url.URL, response *http.Response) *url.URL {
	location, err := response.Location()
	if err != nil {
		return nil
	}
	return target.ResolveReference(location)
}

func logsAdminDestinationParts(target *url.URL) (string, string) {
	if target == nil {
		return "unknown", "unknown"
	}
	return sanitizeLogsAdminHostname(target.Hostname()), sanitizeLogsAdminPath(target.EscapedPath())
}

func sanitizeLogsAdminHostname(hostname string) string {
	if ip := net.ParseIP(hostname); ip != nil {
		return ip.String()
	}
	hostname = strings.TrimSuffix(strings.ToLower(hostname), ".")
	if hostname == "" || len(hostname) > 253 {
		return "unknown"
	}
	for _, label := range strings.Split(hostname, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "unknown"
		}
		for _, character := range label {
			if !(character >= 'a' && character <= 'z') &&
				!(character >= '0' && character <= '9') && character != '-' {
				return "unknown"
			}
		}
	}
	return hostname
}

func sanitizeLogsAdminPath(path string) string {
	if path == "" {
		return "/"
	}
	if len(path) > 512 {
		return "[omitted]"
	}
	for _, character := range path {
		if character < 0x20 || character == 0x7f {
			return "[omitted]"
		}
	}
	return path
}
