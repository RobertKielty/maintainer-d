package provenance

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/go-github/v55/github"
)

func TestParseGitHubBlobURL(t *testing.T) {
	cases := []struct {
		name   string
		url    string
		wantOK bool
		owner  string
		repo   string
		ref    string
		path   string
	}{
		{
			name:   "valid blob url",
			url:    "https://github.com/example-org/example-repo/blob/main/MAINTAINERS.md",
			wantOK: true,
			owner:  "example-org",
			repo:   "example-repo",
			ref:    "main",
			path:   "MAINTAINERS.md",
		},
		{
			name:   "valid blob url with plain query",
			url:    "https://github.com/example-org/example-repo/blob/abc123/nested/dir/file.csv?plain=1",
			wantOK: true,
			owner:  "example-org",
			repo:   "example-repo",
			ref:    "abc123",
			path:   "nested/dir/file.csv",
		},
		{
			name:   "gist is unresolvable",
			url:    "https://gist.github.com/example-user/deadbeefdeadbeefdeadbeefdeadbeef",
			wantOK: false,
		},
		{
			name:   "raw host is unresolvable",
			url:    "https://raw.githubusercontent.com/example-org/example-repo/main/MAINTAINERS.md",
			wantOK: false,
		},
		{
			name:   "non-blob github path is unresolvable",
			url:    "https://github.com/example-org/example-repo",
			wantOK: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			owner, repo, ref, path, ok := ParseGitHubBlobURL(tc.url)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			if owner != tc.owner || repo != tc.repo || ref != tc.ref || path != tc.path {
				t.Errorf("got (%q,%q,%q,%q), want (%q,%q,%q,%q)", owner, repo, ref, path, tc.owner, tc.repo, tc.ref, tc.path)
			}
		})
	}
}

// fakeGitHubServer serves the two endpoints Resolve depends on: a GraphQL
// blame query and the REST commit->PR and PR->reviews lookups. It counts
// requests per endpoint so cache behavior can be asserted.
type fakeGitHubServer struct {
	graphQLCalls  int
	prCalls       int
	reviewCalls   int
	reviewsJSON   string
	graphQLStatus int    // non-zero: fail the blame query with this HTTP status
	prStatus      int    // non-zero: fail the commit->PR listing with this HTTP status
	reviewsStatus int    // non-zero: fail the PR review listing with this HTTP status
	compareStatus int    // non-zero: fail the compare endpoint with this HTTP status
	prHeadSHA     string // non-empty: the merged PR's final head SHA
	prsJSON       string // non-empty: overrides the commit->PR listing body

	// compareStatusByHead maps a review head SHA to the status the compare
	// endpoint reports for base deadbeef...head (default "identical").
	compareStatusByHead map[string]string
}

func (f *fakeGitHubServer) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/graphql":
			f.graphQLCalls++
			if f.graphQLStatus != 0 {
				w.WriteHeader(f.graphQLStatus)
				_, _ = w.Write([]byte(`{}`))
				return
			}
			// The object expression must be a bare commit-ish: "ref:path"
			// resolves to a Blob on the real API, so the Commit fragment
			// (and therefore blame) silently returns nothing.
			body, _ := io.ReadAll(r.Body)
			var q struct {
				Variables struct {
					Expr string `json:"expr"`
				} `json:"variables"`
			}
			if err := json.Unmarshal(body, &q); err == nil && strings.Contains(q.Variables.Expr, ":") {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"data":{"repository":{"object":null}}}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{"repository":{"object":{"blame":{"ranges":[
				{"startingLine":1,"endingLine":50,"commit":{"oid":"deadbeef"}}
			]}}}}}`))
		case "/repos/example-org/example-repo/commits/deadbeef/pulls":
			f.prCalls++
			if f.prStatus != 0 {
				w.WriteHeader(f.prStatus)
				_, _ = w.Write([]byte(`{}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if f.prsJSON != "" {
				_, _ = w.Write([]byte(f.prsJSON))
				return
			}
			head := ""
			if f.prHeadSHA != "" {
				head = `,"head":{"sha":"` + f.prHeadSHA + `"}`
			}
			// An unmerged PR listed first must be skipped: only a merged PR can
			// have introduced the commit to the blamed branch. base.ref matches
			// the "main" branch every default-fixture test passes to Resolve,
			// since a real merged PR always carries a base and branch
			// validation now applies even when only one merged PR comes back.
			_, _ = w.Write([]byte(`[
				{"number":41,"html_url":"https://github.com/example-org/example-repo/pull/41"},
				{"number":42,"html_url":"https://github.com/example-org/example-repo/pull/42","merged_at":"2026-01-02T03:04:05Z","base":{"ref":"main"}` + head + `}
			]`))
		case "/repos/example-org/example-repo/pulls/42/reviews":
			f.reviewCalls++
			if f.reviewsStatus != 0 {
				w.WriteHeader(f.reviewsStatus)
				_, _ = w.Write([]byte(`{}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			reviews := f.reviewsJSON
			if reviews == "" {
				reviews = `[{"state":"APPROVED","commit_id":"deadbeef","user":{"login":"example-human","type":"User"}}]`
			}
			_, _ = w.Write([]byte(reviews))
		default:
			if rest, ok := strings.CutPrefix(r.URL.Path, "/repos/example-org/example-repo/compare/deadbeef..."); ok {
				if f.compareStatus != 0 {
					w.WriteHeader(f.compareStatus)
					_, _ = w.Write([]byte(`{}`))
					return
				}
				status := f.compareStatusByHead[rest]
				if status == "" {
					status = "identical"
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"status":"` + status + `"}`))
				return
			}
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{}`))
		}
	}
}

func newTestResolver(t *testing.T, srv *httptest.Server) *Resolver {
	t.Helper()
	client := github.NewClient(srv.Client())
	baseURL, err := url.Parse(srv.URL + "/")
	if err != nil {
		t.Fatalf("parse base url: %v", err)
	}
	client.BaseURL = baseURL
	return NewResolver(client)
}

func TestResolveCachesBlameAndPRLookups(t *testing.T) {
	fake := &fakeGitHubServer{}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	resolver := newTestResolver(t, srv)
	ctx := context.Background()

	for _, line := range []int{2, 10, 25} {
		prov, err := resolver.Resolve(ctx, "example-org", "example-repo", "main", "main", "MAINTAINERS.md", line)
		if err != nil {
			t.Fatalf("Resolve line %d: %v", line, err)
		}
		if prov.ReviewState != ReviewStateApproved {
			t.Errorf("line %d: ReviewState = %q, want %q", line, prov.ReviewState, ReviewStateApproved)
		}
		if prov.PRNumber != 42 {
			t.Errorf("line %d: PRNumber = %d, want 42", line, prov.PRNumber)
		}
	}

	if fake.graphQLCalls != 1 {
		t.Errorf("graphQLCalls = %d, want 1 (one blame call should cover every line in the file)", fake.graphQLCalls)
	}
	if fake.prCalls != 1 {
		t.Errorf("prCalls = %d, want 1 (one PR lookup should cover every line sharing a commit)", fake.prCalls)
	}
	if fake.reviewCalls != 1 {
		t.Errorf("reviewCalls = %d, want 1", fake.reviewCalls)
	}
}

func TestResolveUnresolvableLineReportsUnknownNotNegative(t *testing.T) {
	fake := &fakeGitHubServer{}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	resolver := newTestResolver(t, srv)

	// Line 1000 falls outside every blame range the fake server returns, so
	// the commit can't be resolved - this must report "unknown", never
	// "direct-push" or "unreviewed", since no review was actually observed.
	prov, err := resolver.Resolve(context.Background(), "example-org", "example-repo", "main", "main", "MAINTAINERS.md", 1000)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if prov.ReviewState != ReviewStateUnknown {
		t.Errorf("ReviewState = %q, want %q", prov.ReviewState, ReviewStateUnknown)
	}
}

func TestResolveNilClientErrors(t *testing.T) {
	var resolver *Resolver
	if _, err := resolver.Resolve(context.Background(), "o", "r", "main", "main", "f.md", 1); err == nil {
		t.Fatal("expected error for nil resolver, got nil")
	}

	empty := NewResolver(nil)
	if _, err := empty.Resolve(context.Background(), "o", "r", "main", "main", "f.md", 1); err == nil {
		t.Fatal("expected error for resolver with nil client, got nil")
	}
}

func TestResolveIgnoresBotApprovals(t *testing.T) {
	fake := &fakeGitHubServer{
		// A bot approval must not satisfy the human-gatekeeper tier; with
		// only bot approvals and human comments the PR counts as unreviewed.
		reviewsJSON: `[
			{"state":"APPROVED","user":{"login":"approve-bot[bot]","type":"Bot"}},
			{"state":"COMMENTED","user":{"login":"fixture-human","type":"User"}}
		]`,
	}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	resolver := newTestResolver(t, srv)
	prov, err := resolver.Resolve(context.Background(), "example-org", "example-repo", "main", "main", "MAINTAINERS.md", 2)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if prov.ReviewState != ReviewStateUnreviewed {
		t.Errorf("ReviewState = %q, want %q (bot approvals must not count as human review)", prov.ReviewState, ReviewStateUnreviewed)
	}
}

func TestResolveCountsHumanApprovalAlongsideBotApproval(t *testing.T) {
	fake := &fakeGitHubServer{
		reviewsJSON: `[
			{"state":"APPROVED","commit_id":"deadbeef","user":{"login":"approve-bot[bot]","type":"Bot"}},
			{"state":"APPROVED","commit_id":"deadbeef","user":{"login":"fixture-human","type":"User"}}
		]`,
	}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	resolver := newTestResolver(t, srv)
	prov, err := resolver.Resolve(context.Background(), "example-org", "example-repo", "main", "main", "MAINTAINERS.md", 2)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if prov.ReviewState != ReviewStateApproved {
		t.Errorf("ReviewState = %q, want %q", prov.ReviewState, ReviewStateApproved)
	}
}

func TestResolveIgnoresApprovalWithMissingUser(t *testing.T) {
	fake := &fakeGitHubServer{
		// A deleted account leaves a review with a null user; an
		// unverifiable reviewer identity must not raise provenance to
		// approved.
		reviewsJSON: `[
			{"state":"APPROVED","user":null},
			{"state":"COMMENTED","user":{"login":"fixture-human","type":"User"}}
		]`,
	}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	resolver := newTestResolver(t, srv)
	prov, err := resolver.Resolve(context.Background(), "example-org", "example-repo", "main", "main", "MAINTAINERS.md", 2)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if prov.ReviewState != ReviewStateUnreviewed {
		t.Errorf("ReviewState = %q, want %q (an approval with no user is unverifiable and must not count as human review)", prov.ReviewState, ReviewStateUnreviewed)
	}
}

func TestResolveCachesFailedBlameLookups(t *testing.T) {
	fake := &fakeGitHubServer{graphQLStatus: http.StatusForbidden}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	resolver := newTestResolver(t, srv)
	ctx := context.Background()

	for _, line := range []int{2, 10, 25} {
		if _, err := resolver.Resolve(ctx, "example-org", "example-repo", "main", "main", "MAINTAINERS.md", line); err == nil {
			t.Fatalf("Resolve line %d: expected an error from the failed blame lookup", line)
		}
	}

	if fake.graphQLCalls != 1 {
		t.Errorf("graphQLCalls = %d, want 1 (a failed blame lookup must be cached, not retried per maintainer in the same file)", fake.graphQLCalls)
	}
}

func TestResolveIgnoresApprovalPredatingBlamedCommit(t *testing.T) {
	fake := &fakeGitHubServer{
		// The human approved an earlier head; the blamed commit was pushed
		// afterwards, so the compare of blamed...reviewed-head reports
		// "behind" and the approval must not vouch for the line.
		reviewsJSON: `[
			{"state":"APPROVED","commit_id":"0ldhead0","user":{"login":"fixture-human","type":"User"}}
		]`,
		compareStatusByHead: map[string]string{"0ldhead0": "behind"},
	}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	resolver := newTestResolver(t, srv)
	prov, err := resolver.Resolve(context.Background(), "example-org", "example-repo", "main", "main", "MAINTAINERS.md", 2)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if prov.ReviewState != ReviewStateUnreviewed {
		t.Errorf("ReviewState = %q, want %q (an approval that predates the blamed commit never saw the line)", prov.ReviewState, ReviewStateUnreviewed)
	}
}

func TestResolveCountsApprovalOnLaterHeadContainingBlamedCommit(t *testing.T) {
	fake := &fakeGitHubServer{
		reviewsJSON: `[
			{"state":"APPROVED","commit_id":"newhead0","user":{"login":"fixture-human","type":"User"}}
		]`,
		compareStatusByHead: map[string]string{"newhead0": "ahead"},
	}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	resolver := newTestResolver(t, srv)
	prov, err := resolver.Resolve(context.Background(), "example-org", "example-repo", "main", "main", "MAINTAINERS.md", 2)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if prov.ReviewState != ReviewStateApproved {
		t.Errorf("ReviewState = %q, want %q (a reviewed head containing the blamed commit vouches for the line)", prov.ReviewState, ReviewStateApproved)
	}
}

func TestResolveCountsFinalHeadApprovalOnSquashMergedPR(t *testing.T) {
	// A squash merge rewrites the PR's commits: the blamed SHA (deadbeef, the
	// squash commit on the target branch) is never an ancestor of the
	// PR-branch head the reviewer approved, so the compare reports
	// "diverged". An approval of the PR's final head still covered
	// everything the merged PR introduced and must count.
	fake := &fakeGitHubServer{
		prHeadSHA: "prheadff",
		reviewsJSON: `[
			{"state":"APPROVED","commit_id":"prheadff","user":{"login":"fixture-human","type":"User"}}
		]`,
		compareStatusByHead: map[string]string{"prheadff": "diverged"},
	}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	resolver := newTestResolver(t, srv)
	prov, err := resolver.Resolve(context.Background(), "example-org", "example-repo", "main", "main", "MAINTAINERS.md", 2)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if prov.ReviewState != ReviewStateApproved {
		t.Errorf("ReviewState = %q, want %q (an approval of the PR's final head vouches for a squash-merged line)", prov.ReviewState, ReviewStateApproved)
	}
}

func TestResolveCachesFailedPRLookups(t *testing.T) {
	fake := &fakeGitHubServer{prStatus: http.StatusForbidden}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	resolver := newTestResolver(t, srv)
	ctx := context.Background()

	// Three lines share the blamed commit; a failing PR listing must be
	// attempted once, not once per line, or an outage is amplified.
	for _, line := range []int{2, 10, 25} {
		if _, err := resolver.Resolve(ctx, "example-org", "example-repo", "main", "main", "MAINTAINERS.md", line); err == nil {
			t.Fatalf("Resolve line %d: expected error from failed PR listing, got nil", line)
		}
	}
	if fake.prCalls != 1 {
		t.Errorf("prCalls = %d, want 1 (a failed PR lookup must be cached like a successful one)", fake.prCalls)
	}
}

func TestResolveDisambiguatesMultipleMergedPRsByBaseRef(t *testing.T) {
	// The blamed commit is associated with two merged PRs: a release-branch
	// PR that reused it, and #42 merged into the blamed ref. Only #42's
	// approval can vouch for the line.
	fake := &fakeGitHubServer{
		prsJSON: `[
			{"number":77,"html_url":"https://github.com/example-org/example-repo/pull/77","merged_at":"2026-01-05T00:00:00Z","base":{"ref":"release-1.0"}},
			{"number":42,"html_url":"https://github.com/example-org/example-repo/pull/42","merged_at":"2026-01-02T03:04:05Z","base":{"ref":"main"}}
		]`,
	}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	resolver := newTestResolver(t, srv)
	prov, err := resolver.Resolve(context.Background(), "example-org", "example-repo", "deadbeef", "main", "MAINTAINERS.md", 2)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if prov.PRNumber != 42 {
		t.Errorf("PRNumber = %d, want 42 (the PR merged into the blamed branch)", prov.PRNumber)
	}
	if prov.ReviewState != ReviewStateApproved {
		t.Errorf("ReviewState = %q, want %q", prov.ReviewState, ReviewStateApproved)
	}
}

func TestResolveDisambiguatesWithPinnedSHA(t *testing.T) {
	// Production scenario: ref is a pinned snapshot SHA, branch is the target.
	// When a commit is in multiple merged PRs, only the one merged into
	// the specified branch can vouch for the line.
	fake := &fakeGitHubServer{
		prsJSON: `[
			{"number":77,"html_url":"https://github.com/example-org/example-repo/pull/77","merged_at":"2026-01-05T00:00:00Z","base":{"ref":"develop"}},
			{"number":42,"html_url":"https://github.com/example-org/example-repo/pull/42","merged_at":"2026-01-02T03:04:05Z","base":{"ref":"main"}}
		]`,
	}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	resolver := newTestResolver(t, srv)
	// ref is a pinned SHA (abc123), branch is where the file actually lives
	prov, err := resolver.Resolve(context.Background(), "example-org", "example-repo", "abc123", "main", "MAINTAINERS.md", 2)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if prov.PRNumber != 42 {
		t.Errorf("PRNumber = %d, want 42 (only the PR merged into 'main' branch vouches)", prov.PRNumber)
	}
	if prov.ReviewState != ReviewStateApproved {
		t.Errorf("ReviewState = %q, want %q", prov.ReviewState, ReviewStateApproved)
	}
}

func TestResolveReportsUnknownWhenBranchNotProvided(t *testing.T) {
	// When multiple PRs contain a commit and no branch is given to
	// disambiguate, the association is ambiguous and unresolvable.
	fake := &fakeGitHubServer{
		prsJSON: `[
			{"number":77,"html_url":"https://github.com/example-org/example-repo/pull/77","merged_at":"2026-01-05T00:00:00Z","base":{"ref":"develop"}},
			{"number":42,"html_url":"https://github.com/example-org/example-repo/pull/42","merged_at":"2026-01-02T03:04:05Z","base":{"ref":"main"}}
		]`,
	}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	resolver := newTestResolver(t, srv)
	// No branch provided (empty string) for disambiguation
	prov, err := resolver.Resolve(context.Background(), "example-org", "example-repo", "abc123", "", "MAINTAINERS.md", 2)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if prov.ReviewState != ReviewStateUnknown {
		t.Errorf("ReviewState = %q, want %q (multi-PR ambiguity without branch must report unknown)", prov.ReviewState, ReviewStateUnknown)
	}
	if prov.PRNumber != 0 {
		t.Errorf("PRNumber = %d, want 0", prov.PRNumber)
	}
}

func TestResolveReportsUnknownWhenMergedPRsAreAmbiguous(t *testing.T) {
	// Two merged PRs into the same base can't be told apart, so no approval
	// may be borrowed from either - the association must report unknown.
	fake := &fakeGitHubServer{
		prsJSON: `[
			{"number":77,"html_url":"https://github.com/example-org/example-repo/pull/77","merged_at":"2026-01-05T00:00:00Z","base":{"ref":"main"}},
			{"number":42,"html_url":"https://github.com/example-org/example-repo/pull/42","merged_at":"2026-01-02T03:04:05Z","base":{"ref":"main"}}
		]`,
	}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	resolver := newTestResolver(t, srv)
	prov, err := resolver.Resolve(context.Background(), "example-org", "example-repo", "main", "main", "MAINTAINERS.md", 2)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if prov.ReviewState != ReviewStateUnknown {
		t.Errorf("ReviewState = %q, want %q", prov.ReviewState, ReviewStateUnknown)
	}
	if prov.PRNumber != 0 {
		t.Errorf("PRNumber = %d, want 0 (no PR may be claimed for an ambiguous association)", prov.PRNumber)
	}
}

func TestResolveReportsUnknownWhenSingleMergedPRBaseMismatchesBranch(t *testing.T) {
	// The commit reached the blamed branch independently of the one merged
	// PR GitHub returns (e.g. merged into "develop" and later cherry-picked
	// or fast-forwarded onto "main") - that PR's approval must not be
	// credited to a line blamed on "main" just because it's the only result.
	fake := &fakeGitHubServer{
		prsJSON: `[
			{"number":77,"html_url":"https://github.com/example-org/example-repo/pull/77","merged_at":"2026-01-05T00:00:00Z","base":{"ref":"develop"}}
		]`,
	}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	resolver := newTestResolver(t, srv)
	prov, err := resolver.Resolve(context.Background(), "example-org", "example-repo", "deadbeef", "main", "MAINTAINERS.md", 2)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if prov.ReviewState != ReviewStateUnknown {
		t.Errorf("ReviewState = %q, want %q", prov.ReviewState, ReviewStateUnknown)
	}
	if prov.PRNumber != 0 {
		t.Errorf("PRNumber = %d, want 0 (the only merged PR's base doesn't match the blamed branch)", prov.PRNumber)
	}
}

func TestResolveErrorsWhenReviewListingFails(t *testing.T) {
	fake := &fakeGitHubServer{reviewsStatus: http.StatusBadGateway}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	resolver := newTestResolver(t, srv)

	// A transient failure listing reviews must surface as an error, never as
	// a nil-error "unknown": observation writers only preserve previously
	// recorded evidence when Resolve errors, so a silent unknown would
	// overwrite an approved row with a downgrade.
	if _, err := resolver.Resolve(context.Background(), "example-org", "example-repo", "main", "main", "MAINTAINERS.md", 2); err == nil {
		t.Fatal("expected error from failed review listing, got nil")
	}
}

func TestResolveErrorsWhenCompareFails(t *testing.T) {
	// The approving review's head differs from both the blamed commit and the
	// PR head, forcing the compare call - which fails.
	fake := &fakeGitHubServer{
		reviewsJSON: `[
			{"state":"APPROVED","commit_id":"feedface","user":{"login":"example-human","type":"User"}}
		]`,
		compareStatus: http.StatusBadGateway,
	}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	resolver := newTestResolver(t, srv)

	// Same policy as a failed review listing: propagate so callers keep the
	// previously recorded observation instead of persisting a downgrade.
	if _, err := resolver.Resolve(context.Background(), "example-org", "example-repo", "main", "main", "MAINTAINERS.md", 2); err == nil {
		t.Fatal("expected error from failed compare, got nil")
	}
}

func TestJSONEscapePreservesTrailingQuote(t *testing.T) {
	got := jsonEscape(`dir/weird".md`)
	want := `dir/weird\".md`
	if got != want {
		t.Errorf("jsonEscape = %q, want %q (a path ending in a quote must keep its escaped quote)", got, want)
	}
}
