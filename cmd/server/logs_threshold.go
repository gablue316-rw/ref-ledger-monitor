package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const refLedgerThresholdPath = "/api/internal/v1/admin/logs/threshold"

type logsThresholdUpdater struct {
	url    string
	token  string
	client *http.Client
}

func logsThresholdActor(username string) string {
	hash := sha256.Sum256([]byte(username))
	return fmt.Sprintf("monitor-%x", hash)
}

func configureLogsThresholdUpdater(repo *logsRepository, environment string) (*logsThresholdUpdater, error) {
	raw, token := strings.TrimSpace(os.Getenv("LOGS_ADMIN_URL")), strings.TrimSpace(os.Getenv("LOGS_ADMIN_TOKEN"))
	if raw == "" && token == "" {
		return nil, nil
	}
	if repo == nil || raw == "" || len(token) < 32 {
		return nil, errors.New("invalid logs threshold update configuration")
	}
	for _, c := range token {
		if c < '!' || c > '~' {
			return nil, errors.New("invalid logs threshold update configuration")
		}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("invalid logs admin URL")
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	loopback := strings.EqualFold(host, "localhost") || ip != nil && ip.IsLoopback()
	if u.Scheme != "https" && !(u.Scheme == "http" && strings.EqualFold(environment, "development") && loopback) {
		return nil, errors.New("logs admin URL requires HTTPS")
	}
	u.Path = refLedgerThresholdPath
	return &logsThresholdUpdater{
		url: u.String(), token: token,
		client: &http.Client{Timeout: 4 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
	}, nil
}

func (updater *logsThresholdUpdater) request(ctx context.Context, method, actor string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, updater.url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+updater.token)
	req.Header.Set("X-Ref-Ledger-Logs-Version", "1")
	req.Header.Set("X-Ref-Ledger-Admin-Actor", actor)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return updater.client.Do(req)
}

func (updater *logsThresholdUpdater) update(ctx context.Context, environment, actor string, threshold int) error {
	resp, err := updater.request(ctx, http.MethodGet, actor, nil)
	if err != nil {
		return err
	}
	var current struct {
		Environment string `json:"environment"`
		Threshold   int    `json:"threshold"`
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&current)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || err != nil || current.Environment != environment || current.Threshold < 1 || current.Threshold > 4 {
		return errors.New("upstream threshold environment or response is invalid")
	}
	payload, _ := json.Marshal(struct {
		Threshold int `json:"threshold"`
	}{threshold})
	resp, err = updater.request(ctx, http.MethodPut, actor, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New("upstream threshold update failed")
	}
	return nil
}

func (app *Application) logsThresholdUpdate(w http.ResponseWriter, r *http.Request) {
	if app.logsUpdater == nil || !app.logs.settings {
		adminUsersError(w, http.StatusServiceUnavailable, "Capture threshold updates are unavailable.")
		return
	}
	var request struct {
		Threshold   int    `json:"threshold"`
		Environment string `json:"environment"`
	}
	if !decodeAdminJSON(w, r, &request) {
		return
	}
	if request.Threshold < 1 || request.Threshold > 4 || request.Environment != app.logs.environment {
		adminUsersError(w, http.StatusBadRequest, "Invalid threshold or environment.")
		return
	}
	actor, ok := r.Context().Value(adminActorKey{}).(string)
	if !ok || actor == "" {
		adminUsersError(w, http.StatusUnauthorized, "Administrator sign-in required.")
		return
	}
	if _, err := app.logs.captureThreshold(r.Context()); err != nil {
		adminUsersError(w, http.StatusServiceUnavailable, "Capture threshold is unavailable.")
		return
	}
	if err := app.logsUpdater.update(r.Context(), app.logs.environment, logsThresholdActor(actor), request.Threshold); err != nil {
		adminUsersError(w, http.StatusBadGateway, "Unable to confirm the threshold update. Refresh the effective threshold before retrying.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "Threshold update submitted."})
}
