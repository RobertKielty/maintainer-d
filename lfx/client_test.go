package lfx

import (
	"context"
	"errors"
	"fmt"
	"maintainerd/dotproject"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheckTokenUsesUserSearchEndpoint(t *testing.T) {
	t.Parallel()

	var gotPath string
	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		assert.Equal(t, "maintainer-d-token-check-no-such-user", r.URL.Query().Get("username"))
		assert.Equal(t, "1", r.URL.Query().Get("pageSize"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Data":[]}`))
	}))
	defer server.Close()

	client := &Client{
		BaseURL:    server.URL,
		HTTPClient: server.Client(),
		Token:      "active-token",
	}

	require.NoError(t, client.CheckToken(context.Background()))
	assert.Equal(t, "/user-service/v2/users/search", gotPath)
	assert.Equal(t, "Bearer active-token", gotAuth)
}

func TestCheckTokenReturnsAccessError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "expired", http.StatusUnauthorized)
	}))
	defer server.Close()

	client := &Client{
		BaseURL:    server.URL,
		HTTPClient: server.Client(),
		Token:      "expired-token",
	}

	err := client.CheckToken(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "401")
}

func TestSearchUsersSetsAllQueryParamsAndCapturesRaw(t *testing.T) {
	t.Parallel()

	var gotQuery map[string][]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = map[string][]string(r.URL.Query())
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Data":[{"ID":"sfid-fixture-1","Name":"Fixture User","Email":"fixture@example.com","Username":"fixture-handle","Account":{"Name":"Fixture Org"},"Unmodelled":"kept-verbatim"}]}`))
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL, HTTPClient: server.Client()}

	users, err := client.SearchUsers(context.Background(), UserSearch{
		GitHubID: "fixture-handle",
		Email:    "fixture@example.com",
		Username: "fixture-username",
		PageSize: 5,
	})
	require.NoError(t, err)

	assert.Equal(t, []string{"fixture-handle"}, gotQuery["githubID"])
	assert.Equal(t, []string{"fixture@example.com"}, gotQuery["email"])
	assert.Equal(t, []string{"fixture-username"}, gotQuery["username"])
	assert.Equal(t, []string{"5"}, gotQuery["pageSize"])

	require.Len(t, users, 1)
	assert.Equal(t, "sfid-fixture-1", users[0].ID)
	assert.Equal(t, "Fixture User", users[0].Name)
	assert.Contains(t, string(users[0].Raw), "kept-verbatim", "Raw must capture the exact bytes decoded, including unmodelled fields")
}

func TestSearchUsersOmitsEmptyQueryParams(t *testing.T) {
	t.Parallel()

	var gotQuery map[string][]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = map[string][]string(r.URL.Query())
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Data":[]}`))
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL, HTTPClient: server.Client()}

	_, err := client.SearchUsers(context.Background(), UserSearch{GitHubID: "  "})
	require.NoError(t, err)

	assert.NotContains(t, gotQuery, "githubID", "blank/whitespace-only fields must not be sent as query params")
	assert.NotContains(t, gotQuery, "email")
	assert.NotContains(t, gotQuery, "username")
	assert.Equal(t, []string{"100"}, gotQuery["pageSize"], "an unset PageSize must default to a full page so pagination math has a real page size to work with")
}

func TestSearchUsersCollectsAllPages(t *testing.T) {
	t.Parallel()

	var gotOffsets []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotOffsets = append(gotOffsets, r.URL.Query().Get("offset"))
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("offset") {
		case "":
			_, _ = w.Write([]byte(`{"Data":[{"ID":"sfid-1"},{"ID":"sfid-2"}],"Metadata":{"Offset":0,"PageSize":2,"TotalSize":3}}`))
		case "2":
			_, _ = w.Write([]byte(`{"Data":[{"ID":"sfid-3"}],"Metadata":{"Offset":2,"PageSize":2,"TotalSize":3}}`))
		default:
			t.Fatalf("unexpected offset %q", r.URL.Query().Get("offset"))
		}
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL, HTTPClient: server.Client()}

	users, err := client.SearchUsers(context.Background(), UserSearch{Username: "handle-with-11-matches", PageSize: 2})
	require.NoError(t, err)

	assert.Equal(t, []string{"", "2"}, gotOffsets, "must keep requesting pages until TotalSize is covered")
	require.Len(t, users, 3, "a strongest-match candidate on a later page must not be dropped")
	assert.Equal(t, "sfid-1", users[0].ID)
	assert.Equal(t, "sfid-2", users[1].ID)
	assert.Equal(t, "sfid-3", users[2].ID)
}

func TestSearchUsersPaginatesWhenTotalSizeIsAbsent(t *testing.T) {
	t.Parallel()

	var gotOffsets []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotOffsets = append(gotOffsets, r.URL.Query().Get("offset"))
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("offset") {
		case "":
			_, _ = w.Write([]byte(`{"Data":[{"ID":"sfid-1"},{"ID":"sfid-2"}]}`))
		case "2":
			_, _ = w.Write([]byte(`{"Data":[{"ID":"sfid-3"}]}`))
		default:
			t.Fatalf("unexpected offset %q", r.URL.Query().Get("offset"))
		}
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL, HTTPClient: server.Client()}

	users, err := client.SearchUsers(context.Background(), UserSearch{Username: "handle", PageSize: 2})
	require.NoError(t, err)

	assert.Equal(t, []string{"", "2"}, gotOffsets, "a full first page without TotalSize must not end pagination")
	require.Len(t, users, 3)
	assert.Equal(t, "sfid-3", users[2].ID)
}

func TestGetUserIdentitiesHappyPath(t *testing.T) {
	t.Parallel()

	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Data":[{"ID":"identity-fixture-1","UserSFID":"sfid-fixture-1","Username":"fixture-handle","Source":"github","IsVerified":true,"Unmodelled":"kept-verbatim"}]}`))
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL, HTTPClient: server.Client()}

	identities, err := client.GetUserIdentities(context.Background(), "sfid-fixture-1")
	require.NoError(t, err)

	assert.Equal(t, "/user-service/v1/users/sfid-fixture-1/identities", gotPath)
	require.Len(t, identities, 1)
	assert.Equal(t, "identity-fixture-1", identities[0].ID)
	assert.Equal(t, "github", identities[0].Source)
	assert.True(t, identities[0].IsVerified)
	assert.Contains(t, string(identities[0].Raw), "kept-verbatim")
}

func TestGetUserIdentitiesRequiresSalesforceID(t *testing.T) {
	t.Parallel()

	client := &Client{BaseURL: "http://unused.example.invalid"}

	_, err := client.GetUserIdentities(context.Background(), "   ")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "salesforce user ID is required")
}

func TestGetUserIdentitiesEscapesPathSegments(t *testing.T) {
	t.Parallel()

	var gotRawPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// r.URL.Path is the *decoded* path (%2F becomes /); EscapedPath
		// reflects what was actually sent on the wire, which is what
		// url.PathEscape controls.
		gotRawPath = r.URL.EscapedPath()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Data":[]}`))
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL, HTTPClient: server.Client()}

	_, err := client.GetUserIdentities(context.Background(), "sfid/with/slashes")
	require.NoError(t, err)
	assert.Equal(t, "/user-service/v1/users/sfid%2Fwith%2Fslashes/identities", gotRawPath)
}

func TestGetReturnsFormattedErrorOnNon2xx(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("access denied for fixture request"))
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL, HTTPClient: server.Client()}

	err := client.get(context.Background(), "/user-service/v2/users/search", nil, &struct{}{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "403")
	assert.Contains(t, err.Error(), "access denied for fixture request")
}

func TestGetEnforcesResponseSizeCap(t *testing.T) {
	t.Parallel()

	// One byte past the 4 MiB cap: a truncated body is not valid JSON, so
	// unmarshal fails - proving get() actually limits the read rather than
	// buffering the whole response.
	oversized := strings.Repeat("a", (4<<20)+1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Data":"`))
		_, _ = w.Write([]byte(oversized))
		_, _ = w.Write([]byte(`"}`))
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL, HTTPClient: server.Client()}

	var target struct {
		Data string `json:"Data"`
	}
	err := client.get(context.Background(), "/user-service/v2/users/search", nil, &target)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decode")
}

func TestPlatformAccessErrorClassifiesFatality(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		err   error
		fatal bool
	}{
		{name: "401 is fatal", err: &HTTPStatusError{StatusCode: http.StatusUnauthorized, Status: "401 Unauthorized"}, fatal: true},
		// A 403 can be resource-specific (ACL scope), and issue #150's
		// verified behavior is that a mid-run 403 does not kill the run.
		{name: "403 is not fatal", err: &HTTPStatusError{StatusCode: http.StatusForbidden, Status: "403 Forbidden"}, fatal: false},
		{name: "429 is fatal", err: &HTTPStatusError{StatusCode: http.StatusTooManyRequests, Status: "429 Too Many Requests"}, fatal: true},
		{name: "500 is not fatal", err: &HTTPStatusError{StatusCode: http.StatusInternalServerError, Status: "500 Internal Server Error"}, fatal: false},
		{name: "timeout is not fatal", err: fmt.Errorf("request: %w", context.DeadlineExceeded), fatal: false},
		{name: "transport error is not fatal", err: errors.New("connection reset by peer"), fatal: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			classified := PlatformAccessError(tc.err)
			require.Error(t, classified)
			var fatal dotproject.FatalSyncError
			assert.Equal(t, tc.fatal, errors.As(classified, &fatal),
				"fatality classification for %q", tc.name)
			// The original error must stay reachable through the wrap chain.
			assert.ErrorContains(t, classified, "LFX Platform")
		})
	}
}
