package lfx

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newE2EClient follows the plugins/fossa/client_e2e_test.go convention:
// skip unless a live token is set, so CI and `make ci-local` are unaffected.
// No handle is ever hardcoded - the probe subject comes from an env var with
// no default, and the test skips (rather than fails) when it is unset.
func newE2EClient(t *testing.T) *Client {
	t.Helper()

	token := os.Getenv("LFX_AUTH_TOKEN")
	if token == "" {
		t.Skip("LFX_AUTH_TOKEN not set; skipping live LFX probe")
	}

	return &Client{
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
		Token:      token,
	}
}

// redactedShape walks a decoded JSON value and emits one line per leaf path
// in the form "path -> goType (len N)". Scalar values are never printed -
// only their Go type and, for strings/arrays/maps, their length - so the
// summary can be safely logged and pasted into chat without disclosing any
// of the underlying payload's content.
func redactedShape(prefix string, value any, out *[]string) {
	switch typed := value.(type) {
	case map[string]any:
		if prefix != "" {
			*out = append(*out, fmt.Sprintf("%s -> object (len %d)", prefix, len(typed)))
		}
		keys := make([]string, 0, len(typed))
		for k := range typed {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			childPrefix := k
			if prefix != "" {
				childPrefix = prefix + "." + k
			}
			redactedShape(childPrefix, typed[k], out)
		}
	case []any:
		*out = append(*out, fmt.Sprintf("%s -> array (len %d)", prefix, len(typed)))
		for i, item := range typed {
			redactedShape(fmt.Sprintf("%s[%d]", prefix, i), item, out)
		}
	case string:
		*out = append(*out, fmt.Sprintf("%s -> string (len %d)", prefix, len(typed)))
	case nil:
		*out = append(*out, fmt.Sprintf("%s -> null", prefix))
	default:
		*out = append(*out, fmt.Sprintf("%s -> %T", prefix, typed))
	}
}

func logRedactedShape(t *testing.T, label string, raw json.RawMessage) {
	t.Helper()
	if len(raw) == 0 {
		t.Logf("%s: <empty>", label)
		return
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Logf("%s: failed to decode for shape summary: %v", label, err)
		return
	}
	var lines []string
	redactedShape("", decoded, &lines)
	sort.Strings(lines)
	t.Logf("%s key shape (values redacted):", label)
	for _, line := range lines {
		t.Logf("  %s", line)
	}

	// Opt-in only: the default committed behavior redacts every value, so
	// anything logged here is safe to paste into chat or a PR. Setting
	// LFX_PROBE_RAW=true prints the real payload for local debugging in your
	// own terminal - do not paste that output anywhere it could be
	// committed, filed, or shared, per the branch's no-PII-in-tests rule.
	if os.Getenv("LFX_PROBE_RAW") == "true" {
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, raw, "", "  "); err != nil {
			t.Logf("%s raw (LFX_PROBE_RAW=true, indent failed): %s", label, string(raw))
			return
		}
		t.Logf("%s raw (LFX_PROBE_RAW=true - local eyes only, do not paste/commit):\n%s", label, pretty.String())
	}
}

// TestLFXLiveProbe grounds the client's typed model in an observed payload
// rather than the guesswork that produced it. It never asserts on or logs
// any actual field value - only shapes - and it never hardcodes a subject:
// the probe reads LFX_PROBE_GITHUB_HANDLE with no default and skips when
// unset, so no personal handle is ever committed to this file.
//
//	LFX_AUTH_TOKEN='<paste>' LFX_PROBE_GITHUB_HANDLE='<your handle>' \
//	  go test -v -run TestLFXLiveProbe ./lfx/...
func TestLFXLiveProbe(t *testing.T) {
	client := newE2EClient(t)

	handle := os.Getenv("LFX_PROBE_GITHUB_HANDLE")
	if handle == "" {
		t.Skip("LFX_PROBE_GITHUB_HANDLE not set; skipping live LFX user probe")
	}

	ctx := context.Background()

	users, err := client.SearchUsers(ctx, UserSearch{GitHubID: handle, PageSize: 10})
	require.NoError(t, err)
	require.Len(t, users, 1, "expected exactly one LFX user for the probe handle")

	user := users[0]
	logRedactedShape(t, "User", user.Raw)
	assert.NotEmpty(t, user.Account, "expected a non-empty Account payload to ground the Account struct redesign")

	identities, err := client.GetUserIdentities(ctx, user.ID)
	require.NoError(t, err)
	require.NotEmpty(t, identities, "expected at least one identity for the probe user")

	var sawGitHubIdentity bool
	for _, identity := range identities {
		logRedactedShape(t, "Identity", identity.Raw)
		if strings.EqualFold(strings.TrimSpace(identity.Source), "github") && strings.EqualFold(identity.Username, handle) {
			sawGitHubIdentity = true
		}
	}
	assert.True(t, sawGitHubIdentity, "expected a github-source identity matching the probe handle")
}

// TestLFXProbeMaintainerIdentities iterates a caller-supplied list of real
// project maintainers' GitHub handles and reports, per handle, how the
// legacy user-service resolves them: how many users SearchUsers matches,
// each match's Salesforce record Type (a two-value enum, "lead" or
// "contact" - not PII, so it is logged directly rather than redacted, and
// is exactly the signal that explained the "old profile" result seen
// earlier: a `lead` record has no linked identities because it was never
// claimed/converted), and how many identities GetUserIdentities finds for
// that specific record. This is exploratory, not a pass/fail check - it has
// no assertions - because the point is to observe real API behavior across
// several accounts, not to encode a hypothesis about correct behavior
// before we have one.
//
// LFX_PROBE_GITHUB_HANDLES has no default and is never a hardcoded roster:
// it is a comma-separated list the caller supplies from their own shell,
// typically copied from a project's MAINTAINERS.md/.project/maintainers.yaml
// GitHub handles (already public). The test skips entirely when it is
// unset.
//
//	LFX_AUTH_TOKEN='<paste>' LFX_PROBE_GITHUB_HANDLES='handle-one,handle-two,handle-three' \
//	  go test -v -run TestLFXProbeMaintainerIdentities ./lfx/...
func TestLFXProbeMaintainerIdentities(t *testing.T) {
	client := newE2EClient(t)

	handlesEnv := os.Getenv("LFX_PROBE_GITHUB_HANDLES")
	if handlesEnv == "" {
		t.Skip("LFX_PROBE_GITHUB_HANDLES not set; skipping live multi-handle maintainer probe")
	}

	ctx := context.Background()

	for handle := range strings.SplitSeq(handlesEnv, ",") {
		handle := strings.TrimSpace(handle)
		if handle == "" {
			continue
		}

		t.Run(handle, func(t *testing.T) {
			users, err := client.SearchUsers(ctx, UserSearch{GitHubID: handle, PageSize: 10})
			if err != nil {
				t.Logf("%s: SearchUsers error: %v", handle, err)
				return
			}
			t.Logf("%s: SearchUsers matched %d user(s)", handle, len(users))

			for i, user := range users {
				var typed struct {
					Type string
				}
				_ = json.Unmarshal(user.Raw, &typed)
				t.Logf("%s: user[%d] Type=%q", handle, i, typed.Type)
				logRedactedShape(t, fmt.Sprintf("%s user[%d]", handle, i), user.Raw)

				identities, err := client.GetUserIdentities(ctx, user.ID)
				if err != nil {
					t.Logf("%s: user[%d] GetUserIdentities error: %v", handle, i, err)
					continue
				}
				t.Logf("%s: user[%d] has %d identities", handle, i, len(identities))
				for j, identity := range identities {
					logRedactedShape(t, fmt.Sprintf("%s user[%d] identity[%d]", handle, i, j), identity.Raw)
				}
			}
		})
	}
}

// TestLFXProbeProjectMaintainers probes the real maintainer-roster endpoint
// found in the downloaded OpenAPI spec (see lfx/LFX-USER-API-NOTES.MD finding
// 12): GET /v1/maintainers, operationId findMaintainers, filterable by
// ProjectID via $filter. This supersedes the spec's original (wrong) guess at
// /v1/projects/{id}/maintainers, which TestLFXLiveProbeProjectMaintainers
// below still probes for its own documented reason.
//
// This is exploratory, not a pass/fail check - it has no assertions. Besides
// the well-formed filtered request, it deliberately also issues one request
// with a malformed $filter expression, to empirically test finding 5's claim
// that a bad filter degrades to an unfiltered listing rather than an error -
// that claim currently rests on spec prose alone.
//
// LFX_PROBE_PROJECT_SFID has no default and the test skips when it is unset -
// it is the caller's own project Salesforce ID, supplied from their own
// shell, never hardcoded here.
//
//	LFX_AUTH_TOKEN='<paste>' LFX_PROBE_PROJECT_SFID='<project sfid>' \
//	  go test -v -run TestLFXProbeProjectMaintainers ./lfx/...
func TestLFXProbeProjectMaintainers(t *testing.T) {
	client := newE2EClient(t)

	projectSFID := os.Getenv("LFX_PROBE_PROJECT_SFID")
	if projectSFID == "" {
		t.Skip("LFX_PROBE_PROJECT_SFID not set; skipping live /v1/maintainers probe")
	}

	ctx := context.Background()
	base := strings.TrimRight(client.BaseURL, "/")
	if base == "" {
		base = DefaultBaseURL
	}

	probeMaintainers := func(t *testing.T, label, filter string) {
		t.Helper()

		// Built inline rather than via client.get, matching the convention
		// established by TestLFXLiveProbeProjectMaintainers below: this keeps
		// gosec's G704 SSRF taint check on this deliberate, operator-driven
		// probe request rather than on the shared client.go request path.
		reqURL := base + "/user-service/v1/maintainers"
		if filter != "" {
			reqURL += "?" + url.Values{"$filter": {filter}}.Encode()
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
		require.NoError(t, err)
		client.addHeaders(req)

		resp, err := client.HTTPClient.Do(req) //nolint:gosec // G704: see comment above; deliberate operator-driven probe request, not tainted external input
		require.NoError(t, err)
		defer resp.Body.Close()
		t.Logf("%s: GET /v1/maintainers status: %s", label, resp.Status)

		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)

		var decoded struct {
			Data     json.RawMessage `json:"Data"`
			Metadata struct {
				Offset    int `json:"Offset"`
				PageSize  int `json:"PageSize"`
				TotalSize int `json:"TotalSize"`
			} `json:"Metadata"`
		}
		if err := json.Unmarshal(body, &decoded); err != nil {
			t.Logf("%s: failed to decode envelope: %v", label, err)
			logRedactedShape(t, label+" raw body", body)
			return
		}

		var records []json.RawMessage
		_ = json.Unmarshal(decoded.Data, &records)
		t.Logf("%s: Metadata.TotalSize=%d, returned record count=%d", label, decoded.Metadata.TotalSize, len(records))
		for i, record := range records {
			logRedactedShape(t, fmt.Sprintf("%s record[%d]", label, i), record)
		}
	}

	t.Run("filtered_by_project", func(t *testing.T) {
		probeMaintainers(t, "filtered_by_project", fmt.Sprintf("ProjectID eq %s", projectSFID))
	})

	t.Run("malformed_filter", func(t *testing.T) {
		// Deliberately invalid $filter syntax - per finding 5, the spec claims
		// this degrades to an unfiltered full listing instead of an error.
		probeMaintainers(t, "malformed_filter", "this is not a valid $filter expression===")
	})
}

// TestLFXLiveProbeProjectMaintainers records evidence for D5: the
// specification currently calls for a ListProjectMaintainers method at
// GET /v1/projects/{projectID}/maintainers, but the lfx-skills V2 repo map
// has no owner for that resource. A live 404 here is stronger evidence than
// the routing inference alone that the spec's endpoint doesn't exist -
// *provided* the request actually reaches the /user-service gateway (an
// earlier version of this probe omitted the required /user-service prefix
// - see LFX-USER-API-NOTES.MD finding 2 - and its 404 was therefore not
// meaningful evidence; this probe now includes it, so a 404 here is real).
// LFX_PROBE_PROJECT_ID has no default and the test skips when it is unset -
// this probe is corroborating evidence, not the sole basis for the D5
// recommendation.
func TestLFXLiveProbeProjectMaintainers(t *testing.T) {
	client := newE2EClient(t)

	projectID := os.Getenv("LFX_PROBE_PROJECT_ID")
	if projectID == "" {
		t.Skip("LFX_PROBE_PROJECT_ID not set; skipping live ListProjectMaintainers probe")
	}

	// Built inline rather than via client.get so gosec's SSRF taint check
	// (correctly, in general) doesn't need to trust that a caller-supplied
	// path segment is safe: this is a local, skip-by-default probe where
	// projectID is an operator-supplied env var read from the operator's
	// own shell, not attacker-controlled input reaching a deployed service.
	base := strings.TrimRight(client.BaseURL, "/")
	if base == "" {
		base = DefaultBaseURL
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, base+"/user-service/v1/projects/"+url.PathEscape(projectID)+"/maintainers", nil)
	require.NoError(t, err)
	client.addHeaders(req)

	resp, err := client.HTTPClient.Do(req) //nolint:gosec // G704: see comment above; deliberate operator-driven probe request, not tainted external input
	require.NoError(t, err)
	defer resp.Body.Close()
	t.Logf("GET /v1/projects/{id}/maintainers status: %s", resp.Status)
}
