package dotproject

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"maintainerd/model"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type fakeSyncStore struct {
	projects   []model.Project
	persisted  map[uint]persistedSync
	persistErr error
}

type persistedSync struct {
	patch model.Project
	state *model.DotProjectSyncState
}

func (f *fakeSyncStore) ListProjects() ([]model.Project, error) {
	return append([]model.Project{}, f.projects...), nil
}

func (f *fakeSyncStore) PersistDotProjectSync(projectID uint, patch model.Project, state *model.DotProjectSyncState) error {
	if f.persistErr != nil {
		return f.persistErr
	}
	if f.persisted == nil {
		f.persisted = make(map[uint]persistedSync)
	}
	f.persisted[projectID] = persistedSync{patch: patch, state: state}
	return nil
}

type fakeDiscoveryRunner struct {
	results map[uint]*DiscoveryResult
	errors  map[uint]error
	seen    map[uint]model.Project
}

func (f *fakeDiscoveryRunner) Discover(_ context.Context, project model.Project) (*DiscoveryResult, error) {
	if f.seen == nil {
		f.seen = make(map[uint]model.Project)
	}
	f.seen[project.ID] = project
	if err := f.errors[project.ID]; err != nil {
		return nil, err
	}
	if result, ok := f.results[project.ID]; ok {
		return result, nil
	}
	return &DiscoveryResult{}, nil
}

type fakeMaintainerEnricher struct {
	summary EnrichmentSummary
	err     error
}

func (f fakeMaintainerEnricher) EnrichProject(_ context.Context, _ model.Project, _ *DiscoveryResult) (EnrichmentSummary, error) {
	return f.summary, f.err
}

func TestSyncProjectPersistsAdoptedDiscovery(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 9, 10, 11, 12, 0, time.UTC)
	store := &fakeSyncStore{}
	var visitedProject model.Project
	var visitedFile FileDiscovery
	syncer := &Syncer{
		Store: store,
		Discoverer: &fakeDiscoveryRunner{
			results: map[uint]*DiscoveryResult{
				42: {
					RepoExists:             true,
					RepoRef:                "https://github.com/example-org/.project",
					DefaultBranch:          "main",
					ProjectFile:            FileDiscovery{Exists: true, BlobURL: "https://github.com/example-org/.project/blob/main/project.yaml", ETag: "\"project\"", BodyHash: "hash-project"},
					MaintainersFile:        FileDiscovery{Exists: true, Path: "MAINTAINERS.yaml", BlobURL: "https://github.com/example-org/.project/blob/main/MAINTAINERS.yaml", ETag: "\"maintainers\"", BodyHash: "hash-maintainers", Body: "maintainers:\n  - teams: []\n"},
					SecurityFile:           FileDiscovery{Exists: true, BlobURL: "https://github.com/example-org/.project/blob/main/SECURITY.md", ETag: "\"security\"", BodyHash: "hash-security"},
					ContributingFile:       FileDiscovery{Exists: true, BlobURL: "https://github.com/example-org/.project/blob/main/CONTRIBUTING.md", ETag: "\"contributing\"", BodyHash: "hash-contributing"},
					GovernanceFile:         FileDiscovery{Exists: true, BlobURL: "https://github.com/example-org/.project/blob/main/GOVERNANCE.md", ETag: "\"governance\"", BodyHash: "hash-governance"},
					MaintainersFilename:    "MAINTAINERS.yaml",
					SchemaVersion:          "1.0.0",
					SchemaSupported:        true,
					MaintainerCount:        uintPtr(5),
					ProjectParseStatus:     ParseStatusParsed,
					MaintainersParseStatus: ParseStatusParsed,
				},
			},
		},
		MaintainersFileVisitor: func(project model.Project, file FileDiscovery) {
			visitedProject = project
			visitedFile = file
		},
		Now: func() time.Time { return now },
	}

	status, err := syncer.SyncProject(context.Background(), model.Project{Model: gorm.Model{ID: 42}, GitHubOrg: "example-org"})
	require.NoError(t, err)
	assert.Equal(t, AdoptionStatusAdopted, status)

	persisted := store.persisted[42]
	assert.Equal(t, "https://github.com/example-org/.project", persisted.patch.DotProjectRepoRef)
	assert.Equal(t, "https://github.com/example-org/.project/blob/main/project.yaml", persisted.patch.DotProjectProjectRef)
	assert.Equal(t, "https://github.com/example-org/.project/blob/main/MAINTAINERS.yaml", persisted.patch.DotProjectMaintainerRef)
	assert.Equal(t, uint(42), visitedProject.ID)
	assert.Equal(t, "https://github.com/example-org/.project/blob/main/MAINTAINERS.yaml", visitedFile.BlobURL)
	assert.Equal(t, "1.0.0", persisted.patch.DotProjectSchemaVersion)
	assert.Equal(t, AdoptionStatusAdopted, persisted.patch.DotProjectAdoptionStatus)
	require.NotNil(t, persisted.patch.DotProjectLastSyncedAt)
	assert.True(t, persisted.patch.DotProjectLastSyncedAt.Equal(now))
	require.NotNil(t, persisted.patch.DotProjectMaintainerCount)
	assert.Equal(t, uint(5), *persisted.patch.DotProjectMaintainerCount)

	require.NotNil(t, persisted.state)
	assert.True(t, persisted.state.RepoExists)
	assert.True(t, persisted.state.ProjectFileExists)
	assert.True(t, persisted.state.MaintainersFileExists)
	assert.Equal(t, "main", persisted.state.DefaultBranch)
	assert.Equal(t, "MAINTAINERS.yaml", persisted.state.MaintainersFilename)
	assert.Equal(t, "1.0.0", persisted.state.SchemaVersion)
	assert.Equal(t, ImporterVersion, persisted.state.ImporterVersion)
	require.NotNil(t, persisted.state.MaintainersFileBody)
	assert.Equal(t, "maintainers:\n  - teams: []\n", *persisted.state.MaintainersFileBody)
	require.NotNil(t, persisted.state.LastCheckedAt)
	assert.True(t, persisted.state.LastCheckedAt.Equal(now))
	assert.Nil(t, persisted.state.ParseError)
}

func TestSyncProjectPersistsSyncError(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 9, 10, 11, 12, 0, time.UTC)
	store := &fakeSyncStore{}
	syncer := &Syncer{
		Store: store,
		Discoverer: &fakeDiscoveryRunner{
			errors: map[uint]error{
				9: errors.New("github api timeout"),
			},
		},
		Now: func() time.Time { return now },
	}

	status, err := syncer.SyncProject(context.Background(), model.Project{Model: gorm.Model{ID: 9}, GitHubOrg: "example-org"})
	require.Error(t, err)
	assert.Equal(t, AdoptionStatusError, status)

	persisted := store.persisted[9]
	assert.Equal(t, AdoptionStatusError, persisted.patch.DotProjectAdoptionStatus)
	require.NotNil(t, persisted.patch.DotProjectLastSyncedAt)
	assert.True(t, persisted.patch.DotProjectLastSyncedAt.Equal(now))
	require.NotNil(t, persisted.state)
	require.NotNil(t, persisted.state.SyncError)
	assert.Equal(t, "github api timeout", *persisted.state.SyncError)
}

func TestSyncProjectDoesNotInferGitHubOrgFromLegacyMaintainerRef(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 9, 10, 11, 12, 0, time.UTC)
	store := &fakeSyncStore{}
	discoverer := &fakeDiscoveryRunner{
		errors: map[uint]error{
			77: errors.New("project github org is required"),
		},
	}
	syncer := &Syncer{
		Store:      store,
		Discoverer: discoverer,
		Now:        func() time.Time { return now },
	}

	status, err := syncer.SyncProject(context.Background(), model.Project{
		Model:               gorm.Model{ID: 77},
		LegacyMaintainerRef: "https://github.com/example-org/community/blob/main/maintainers.md",
	})
	require.Error(t, err)
	assert.Equal(t, AdoptionStatusError, status)
	require.Contains(t, discoverer.seen, uint(77))
	assert.Equal(t, "", discoverer.seen[77].GitHubOrg)
	persisted := store.persisted[77]
	require.NotNil(t, persisted.state.SyncError)
	assert.Equal(t, "project github org is required", *persisted.state.SyncError)
}

func TestSyncProjectKeepsOrgEmptyWhenLegacyRefIsNotGitHub(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 9, 10, 11, 12, 0, time.UTC)
	store := &fakeSyncStore{}
	discoverer := &fakeDiscoveryRunner{
		errors: map[uint]error{
			88: errors.New("project github org is required"),
		},
	}
	syncer := &Syncer{
		Store:      store,
		Discoverer: discoverer,
		Now:        func() time.Time { return now },
	}

	status, err := syncer.SyncProject(context.Background(), model.Project{
		Model:               gorm.Model{ID: 88},
		LegacyMaintainerRef: "https://gitlab.example.com/group/project/-/blob/main/maintainers.yaml",
	})
	require.Error(t, err)
	assert.Equal(t, AdoptionStatusError, status)
	require.Contains(t, discoverer.seen, uint(88))
	assert.Equal(t, "", discoverer.seen[88].GitHubOrg)
}

func TestSyncAllSummarizesStatuses(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 9, 10, 11, 12, 0, time.UTC)
	store := &fakeSyncStore{
		projects: []model.Project{
			{Model: gorm.Model{ID: 1}, Name: "Project One", GitHubOrg: "org-one"},
			{Model: gorm.Model{ID: 2}, Name: "Project Two", GitHubOrg: "org-two"},
			{Model: gorm.Model{ID: 3}, Name: "Project Three", GitHubOrg: "org-three"},
			{Model: gorm.Model{ID: 4}, Name: "Project Four", GitHubOrg: "org-four"},
		},
	}
	syncer := &Syncer{
		Store: store,
		Discoverer: &fakeDiscoveryRunner{
			results: map[uint]*DiscoveryResult{
				1: {RepoExists: false},
				2: {RepoExists: true},
				3: {RepoExists: true, ProjectFile: FileDiscovery{Exists: true}, MaintainersFile: FileDiscovery{Exists: true}, SchemaSupported: true, MaintainerCount: uintPtr(2)},
			},
			errors: map[uint]error{
				4: errors.New("boom"),
			},
		},
		Now: func() time.Time { return now },
	}

	summary, err := syncer.SyncAll(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 4, summary.Loaded)
	assert.Equal(t, 4, summary.Total)
	assert.Equal(t, 3, summary.Synced)
	assert.Equal(t, 1, summary.Errored)
	assert.Equal(t, 1, summary.NotFound)
	assert.Equal(t, 1, summary.RepoOnly)
	assert.Equal(t, 1, summary.Adopted)
	assert.Equal(t, 0, summary.Partial)
	assert.Len(t, summary.ErrorSummaries, 1)
	assert.Contains(t, summary.ErrorSummaries[0], "Project Four")
	assert.Contains(t, summary.ErrorSummaries[0], "boom")
}

func TestSyncAllReportsProjectProgress(t *testing.T) {
	t.Parallel()

	store := &fakeSyncStore{
		projects: []model.Project{
			{Model: gorm.Model{ID: 1}, Name: "Project One", GitHubOrg: "org-one"},
			{Model: gorm.Model{ID: 2}, Name: "Project Two", GitHubOrg: "org-two"},
		},
	}
	var updates []SyncProgress
	syncer := &Syncer{
		Store:      store,
		Discoverer: &fakeDiscoveryRunner{},
		Progress: func(progress SyncProgress) {
			updates = append(updates, progress)
		},
	}

	_, err := syncer.SyncAll(context.Background())
	require.NoError(t, err)

	require.Len(t, updates, 3, "one update per project visited plus a final completion update")
	assert.Equal(t, SyncProgress{TotalProjects: 2, ProjectsProcessed: 0, CurrentProject: "Project One"}, updates[0])
	assert.Equal(t, SyncProgress{TotalProjects: 2, ProjectsProcessed: 1, CurrentProject: "Project Two"}, updates[1])
	assert.Equal(t, SyncProgress{TotalProjects: 2, ProjectsProcessed: 2, CurrentProject: ""}, updates[2])
}

func TestSyncAllSummarizesMaintainersParseWarnings(t *testing.T) {
	t.Parallel()

	store := &fakeSyncStore{
		projects: []model.Project{
			{Model: gorm.Model{ID: 1}, Name: "Project One", GitHubOrg: "org-one"},
		},
	}
	syncer := &Syncer{
		Store: store,
		Discoverer: &fakeDiscoveryRunner{
			results: map[uint]*DiscoveryResult{
				1: {
					RepoExists:             true,
					MaintainersFile:        FileDiscovery{Exists: true, BlobURL: "https://github.com/org-one/.project/blob/main/maintainers.yaml"},
					MaintainersParseStatus: ParseStatusInvalidShape,
					MaintainersParseError:  "maintainers must contain at least one entry",
				},
			},
		},
	}

	summary, err := syncer.SyncAll(context.Background())
	require.NoError(t, err)

	require.Len(t, summary.WarningSummaries, 1)
	assert.Contains(t, summary.WarningSummaries[0], "Project One")
	assert.Contains(t, summary.WarningSummaries[0], "maintainers must contain at least one entry")
	assert.Contains(t, summary.WarningSummaries[0], "https://github.com/org-one/.project/blob/main/maintainers.yaml")
	require.Len(t, summary.GistReportRows, 1)
	assert.NotEmpty(t, summary.GistReportRows[0].Warning)
}

func TestSyncAllStopsOnFatalEnrichmentError(t *testing.T) {
	t.Parallel()

	store := &fakeSyncStore{
		projects: []model.Project{
			{Model: gorm.Model{ID: 1}, Name: "Project One", GitHubOrg: "org-one"},
			{Model: gorm.Model{ID: 2}, Name: "Project Two", GitHubOrg: "org-two"},
		},
	}
	discoverer := &fakeDiscoveryRunner{
		results: map[uint]*DiscoveryResult{
			1: {RepoExists: true},
			2: {RepoExists: true},
		},
	}
	syncer := &Syncer{
		Store:      store,
		Discoverer: discoverer,
		Enricher:   fakeMaintainerEnricher{err: FatalSyncError{Err: errors.New("LFX Platform access failed; update token")}},
	}

	summary, err := syncer.SyncAll(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "LFX Platform access failed")
	assert.Equal(t, 2, summary.Loaded)
	assert.Equal(t, 1, summary.Total)
	assert.Equal(t, 0, summary.Synced)
	require.Contains(t, discoverer.seen, uint(1))
	assert.NotContains(t, discoverer.seen, uint(2))
}

func TestSyncAllTreatsEnricherTimeoutAsProjectError(t *testing.T) {
	t.Parallel()

	store := &fakeSyncStore{
		projects: []model.Project{
			{Model: gorm.Model{ID: 1}, Name: "Project One", GitHubOrg: "org-one"},
			{Model: gorm.Model{ID: 2}, Name: "Project Two", GitHubOrg: "org-two"},
		},
	}
	discoverer := &fakeDiscoveryRunner{
		results: map[uint]*DiscoveryResult{
			1: {RepoExists: true},
			2: {RepoExists: true},
		},
	}
	syncer := &Syncer{
		Store:      store,
		Discoverer: discoverer,
		// A single slow LFX request (per-request timeout, run context still
		// healthy) is not wrapped in FatalSyncError, so the run must record
		// the project as errored and keep going.
		Enricher: fakeMaintainerEnricher{
			summary: EnrichmentSummary{Attempted: 3, Matched: 2, Errored: 1},
			err:     fmt.Errorf("LFX Platform request timed out: %w", context.DeadlineExceeded),
		},
	}

	summary, err := syncer.SyncAll(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 2, summary.Loaded)
	assert.Equal(t, 2, summary.Total)
	assert.Equal(t, 2, summary.Errored)
	assert.False(t, summary.StoppedEarly)
	require.Contains(t, discoverer.seen, uint(1))
	require.Contains(t, discoverer.seen, uint(2))
	// The errored projects still enriched maintainers before failing; the
	// ordinary-error branch must fold those partial counters into the run
	// summary instead of dropping them.
	assert.Equal(t, 6, summary.Enrichment.Attempted)
	assert.Equal(t, 4, summary.Enrichment.Matched)
	assert.Equal(t, 2, summary.Enrichment.Errored)
}

// cancelingEnricher cancels the run context and then fails, simulating an
// enrichment call that discovers the deadline has passed mid-request.
type cancelingEnricher struct {
	cancel context.CancelFunc
	err    error
}

func (c cancelingEnricher) EnrichProject(_ context.Context, _ model.Project, _ *DiscoveryResult) (EnrichmentSummary, error) {
	c.cancel()
	return EnrichmentSummary{}, c.err
}

func TestSyncProjectPreservesSyncTimestampWhenEnrichmentHitsDeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	previous := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store := &fakeSyncStore{}
	syncer := &Syncer{
		Store:      store,
		Discoverer: &fakeDiscoveryRunner{results: map[uint]*DiscoveryResult{7: {RepoExists: true}}},
		Enricher:   cancelingEnricher{cancel: cancel, err: fmt.Errorf("boom: %w", context.Canceled)},
		Now:        func() time.Time { return time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC) },
	}

	project := model.Project{Model: gorm.Model{ID: 7}, DotProjectLastSyncedAt: &previous}
	_, _, _, _, err := syncer.syncProject(ctx, project)
	require.Error(t, err)

	persisted, ok := store.persisted[7]
	require.True(t, ok, "discovery data must still be persisted despite the enrichment deadline")
	require.NotNil(t, persisted.patch.DotProjectLastSyncedAt)
	assert.True(t, persisted.patch.DotProjectLastSyncedAt.Equal(previous),
		"a deadline mid-enrichment must not advance the sync timestamp, or ListProjects' anti-starvation order pushes this partially processed project to the back")
}

func TestSyncAllSkipsArchivedAndMaintainerD(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 9, 10, 11, 12, 0, time.UTC)
	store := &fakeSyncStore{
		projects: []model.Project{
			{Model: gorm.Model{ID: 1}, Name: "Project One", Maturity: model.Sandbox, GitHubOrg: "org-one"},
			{Model: gorm.Model{ID: 2}, Name: "maintainer-d", Maturity: model.Graduated, GitHubOrg: "maintainer-d"},
			{Model: gorm.Model{ID: 3}, Name: "Project Archived", Maturity: model.Archived, GitHubOrg: "org-archived"},
		},
	}
	syncer := &Syncer{
		Store: store,
		Discoverer: &fakeDiscoveryRunner{
			results: map[uint]*DiscoveryResult{
				1: {RepoExists: false},
			},
		},
		Now: func() time.Time { return now },
	}

	summary, err := syncer.SyncAll(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 3, summary.Loaded)
	assert.Equal(t, 1, summary.Total)
	assert.Equal(t, 2, summary.Skipped)
	assert.Equal(t, 1, summary.SkippedArchived)
	assert.Equal(t, 1, summary.SkippedExcluded)
	assert.Equal(t, 1, summary.Synced)
	assert.Equal(t, 1, summary.NotFound)
	assert.NotContains(t, store.persisted, uint(2))
	assert.NotContains(t, store.persisted, uint(3))
}

func uintPtr(v uint) *uint {
	return &v
}

// cancelingDiscoveryRunner cancels the run context while "discovering" one
// specific project, simulating the run's time budget expiring mid-project.
type cancelingDiscoveryRunner struct {
	inner    *fakeDiscoveryRunner
	cancelOn uint
	cancel   context.CancelFunc
}

func (c *cancelingDiscoveryRunner) Discover(ctx context.Context, project model.Project) (*DiscoveryResult, error) {
	if project.ID == c.cancelOn {
		c.cancel()
		return nil, ctx.Err()
	}
	return c.inner.Discover(ctx, project)
}

func TestSyncAllStopsCleanlyWhenRunContextExpires(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store := &fakeSyncStore{
		projects: []model.Project{
			{Model: gorm.Model{ID: 1}, Name: "Project One", GitHubOrg: "org-one"},
			{Model: gorm.Model{ID: 2}, Name: "Project Two", GitHubOrg: "org-two"},
			{Model: gorm.Model{ID: 3}, Name: "Project Three", GitHubOrg: "org-three"},
		},
	}
	syncer := &Syncer{
		Store: store,
		Discoverer: &cancelingDiscoveryRunner{
			inner:    &fakeDiscoveryRunner{results: map[uint]*DiscoveryResult{1: {RepoExists: true}}},
			cancelOn: 2,
			cancel:   cancel,
		},
	}

	summary, err := syncer.SyncAll(ctx)
	require.NoError(t, err)
	assert.True(t, summary.StoppedEarly)
	assert.Equal(t, 1, summary.RemainingProjects)
	assert.Equal(t, 1, summary.Synced)
	assert.Equal(t, 0, summary.Errored)
	assert.Len(t, summary.WarningSummaries, 1)
	assert.NotContains(t, store.persisted, uint(2),
		"a project interrupted by the run deadline must not be persisted as errored with a fresh sync timestamp - that would misreport it and push it to the back of the anti-starvation order")
}

// unrelatedFailureDiscoveryRunner cancels the run context while failing one
// project for a reason that has nothing to do with the deadline, simulating
// an ordinary failure racing the clock.
type unrelatedFailureDiscoveryRunner struct {
	failOn uint
	cancel context.CancelFunc
	err    error
}

func (u *unrelatedFailureDiscoveryRunner) Discover(ctx context.Context, project model.Project) (*DiscoveryResult, error) {
	if project.ID == u.failOn {
		u.cancel()
		return nil, u.err
	}
	return nil, ctx.Err()
}

func TestSyncAllRecordsUnrelatedErrorRacingTheDeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store := &fakeSyncStore{
		projects: []model.Project{
			{Model: gorm.Model{ID: 1}, Name: "Project One", GitHubOrg: "org-one"},
			{Model: gorm.Model{ID: 2}, Name: "Project Two", GitHubOrg: "org-two"},
		},
	}
	dbErr := fmt.Errorf("database exploded")
	syncer := &Syncer{
		Store:      store,
		Discoverer: &unrelatedFailureDiscoveryRunner{failOn: 1, cancel: cancel, err: dbErr},
	}

	// Project One fails for a non-deadline reason just as the run context
	// expires: it must be recorded as that project's error, not relabelled
	// as an expected timeout stop. Project Two then surfaces the real
	// context error and stops the run early.
	summary, err := syncer.SyncAll(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, summary.Errored, "the unrelated failure must be recorded as a project error")
	require.Len(t, summary.ErrorSummaries, 1)
	assert.Contains(t, summary.ErrorSummaries[0], "database exploded")
	assert.True(t, summary.StoppedEarly, "the expired run context still stops the run on the next project")
}

func TestSyncAllTreatsRequestTimeoutAsProjectError(t *testing.T) {
	t.Parallel()

	store := &fakeSyncStore{
		projects: []model.Project{
			{Model: gorm.Model{ID: 1}, Name: "Project One", GitHubOrg: "org-one"},
			{Model: gorm.Model{ID: 2}, Name: "Project Two", GitHubOrg: "org-two"},
			{Model: gorm.Model{ID: 3}, Name: "Project Three", GitHubOrg: "org-three"},
		},
	}
	syncer := &Syncer{
		Store: store,
		Discoverer: &fakeDiscoveryRunner{
			results: map[uint]*DiscoveryResult{1: {RepoExists: true}, 3: {RepoExists: true}},
			// A single slow request surfaces as an error that matches
			// errors.Is(err, context.DeadlineExceeded) via
			// http.Client.Timeout; while the run context is healthy this
			// must stay a per-project error, not end the run.
			errors: map[uint]error{2: fmt.Errorf("Get \"https://api.github.example\": %w", context.DeadlineExceeded)},
		},
	}

	summary, err := syncer.SyncAll(context.Background())
	require.NoError(t, err)
	assert.False(t, summary.StoppedEarly)
	assert.Equal(t, 0, summary.RemainingProjects)
	assert.Equal(t, 1, summary.Errored)
	assert.Equal(t, 2, summary.Synced)
}
