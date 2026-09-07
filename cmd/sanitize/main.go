package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"maintainerd/db"
	"maintainerd/dotproject"
	"maintainerd/model"
	"maintainerd/provenance"
	"maintainerd/refparse"

	"github.com/google/go-github/v55/github"
	"golang.org/x/oauth2"
	"gorm.io/gorm"
)

// defaultIntervalSeconds mirrors the CronJob schedule in
// deploy/manifests/cronjob-sanitize.yaml ("*/5 * * * *"). Override with
// MD_SANITIZE_INTERVAL_SECONDS if that schedule ever changes.
const defaultIntervalSeconds = 300

func main() {
	ctx := context.Background()

	dbDSN := envOr("MD_DB_DSN", "")
	if dbDSN == "" {
		log.Fatal("MD_DB_DSN is required")
	}

	dbConn, err := db.OpenGorm("postgres", dbDSN, &gorm.Config{})
	if err != nil {
		log.Fatalf("failed to open DB: %v", err)
	}
	store := db.NewSQLStore(dbConn)

	var resolver *provenance.Resolver
	if githubToken := strings.TrimSpace(os.Getenv("GITHUB_API_TOKEN")); githubToken != "" {
		ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: githubToken})
		tc := oauth2.NewClient(ctx, ts)
		resolver = provenance.NewResolver(github.NewClient(tc))
	}

	if err := sanitize(ctx, store, resolver); err != nil {
		log.Fatalf("sanitize failed: %v", err)
	}

	log.Println("sanitize completed successfully")
}

func sanitize(ctx context.Context, store *db.SQLStore, resolver *provenance.Resolver) error {
	// Ensure cache table exists.
	if err := store.DB().AutoMigrate(&model.MaintainerRefCache{}, &model.SanitizeRunStatus{}); err != nil {
		return fmt.Errorf("auto-migrate cache: %w", err)
	}

	projects, err := store.ListProjectsWithMaintainers()
	if err != nil {
		return fmt.Errorf("list projects: %w", err)
	}

	client := &http.Client{Timeout: 20 * time.Second}
	for _, p := range projects {
		// A project that has adopted .project is managed from its
		// maintainers.yaml by dot-project-sync; its legacy ref file is on its
		// way out and often stale. Reconciling status against it would archive
		// maintainers who only exist in the new file, so sanitize leaves those
		// projects alone entirely. "partial" alone is not enough: it also
		// covers repos where the maintainers file is missing or unparseable
		// (adoptionStatusFor), so a partial project only bypasses legacy
		// reconciliation when a maintainer roster was actually parsed -
		// auto-add runs off a parsed roster regardless of project.yaml.
		dotProjectAuthoritative := p.DotProjectAdoptionStatus == dotproject.AdoptionStatusAdopted ||
			(p.DotProjectAdoptionStatus == dotproject.AdoptionStatusPartial && p.DotProjectMaintainerCount != nil)
		if dotProjectAuthoritative {
			log.Printf("sanitize: skip project %d (%s), .project adoption status is %q with a parsed maintainer roster so the legacy ref is no longer authoritative", p.ID, p.Name, p.DotProjectAdoptionStatus)
			continue
		}
		ref := strings.TrimSpace(p.LegacyMaintainerRef)
		if ref == "" {
			continue
		}

		refOwner, refRepo, refBranch, refPath, refResolvable := provenance.ParseGitHubBlobURL(ref)

		cache, cacheErr := store.GetMaintainerRefCache(p.ID)
		if cacheErr != nil {
			log.Printf("sanitize: could not load cache for project %d (%s): %v", p.ID, p.Name, cacheErr)
		}
		// The conditional fetch runs against the branch URL so its ETag stays
		// comparable run to run. This job runs every 5 minutes; on a 304 the
		// file is byte-identical to what the last run already resolved, so
		// GetBranch pinning and per-line blame/PR/review resolution are
		// skipped entirely and stored provenance is reused instead of burning
		// GitHub API budget re-deriving the same answer.
		body, meta, notModified, err := fetchMaintainerRef(ctx, client, ref, cache)
		if err != nil {
			log.Printf("sanitize: skip project %d (%s), fetch error: %v", p.ID, p.Name, err)
			continue
		}
		if body == "" {
			log.Printf("sanitize: skip project %d (%s), empty body", p.ID, p.Name)
			continue
		}

		// Pin blame lookups, permalinks, AND the analyzed body to one snapshot
		// of the branch: blame line numbers describe the file as of the ref
		// they were resolved at, so working from the moving branch name would
		// let line numbers and review evidence drift apart if the branch
		// advances between requests. The pin is memoized and lazy so a 304
		// run whose observations all reuse stored provenance never spends the
		// GetBranch call - but a 304 row that still needs resolving (no
		// reusable observation yet, e.g. a pre-existing ref cache on rollout)
		// does pin, and the 304 guarantees the cached body matches the tip.
		// The conditional-fetch validators (ETag/Last-Modified) describe the
		// branch response; if the analysis body is later swapped for a pinned
		// snapshot, the cache must still store the branch body those
		// validators were issued for, or a future 304 would reuse bytes its
		// validator never described.
		branchBody := body
		refResolvedBranch := refBranch
		refSnapshot := refBranch
		refSnapshotPath := refPath
		pinAttempted := false
		pinSnapshot := func() (string, string, string) {
			if pinAttempted || !refResolvable || resolver == nil {
				return refSnapshot, refSnapshotPath, refResolvedBranch
			}
			pinAttempted = true
			segments := strings.Split(refPath, "/")
			branch, path := refBranch, refPath
			b, _, err := resolver.Client.Repositories.GetBranch(ctx, refOwner, refRepo, branch, false)
			// A blob URL cannot delimit a branch containing "/" from the file
			// path (".../blob/release/v2/MAINTAINERS.md" parses as branch
			// "release", path "v2/MAINTAINERS.md"), so when the first split is
			// not a real branch, consume path segments into progressively
			// longer branch candidates until one resolves.
			for i := 0; err != nil && i < len(segments)-1; i++ {
				branch = refBranch + "/" + strings.Join(segments[:i+1], "/")
				path = strings.Join(segments[i+1:], "/")
				b, _, err = resolver.Client.Repositories.GetBranch(ctx, refOwner, refRepo, branch, false)
			}
			if err != nil {
				log.Printf("sanitize: could not pin %s/%s@%s to a commit, falling back to the branch name: %v", refOwner, refRepo, refBranch, err)
				return refSnapshot, refSnapshotPath, refResolvedBranch
			}
			refResolvedBranch = branch
			refSnapshotPath = path
			if sha := strings.TrimSpace(b.GetCommit().GetSHA()); sha != "" {
				refSnapshot = sha
			}
			return refSnapshot, refSnapshotPath, refResolvedBranch
		}
		if !notModified && refResolvable && resolver != nil {
			if sha, pinnedPath, _ := pinSnapshot(); sha != refResolvedBranch {
				pinnedURL := fmt.Sprintf("https://github.com/%s/%s/blob/%s/%s", refOwner, refRepo, sha, pinnedPath)
				if pinnedBody, _, _, err := fetchMaintainerRef(ctx, client, pinnedURL, nil); err == nil && pinnedBody != "" {
					body = pinnedBody
				} else {
					// The SHA is only trustworthy alongside the body fetched
					// at that SHA; without it, blame and permalinks would
					// describe different content than the branch body being
					// analyzed, so fall back to the resolved branch name for both.
					log.Printf("sanitize: could not fetch pinned snapshot %s, analyzing the branch body at the branch name instead: %v", pinnedURL, err)
					refSnapshot = refResolvedBranch
				}
			}
		}

		projectStatuses, err := store.ListMaintainerProjectStatuses(p.ID)
		if err != nil {
			log.Printf("sanitize: could not load per-project statuses for project %d (%s): %v", p.ID, p.Name, err)
			continue
		}

		handleLocations := refparse.ExtractGitHubHandleLocations(body)
		observedAt := time.Now()

		for _, m := range p.Maintainers {
			handle := strings.TrimSpace(strings.ToLower(m.GitHubAccount))
			name := strings.TrimSpace(m.Name)
			if name == "" && (handle == "" || handle == "github_missing") {
				continue
			}

			present := false
			matchedByHandle := false
			if handle != "" && handle != "github_missing" && handlePresent(body, handle) {
				present = true
				matchedByHandle = true
			} else if namePresent(body, name) {
				present = true
			}

			if matchedByHandle {
				writeLegacyRefObservation(ctx, store, resolver, p, m, handle, handleLocations, refOwner, refRepo, pinSnapshot, refPath, refResolvable, notModified, observedAt)
			}

			currentStatus := projectStatuses[m.ID]
			if present {
				if currentStatus != model.ActiveMaintainer {
					if err := store.UpdateMaintainerProjectStatus(m.ID, p.ID, model.ActiveMaintainer); err != nil {
						log.Printf("sanitize: failed to mark active maintainer %d (%s) for project %d (%s): %v", m.ID, name, p.ID, p.Name, err)
					} else {
						log.Printf("sanitize: marked maintainer %d (%s) active for project %d (%s)", m.ID, name, p.ID, p.Name)
					}
				}
				continue
			}

			if currentStatus != model.ArchivedMaintainer {
				if err := store.UpdateMaintainerProjectStatus(m.ID, p.ID, model.ArchivedMaintainer); err != nil {
					log.Printf("sanitize: failed to archive maintainer %d (%s) for project %d (%s): %v", m.ID, handle, p.ID, p.Name, err)
				} else {
					log.Printf("sanitize: archived maintainer %d (%s) for project %d (%s)", m.ID, handle, p.ID, p.Name)
				}
			}
		}

		// Update cache metadata.
		now := time.Now()
		if cache == nil {
			cache = &model.MaintainerRefCache{ProjectID: p.ID}
		}
		if !notModified {
			cache.ETag = meta.ETag
			cache.LastModified = meta.LastModified
			cache.BodyHash = hashBody(branchBody)
			cache.Body = branchBody
		}
		cache.LastChecked = &now
		if err := store.UpsertMaintainerRefCache(cache); err != nil {
			log.Printf("sanitize: failed to upsert cache for project %d: %v", p.ID, err)
		}
	}

	intervalSeconds := defaultIntervalSeconds
	if v := envOr("MD_SANITIZE_INTERVAL_SECONDS", ""); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			intervalSeconds = n
		}
	}
	runStatus := &model.SanitizeRunStatus{LastRunAt: time.Now(), IntervalSeconds: intervalSeconds}
	if err := store.UpsertSanitizeRunStatus(runStatus); err != nil {
		log.Printf("sanitize: failed to record run status: %v", err)
	}
	return nil
}

type fetchMeta struct {
	ETag         string
	LastModified *time.Time
}

func fetchMaintainerRef(ctx context.Context, client *http.Client, urlStr string, cache *model.MaintainerRefCache) (string, fetchMeta, bool, error) {
	raw, err := normalizeMaintainerRefURL(urlStr)
	if err != nil {
		return "", fetchMeta{}, false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return "", fetchMeta{}, false, err
	}
	// Only send conditional headers when we have a cached body to fall back on;
	// otherwise a 304 would leave us with no content to re-validate maintainer status against.
	if cache != nil && cache.Body != "" {
		if cache.ETag != "" {
			req.Header.Set("If-None-Match", cache.ETag)
		}
		if cache.LastModified != nil {
			req.Header.Set("If-Modified-Since", cache.LastModified.UTC().Format(http.TimeFormat))
		}
	}
	// #nosec G704 -- URL is validated and allowlisted in normalizeMaintainerRefURL.
	resp, err := client.Do(req)
	if err != nil {
		return "", fetchMeta{}, false, err
	}
	defer resp.Body.Close()
	meta := fetchMeta{}
	if etag := resp.Header.Get("ETag"); etag != "" {
		meta.ETag = etag
	}
	if lm := resp.Header.Get("Last-Modified"); lm != "" {
		if t, err := time.Parse(http.TimeFormat, lm); err == nil {
			meta.LastModified = &t
		}
	}

	if resp.StatusCode == http.StatusNotModified {
		return cache.Body, fetchMeta{ETag: cache.ETag, LastModified: cache.LastModified}, true, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", meta, false, fmt.Errorf("status %d", resp.StatusCode)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", meta, false, err
	}
	return string(b), meta, false, nil
}

func normalizeMaintainerRefURL(u string) (string, error) {
	raw := toRawGitHubURL(u)
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if !strings.EqualFold(parsed.Scheme, "https") {
		return "", fmt.Errorf("unsupported scheme")
	}
	if !strings.EqualFold(parsed.Host, "raw.githubusercontent.com") {
		return "", fmt.Errorf("unsupported host %q", parsed.Host)
	}
	return parsed.String(), nil
}

func toRawGitHubURL(u string) string {
	if strings.Contains(u, "github.com") && strings.Contains(u, "/blob/") {
		parts := strings.Split(u, "github.com/")
		if len(parts) == 2 {
			rest := parts[1]
			rest = strings.Replace(rest, "/blob/", "/", 1)
			return "https://raw.githubusercontent.com/" + rest
		}
	}
	return u
}

// Matches GitHub handles with or without a leading @ (and github.com/ links).
var handleRE = regexp.MustCompile(`(?i)(?:github\.com/|@)?([a-z0-9](?:[a-z0-9-]{0,38}))`)

func handlePresent(body, handle string) bool {
	lower := strings.ToLower(body)
	handle = strings.ToLower(handle)
	matches := handleRE.FindAllStringSubmatch(lower, -1)
	for _, m := range matches {
		if len(m) > 1 && strings.EqualFold(m[1], handle) {
			return true
		}
	}
	return false
}

var spaceRE = regexp.MustCompile(`\s+`)

func namePresent(body, name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	escaped := regexp.QuoteMeta(name)
	// allow flexible whitespace between name parts
	escaped = spaceRE.ReplaceAllString(escaped, `\s+`)
	re, err := regexp.Compile(`(?i)(^|[^a-z0-9_])` + escaped + `([^a-z0-9_]|$)`)
	if err != nil {
		return false
	}
	return re.FindStringIndex(body) != nil
}

// writeLegacyRefObservation records the evidence behind a legacy-ref handle
// match: the line it was found on, and - when a resolver is configured and
// the ref is a github.com blob URL - the commit, PR, and review state that
// introduced it. An unresolvable ref (gist-hosted, no resolver configured)
// records ReviewStateUnknown rather than being treated as unreviewed.
func writeLegacyRefObservation(ctx context.Context, store *db.SQLStore, resolver *provenance.Resolver, p model.Project, m model.Maintainer, handle string, handleLocations map[string][]int, owner, repo string, pinRef func() (string, string, string), path string, refResolvable bool, reuseProvenance bool, observedAt time.Time) {
	// handlePresent matches more spellings than the location extractor
	// recognizes (e.g. a bare word in prose), so a match can have no known
	// line. Record the observation anyway - the evidence that the handle is
	// in the file stands - just without line-level provenance.
	line := 0
	if lines := handleLocations[handle]; len(lines) > 0 {
		line = lines[0]
	}

	var prov provenance.LineProvenance
	reviewState := provenance.ReviewStateUnknown
	lineURL := ""
	if reuseProvenance {
		// The file is unchanged since the last run (HTTP 304), so the
		// line-level evidence recorded then still describes it; only the
		// observation timestamp needs refreshing. Rows whose earlier
		// resolution came back unknown (rate-limited, transient failure)
		// fall through and are retried below.
		existing, err := store.GetLatestMaintainerIdentityObservationByRef(provenance.SourceLegacyRef, p.ID, "github:"+handle)
		if err != nil {
			log.Printf("sanitize: could not load stored legacy-ref observation for maintainer %d (%s) project %d: %v", m.ID, handle, p.ID, err)
		} else if existing != nil && existing.SourceReviewState != "" && existing.SourceReviewState != provenance.ReviewStateUnknown {
			prov = provenance.LineProvenance{
				CommitSHA: existing.SourceCommitSHA,
				PRNumber:  existing.SourcePRNumber,
				PRURL:     existing.SourcePRURL,
			}
			reviewState = existing.SourceReviewState
			line = existing.SourceLine
			lineURL = existing.SourceLineURL
			path = existing.SourceFilePath
		}
	}
	if reviewState == provenance.ReviewStateUnknown && refResolvable && line > 0 {
		// pinRef() yields the pinned snapshot commit when one can be resolved
		// (memoized; falls back to the branch name), so the permalink and the
		// blame evidence describe the same file state - including on the 304
		// path, where a row without reusable provenance still pins before
		// persisting a permanent SourceLineURL. It also yields the file path,
		// which pinning may have corrected when the branch name contains "/".
		// The third return value is the resolved branch name, needed to
		// disambiguate when a commit is in multiple merged PRs.
		ref, pinnedPath, branch := pinRef()
		path = pinnedPath
		lineURL = fmt.Sprintf("https://github.com/%s/%s/blob/%s/%s#L%d", owner, repo, ref, path, line)
		if resolver != nil {
			resolved, err := resolver.Resolve(ctx, owner, repo, ref, branch, path, line)
			if err != nil {
				log.Printf("sanitize: provenance resolve failed for %s/%s %s#L%d: %v", owner, repo, path, line, err)
			} else {
				prov = resolved
				reviewState = resolved.ReviewState
			}
		}
	}

	maintainerID := m.ID
	projectID := p.ID
	observation := &model.MaintainerIdentityObservation{
		MaintainerID:      &maintainerID,
		ProjectID:         &projectID,
		Source:            provenance.SourceLegacyRef,
		SourceRef:         "github:" + handle,
		GitHubUser:        handle,
		MatchStatus:       "matched",
		MatchReason:       "present in legacy maintainer reference file",
		Confidence:        provenance.Confidence(provenance.SourceLegacyRef, reviewState, true),
		SourceFilePath:    path,
		SourceLine:        line,
		SourceCommitSHA:   prov.CommitSHA,
		SourceLineURL:     lineURL,
		SourcePRNumber:    prov.PRNumber,
		SourcePRURL:       prov.PRURL,
		SourceReviewState: reviewState,
		ObservedAt:        observedAt,
	}
	if _, err := store.UpsertMaintainerIdentityObservation(observation); err != nil {
		log.Printf("sanitize: failed to write legacy-ref observation for maintainer %d (%s) project %d: %v", m.ID, handle, p.ID, err)
	}
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func hashBody(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}
