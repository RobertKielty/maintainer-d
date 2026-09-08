package lfx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maintainerd/dotproject"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const DefaultBaseURL = "https://api-gw.platform.linuxfoundation.org"
const TokenRefreshURL = "https://app.lfx.dev/settings" //nolint:gosec // public settings page URL, not a credential

type Client struct {
	BaseURL    string
	HTTPClient *http.Client
	Token      string
	ACL        string
	Username   string
	Email      string
	MinDelay   time.Duration

	mu          sync.Mutex
	lastRequest time.Time
}

// HTTPStatusError is returned by Client.get for any non-2xx response, so
// callers can classify LFX failures by status code instead of guessing from
// error text.
type HTTPStatusError struct {
	StatusCode int
	Status     string
	Body       string
	URL        string
}

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("GET %s: %s: %s", e.URL, e.Status, e.Body)
}

// PlatformAccessError wraps an LFX API failure with a message matched to its
// actual cause: an expired/invalid token (401/403) genuinely needs a token
// refresh, but a timeout, rate limit, or 5xx does not - pointing at
// LFX_AUTH_TOKEN in those cases is misleading and sends whoever is
// debugging an outage to the wrong place.
func PlatformAccessError(err error) error {
	if err == nil {
		return nil
	}
	var httpErr *HTTPStatusError
	switch {
	case errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusUnauthorized:
		// A dead token (401) and a rate limit (429) are marked fatal to a
		// sync run: every remaining project would hit the same wall, so
		// retrying per-project just burns quota. A 403 stays an ordinary
		// per-project error - it can be resource-specific (ACL scope), and
		// issue #150's verified behavior is that a mid-run LFX-path 403 does
		// not kill the run. Timeouts, 5xx and transport errors below are
		// ordinary per-project errors too.
		return dotproject.FatalSyncError{Err: fmt.Errorf("LFX Platform access failed (HTTP 401, invalid or expired token); update LFX_AUTH_TOKEN with a fresh token from %s: %w", TokenRefreshURL, err)}
	case errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusForbidden:
		return fmt.Errorf("LFX Platform denied this request (HTTP 403); the token may lack access to this resource (X-ACL scope) or be expired - check LFX_AUTH_TOKEN and LFX_ACL, refresh at %s: %w", TokenRefreshURL, err)
	case errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusTooManyRequests:
		return dotproject.FatalSyncError{Err: fmt.Errorf("LFX Platform rate limited this request (HTTP 429); not a token problem, back off LFX_REQUEST_DELAY: %w", err)}
	case errors.As(err, &httpErr):
		return fmt.Errorf("LFX Platform returned HTTP %d; not necessarily a token problem: %w", httpErr.StatusCode, err)
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("LFX Platform request timed out (context deadline exceeded); not a token problem, this is a slow response or the run's overall time budget was exhausted: %w", err)
	default:
		// DNS failures, TLS errors, connection resets, and cancellations all
		// land here; none of them are cured by a fresh token, so the token
		// guidance stays reserved for the explicit 401/403 branch above.
		return fmt.Errorf("LFX Platform request failed (transport error, not necessarily a token problem): %w", err)
	}
}

func (c *Client) CheckToken(ctx context.Context) error {
	username := strings.TrimSpace(c.Username)
	if username == "" {
		username = "maintainer-d-token-check-no-such-user"
	}
	_, err := c.SearchUsers(ctx, UserSearch{Username: username, PageSize: 1})
	return err
}

type UserSearch struct {
	GitHubID string
	Email    string
	Username string
	PageSize int
}

type User struct {
	ID               string          `json:"ID"`
	FirstName        string          `json:"FirstName"`
	LastName         string          `json:"LastName"`
	Name             string          `json:"Name"`
	Email            string          `json:"Email"`
	Username         string          `json:"Username"`
	Account          json.RawMessage `json:"Account"`
	Type             string          `json:"Type"` // "lead" | "contact" - see LFX-USER-API-NOTES.MD finding 8
	GithubID         string          `json:"GithubID"`
	LastModifiedDate string          `json:"LastModifiedDate"`
	Raw              json.RawMessage `json:"-"`
}

type Identity struct {
	ID         string          `json:"ID"`
	UserSFID   string          `json:"UserSFID"`
	Email      string          `json:"Email"`
	FirstName  string          `json:"FirstName"`
	LastName   string          `json:"LastName"`
	Username   string          `json:"Username"`
	Source     string          `json:"Source"`
	IsVerified bool            `json:"IsVerified"`
	Raw        json.RawMessage `json:"-"`
}

func (c *Client) SearchUsers(ctx context.Context, query UserSearch) ([]User, error) {
	pageSize := query.PageSize
	if pageSize <= 0 {
		pageSize = 100
	}

	var users []User
	offset := 0
	for {
		values := url.Values{}
		values.Set("pageSize", fmt.Sprintf("%d", pageSize))
		if offset > 0 {
			values.Set("offset", fmt.Sprintf("%d", offset))
		}
		if githubID := strings.TrimSpace(query.GitHubID); githubID != "" {
			values.Set("githubID", githubID)
		}
		if email := strings.TrimSpace(query.Email); email != "" {
			values.Set("email", email)
		}
		if username := strings.TrimSpace(query.Username); username != "" {
			values.Set("username", username)
		}

		var response struct {
			Data     []json.RawMessage `json:"Data"`
			Metadata struct {
				Offset    int `json:"Offset"`
				PageSize  int `json:"PageSize"`
				TotalSize int `json:"TotalSize"`
			} `json:"Metadata"`
		}
		if err := c.get(ctx, "/user-service/v2/users/search", values, &response); err != nil {
			return nil, err
		}
		for _, raw := range response.Data {
			var user User
			if err := json.Unmarshal(raw, &user); err != nil {
				return nil, err
			}
			user.Raw = raw
			users = append(users, user)
		}

		offset += len(response.Data)
		// A short page (fewer rows than requested) means there is nothing left,
		// even if TotalSize is unset or wrong; this also guards against an
		// infinite loop if the server ever returns zero rows for a nonzero
		// TotalSize.
		if len(response.Data) < pageSize || offset >= response.Metadata.TotalSize {
			break
		}
	}
	if users == nil {
		users = []User{}
	}
	return users, nil
}

func (c *Client) GetUserIdentities(ctx context.Context, salesforceID string) ([]Identity, error) {
	id := strings.TrimSpace(salesforceID)
	if id == "" {
		return nil, fmt.Errorf("salesforce user ID is required")
	}

	var response struct {
		Data []json.RawMessage `json:"Data"`
	}
	path := "/user-service/v1/users/" + url.PathEscape(id) + "/identities"
	if err := c.get(ctx, path, nil, &response); err != nil {
		return nil, err
	}
	identities := make([]Identity, 0, len(response.Data))
	for _, raw := range response.Data {
		var identity Identity
		if err := json.Unmarshal(raw, &identity); err != nil {
			return nil, err
		}
		identity.Raw = raw
		identities = append(identities, identity)
	}
	return identities, nil
}

func (c *Client) get(ctx context.Context, path string, values url.Values, target any) error {
	if c == nil {
		return fmt.Errorf("lfx client is required")
	}
	base := strings.TrimRight(c.BaseURL, "/")
	if base == "" {
		base = DefaultBaseURL
	}
	endpoint, err := url.Parse(base + path)
	if err != nil {
		return err
	}
	if len(values) > 0 {
		endpoint.RawQuery = values.Encode()
	}

	client := c.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return err
	}
	c.addHeaders(req)

	if err := c.wait(ctx); err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &HTTPStatusError{
			StatusCode: resp.StatusCode,
			Status:     resp.Status,
			Body:       strings.TrimSpace(string(body)),
			URL:        endpoint.String(),
		}
	}
	if target == nil {
		return nil
	}
	if err := json.Unmarshal(body, target); err != nil {
		return fmt.Errorf("decode %s: %w", endpoint.String(), err)
	}
	return nil
}

func (c *Client) wait(ctx context.Context) error {
	delay := c.MinDelay
	if delay <= 0 {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.lastRequest.IsZero() {
		wait := delay - time.Since(c.lastRequest)
		if wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
	c.lastRequest = time.Now()
	return nil
}

func (c *Client) addHeaders(req *http.Request) {
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "maintainer-d-lfx-enricher")
	if token := strings.TrimSpace(c.Token); token != "" {
		if strings.HasPrefix(strings.ToLower(token), "bearer ") {
			req.Header.Set("Authorization", token)
		} else {
			req.Header.Set("Authorization", "Bearer "+token)
		}
	}
	if acl := strings.TrimSpace(c.ACL); acl != "" {
		req.Header.Set("X-ACL", acl)
	}
	if username := strings.TrimSpace(c.Username); username != "" {
		req.Header.Set("X-USERNAME", username)
	}
	if email := strings.TrimSpace(c.Email); email != "" {
		req.Header.Set("X-EMAIL", email)
	}
	req.Header.Set("X-REQUEST-ID", fmt.Sprintf("maintainer-d-lfx-%d", time.Now().UnixNano()))
}
