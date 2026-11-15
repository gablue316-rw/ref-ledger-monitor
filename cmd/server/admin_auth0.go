package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type adminAuth0Identity struct {
	Issuer  string `bson:"issuer"`
	Subject string `bson:"subject"`
}

type adminAuth0Client struct {
	issuer   string
	clientID string
	secret   string
	http     *http.Client
}

func newAdminAuth0Client(identity adminAuth0Identity) (*adminAuth0Client, error) {
	issuer := strings.TrimRight(strings.TrimSpace(os.Getenv("AUTH0_MANAGEMENT_ISSUER")), "/") + "/"
	u, err := url.Parse(issuer)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Path != "/" || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("configure AUTH0_MANAGEMENT_ISSUER as the HTTPS Auth0 tenant URL")
	}
	if identity.Subject == "" || strings.TrimSpace(identity.Subject) != identity.Subject || identity.Issuer != issuer {
		return nil, errors.New("stored mobileIdentity must contain the exact configured issuer (with trailing slash) and subject; deletion by email is not allowed")
	}
	id, secret := os.Getenv("AUTH0_MANAGEMENT_CLIENT_ID"), os.Getenv("AUTH0_MANAGEMENT_CLIENT_SECRET")
	if id == "" || secret == "" {
		return nil, errors.New("configure AUTH0_MANAGEMENT_CLIENT_ID and AUTH0_MANAGEMENT_CLIENT_SECRET")
	}
	return &adminAuth0Client{issuer: issuer, clientID: id, secret: secret, http: &http.Client{
		Timeout:       10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

// deleteIdentity runs outside MongoDB's retryable transaction callback. An
// already absent identity is success, so local cleanup can safely be retried.
func (c *adminAuth0Client) deleteIdentity(ctx context.Context, subject string) error {
	body, err := json.Marshal(map[string]string{"grant_type": "client_credentials", "client_id": c.clientID, "client_secret": c.secret, "audience": c.issuer + "api/v2/"})
	if err != nil {
		return errors.New("unable to encode Auth0 token request")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.issuer+"oauth/token", bytes.NewReader(body))
	if err != nil {
		return errors.New("unable to construct Auth0 token request")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return errors.New("Auth0 token request failed; check connectivity")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Auth0 token request returned HTTP %d; check Management API credentials and authorization", resp.StatusCode)
	}
	var token struct {
		AccessToken string `json:"access_token"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 65536)).Decode(&token) != nil || token.AccessToken == "" {
		return errors.New("invalid Auth0 token response")
	}
	req, err = http.NewRequestWithContext(ctx, http.MethodDelete, c.issuer+"api/v2/users/"+url.PathEscape(subject), nil)
	if err != nil {
		return errors.New("unable to construct Auth0 deletion request")
	}
	req.Header.Set("Authorization", "Bearer "+token.AccessToken)
	resp, err = c.http.Do(req)
	if err != nil {
		return errors.New("Auth0 deletion could not be confirmed; retry the deletion")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("Auth0 deletion returned HTTP %d; verify delete:users permission and retry", resp.StatusCode)
	}
	return nil
}
