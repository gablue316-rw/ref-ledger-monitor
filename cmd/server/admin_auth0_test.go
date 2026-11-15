package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAdminAuth0Deletion(t *testing.T) {
	for _, status := range []int{204, 404, 401, 403, 429, 500} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			deletes := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/oauth/token":
					var body map[string]string
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					if r.Method != "POST" || body["grant_type"] != "client_credentials" || body["client_secret"] != "secret" {
						t.Error("incorrect token request")
					}
					w.Header().Set("Content-Type", "application/json")
					w.Write([]byte(`{"access_token":"test-token"}`))
				case "/api/v2/users/auth0|user/with?chars":
					deletes++
					if r.Method != "DELETE" || r.Header.Get("Authorization") != "Bearer test-token" {
						t.Error("incorrect delete request")
					}
					w.WriteHeader(status)
				default:
					t.Errorf("unexpected path %q", r.URL.Path)
					w.WriteHeader(400)
				}
			}))
			defer server.Close()
			client := adminAuth0Client{issuer: server.URL + "/", clientID: "client", secret: "secret", http: server.Client()}
			err := client.deleteIdentity(context.Background(), "auth0|user/with?chars")
			if (err == nil) != (status == 204 || status == 404) {
				t.Fatalf("status %d: %v", status, err)
			}
			if deletes != 1 {
				t.Fatalf("delete count %d", deletes)
			}
		})
	}
}

func TestAdminAuth0TokenFailureStopsDeletion(t *testing.T) {
	for _, body := range []string{`{"error":"secret"}`, `not JSON`, `{"access_token":""}`} {
		calls := 0
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.Write([]byte(body)) }))
		c := adminAuth0Client{issuer: s.URL + "/", http: s.Client()}
		err := c.deleteIdentity(context.Background(), "auth0|123")
		s.Close()
		if err == nil || calls != 1 || strings.Contains(err.Error(), "secret") {
			t.Fatalf("unsafe token failure: calls=%d err=%v", calls, err)
		}
	}
}

func TestAdminAuth0ConfigRejectsWrongIdentity(t *testing.T) {
	t.Setenv("AUTH0_MANAGEMENT_ISSUER", "https://example.us.auth0.com/")
	t.Setenv("AUTH0_MANAGEMENT_CLIENT_ID", "id")
	t.Setenv("AUTH0_MANAGEMENT_CLIENT_SECRET", "secret")
	for _, identity := range []adminAuth0Identity{{}, {Issuer: "https://other.us.auth0.com/", Subject: "auth0|1"}, {Issuer: "https://example.us.auth0.com/"}} {
		if _, err := newAdminAuth0Client(identity); err == nil {
			t.Fatal("invalid identity accepted")
		}
	}
	if _, err := newAdminAuth0Client(adminAuth0Identity{Issuer: "https://example.us.auth0.com/", Subject: "auth0|1"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AUTH0_MANAGEMENT_ISSUER", "http://example.us.auth0.com/")
	if _, err := newAdminAuth0Client(adminAuth0Identity{}); err == nil {
		t.Fatal("HTTP accepted")
	}
}

func TestAdminAuth0DoesNotFollowRedirect(t *testing.T) {
	targetCalls := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetCalls++ }))
	defer target.Close()
	source := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	t.Setenv("AUTH0_MANAGEMENT_ISSUER", source.URL+"/")
	t.Setenv("AUTH0_MANAGEMENT_CLIENT_ID", "id")
	t.Setenv("AUTH0_MANAGEMENT_CLIENT_SECRET", "secret")
	c, err := newAdminAuth0Client(adminAuth0Identity{Issuer: source.URL + "/", Subject: "auth0|1"})
	if err != nil {
		t.Fatal(err)
	}
	c.http.Transport = source.Client().Transport
	if err = c.deleteIdentity(context.Background(), "auth0|1"); err == nil || targetCalls != 0 {
		t.Fatalf("redirect leaked credentials: %v, calls %d", err, targetCalls)
	}
}

// These tests are sequential; restore the default transport after each subtest.
func mockAdminAuth0(t *testing.T, status int) (string, *int) {
	t.Helper()
	calls := new(int)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/token" {
			w.Write([]byte(`{"access_token":"test"}`))
			return
		}
		*calls++
		w.WriteHeader(status)
	}))
	original := http.DefaultTransport
	http.DefaultTransport = server.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = original; server.Close() })
	t.Setenv("AUTH0_MANAGEMENT_ISSUER", server.URL+"/")
	t.Setenv("AUTH0_MANAGEMENT_CLIENT_ID", "id")
	t.Setenv("AUTH0_MANAGEMENT_CLIENT_SECRET", "secret")
	return server.URL + "/", calls
}

func TestAdminAuth0TokenHTTPFailure(t *testing.T) {
	calls := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte("private token response"))
	}))
	defer s.Close()
	c := adminAuth0Client{issuer: s.URL + "/", http: s.Client()}
	err := c.deleteIdentity(context.Background(), "auth0|123")
	if err == nil || calls != 1 || strings.Contains(err.Error(), "private") {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}
