package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"maintainerd/db"
	"maintainerd/dotproject"
	"maintainerd/lfx"
	"maintainerd/model"
	"maintainerd/provenance"

	"github.com/google/go-github/v55/github"
	"go.uber.org/zap"
	"golang.org/x/oauth2"
	"gorm.io/gorm"
)

type postSyncMetrics struct {
	DBSizeBytes              int64
	DotProjectSyncStateBytes int64
	CachedFiles              int64
	MaintainersBodyBytes     int64
	AvgMaintainersBodyBytes  int64
	MaxMaintainersBodyBytes  int64
	ProjectsTotal            int64
	ReposFound               int64
	CachedBodies             int64
}

// syncConfig is marshaled verbatim into audit-log rows and the startup log
// line; it must never grow a token, password, or other credential field.
type syncConfig struct {
	CheckFoundationCSV bool   `json:"check_foundation_csv"`
	AutoAddMaintainers bool   `json:"auto_add_maintainers"`
	Actor              string `json:"actor"`
	FoundationOwner    string `json:"foundation_owner"`
	FoundationRepo     string `json:"foundation_repo"`
	FoundationRef      string `json:"foundation_ref"`
	FoundationPath     string `json:"foundation_path"`
	WriteGist          bool   `json:"write_gist"`
	GistID             string `json:"gist_id"`
	GistFilename       string `json:"gist_filename"`
	GistDescription    string `json:"gist_description"`
}

const lfxTokenHelp = "LFX Platform access failed; update LFX_AUTH_TOKEN with a fresh token from " + lfx.TokenRefreshURL

func main() {
	cfg := parseFlags()
	log.Printf("dot-project sync config=%s", configJSON(cfg))
	timeout := envDuration("DOT_PROJECT_SYNC_TIMEOUT", 50*time.Minute)
	timeoutSource := "default"
	if strings.TrimSpace(os.Getenv("DOT_PROJECT_SYNC_TIMEOUT")) != "" {
		timeoutSource = "DOT_PROJECT_SYNC_TIMEOUT"
	} else if strings.EqualFold(strings.TrimSpace(os.Getenv("LFX_ENRICH_ALL_MAINTAINERS")), "true") {
		timeout = time.Hour
		timeoutSource = "LFX_ENRICH_ALL_MAINTAINERS=true"
	}
	log.Printf("dot-project sync run timeout=%s source=%s", timeout, timeoutSource)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	dbDriver := envOr("MD_DB_DRIVER", "postgres")
	if dbDriver != "postgres" {
		log.Fatalf("dot-project-sync requires MD_DB_DRIVER=postgres, got %q", dbDriver)
	}
	dbDSN := envOr("MD_DB_DSN", "")
	if dbDSN == "" {
		log.Fatal("MD_DB_DSN is required when MD_DB_DRIVER=postgres")
	}

	githubToken := strings.TrimSpace(os.Getenv("GITHUB_API_TOKEN"))
	if githubToken == "" {
		log.Fatal("GITHUB_API_TOKEN is required")
	}

	dbConn, err := db.OpenGorm(dbDriver, dbDSN, &gorm.Config{})
	if err != nil {
		log.Fatalf("failed to open DB: %v", err)
	}

	store := db.NewSQLStore(dbConn)
	logger := zap.NewNop().Sugar()
	if err := store.LogAuditEvent(logger, buildStartAuditEvent(cfg)); err != nil {
		log.Printf("dot-project sync start audit log failed: %v", err)
	}
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: githubToken})
	tc := oauth2.NewClient(ctx, ts)
	client := github.NewClient(tc)
	foundationIndex, foundationErr := loadFoundationMaintainers(ctx, client, cfg)
	if foundationErr != nil {
		log.Printf("foundation csv load failed: %v", foundationErr)
	}

	lfxClient, err := buildRequiredLFXClient()
	if err != nil {
		log.Fatal(err)
	}

	syncer := &dotproject.Syncer{
		Store: store,
		Discoverer: &dotproject.Discoverer{
			Client: &dotproject.GitHubRepositoryClient{Client: client},
		},
		AutoAdder: buildAutoAdder(store, cfg, foundationIndex, lfxIdentityResolver{client: lfxClient}, client),
		Enricher:  buildLFXEnricher(store, lfxClient),
		MaintainersFileVisitor: func(project model.Project, file dotproject.FileDiscovery) {
			log.Printf("%s has a %s file", projectLabel(project), dotProjectFileURL(file))
		},
		Progress: func(progress dotproject.SyncProgress) {
			if progress.CurrentProject == "" {
				return // final call after the loop completes; nothing left to announce
			}
			if progress.ProjectsProcessed == 0 {
				log.Printf("dot-project sync starting: %d project(s) to process", progress.TotalProjects)
			}
			log.Printf("dot-project sync processing project %d of %d: %s", progress.ProjectsProcessed+1, progress.TotalProjects, progress.CurrentProject)
		},
	}

	summary, err := syncer.SyncAll(ctx)
	if err != nil {
		log.Fatalf("dot-project sync failed: %v", err)
	}
	// Post-run bookkeeping must not reuse the run's context: after a clean
	// stopped-early return it is already exhausted, and even a run that
	// finished just inside the deadline leaves only milliseconds - either
	// way the gist publish or metrics collection would convert a successful
	// sync into a failed run.
	postCtx, cancelPost := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancelPost()
	if cfg.WriteGist {
		gist, err := publishDotProjectGist(postCtx, client, cfg, summary.GistReportRows)
		if err != nil {
			log.Fatalf("dot-project gist publish failed: %v", err)
		}
		log.Printf("dot-project gist published url=%s id=%s rows=%d", gist.GetHTMLURL(), gist.GetID(), len(summary.GistReportRows))
	}
	for _, warning := range summary.WarningSummaries {
		log.Printf("dot-project sync warning: %s", warning)
	}
	for _, errorSummary := range summary.ErrorSummaries {
		log.Printf("dot-project sync project error: %s", errorSummary)
	}

	metrics, metricsErr := collectPostSyncMetrics(postCtx, store)
	if metricsErr != nil {
		log.Printf("dot-project sync post-sync metrics failed: %v", metricsErr)
	}

	if err := store.LogAuditEvent(logger, buildAuditEvent(summary, metrics, metricsErr, cfg)); err != nil {
		log.Printf("dot-project sync audit log failed: %v", err)
	}

	log.Printf(
		"dot-project sync complete loaded=%d total=%d skipped=%d skipped_archived=%d skipped_excluded=%d synced=%d errored=%d github_errors=%d rate_limit_errors=%d not_found=%d repo_only=%d partial=%d adopted=%d auto_add_candidates=%d auto_add_created=%d auto_add_linked=%d auto_add_would_create=%d auto_add_would_link=%d auto_add_skipped_foundation=%d auto_add_skipped_project=%d auto_add_skipped_invalid=%d auto_add_errored=%d lfx_attempted=%d lfx_matched=%d lfx_ambiguous=%d lfx_unmatched=%d lfx_errored=%d stopped_early=%t remaining=%d",
		summary.Loaded,
		summary.Total,
		summary.Skipped,
		summary.SkippedArchived,
		summary.SkippedExcluded,
		summary.Synced,
		summary.Errored,
		summary.GitHubErrorCount,
		summary.RateLimitErrorCount,
		summary.NotFound,
		summary.RepoOnly,
		summary.Partial,
		summary.Adopted,
		summary.AutoAdd.Candidates,
		summary.AutoAdd.CreatedMaintainers,
		summary.AutoAdd.LinkedMaintainers,
		summary.AutoAdd.WouldCreateMaintainers,
		summary.AutoAdd.WouldLinkMaintainers,
		summary.AutoAdd.SkippedFoundationMissing,
		summary.AutoAdd.SkippedProjectMismatch,
		summary.AutoAdd.SkippedInvalidMaintainers,
		summary.AutoAdd.Errored,
		summary.Enrichment.Attempted,
		summary.Enrichment.Matched,
		summary.Enrichment.Ambiguous,
		summary.Enrichment.Unmatched,
		summary.Enrichment.Errored,
		summary.StoppedEarly,
		summary.RemainingProjects,
	)
}

func parseFlags() syncConfig {
	cfg := syncConfig{}
	cfg.Actor = strings.TrimSpace(os.Getenv("DOT_PROJECT_SYNC_ACTOR"))
	flag.BoolVar(&cfg.CheckFoundationCSV, "check-foundation-csv", true, "require cncf/foundation project-maintainers.csv membership before auto-adding dot-project maintainers")
	flag.BoolVar(&cfg.AutoAddMaintainers, "auto-add-maintainers", false, "write eligible dot-project maintainers to maintainers and maintainer_projects")
	flag.StringVar(&cfg.FoundationOwner, "foundation-csv-owner", "cncf", "GitHub owner for foundation project-maintainers.csv")
	flag.StringVar(&cfg.FoundationRepo, "foundation-csv-repo", "foundation", "GitHub repository for foundation project-maintainers.csv")
	flag.StringVar(&cfg.FoundationRef, "foundation-csv-ref", "main", "Git ref for foundation project-maintainers.csv")
	flag.StringVar(&cfg.FoundationPath, "foundation-csv-path", "project-maintainers.csv", "Path to foundation project-maintainers.csv")
	flag.BoolVar(&cfg.WriteGist, "write-gist", false, "write a public gist containing the dot-project Markdown report")
	flag.StringVar(&cfg.GistID, "gist-id", "", "existing gist ID to update; leave empty to create a new public gist")
	flag.StringVar(&cfg.GistFilename, "gist-filename", "dot-project-repos.md", "filename for the dot-project Markdown gist")
	flag.StringVar(&cfg.GistDescription, "gist-description", "maintainer-d dot-project repository report", "description for the dot-project Markdown gist")
	flag.Parse()
	return cfg
}

func publishDotProjectGist(ctx context.Context, client *github.Client, cfg syncConfig, rows []dotproject.GistReportRow) (*github.Gist, error) {
	if client == nil {
		return nil, fmt.Errorf("github client is required")
	}
	content, err := dotproject.GistReportMarkdown(rows)
	if err != nil {
		return nil, err
	}
	filename := strings.TrimSpace(cfg.GistFilename)
	if filename == "" {
		filename = "dot-project-repos.md"
	}
	gist := &github.Gist{
		Description: github.String(strings.TrimSpace(cfg.GistDescription)),
		Public:      github.Bool(true),
		Files: map[github.GistFilename]github.GistFile{
			github.GistFilename(filename): {
				Content: github.String(content),
			},
		},
	}
	if gistID := strings.TrimSpace(cfg.GistID); gistID != "" {
		updated, _, err := client.Gists.Edit(ctx, gistID, gist)
		if err != nil {
			return nil, fmt.Errorf("update gist %s: %w", gistID, err)
		}
		return updated, nil
	}
	created, _, err := client.Gists.Create(ctx, gist)
	if err != nil {
		return nil, fmt.Errorf("create public gist: %w", err)
	}
	return created, nil
}

func loadFoundationMaintainers(ctx context.Context, client *github.Client, cfg syncConfig) (*dotproject.FoundationMaintainerIndex, error) {
	if !cfg.CheckFoundationCSV {
		return nil, nil
	}
	owner := strings.TrimSpace(cfg.FoundationOwner)
	repo := strings.TrimSpace(cfg.FoundationRepo)
	ref := strings.TrimSpace(cfg.FoundationRef)
	path := strings.TrimSpace(cfg.FoundationPath)
	if owner == "" || repo == "" || ref == "" || path == "" {
		return nil, fmt.Errorf("foundation csv owner, repo, ref, and path are required")
	}
	branch, _, err := client.Repositories.GetBranch(ctx, owner, repo, ref, false)
	if err != nil {
		return nil, fmt.Errorf("get foundation csv branch %s/%s@%s: %w", owner, repo, ref, err)
	}
	commitSHA := branch.GetCommit().GetSHA()
	if commitSHA == "" {
		return nil, fmt.Errorf("foundation csv branch %s/%s@%s did not return a commit SHA", owner, repo, ref)
	}
	file, _, _, err := client.Repositories.GetContents(ctx, owner, repo, path, &github.RepositoryContentGetOptions{Ref: commitSHA})
	if err != nil {
		return nil, fmt.Errorf("get foundation csv content %s/%s/%s@%s: %w", owner, repo, path, commitSHA, err)
	}
	if file == nil {
		return nil, fmt.Errorf("foundation csv path %s/%s/%s@%s is not a file", owner, repo, path, commitSHA)
	}
	content, err := file.GetContent()
	if err != nil {
		return nil, fmt.Errorf("decode foundation csv content: %w", err)
	}
	index, err := dotproject.ParseFoundationMaintainersCSV(strings.NewReader(content))
	if err != nil {
		return nil, err
	}
	index.CommitSHA = commitSHA
	index.SourceURL = foundationCSVBlobURL(owner, repo, commitSHA, path)
	index.Branch = ref
	return index, nil
}

func foundationCSVBlobURL(owner, repo, ref, path string) string {
	return fmt.Sprintf("https://github.com/%s/%s/blob/%s/%s?plain=1", owner, repo, ref, path)
}

func buildAutoAdder(store *db.SQLStore, cfg syncConfig, foundation *dotproject.FoundationMaintainerIndex, lfxResolver dotproject.LFXIdentityResolver, githubClient *github.Client) dotproject.MaintainerAutoAdder {
	return &dotproject.AutoMaintainerAdder{
		Store:              store,
		Foundation:         foundation,
		LFX:                lfxResolver,
		Provenance:         provenance.NewResolver(githubClient),
		Actor:              cfg.Actor,
		CheckFoundationCSV: cfg.CheckFoundationCSV,
		AutoAddMaintainers: cfg.AutoAddMaintainers,
		Logger:             zap.NewNop().Sugar(),
	}
}

func buildLFXEnricher(store *db.SQLStore, client lfx.UserSearcher) dotproject.MaintainerEnricher {
	enrichAll := strings.EqualFold(strings.TrimSpace(os.Getenv("LFX_ENRICH_ALL_MAINTAINERS")), "true")
	defaultMaxLookups := 100
	if enrichAll {
		defaultMaxLookups = 0
	}
	maxLookups := max(envInt("LFX_MAX_LOOKUPS", defaultMaxLookups), 0)
	lastProgressLog := time.Time{}
	lastProgressProcessed := -1
	return &lfx.Enricher{Store: store, Client: client, EnrichAll: enrichAll, MaxLookups: maxLookups, Progress: func(progress lfx.EnrichmentProgress) {
		if progress.Total <= 0 {
			return
		}
		now := time.Now()
		shouldLog := progress.Processed == 0 ||
			progress.Processed == progress.Total ||
			progress.Processed-lastProgressProcessed >= 25 ||
			lastProgressLog.IsZero() ||
			now.Sub(lastProgressLog) >= 15*time.Second
		if !shouldLog {
			return
		}
		lastProgressLog = now
		lastProgressProcessed = progress.Processed
		log.Printf(
			"lfx enrichment progress processed=%d total=%d current=%s attempted=%d matched=%d ambiguous=%d unmatched=%d errored=%d skipped_limit=%d",
			progress.Processed,
			progress.Total,
			progress.Current,
			progress.Summary.Attempted,
			progress.Summary.Matched,
			progress.Summary.Ambiguous,
			progress.Summary.Unmatched,
			progress.Summary.Errored,
			progress.Summary.SkippedLimit,
		)
	}}
}

func buildRequiredLFXClient() (*lfx.Client, error) {
	token := strings.TrimSpace(os.Getenv("LFX_AUTH_TOKEN"))
	acl := strings.TrimSpace(os.Getenv("LFX_ACL"))
	if token == "" {
		return nil, fmt.Errorf("%s", lfxTokenHelp)
	}
	return buildLFXClient(token, acl), nil
}

func buildLFXClient(token, acl string) *lfx.Client {
	return &lfx.Client{
		BaseURL: strings.TrimSpace(envOr("LFX_BASE_URL", lfx.DefaultBaseURL)),
		HTTPClient: &http.Client{
			Timeout: envDuration("LFX_TIMEOUT", 30*time.Second),
		},
		MinDelay: envDuration("LFX_REQUEST_DELAY", 250*time.Millisecond),
		Token:    token,
		ACL:      acl,
		Username: strings.TrimSpace(os.Getenv("LFX_USERNAME")),
		Email:    strings.TrimSpace(os.Getenv("LFX_EMAIL")),
	}
}

type lfxIdentityResolver struct {
	client lfx.UserSearcher
}

func (r lfxIdentityResolver) ResolveMaintainerIdentity(ctx context.Context, githubHandle, email string) (dotproject.LFXIdentityResult, error) {
	githubHandle = strings.TrimSpace(githubHandle)
	email = strings.TrimSpace(email)
	if r.client == nil || (githubHandle == "" && email == "") {
		return dotproject.LFXIdentityResult{}, nil
	}
	var users []lfx.User
	var err error
	matchedByGitHubID := false
	if githubHandle != "" {
		users, err = r.client.SearchUsers(ctx, lfx.UserSearch{GitHubID: githubHandle, PageSize: 10})
		if err != nil {
			return dotproject.LFXIdentityResult{}, lfx.PlatformAccessError(err)
		}
		matchedByGitHubID = len(users) > 0
	}
	if len(users) == 0 && email != "" {
		users, err = r.client.SearchUsers(ctx, lfx.UserSearch{Email: email, PageSize: 10})
		if err != nil {
			return dotproject.LFXIdentityResult{}, lfx.PlatformAccessError(err)
		}
	}
	matchedByUsername := false
	if len(users) == 0 && githubHandle != "" {
		// Some LFX/PCC records have no GithubID field populated, but the LF
		// Username (the openprofile.dev slug) matches the GitHub handle. A
		// coincidental string match, not a verified linkage, so it must not
		// inherit "strong" the way a GitHubID/email match does below.
		users, err = r.client.SearchUsers(ctx, lfx.UserSearch{Username: githubHandle, PageSize: 10})
		if err != nil {
			return dotproject.LFXIdentityResult{}, lfx.PlatformAccessError(err)
		}
		matchedByUsername = len(users) > 0
	}
	if len(users) != 1 {
		return dotproject.LFXIdentityResult{Confidence: "unmatched", Reason: "LFX user search did not return a single user"}, nil
	}
	user := users[0]
	identities, err := r.client.GetUserIdentities(ctx, user.ID)
	if err != nil {
		return dotproject.LFXIdentityResult{}, lfx.PlatformAccessError(err)
	}
	// Start weak and grant strong only for corroborated matches, mirroring
	// lfx.confidenceFor: the email fallback can match a secondary address
	// while returning a profile whose primary email differs, and that
	// uncorroborated profile must not read as strong (it feeds auto-add).
	confidence := "weak"
	var reason string
	switch {
	case matchedByUsername:
		reason = "single LFX user match by username only"
	case matchedByGitHubID && strings.EqualFold(strings.TrimSpace(user.Type), "contact"):
		confidence = "strong"
		reason = "single LFX user match by GitHub ID on a claimed (contact) profile"
	case matchedByGitHubID:
		// A bare GitHub-ID match on a "lead" (a stale, never-claimed
		// Salesforce row - see lfx/LFX-USER-API-NOTES.MD finding 8) must not
		// read as strong; the identity loop below can still upgrade it to
		// exact, mirroring lfx.confidenceFor.
		reason = "single LFX user match by GitHub ID on an unclaimed (lead) profile"
	case email != "" && strings.EqualFold(strings.TrimSpace(user.Email), email):
		confidence = "strong"
		reason = "single LFX user match by corroborated email"
	default:
		reason = "single LFX user match by email without a corroborating primary email"
	}
	result := dotproject.LFXIdentityResult{
		UserID:     strings.TrimSpace(user.ID),
		LFID:       strings.TrimSpace(user.Username),
		Name:       firstNonEmpty(strings.TrimSpace(user.Name), strings.TrimSpace(user.FirstName+" "+user.LastName)),
		Email:      strings.TrimSpace(user.Email),
		GitHubUser: githubHandle,
		Confidence: confidence,
		Reason:     reason,
	}
	for _, identity := range identities {
		if strings.EqualFold(strings.TrimSpace(identity.Source), "github") && githubHandle != "" && strings.EqualFold(identity.Username, githubHandle) {
			result.Confidence = "exact"
			result.Reason = "LFX github identity matched maintainer handle"
			if result.Email == "" {
				result.Email = strings.TrimSpace(identity.Email)
			}
			break
		}
	}
	result.Company = accountCompanyName(user.Account)
	raw := struct {
		User       lfx.User       `json:"user"`
		Identities []lfx.Identity `json:"identities,omitempty"`
	}{User: user, Identities: identities}
	body, err := json.Marshal(raw)
	if err != nil {
		return result, err
	}
	result.RawPayload = string(body)
	return result, nil
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func projectLabel(project model.Project) string {
	if name := strings.TrimSpace(project.Name); name != "" {
		return name
	}
	return fmt.Sprintf("project %d", project.ID)
}

func dotProjectFileURL(file dotproject.FileDiscovery) string {
	if value := strings.TrimSpace(file.BlobURL); value != "" {
		return value
	}
	if value := strings.TrimSpace(file.RawURL); value != "" {
		return value
	}
	if value := strings.TrimSpace(file.Path); value != "" {
		return value
	}
	return "maintainers.yaml"
}

func envInt(key string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return value
}

func envDuration(key string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	value, err := time.ParseDuration(raw)
	if err != nil {
		return fallback
	}
	return value
}

func collectPostSyncMetrics(ctx context.Context, store *db.SQLStore) (*postSyncMetrics, error) {
	if store == nil || store.DB() == nil {
		return nil, fmt.Errorf("sql store is not initialized")
	}
	if store.DB().Name() != "postgres" {
		return nil, fmt.Errorf("dot-project-sync post-sync metrics require postgres, got %q", store.DB().Name())
	}

	metrics := &postSyncMetrics{}

	sizeRow := struct {
		DBSizeBytes              int64
		DotProjectSyncStateBytes int64
	}{}
	if err := store.DB().WithContext(ctx).Raw(`
		select
			pg_database_size(current_database()) as db_size_bytes,
			pg_total_relation_size('dot_project_sync_states') as dot_project_sync_state_bytes
	`).Scan(&sizeRow).Error; err != nil {
		return metrics, err
	}
	metrics.DBSizeBytes = sizeRow.DBSizeBytes
	metrics.DotProjectSyncStateBytes = sizeRow.DotProjectSyncStateBytes

	bodyRow := struct {
		CachedFiles             int64
		MaintainersBodyBytes    int64
		AvgMaintainersBodyBytes int64
		MaxMaintainersBodyBytes int64
	}{}
	if err := store.DB().WithContext(ctx).Raw(`
		select
			count(*) filter (where maintainers_file_body is not null) as cached_files,
			coalesce(sum(pg_column_size(maintainers_file_body)), 0) as maintainers_body_bytes,
			coalesce(avg(pg_column_size(maintainers_file_body))::bigint, 0) as avg_maintainers_body_bytes,
			coalesce(max(pg_column_size(maintainers_file_body)), 0) as max_maintainers_body_bytes
		from dot_project_sync_states
	`).Scan(&bodyRow).Error; err != nil {
		return metrics, err
	}
	metrics.CachedFiles = bodyRow.CachedFiles
	metrics.MaintainersBodyBytes = bodyRow.MaintainersBodyBytes
	metrics.AvgMaintainersBodyBytes = bodyRow.AvgMaintainersBodyBytes
	metrics.MaxMaintainersBodyBytes = bodyRow.MaxMaintainersBodyBytes

	coverageRow := struct {
		ProjectsTotal int64
		ReposFound    int64
		CachedBodies  int64
	}{}
	if err := store.DB().WithContext(ctx).Raw(`
		select
			count(*) as projects_total,
			count(*) filter (where repo_exists) as repos_found,
			count(*) filter (where maintainers_file_body is not null) as cached_bodies
		from dot_project_sync_states
	`).Scan(&coverageRow).Error; err != nil {
		return metrics, err
	}
	metrics.ProjectsTotal = coverageRow.ProjectsTotal
	metrics.ReposFound = coverageRow.ReposFound
	metrics.CachedBodies = coverageRow.CachedBodies

	return metrics, nil
}

func buildAuditEvent(summary dotproject.SyncSummary, metrics *postSyncMetrics, metricsErr error, cfg syncConfig) model.AuditLog {
	metadata := map[string]any{
		"loaded":                        summary.Loaded,
		"scanned":                       summary.Total,
		"skipped":                       summary.Skipped,
		"skipped_archived":              summary.SkippedArchived,
		"skipped_excluded":              summary.SkippedExcluded,
		"synced":                        summary.Synced,
		"errored":                       summary.Errored,
		"github_error_count":            summary.GitHubErrorCount,
		"rate_limit_error_count":        summary.RateLimitErrorCount,
		"not_found":                     summary.NotFound,
		"repo_only":                     summary.RepoOnly,
		"partial":                       summary.Partial,
		"adopted":                       summary.Adopted,
		"lfx_enrichment_attempted":      summary.Enrichment.Attempted,
		"lfx_enrichment_matched":        summary.Enrichment.Matched,
		"lfx_enrichment_ambiguous":      summary.Enrichment.Ambiguous,
		"lfx_enrichment_unmatched":      summary.Enrichment.Unmatched,
		"lfx_enrichment_errored":        summary.Enrichment.Errored,
		"lfx_enrichment_skipped_recent": summary.Enrichment.SkippedRecent,
		"lfx_enrichment_skipped_limit":  summary.Enrichment.SkippedLimit,
		"auto_add_candidates":           summary.AutoAdd.Candidates,
		"auto_add_dry_run_candidates":   summary.AutoAdd.DryRunCandidates,
		"auto_add_created":              summary.AutoAdd.CreatedMaintainers,
		"auto_add_linked":               summary.AutoAdd.LinkedMaintainers,
		"auto_add_would_create":         summary.AutoAdd.WouldCreateMaintainers,
		"auto_add_would_link":           summary.AutoAdd.WouldLinkMaintainers,
		"auto_add_skipped_foundation":   summary.AutoAdd.SkippedFoundationMissing,
		"auto_add_skipped_csv_load":     summary.AutoAdd.SkippedCSVLoadFailed,
		"auto_add_skipped_project":      summary.AutoAdd.SkippedProjectMismatch,
		"auto_add_skipped_invalid":      summary.AutoAdd.SkippedInvalidMaintainers,
		"auto_add_lfx_attempted":        summary.AutoAdd.LFXAttempted,
		"auto_add_lfx_matched":          summary.AutoAdd.LFXMatched,
		"auto_add_lfx_unmatched":        summary.AutoAdd.LFXUnmatched,
		"auto_add_lfx_errored":          summary.AutoAdd.LFXErrored,
		"auto_add_errored":              summary.AutoAdd.Errored,
		"auto_add_audit_failures":       summary.AutoAdd.AuditFailures,
		"config":                        cfg,
		"gist_report_rows":              len(summary.GistReportRows),
	}
	if len(summary.ErrorSummaries) > 0 {
		metadata["errors"] = summary.ErrorSummaries
	}
	if len(summary.WarningSummaries) > 0 {
		metadata["warnings"] = summary.WarningSummaries
	}
	if metrics != nil {
		metadata["db_size_bytes"] = metrics.DBSizeBytes
		metadata["dot_project_sync_state_bytes"] = metrics.DotProjectSyncStateBytes
		metadata["cached_files"] = metrics.CachedFiles
		metadata["maintainers_body_bytes"] = metrics.MaintainersBodyBytes
		metadata["avg_maintainers_body_bytes"] = metrics.AvgMaintainersBodyBytes
		metadata["max_maintainers_body_bytes"] = metrics.MaxMaintainersBodyBytes
		metadata["projects_total"] = metrics.ProjectsTotal
		metadata["repos_found"] = metrics.ReposFound
		metadata["cached_bodies"] = metrics.CachedBodies
	}
	if metricsErr != nil {
		metadata["db_metrics_error"] = strings.TrimSpace(metricsErr.Error())
	}
	body, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		body = []byte("{}")
	}
	message := fmt.Sprintf(
		"DOT_PROJECT_SYNC_RUN: scanned=%d synced=%d errored=%d github_errors=%d rate_limit_errors=%d skipped=%d auto_add_candidates=%d would_create=%d would_link=%d created=%d linked=%d",
		summary.Total,
		summary.Synced,
		summary.Errored,
		summary.GitHubErrorCount,
		summary.RateLimitErrorCount,
		summary.Skipped,
		summary.AutoAdd.Candidates,
		summary.AutoAdd.WouldCreateMaintainers,
		summary.AutoAdd.WouldLinkMaintainers,
		summary.AutoAdd.CreatedMaintainers,
		summary.AutoAdd.LinkedMaintainers,
	)
	return model.AuditLog{
		Action:   "DOT_PROJECT_SYNC_RUN_FINISHED",
		Message:  message,
		Metadata: string(body),
	}
}

func buildStartAuditEvent(cfg syncConfig) model.AuditLog {
	metadata := map[string]any{
		"config": cfg,
	}
	body, err := json.Marshal(metadata)
	if err != nil {
		body = []byte("{}")
	}
	actor := strings.TrimSpace(cfg.Actor)
	if actor == "" {
		actor = "dot-project-sync"
	}
	return model.AuditLog{
		Action:   "DOT_PROJECT_SYNC_RUN_STARTED",
		Message:  fmt.Sprintf("DOT_PROJECT_SYNC_RUN_STARTED: actor=%s auto_add_maintainers=%t check_foundation_csv=%t", actor, cfg.AutoAddMaintainers, cfg.CheckFoundationCSV),
		Metadata: string(body),
	}
}

func configJSON(cfg syncConfig) json.RawMessage {
	body, err := json.Marshal(cfg)
	if err != nil {
		return json.RawMessage("{}")
	}
	return body
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func accountCompanyName(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return ""
	}
	if value := firstNonEmpty(
		mapStringValue(payload, "Company"),
		mapStringValue(payload, "company"),
		mapStringValue(payload, "CompanyName"),
		mapStringValue(payload, "companyName"),
		mapStringValue(payload, "Name"),
		mapStringValue(payload, "name"),
	); value != "" {
		return value
	}
	return ""
}

func mapStringValue(payload map[string]any, key string) string {
	value, ok := payload[key]
	if !ok || value == nil {
		return ""
	}
	if str, ok := value.(string); ok {
		return strings.TrimSpace(str)
	}
	return ""
}
