package dotproject

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"maintainerd/model"

	"github.com/google/go-github/v55/github"
)

const (
	AdoptionStatusNotFound = "not_found"
	AdoptionStatusRepoOnly = "repo_only"
	AdoptionStatusPartial  = "partial"
	AdoptionStatusAdopted  = "adopted"
	AdoptionStatusError    = "error"
)

type SyncStore interface {
	ListProjects() ([]model.Project, error)
	PersistDotProjectSync(projectID uint, patch model.Project, state *model.DotProjectSyncState) error
}

type DiscoveryRunner interface {
	Discover(ctx context.Context, project model.Project) (*DiscoveryResult, error)
}

type MaintainerEnricher interface {
	EnrichProject(ctx context.Context, project model.Project, result *DiscoveryResult) (EnrichmentSummary, error)
}

type MaintainerAutoAdder interface {
	ProcessProject(ctx context.Context, project model.Project, result *DiscoveryResult) (AutoAddSummary, error)
}

type EnrichmentSummary struct {
	Attempted     int
	Matched       int
	Ambiguous     int
	Unmatched     int
	Errored       int
	SkippedRecent int
	SkippedLimit  int
}

type SyncSummary struct {
	Loaded              int
	Total               int
	Skipped             int
	SkippedArchived     int
	SkippedExcluded     int
	Synced              int
	Errored             int
	NotFound            int
	Adopted             int
	Partial             int
	RepoOnly            int
	GitHubErrorCount    int
	RateLimitErrorCount int
	Enrichment          EnrichmentSummary
	AutoAdd             AutoAddSummary
	GistReportRows      []GistReportRow
	WarningSummaries    []string
	ErrorSummaries      []string
	StoppedEarly        bool
	RemainingProjects   int
}

type Syncer struct {
	Store                  SyncStore
	Discoverer             DiscoveryRunner
	Enricher               MaintainerEnricher
	AutoAdder              MaintainerAutoAdder
	MaintainersFileVisitor func(project model.Project, file FileDiscovery)
	Now                    func() time.Time
	Progress               func(SyncProgress)
}

// SyncProgress reports which project a SyncAll run is currently visiting, so
// a caller can render a projects-processed progress bar alongside the
// per-project candidate-level EnrichmentProgress.
type SyncProgress struct {
	TotalProjects     int
	ProjectsProcessed int
	CurrentProject    string
}

func (s *Syncer) reportSyncProgress(progress SyncProgress) {
	if s == nil || s.Progress == nil {
		return
	}
	s.Progress(progress)
}

type FatalSyncError struct {
	Err error
}

func (e FatalSyncError) Error() string {
	if e.Err == nil {
		return "fatal sync error"
	}
	return e.Err.Error()
}

func (e FatalSyncError) Unwrap() error {
	return e.Err
}

func (s *Syncer) SyncAll(ctx context.Context) (SyncSummary, error) {
	if s == nil || s.Store == nil || s.Discoverer == nil {
		return SyncSummary{}, fmt.Errorf("dot-project syncer store and discoverer are required")
	}

	projects, err := s.Store.ListProjects()
	if err != nil {
		return SyncSummary{}, err
	}

	summary := SyncSummary{Loaded: len(projects)}
	totalProjects := len(projects)
	// processed feeds the final progress callback: on a deadline break it
	// must reflect how far the loop actually got, not jump to 100%.
	processed := totalProjects
	for i, project := range projects {
		s.reportSyncProgress(SyncProgress{
			TotalProjects:     totalProjects,
			ProjectsProcessed: i,
			CurrentProject:    projectLabel(project),
		})
		if !shouldSyncProject(project) {
			summary.Skipped++
			if isArchivedProject(project) {
				summary.SkippedArchived++
			} else {
				summary.SkippedExcluded++
			}
			continue
		}
		summary.Total++
		status, enrichment, autoAdd, gistReportRow, err := s.syncProject(ctx, project)
		// The project may have persisted work (enrichment rows, auto-added
		// maintainers) before an error surfaced; fold its partial counters in
		// unconditionally so every exit path - ordinary error, fatal abort,
		// deadline break, success - reports what actually happened instead of
		// dropping the interrupted project's attempted/matched/error metrics.
		summary.Enrichment.add(enrichment)
		summary.AutoAdd.add(autoAdd)
		if gistReportRow != nil {
			summary.GistReportRows = append(summary.GistReportRows, *gistReportRow)
		}
		if err != nil {
			// A run-level deadline being exhausted mid-project is expected
			// under load, not a broken integration: stop cleanly and let the
			// caller report a partial success, rather than failing the whole
			// run and losing everything already persisted this pass. This
			// must catch the deadline wherever it surfaced - GitHub
			// discovery returns it as a plain error, not a FatalSyncError,
			// and counting the rest of the run as errors would misreport an
			// exhausted time budget as dozens of broken projects. The gate
			// is the run context itself, not the error's shape: a single
			// slow request also reports errors.Is(err,
			// context.DeadlineExceeded) via http.Client.Timeout, and that
			// must stay an ordinary project error while the run has budget
			// left. The error must also actually carry the run context's
			// error: an unrelated failure (say, a database persist error)
			// that races the deadline must be recorded as this project's
			// error, not silently relabelled as an expected timeout - the
			// next project's first context-aware call fails with the real
			// context error and stops the run anyway.
			if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
				summary.StoppedEarly = true
				processed = i
				summary.RemainingProjects = totalProjects - i - 1
				summary.recordWarning(projectLabel(project), fmt.Sprintf(
					"sync run ran out of time (context deadline exceeded); %d project(s) after this one were not attempted this run",
					summary.RemainingProjects))
				break
			}
			var fatal FatalSyncError
			if errors.As(err, &fatal) {
				// A genuine LFX auth failure (no deadline involved) still
				// aborts the run - retrying it for every remaining project
				// would just burn the GitHub API rate limit for certain
				// failures.
				return summary, err
			}
			summary.Errored++
			summary.recordError(projectLabel(project), err)
			continue
		}
		if gistReportRow != nil && strings.TrimSpace(gistReportRow.Warning) != "" {
			summary.recordWarning(projectLabel(project), gistReportRow.Warning)
		}
		summary.Synced++
		switch status {
		case AdoptionStatusNotFound:
			summary.NotFound++
		case AdoptionStatusAdopted:
			summary.Adopted++
		case AdoptionStatusPartial:
			summary.Partial++
		case AdoptionStatusRepoOnly:
			summary.RepoOnly++
		}
	}
	s.reportSyncProgress(SyncProgress{
		TotalProjects:     totalProjects,
		ProjectsProcessed: processed,
		CurrentProject:    "",
	})
	return summary, nil
}

func (s *EnrichmentSummary) add(other EnrichmentSummary) {
	s.Attempted += other.Attempted
	s.Matched += other.Matched
	s.Ambiguous += other.Ambiguous
	s.Unmatched += other.Unmatched
	s.Errored += other.Errored
	s.SkippedRecent += other.SkippedRecent
	s.SkippedLimit += other.SkippedLimit
}

func (s *SyncSummary) recordError(projectLabel string, err error) {
	if err == nil {
		return
	}
	if IsGitHubRateLimitError(err) {
		s.RateLimitErrorCount++
		s.GitHubErrorCount++
	} else if IsGitHubAPIError(err) {
		s.GitHubErrorCount++
	}
	message := strings.TrimSpace(err.Error())
	if message == "" {
		return
	}
	label := strings.TrimSpace(projectLabel)
	if label == "" {
		label = "unknown project"
	}
	message = fmt.Sprintf("%s: %s", label, message)
	for _, existing := range s.ErrorSummaries {
		if existing == message {
			return
		}
	}
	if len(s.ErrorSummaries) >= 10 {
		return
	}
	s.ErrorSummaries = append(s.ErrorSummaries, message)
}

func (s *SyncSummary) recordWarning(projectLabel, warning string) {
	warning = strings.TrimSpace(warning)
	if warning == "" {
		return
	}
	label := strings.TrimSpace(projectLabel)
	if label == "" {
		label = "unknown project"
	}
	message := fmt.Sprintf("%s: %s", label, warning)
	for _, existing := range s.WarningSummaries {
		if existing == message {
			return
		}
	}
	if len(s.WarningSummaries) >= 25 {
		return
	}
	s.WarningSummaries = append(s.WarningSummaries, message)
}

func projectLabel(project model.Project) string {
	if name := strings.TrimSpace(project.Name); name != "" {
		return name
	}
	return fmt.Sprintf("project %d", project.ID)
}

func (s *Syncer) SyncProject(ctx context.Context, project model.Project) (string, error) {
	status, _, _, _, err := s.syncProject(ctx, project)
	return status, err
}

func (s *Syncer) syncProject(ctx context.Context, project model.Project) (string, EnrichmentSummary, AutoAddSummary, *GistReportRow, error) {
	now := time.Now().UTC()
	if s != nil && s.Now != nil {
		now = s.Now().UTC()
	}

	result, err := s.Discoverer.Discover(ctx, project)
	if err != nil {
		// An exhausted run deadline is a clean stop, not a broken project:
		// persisting AdoptionStatusError with a fresh DotProjectLastSyncedAt
		// here would mark an unprocessed project as errored and push it to
		// the freshest end of the anti-starvation order, so it would also be
		// retried last next run. Leave it untouched for the caller's
		// stopped-early branch.
		if ctx.Err() != nil {
			return AdoptionStatusError, EnrichmentSummary{}, AutoAddSummary{}, nil, err
		}
		status := AdoptionStatusError
		state := &model.DotProjectSyncState{
			ProjectID:       project.ID,
			ImporterVersion: ImporterVersion,
			LastCheckedAt:   &now,
			SyncError:       stringPtr(err.Error()),
		}
		patch := project
		patch.DotProjectLastSyncedAt = &now
		patch.DotProjectAdoptionStatus = status
		persistErr := s.Store.PersistDotProjectSync(project.ID, patch, state)
		if persistErr != nil {
			return status, EnrichmentSummary{}, AutoAddSummary{}, nil, fmt.Errorf("persist sync error for project %d: %w", project.ID, persistErr)
		}
		return status, EnrichmentSummary{}, AutoAddSummary{}, nil, err
	}

	status := adoptionStatusFor(result)
	if s.MaintainersFileVisitor != nil && result.MaintainersFile.Exists {
		s.MaintainersFileVisitor(project, result.MaintainersFile)
	}
	state := buildSyncState(project.ID, now, result)
	patch := buildProjectPatch(now, status, result)
	var gistReportRow *GistReportRow
	if row, ok := BuildGistReportRow(project, result); ok {
		gistReportRow = &row
	}
	// The freshness timestamp drives ListProjects' anti-starvation ordering
	// (oldest dot_project_last_synced_at first), so it must only advance once
	// enrichment and auto-add have both run to completion for this project.
	// Persisting it right after discovery let a deadline mid-enrichment mark
	// a partially processed project as freshly synced, pushing it behind
	// every older project on the next run and starving it indefinitely.
	persist := func() error {
		if err := s.Store.PersistDotProjectSync(project.ID, patch, state); err != nil {
			return fmt.Errorf("persist sync error for project %d: %w", project.ID, err)
		}
		return nil
	}
	enrichment := EnrichmentSummary{}
	if s.Enricher != nil {
		enrichment, err = s.Enricher.EnrichProject(ctx, project, result)
		if err != nil {
			if enrichment.Errored == 0 {
				enrichment.Errored++
			}
			if ctx.Err() != nil {
				patch.DotProjectLastSyncedAt = project.DotProjectLastSyncedAt
			}
			if persistErr := persist(); persistErr != nil {
				return status, enrichment, AutoAddSummary{}, gistReportRow, persistErr
			}
			// Fatality is decided at the source (lfx.PlatformAccessError wraps
			// only 401 and 429 in FatalSyncError; 403 is deliberately nonfatal
			// because a per-resource ACL denial mid-run must not abort the
			// rest); a timeout or 5xx on one project must not abort the run.
			return status, enrichment, AutoAddSummary{}, gistReportRow, err
		}
	}
	autoAdd := AutoAddSummary{}
	if s.AutoAdder != nil {
		autoAdd, err = s.AutoAdder.ProcessProject(ctx, project, result)
		if err != nil {
			if autoAdd.Errored == 0 {
				autoAdd.Errored++
			}
			if ctx.Err() != nil {
				patch.DotProjectLastSyncedAt = project.DotProjectLastSyncedAt
			}
			if persistErr := persist(); persistErr != nil {
				return status, enrichment, autoAdd, gistReportRow, persistErr
			}
			return status, enrichment, autoAdd, gistReportRow, err
		}
	}
	if err := persist(); err != nil {
		return status, enrichment, autoAdd, gistReportRow, err
	}
	return status, enrichment, autoAdd, gistReportRow, nil
}

func buildProjectPatch(now time.Time, status string, result *DiscoveryResult) model.Project {
	patch := model.Project{
		DotProjectLastSyncedAt:   &now,
		DotProjectAdoptionStatus: status,
	}
	if result == nil {
		return patch
	}
	patch.DotProjectRepoRef = strings.TrimSpace(result.RepoRef)
	patch.DotProjectProjectRef = strings.TrimSpace(result.ProjectFile.BlobURL)
	patch.DotProjectMaintainerRef = strings.TrimSpace(result.MaintainersFile.BlobURL)
	patch.DotProjectSecurityRef = strings.TrimSpace(result.SecurityFile.BlobURL)
	patch.DotProjectContributingRef = strings.TrimSpace(result.ContributingFile.BlobURL)
	patch.DotProjectGovernanceRef = strings.TrimSpace(result.GovernanceFile.BlobURL)
	patch.DotProjectSchemaVersion = strings.TrimSpace(result.SchemaVersion)
	patch.DotProjectMaintainerCount = result.MaintainerCount
	if !result.RepoExists {
		patch.DotProjectRepoRef = ""
	}
	return patch
}

func buildSyncState(projectID uint, now time.Time, result *DiscoveryResult) *model.DotProjectSyncState {
	state := &model.DotProjectSyncState{
		ProjectID:       projectID,
		ImporterVersion: ImporterVersion,
		LastCheckedAt:   &now,
	}
	if result == nil {
		return state
	}

	state.RepoExists = result.RepoExists
	state.ProjectFileExists = result.ProjectFile.Exists
	state.MaintainersFileExists = result.MaintainersFile.Exists
	state.SecurityFileExists = result.SecurityFile.Exists
	state.ContributingFileExists = result.ContributingFile.Exists
	state.GovernanceFileExists = result.GovernanceFile.Exists
	state.DefaultBranch = strings.TrimSpace(result.DefaultBranch)
	state.MaintainersFilename = strings.TrimSpace(result.MaintainersFilename)
	state.SchemaVersion = strings.TrimSpace(result.SchemaVersion)
	state.ProjectFileETag = strings.TrimSpace(result.ProjectFile.ETag)
	state.MaintainersFileETag = strings.TrimSpace(result.MaintainersFile.ETag)
	state.SecurityFileETag = strings.TrimSpace(result.SecurityFile.ETag)
	state.ContributingFileETag = strings.TrimSpace(result.ContributingFile.ETag)
	state.GovernanceFileETag = strings.TrimSpace(result.GovernanceFile.ETag)
	state.ProjectFileBodyHash = strings.TrimSpace(result.ProjectFile.BodyHash)
	state.MaintainersFileBodyHash = strings.TrimSpace(result.MaintainersFile.BodyHash)
	if result.MaintainersFile.Exists {
		body := result.MaintainersFile.Body
		state.MaintainersFileBody = &body
	}
	state.SecurityFileBodyHash = strings.TrimSpace(result.SecurityFile.BodyHash)
	state.ContributingFileBodyHash = strings.TrimSpace(result.ContributingFile.BodyHash)
	state.GovernanceFileBodyHash = strings.TrimSpace(result.GovernanceFile.BodyHash)
	if parseErr := joinParseErrors(result); parseErr != "" {
		state.ParseError = &parseErr
	}
	return state
}

func adoptionStatusFor(result *DiscoveryResult) string {
	if result == nil || !result.RepoExists {
		return AdoptionStatusNotFound
	}
	hasAnyFile := result.ProjectFile.Exists || result.MaintainersFile.Exists || result.SecurityFile.Exists || result.ContributingFile.Exists || result.GovernanceFile.Exists
	if !hasAnyFile {
		return AdoptionStatusRepoOnly
	}
	if result.ProjectFile.Exists && result.MaintainersFile.Exists && result.SchemaSupported && result.MaintainerCount != nil {
		return AdoptionStatusAdopted
	}
	return AdoptionStatusPartial
}

func joinParseErrors(result *DiscoveryResult) string {
	if result == nil {
		return ""
	}
	parts := make([]string, 0, 2)
	if msg := strings.TrimSpace(result.ProjectParseError); msg != "" {
		parts = append(parts, "project.yaml: "+msg)
	}
	if msg := strings.TrimSpace(result.MaintainersParseError); msg != "" {
		parts = append(parts, result.MaintainersFilename+": "+msg)
	}
	return strings.Join(parts, "; ")
}

func stringPtr(value string) *string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return &value
}

func shouldSyncProject(project model.Project) bool {
	if isArchivedProject(project) {
		return false
	}
	name := strings.ToLower(strings.TrimSpace(project.Name))
	return name != "maintainer-d"
}

func isArchivedProject(project model.Project) bool {
	return project.Maturity == model.Archived
}

// IsGitHubRateLimitError reports whether err is a primary or secondary (abuse) GitHub rate
// limit error.
func IsGitHubRateLimitError(err error) bool {
	var rateLimitErr *github.RateLimitError
	if errors.As(err, &rateLimitErr) {
		return true
	}
	var abuseRateLimitErr *github.AbuseRateLimitError
	return errors.As(err, &abuseRateLimitErr)
}

// IsGitHubAPIError reports whether err originated from a GitHub API response (rate limit,
// error response, or accepted-but-not-ready), as opposed to a transport-level failure.
func IsGitHubAPIError(err error) bool {
	if IsGitHubRateLimitError(err) {
		return true
	}
	var responseErr *github.ErrorResponse
	if errors.As(err, &responseErr) {
		return true
	}
	var acceptedErr *github.AcceptedError
	return errors.As(err, &acceptedErr)
}

// IsGitHubNotFoundError reports whether err is a 404 response, e.g. a deleted or renamed
// GitHub account. Callers typically want to exclude these from error-rate thresholds since
// they represent expected attrition rather than an operational fault.
func IsGitHubNotFoundError(err error) bool {
	var responseErr *github.ErrorResponse
	if errors.As(err, &responseErr) {
		return responseErr.Response != nil && responseErr.Response.StatusCode == http.StatusNotFound
	}
	return false
}

// GitHubRateLimitWait returns how long to wait before retrying after a rate limit error,
// honoring Retry-After (secondary/abuse limits) or the primary limit's reset time when
// available, falling back to floor otherwise.
func GitHubRateLimitWait(err error, floor time.Duration) time.Duration {
	var abuseErr *github.AbuseRateLimitError
	if errors.As(err, &abuseErr) && abuseErr.RetryAfter != nil {
		return *abuseErr.RetryAfter
	}
	var rateLimitErr *github.RateLimitError
	if errors.As(err, &rateLimitErr) {
		if wait := time.Until(rateLimitErr.Rate.Reset.Time); wait > 0 {
			return wait
		}
	}
	return floor
}
