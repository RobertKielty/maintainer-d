// Package provenance resolves the human-review evidence behind a line in a
// GitHub-hosted file: which commit last touched it (via blame), which pull
// request that commit belongs to, and whether that PR carries an approving
// review. This is the evidentiary signal behind a maintainer-file match -
// presence in a file is weak on its own; a PR-reviewed addition is strong.
package provenance

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/google/go-github/v55/github"
)

// Review states. "unknown" must never be treated as negative evidence - it
// means the source could not be resolved (e.g. a gist), not that no review
// happened.
const (
	ReviewStateApproved   = "approved"
	ReviewStateUnreviewed = "unreviewed"
	ReviewStateDirectPush = "direct-push"
	ReviewStateUnknown    = "unknown"
)

// LineProvenance is the resolved evidence for one line in one file.
type LineProvenance struct {
	CommitSHA   string
	PRNumber    int
	PRURL       string
	ReviewState string
}

// Resolver resolves line provenance against the GitHub REST and GraphQL
// APIs, caching aggressively: one blame call covers every line in a file,
// and one PR/review lookup covers every line a commit touched.
type Resolver struct {
	Client *github.Client

	mu        sync.Mutex
	blameByFK map[fileKey]blameResult
	prByCK    map[commitKey]prResult
}

// blameResult caches failures alongside successes: a rate-limited or broken
// blame lookup would otherwise be retried once per maintainer in the same
// file, amplifying exactly the quota incident this path is most exposed to.
// One failure costs at most one request per file per resolver lifetime.
type blameResult struct {
	ranges []blameRange
	err    error
}

type fileKey struct {
	owner, repo, ref, path string
}

// commitKey scopes the PR cache to a repository and the target branch: the
// same commit SHA exists in an upstream repo and its forks (different PRs),
// and when several merged PRs share the commit, which one vouches for the
// line depends on which branch the blamed file lives on.
type commitKey struct {
	owner, repo, branch, sha string
}

type blameRange struct {
	startingLine, endingLine int
	commitSHA                string
}

type prInfo struct {
	number      int
	url         string
	reviewState string
}

// prResult caches failures alongside successes for the same reason
// blameResult does: a rate-limited PR listing would otherwise be retried
// once per maintainer line sharing the blamed commit, amplifying the outage
// despite the one-lookup-per-commit invariant.
type prResult struct {
	info prInfo
	err  error
}

// NewResolver returns a Resolver backed by client. client must not be nil.
func NewResolver(client *github.Client) *Resolver {
	return &Resolver{
		Client:    client,
		blameByFK: make(map[fileKey]blameResult),
		prByCK:    make(map[commitKey]prResult),
	}
}

// Resolve returns the provenance for the given 1-based line of path, at ref
// (a branch or commit-ish), in owner/repo. branch names the branch the file
// lives on and is only consulted to disambiguate a commit associated with
// several merged PRs: callers blame a pinned snapshot SHA, which can never
// equal a PR's base branch, so the two must be supplied separately. An empty
// branch leaves multi-PR associations unresolvable (reported as unknown).
// Errors are returned only for transport/API failures; an
// unresolvable-but-reachable source (e.g. no PR associated with the commit)
// is reported as ReviewStateDirectPush, not an error.
func (r *Resolver) Resolve(ctx context.Context, owner, repo, ref, branch, path string, line int) (LineProvenance, error) {
	if r == nil || r.Client == nil {
		return LineProvenance{}, fmt.Errorf("provenance resolver is not configured")
	}
	if line <= 0 {
		return LineProvenance{ReviewState: ReviewStateUnknown}, nil
	}

	ranges, err := r.blame(ctx, owner, repo, ref, path)
	if err != nil {
		return LineProvenance{}, err
	}
	sha := commitForLine(ranges, line)
	if sha == "" {
		return LineProvenance{ReviewState: ReviewStateUnknown}, nil
	}

	info, err := r.prForCommit(ctx, owner, repo, branch, sha)
	if err != nil {
		return LineProvenance{}, err
	}
	return LineProvenance{
		CommitSHA:   sha,
		PRNumber:    info.number,
		PRURL:       info.url,
		ReviewState: info.reviewState,
	}, nil
}

func commitForLine(ranges []blameRange, line int) string {
	for _, rng := range ranges {
		if line >= rng.startingLine && line <= rng.endingLine {
			return rng.commitSHA
		}
	}
	return ""
}

func (r *Resolver) blame(ctx context.Context, owner, repo, ref, path string) ([]blameRange, error) {
	key := fileKey{owner: owner, repo: repo, ref: ref, path: path}

	r.mu.Lock()
	if cached, ok := r.blameByFK[key]; ok {
		r.mu.Unlock()
		return cached.ranges, cached.err
	}
	r.mu.Unlock()

	ranges, err := r.fetchBlame(ctx, owner, repo, ref, path)

	r.mu.Lock()
	r.blameByFK[key] = blameResult{ranges: ranges, err: err}
	r.mu.Unlock()
	return ranges, err
}

func (r *Resolver) prForCommit(ctx context.Context, owner, repo, branch, sha string) (prInfo, error) {
	key := commitKey{owner: owner, repo: repo, branch: branch, sha: sha}

	r.mu.Lock()
	if cached, ok := r.prByCK[key]; ok {
		r.mu.Unlock()
		return cached.info, cached.err
	}
	r.mu.Unlock()

	info, err := r.fetchPRForCommit(ctx, owner, repo, branch, sha)

	r.mu.Lock()
	r.prByCK[key] = prResult{info: info, err: err}
	r.mu.Unlock()
	return info, err
}

func (r *Resolver) fetchPRForCommit(ctx context.Context, owner, repo, branch, sha string) (prInfo, error) {
	// The PR merged into the blamed branch can sit on any page; stopping at
	// the first page would misreport it as unassociated (direct-push) or
	// ambiguous (unknown) whenever a commit has enough associated PRs
	// (backports, cherry-picks onto release branches) to spill past one page.
	var prs []*github.PullRequest
	prOpts := &github.ListOptions{PerPage: 100}
	for {
		page, resp, err := r.Client.PullRequests.ListPullRequestsWithCommit(ctx, owner, repo, sha, prOpts)
		if err != nil {
			return prInfo{}, fmt.Errorf("list pull requests for commit: %w", err)
		}
		prs = append(prs, page...)
		if resp.NextPage == 0 {
			break
		}
		prOpts.Page = resp.NextPage
	}
	// Only a merged PR can have introduced the commit to the blamed branch;
	// a commit pushed directly can still be *associated* with an open or
	// closed-unmerged PR, whose review state must not be inherited.
	var merged []*github.PullRequest
	for _, candidate := range prs {
		if candidate.MergedAt != nil {
			merged = append(merged, candidate)
		}
	}
	if len(merged) == 0 {
		return prInfo{reviewState: ReviewStateDirectPush}, nil
	}
	pr := merged[0]
	// GitHub associates the same commit with every merged PR that contains
	// it - a release-branch PR reusing a commit from the default branch, for
	// example, or the same commit independently reaching both branches.
	// Borrowing an approval from a PR merged into a different branch would
	// inflate the line's evidence, so whenever the blamed branch is known,
	// only a PR whose base matches it can vouch for the line - even when
	// exactly one merged PR came back, since that one PR can still be the
	// wrong one. The branch travels separately from the blame ref because
	// blame runs against a pinned snapshot SHA, which never equals a base
	// branch name. When the base can't single one out (no branch known and
	// several PRs merged, or a known branch matching none/more than one),
	// the association is ambiguous and must be reported as unknown rather
	// than guessed.
	branch = strings.TrimSpace(branch)
	switch {
	case branch != "":
		var matching []*github.PullRequest
		for _, candidate := range merged {
			if strings.EqualFold(strings.TrimSpace(candidate.GetBase().GetRef()), branch) {
				matching = append(matching, candidate)
			}
		}
		if len(matching) != 1 {
			return prInfo{reviewState: ReviewStateUnknown}, nil
		}
		pr = matching[0]
	case len(merged) > 1:
		return prInfo{reviewState: ReviewStateUnknown}, nil
	}
	info := prInfo{
		number: pr.GetNumber(),
		url:    pr.GetHTMLURL(),
	}

	// An approving review can sit on any page; stopping at the first page
	// would persist a reviewed PR as "unreviewed" and lower confidence.
	opts := &github.ListOptions{PerPage: 100}
	for {
		reviews, resp, err := r.Client.PullRequests.ListReviews(ctx, owner, repo, pr.GetNumber(), opts)
		if err != nil {
			// A transient API failure must propagate: writers only preserve a
			// previously recorded observation when Resolve errors, so returning
			// a nil-error "unknown" here would overwrite an already-captured
			// approved state with a downgrade and cache it for the whole run.
			return prInfo{}, fmt.Errorf("list reviews for PR %d: %w", pr.GetNumber(), err)
		}
		for _, review := range reviews {
			// Only a verifiable human approval counts: the review state feeds
			// the documented human-gatekeeper confidence tier, and GitHub
			// Apps / bot accounts (CI approvers, merge bots) would otherwise
			// raise an observation to that tier without any human having
			// looked. A review with no user at all (deleted account) is
			// unverifiable and must not be counted as human either.
			if review.GetUser() == nil || !strings.EqualFold(review.GetState(), "APPROVED") || strings.EqualFold(review.GetUser().GetType(), "Bot") {
				continue
			}
			// The approval only proves review of the blamed line if the head
			// the reviewer approved contains the blamed commit: an approval
			// can predate a later push that introduced the line, and without
			// stale-review dismissal that stale approval still merges the PR.
			reviewHead := strings.TrimSpace(review.GetCommitID())
			if reviewHead == "" {
				continue
			}
			if reviewHead == sha {
				info.reviewState = ReviewStateApproved
				return info, nil
			}
			// A squash or rebase merge rewrites the PR's commits into new
			// SHAs on the target branch, so the blamed commit is never an
			// ancestor of the PR-branch head the reviewer approved -
			// CompareCommits reports "diverged" and a genuinely approved PR
			// would be persisted as unreviewed. An approval of the PR's
			// final head covered everything this merged PR introduced,
			// including the blamed line, whatever merge strategy rewrote it.
			if reviewHead == strings.TrimSpace(pr.GetHead().GetSHA()) {
				info.reviewState = ReviewStateApproved
				return info, nil
			}
			cmp, _, err := r.Client.Repositories.CompareCommits(ctx, owner, repo, sha, reviewHead, nil)
			if err != nil {
				// Same policy as a failed review listing: propagate so the
				// caller keeps the previously recorded observation.
				return prInfo{}, fmt.Errorf("compare %s...%s: %w", sha, reviewHead, err)
			}
			// "ahead" or "identical" means the reviewed head contains the
			// blamed commit; "behind"/"diverged" means the approval never saw
			// the line and must not count.
			if status := cmp.GetStatus(); status == "ahead" || status == "identical" {
				info.reviewState = ReviewStateApproved
				return info, nil
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	info.reviewState = ReviewStateUnreviewed
	return info, nil
}

// graphQLBlameQuery fetches the blame ranges for one file in one request.
// go-github v55 has no blame support (REST doesn't expose it), so this
// issues a single GraphQL call reusing the client's authenticated
// http.Client rather than adding a GraphQL client dependency for one query.
const graphQLBlameQuery = `query($owner:String!,$repo:String!,$expr:String!) {
  repository(owner:$owner, name:$repo) {
    object(expression:$expr) {
      ... on Commit {
        blame(path: $path) {
          ranges {
            startingLine
            endingLine
            commit { oid }
          }
        }
      }
    }
  }
}`

func (r *Resolver) fetchBlame(ctx context.Context, owner, repo, ref, path string) ([]blameRange, error) {
	body, err := json.Marshal(map[string]any{
		"query": strings.Replace(graphQLBlameQuery, "$path", `"`+jsonEscape(path)+`"`, 1),
		"variables": map[string]string{
			"owner": owner,
			"repo":  repo,
			// The expression must resolve to a Commit for the "... on Commit"
			// fragment to match; "ref:path" resolves to a Blob, which GitHub
			// returns as null here. The path goes to blame(path:) only.
			"expr": ref,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("marshal blame query: %w", err)
	}

	endpoint := graphQLEndpoint(r.Client)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build blame request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := r.Client.Client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("blame request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("read blame response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("blame request returned status %d", resp.StatusCode)
	}

	var parsed struct {
		Data struct {
			Repository struct {
				Object struct {
					Blame struct {
						Ranges []struct {
							StartingLine int `json:"startingLine"`
							EndingLine   int `json:"endingLine"`
							Commit       struct {
								OID string `json:"oid"`
							} `json:"commit"`
						} `json:"ranges"`
					} `json:"blame"`
				} `json:"object"`
			} `json:"repository"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("decode blame response: %w", err)
	}
	if len(parsed.Errors) > 0 {
		return nil, fmt.Errorf("blame query error: %s", parsed.Errors[0].Message)
	}

	ranges := make([]blameRange, 0, len(parsed.Data.Repository.Object.Blame.Ranges))
	for _, rng := range parsed.Data.Repository.Object.Blame.Ranges {
		ranges = append(ranges, blameRange{
			startingLine: rng.StartingLine,
			endingLine:   rng.EndingLine,
			commitSHA:    rng.Commit.OID,
		})
	}
	return ranges, nil
}

func graphQLEndpoint(client *github.Client) string {
	base := client.BaseURL
	if base == nil {
		return "https://api.github.com/graphql"
	}
	// GitHub Enterprise REST clients are rooted at .../api/v3/; GraphQL lives
	// at .../api/graphql on the same host.
	if strings.Contains(base.Host, "api.github.com") {
		return "https://api.github.com/graphql"
	}
	trimmed := strings.TrimSuffix(strings.TrimSuffix(base.String(), "/"), "/api/v3")
	return trimmed + "/api/graphql"
}

func jsonEscape(s string) string {
	escaped, err := json.Marshal(s)
	if err != nil {
		return s
	}
	// Slice off exactly the outer JSON delimiters: strings.Trim would also
	// eat an escaped quote at the end of a path ending in `"`, producing a
	// malformed GraphQL string.
	return string(escaped[1 : len(escaped)-1])
}

// ParseGitHubBlobURL extracts owner/repo/ref/path from a github.com blob
// URL such as https://github.com/cncf/foundation/blob/main/project-maintainers.csv.
// It returns ok=false for anything it cannot resolve (gists, raw hosts,
// non-blob paths) - those sources must record ReviewStateUnknown, never be
// treated as unreviewed.
func ParseGitHubBlobURL(rawURL string) (owner, repo, ref, path string, ok bool) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || !strings.EqualFold(parsed.Host, "github.com") {
		return "", "", "", "", false
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) < 5 || parts[2] != "blob" {
		return "", "", "", "", false
	}
	return parts[0], parts[1], parts[3], strings.Join(parts[4:], "/"), true
}
