package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const announcementMonitorPath = "/api/admin/announcements"
const announcementUpstreamPath = "/api/internal/v1/admin/announcements"
const announcementResponseLimit = 1 << 20
const announcementListResponseLimit = 16 << 20

var announcementID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

type announcementProxy struct {
	base   *url.URL
	token  string
	client *http.Client
}

func configureAnnouncementProxy(environment string) (*announcementProxy, error) {
	raw, token := strings.TrimSpace(os.Getenv("ANNOUNCEMENTS_ADMIN_URL")), os.Getenv("ANNOUNCEMENTS_ADMIN_TOKEN")
	if raw == "" && token == "" {
		return nil, nil
	}
	if raw == "" || len(token) < 32 {
		return nil, errors.New("invalid announcements configuration")
	}
	for _, c := range token {
		if c < '!' || c > '~' {
			return nil, errors.New("invalid announcements configuration")
		}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.Host == "" || u.Hostname() == "" || u.User != nil ||
		strings.ContainsAny(raw, "?#") || (u.Path != "" && u.Path != "/") || u.RawPath != "" {
		return nil, errors.New("invalid announcements admin URL")
	}
	ip := net.ParseIP(u.Hostname())
	loopback := strings.EqualFold(u.Hostname(), "localhost") || ip != nil && ip.IsLoopback()
	if u.Scheme != "https" && !(u.Scheme == "http" && strings.EqualFold(environment, "development") && loopback) {
		return nil, errors.New("announcements admin URL requires HTTPS")
	}
	u.Path = announcementUpstreamPath
	return &announcementProxy{
		base: u, token: token,
		client: &http.Client{Timeout: 4 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
	}, nil
}

type announcementInput struct {
	Message         string `json:"message"`
	Audience        string `json:"audience"`
	TenantID        string `json:"tenantId,omitempty"`
	UserID          string `json:"userId,omitempty"`
	Username        string `json:"username,omitempty"`
	SessionTargetID string `json:"sessionTargetId,omitempty"`
	StartsAt        string `json:"startsAt,omitempty"`
	EndsAt          string `json:"endsAt,omitempty"`
	Timezone        string `json:"timezone,omitempty"`
	Version         *int   `json:"version,omitempty"`
}

type announcement struct {
	ID              string `json:"id"`
	Message         string `json:"message"`
	Audience        string `json:"audience"`
	TenantID        string `json:"tenantId,omitempty"`
	UserID          string `json:"userId,omitempty"`
	Username        string `json:"username,omitempty"`
	SessionTargetID string `json:"sessionTargetId,omitempty"`
	StartsAt        string `json:"startsAt"`
	EndsAt          string `json:"endsAt,omitempty"`
	Timezone        string `json:"timezone,omitempty"`
	Cleared         bool   `json:"cleared"`
	ClearedAt       string `json:"clearedAt,omitempty"`
	CreatedBy       string `json:"createdBy,omitempty"`
	CreatedAt       string `json:"createdAt"`
	UpdatedAt       string `json:"updatedAt"`
	Version         int    `json:"version"`
}

type announcementSession struct {
	SessionTargetID string `json:"sessionTargetId"`
	TenantID        string `json:"tenantId"`
	UserID          string `json:"userId"`
	Username        string `json:"username"`
	Platform        string `json:"platform"`
	CreatedAt       string `json:"createdAt"`
	ExpiresAt       string `json:"expiresAt"`
}

type announcementSessions struct {
	TenantID string                `json:"tenantId"`
	UserID   string                `json:"userId"`
	Username string                `json:"username"`
	Sessions []announcementSession `json:"sessions"`
}

type announcementUser struct {
	ID       primitive.ObjectID `bson:"_id" json:"id"`
	Username string             `bson:"username" json:"username"`
	Name     string             `bson:"name" json:"name"`
	TenantID string             `bson:"tenantId" json:"tenantId"`
}

func (app *Application) adminAnnouncementUsersSearch(w http.ResponseWriter, r *http.Request) {
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
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value < 0 || value > 1000000 {
			adminUsersError(w, http.StatusBadRequest, "Invalid page offset.")
			return
		}
		offset = value
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
	const pageSize int64 = 50
	cursor, err := app.db.Collection("users").Find(ctx, filter, options.Find().
		SetProjection(bson.M{"_id": 1, "username": 1, "name": 1, "tenantId": 1}).
		SetSort(bson.D{{Key: "username", Value: 1}, {Key: "_id", Value: 1}}).
		SetSkip(offset).SetLimit(pageSize+1))
	if err != nil {
		adminUsersError(w, http.StatusInternalServerError, "Unable to load users.")
		return
	}
	defer cursor.Close(ctx)
	users := make([]announcementUser, 0)
	if err := cursor.All(ctx, &users); err != nil {
		adminUsersError(w, http.StatusInternalServerError, "Unable to read users.")
		return
	}
	hasMore := len(users) > int(pageSize)
	if hasMore {
		users = users[:pageSize]
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"users": users, "hasMore": hasMore, "offset": offset, "pageSize": pageSize,
	})
}

func (app *Application) adminAnnouncementsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	suffix := strings.TrimPrefix(r.URL.Path, announcementMonitorPath)
	var allow, path string
	var sessions bool
	switch {
	case suffix == "":
		allow, path = "GET, POST", announcementUpstreamPath
		if r.Method != http.MethodGet && r.Method != http.MethodPost {
			w.Header().Set("Allow", allow)
			adminUsersError(w, http.StatusMethodNotAllowed, "Method not allowed.")
			return
		}
	case suffix == "/sessions":
		if !adminUsersMethod(w, r, http.MethodGet) {
			return
		}
		query := r.URL.Query()
		if len(query) != 1 || len(query["username"]) != 1 || strings.TrimSpace(query.Get("username")) == "" || len(query.Get("username")) > 254 {
			adminUsersError(w, http.StatusBadRequest, "A username is required.")
			return
		}
		path, sessions = announcementUpstreamPath+"/sessions", true
	default:
		parts := strings.Split(strings.TrimPrefix(suffix, "/"), "/")
		if !strings.HasPrefix(suffix, "/") || len(parts) < 1 || len(parts) > 2 || !announcementID.MatchString(parts[0]) ||
			parts[0] == "sessions" || len(parts) == 2 && parts[1] != "clear" {
			http.NotFound(w, r)
			return
		}
		allow = http.MethodPut
		if len(parts) == 2 {
			allow = http.MethodPost
		}
		if !adminUsersMethod(w, r, allow) {
			return
		}
		path = announcementUpstreamPath + suffix
	}
	if !sessions && r.URL.RawQuery != "" {
		adminUsersError(w, http.StatusBadRequest, "Unexpected query parameters.")
		return
	}
	if app.announcements == nil {
		adminUsersError(w, http.StatusServiceUnavailable, "Announcements are unavailable.")
		return
	}
	actor, ok := r.Context().Value(adminActorKey{}).(string)
	if !ok || actor == "" {
		adminUsersError(w, http.StatusUnauthorized, "Administrator sign-in required.")
		return
	}
	var body []byte
	if r.Method == http.MethodPost && suffix == "" || r.Method == http.MethodPut {
		var input *announcementInput
		if !decodeAdminJSON(w, r, &input) {
			return
		}
		if input == nil {
			adminUsersError(w, http.StatusBadRequest, "Invalid JSON request.")
			return
		}
		body, _ = json.Marshal(input)
	} else if r.Method == http.MethodPost {
		var input *struct{}
		if !decodeAdminJSON(w, r, &input) {
			return
		}
		if input == nil {
			adminUsersError(w, http.StatusBadRequest, "Invalid JSON request.")
			return
		}
		body = []byte("{}")
	}
	u := *app.announcements.base
	u.Path = path
	if sessions {
		u.RawQuery = url.Values{"username": {r.URL.Query().Get("username")}}.Encode()
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, u.String(), bytes.NewReader(body))
	if err != nil {
		announcementUpstreamFailure(w)
		return
	}
	req.Header.Set("Authorization", "Bearer "+app.announcements.token)
	req.Header.Set("X-Ref-Ledger-Admin-Actor", logsThresholdActor(actor))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := app.announcements.client.Do(req)
	if err != nil {
		announcementUpstreamFailure(w)
		return
	}
	defer resp.Body.Close()
	limit := announcementResponseLimit
	if suffix == "" && r.Method == http.MethodGet {
		limit = announcementListResponseLimit
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, int64(limit)+1))
	if err != nil || len(data) > limit {
		announcementUpstreamFailure(w)
		return
	}
	switch resp.StatusCode {
	case http.StatusBadRequest:
		adminUsersError(w, resp.StatusCode, "Invalid announcement request. Review the message, audience, and schedule.")
		return
	case http.StatusNotFound:
		adminUsersError(w, resp.StatusCode, "Announcement or selected account was not found. Refresh and verify the selection.")
		return
	case http.StatusConflict:
		adminUsersError(w, resp.StatusCode, "The announcement changed. Refresh before retrying.")
		return
	case http.StatusUnprocessableEntity:
		adminUsersError(w, resp.StatusCode, "The session is invalid, expired, or does not match the selected account. Select a new session.")
		return
	}
	expected := http.StatusOK
	if suffix == "" && r.Method == http.MethodPost {
		expected = http.StatusCreated
	}
	if resp.StatusCode != expected {
		announcementUpstreamFailure(w)
		return
	}
	var result any
	switch {
	case sessions:
		var value announcementSessions
		if json.Unmarshal(data, &value) != nil || value.TenantID == "" || value.UserID == "" || value.Username == "" || value.Sessions == nil {
			announcementUpstreamFailure(w)
			return
		}
		for _, session := range value.Sessions {
			if session.SessionTargetID == "" || session.TenantID != value.TenantID || session.UserID != value.UserID || session.Username != value.Username {
				announcementUpstreamFailure(w)
				return
			}
		}
		result = value
	case suffix == "" && r.Method == http.MethodGet:
		var value struct {
			Announcements []announcement `json:"announcements"`
		}
		if json.Unmarshal(data, &value) != nil || value.Announcements == nil {
			announcementUpstreamFailure(w)
			return
		}
		result = value
	default:
		var value announcement
		if json.Unmarshal(data, &value) != nil || value.ID == "" {
			announcementUpstreamFailure(w)
			return
		}
		result = value
	}
	safe, err := json.Marshal(result)
	if err != nil || bytes.Contains(safe, []byte(app.announcements.token)) {
		announcementUpstreamFailure(w)
		return
	}
	writeJSON(w, resp.StatusCode, result)
}

func announcementUpstreamFailure(w http.ResponseWriter) {
	adminUsersError(w, http.StatusBadGateway, "The announcement operation could not be confirmed. Refresh before retrying.")
}
