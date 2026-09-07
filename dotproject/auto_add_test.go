package dotproject

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"maintainerd/model"
	"maintainerd/provenance"

	"github.com/google/go-github/v55/github"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

type fakeAutoAddStore struct {
	maintainers map[string]model.Maintainer
	linked      map[uint]map[uint]bool
	observed    []model.MaintainerIdentityObservation
	audits      []model.AuditLog
	nextID      uint
}

func newFakeAutoAddStore() *fakeAutoAddStore {
	return &fakeAutoAddStore{
		maintainers: make(map[string]model.Maintainer),
		linked:      make(map[uint]map[uint]bool),
		nextID:      100,
	}
}

func (f *fakeAutoAddStore) GetMaintainerMapByGitHubAccount() (map[string]model.Maintainer, error) {
	result := make(map[string]model.Maintainer, len(f.maintainers))
	for key, maintainer := range f.maintainers {
		result[key] = maintainer
	}
	return result, nil
}

func (f *fakeAutoAddStore) GetMaintainersByProject(projectID uint) ([]model.Maintainer, error) {
	var result []model.Maintainer
	for _, maintainer := range f.maintainers {
		if f.linked[maintainer.ID][projectID] {
			result = append(result, maintainer)
		}
	}
	return result, nil
}

func (f *fakeAutoAddStore) UpsertMaintainerWithIdentity(projectID uint, name, email, githubHandle, company, lfxUserID string) (*model.Maintainer, bool, bool, error) {
	key := NormalizeGitHubHandle(githubHandle)
	maintainer, ok := f.maintainers[key]
	created := false
	if !ok {
		f.nextID++
		maintainer = model.Maintainer{
			Model:            gorm.Model{ID: f.nextID},
			Name:             strings.TrimSpace(name),
			Email:            strings.TrimSpace(email),
			GitHubAccount:    strings.TrimSpace(githubHandle),
			LFXUserID:        strings.TrimSpace(lfxUserID),
			MaintainerStatus: model.ActiveMaintainer,
			Company:          model.Company{Name: company},
		}
		f.maintainers[key] = maintainer
		created = true
	}
	if f.linked[maintainer.ID] == nil {
		f.linked[maintainer.ID] = make(map[uint]bool)
	}
	linked := !f.linked[maintainer.ID][projectID]
	f.linked[maintainer.ID][projectID] = true
	return &maintainer, created, linked, nil
}

func (f *fakeAutoAddStore) UpsertMaintainerIdentityObservation(observation *model.MaintainerIdentityObservation) (*model.MaintainerIdentityObservation, error) {
	f.observed = append(f.observed, *observation)
	return observation, nil
}

func (f *fakeAutoAddStore) AdoptMaintainerIdentityObservations(maintainerID, projectID uint, sourceRef string) (int64, error) {
	var adopted int64
	for i := range f.observed {
		observation := &f.observed[i]
		if observation.MaintainerID == nil && observation.ProjectID != nil && *observation.ProjectID == projectID && observation.SourceRef == sourceRef {
			id := maintainerID
			observation.MaintainerID = &id
			adopted++
		}
	}
	return adopted, nil
}

func (f *fakeAutoAddStore) GetLatestMaintainerIdentityObservation(source string, maintainerID uint) (*model.MaintainerIdentityObservation, error) {
	for i := len(f.observed) - 1; i >= 0; i-- {
		observation := f.observed[i]
		if observation.Source == source && observation.MaintainerID != nil && *observation.MaintainerID == maintainerID {
			copy := observation
			return &copy, nil
		}
	}
	return nil, nil
}

func (f *fakeAutoAddStore) GetLatestMaintainerIdentityObservationByRef(source string, projectID uint, sourceRef string) (*model.MaintainerIdentityObservation, error) {
	for i := len(f.observed) - 1; i >= 0; i-- {
		observation := f.observed[i]
		if observation.Source == source && observation.ProjectID != nil && *observation.ProjectID == projectID && observation.SourceRef == sourceRef {
			copy := observation
			return &copy, nil
		}
	}
	return nil, nil
}

func (f *fakeAutoAddStore) LogAuditEvent(_ *zap.SugaredLogger, event model.AuditLog) error {
	f.audits = append(f.audits, event)
	return nil
}

type fakeLFXResolver struct {
	result LFXIdentityResult
	err    error
	calls  int
}

func (f *fakeLFXResolver) ResolveMaintainerIdentity(_ context.Context, _, _ string) (LFXIdentityResult, error) {
	f.calls++
	return f.result, f.err
}

func TestAutoMaintainerAdderDryRunWritesObservationsButNoMaintainers(t *testing.T) {
	t.Parallel()

	index, err := ParseFoundationMaintainersCSV(strings.NewReader(`,Project,Maintainer Name,Company,Github Name
Graduated,Kubernetes,Alice Example,Acme,AliceExample
`))
	require.NoError(t, err)
	index.SourceURL = "https://github.com/cncf/foundation/blob/abc/project-maintainers.csv"
	index.CommitSHA = "abc"

	store := newFakeAutoAddStore()
	adder := &AutoMaintainerAdder{
		Store:              store,
		Foundation:         index,
		CheckFoundationCSV: true,
		AutoAddMaintainers: false,
	}

	summary, err := adder.ProcessProject(context.Background(), model.Project{Model: gorm.Model{ID: 1}, Name: "Kubernetes"}, &DiscoveryResult{
		MaintainersFile: FileDiscovery{Exists: true, Body: `maintainers:
  - teams:
      - name: project-maintainers
        members:
          - AliceExample
          - missing-handle
`},
	})
	require.NoError(t, err)

	assert.Equal(t, 2, summary.Candidates)
	assert.Equal(t, 1, summary.WouldCreateMaintainers)
	assert.Equal(t, 1, summary.SkippedFoundationMissing)
	assert.Len(t, summary.WouldCreate, 1)
	assert.Equal(t, "Kubernetes", summary.WouldCreate[0].Project)
	assert.Equal(t, "AliceExample", summary.WouldCreate[0].GitHub)
	assert.Empty(t, store.maintainers)
	require.Len(t, store.observed, 4)
	statuses := make(map[string]bool)
	sources := make(map[string]int)
	for _, observation := range store.observed {
		sources[observation.Source]++
		if observation.Source == FoundationCSVSource {
			statuses[observation.MatchStatus] = true
		}
	}
	assert.True(t, statuses["matched"])
	assert.True(t, statuses["unmatched"])
	assert.Equal(t, 2, sources[FoundationCSVSource])
	assert.Equal(t, 2, sources["dot-project"])
}

func TestAutoMaintainerAdderSkipsInvalidMaintainersFile(t *testing.T) {
	t.Parallel()

	store := newFakeAutoAddStore()
	adder := &AutoMaintainerAdder{
		Store:              store,
		CheckFoundationCSV: false,
		AutoAddMaintainers: false,
	}

	summary, err := adder.ProcessProject(context.Background(), model.Project{Model: gorm.Model{ID: 1}, Name: "Kubernetes"}, &DiscoveryResult{
		MaintainersFile: FileDiscovery{Exists: true, Body: `maintainers: []`},
	})
	require.NoError(t, err)

	assert.Equal(t, 1, summary.SkippedInvalidMaintainers)
	assert.Equal(t, 0, summary.Candidates)
	assert.Empty(t, store.maintainers)
	assert.Empty(t, store.observed)
	assert.Empty(t, store.audits)
}

func TestAutoMaintainerAdderDryRunIncludesLFXIdentityInWouldCreateSummary(t *testing.T) {
	t.Parallel()

	index, err := ParseFoundationMaintainersCSV(strings.NewReader(`,Project,Maintainer Name,Company,Github Name
Graduated,Kubernetes,Alice Example,Acme,AliceExample
`))
	require.NoError(t, err)

	store := newFakeAutoAddStore()
	adder := &AutoMaintainerAdder{
		Store:              store,
		Foundation:         index,
		LFX:                &fakeLFXResolver{result: LFXIdentityResult{UserID: "003-test", LFID: "alice-lfid", Name: "Alice LFX", Email: "alice.lfx@example.com", Company: "LFX Co", Confidence: "exact"}},
		CheckFoundationCSV: true,
		AutoAddMaintainers: false,
	}

	summary, err := adder.ProcessProject(context.Background(), model.Project{Model: gorm.Model{ID: 1}, Name: "Kubernetes"}, &DiscoveryResult{
		MaintainersFile: FileDiscovery{Exists: true, Body: `maintainers:
  - teams:
      - name: project-maintainers
        members: [AliceExample]
`},
	})
	require.NoError(t, err)

	require.Len(t, summary.WouldCreate, 1)
	assert.Equal(t, "alice-lfid", summary.WouldCreate[0].LFXID)
	assert.Equal(t, "Alice LFX", summary.WouldCreate[0].Name)
	assert.Equal(t, "LFX Co", summary.WouldCreate[0].Company)
	assert.Equal(t, "alice.lfx@example.com", summary.WouldCreate[0].Email)
}

func TestAutoMaintainerAdderDryRunUsesSavedLFXObservationForWouldCreateSummary(t *testing.T) {
	t.Parallel()

	index, err := ParseFoundationMaintainersCSV(strings.NewReader(`,Project,Maintainer Name,Company,Github Name
Graduated,Kubernetes,Alice Example,Acme,AliceExample
`))
	require.NoError(t, err)

	store := newFakeAutoAddStore()
	projectID := uint(1)
	store.observed = append(store.observed, model.MaintainerIdentityObservation{
		ProjectID:    &projectID,
		Source:       "lfx",
		SourceRef:    "github:aliceexample",
		SourceUserID: "003-test",
		LFID:         "alice-lfid",
		Name:         "Alice LFX",
		CompanyName:  "LFX Co",
		Email:        "alice.lfx@example.com",
		MatchStatus:  "matched",
	})
	adder := &AutoMaintainerAdder{
		Store:              store,
		Foundation:         index,
		CheckFoundationCSV: true,
		AutoAddMaintainers: false,
	}

	summary, err := adder.ProcessProject(context.Background(), model.Project{Model: gorm.Model{ID: projectID}, Name: "Kubernetes"}, &DiscoveryResult{
		MaintainersFile: FileDiscovery{Exists: true, Body: `maintainers:
  - teams:
      - name: project-maintainers
        members: [AliceExample]
`},
	})
	require.NoError(t, err)

	require.Len(t, summary.WouldCreate, 1)
	assert.Equal(t, "alice-lfid", summary.WouldCreate[0].LFXID)
	assert.Equal(t, "Alice LFX", summary.WouldCreate[0].Name)
	assert.Equal(t, "LFX Co", summary.WouldCreate[0].Company)
	assert.Equal(t, "alice.lfx@example.com", summary.WouldCreate[0].Email)
}

func TestAutoMaintainerAdderWriteModeLinksExistingMaintainer(t *testing.T) {
	t.Parallel()

	index, err := ParseFoundationMaintainersCSV(strings.NewReader(`,Project,Maintainer Name,Company,Github Name
Graduated,Kubernetes,Alice Example,Acme,AliceExample
`))
	require.NoError(t, err)

	store := newFakeAutoAddStore()
	existing := model.Maintainer{
		Model:            gorm.Model{ID: 7},
		Name:             "Alice Existing",
		Email:            "alice.lfx@example.com",
		GitHubAccount:    "aliceexample",
		MaintainerStatus: model.ActiveMaintainer,
	}
	store.maintainers["aliceexample"] = existing
	adder := &AutoMaintainerAdder{
		Store:              store,
		Foundation:         index,
		CheckFoundationCSV: true,
		AutoAddMaintainers: true,
	}

	summary, err := adder.ProcessProject(context.Background(), model.Project{Model: gorm.Model{ID: 1}, Name: "Kubernetes"}, &DiscoveryResult{
		MaintainersFile: FileDiscovery{Exists: true, Body: `maintainers:
  - teams:
      - name: project-maintainers
        members: [AliceExample]
`},
	})
	require.NoError(t, err)

	assert.Equal(t, 1, summary.LinkedMaintainers)
	assert.True(t, store.linked[existing.ID][1])
	require.Len(t, store.audits, 1)
	assert.Equal(t, "ADD_DOT_PROJECT_MAINTAINER", store.audits[0].Action)
	assert.Contains(t, store.audits[0].Message, "aliceexample was added")
}

// A maintainer created by auto-add is created *after* its dot-project and
// foundation observations are written, so those rows start with a NULL
// maintainer ID. They must be adopted in the same run, or the evidence that
// justified creating the maintainer is orphaned forever (the observation
// upsert key treats NULL and a concrete maintainer ID as different rows).
func TestAutoMaintainerAdderWriteModeAdoptsObservationsForCreatedMaintainer(t *testing.T) {
	t.Parallel()

	index, err := ParseFoundationMaintainersCSV(strings.NewReader(`,Project,Maintainer Name,Company,Github Name
Graduated,Kubernetes,Alice Example,Acme,AliceExample
`))
	require.NoError(t, err)

	store := newFakeAutoAddStore()
	adder := &AutoMaintainerAdder{
		Store:              store,
		Foundation:         index,
		CheckFoundationCSV: true,
		AutoAddMaintainers: true,
	}

	summary, err := adder.ProcessProject(context.Background(), model.Project{Model: gorm.Model{ID: 1}, Name: "Kubernetes"}, &DiscoveryResult{
		MaintainersFile: FileDiscovery{Exists: true, Body: `maintainers:
  - teams:
      - name: project-maintainers
        members: [AliceExample]
`},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, summary.CreatedMaintainers)

	created, ok := store.maintainers["aliceexample"]
	require.True(t, ok)
	require.NotEmpty(t, store.observed)
	for _, observation := range store.observed {
		require.NotNil(t, observation.MaintainerID, "observation from source %q must be attached to the created maintainer", observation.Source)
		assert.Equal(t, created.ID, *observation.MaintainerID, "observation from source %q", observation.Source)
	}
}

// With the Foundation CSV gate off no lookup ever happens, so writing a
// "matched" foundation-csv row would fabricate evidence that was never
// queried. Only the dot-project observation may be recorded.
func TestAutoMaintainerAdderSkipsFoundationObservationWhenCSVGateOff(t *testing.T) {
	t.Parallel()

	store := newFakeAutoAddStore()
	adder := &AutoMaintainerAdder{
		Store:              store,
		CheckFoundationCSV: false,
		AutoAddMaintainers: false,
	}

	_, err := adder.ProcessProject(context.Background(), model.Project{Model: gorm.Model{ID: 1}, Name: "Kubernetes"}, &DiscoveryResult{
		MaintainersFile: FileDiscovery{Exists: true, Body: `maintainers:
  - teams:
      - name: project-maintainers
        members: [AliceExample]
`},
	})
	require.NoError(t, err)

	require.NotEmpty(t, store.observed)
	for _, observation := range store.observed {
		assert.NotEqual(t, FoundationCSVSource, observation.Source, "no foundation-csv observation may be written when the CSV was never consulted")
	}
}

func TestFoundationCSVPathFollowsConfiguredSource(t *testing.T) {
	t.Parallel()

	custom := &AutoMaintainerAdder{Foundation: &FoundationMaintainerIndex{
		SourceURL: "https://github.com/example-org/foundation/blob/abc123/data/region/maintainers.csv",
	}}
	assert.Equal(t, "data/region/maintainers.csv", custom.foundationCSVPath(),
		"SourceFilePath must follow the configured CSV path, not a hard-coded default")

	unparseable := &AutoMaintainerAdder{Foundation: &FoundationMaintainerIndex{SourceURL: "not-a-blob-url"}}
	assert.Equal(t, "project-maintainers.csv", unparseable.foundationCSVPath())

	assert.Equal(t, "project-maintainers.csv", (&AutoMaintainerAdder{}).foundationCSVPath())
}

// A provenance lookup that errors (transient API failure, expired run
// context) must not produce an observation write at all: the store upsert
// overwrites every provenance column, so persisting the empty result would
// blank evidence a previous healthy run recorded. Legitimately absent
// provenance (no resolver, no line) still writes - that path is covered by
// TestAutoMaintainerAdderDryRunWritesObservationsButNoMaintainers.
func TestAutoMaintainerAdderSkipsObservationWriteWhenProvenanceResolveFails(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"boom"}`, http.StatusBadGateway)
	}))
	defer server.Close()
	client := github.NewClient(nil)
	baseURL, err := url.Parse(server.URL + "/")
	require.NoError(t, err)
	client.BaseURL = baseURL

	index, err := ParseFoundationMaintainersCSV(strings.NewReader(`,Project,Maintainer Name,Company,Github Name
Graduated,Kubernetes,Alice Example,Acme,AliceExample
`))
	require.NoError(t, err)
	index.SourceURL = "https://github.com/cncf/foundation/blob/abc/project-maintainers.csv"
	index.CommitSHA = "abc"

	store := newFakeAutoAddStore()
	adder := &AutoMaintainerAdder{
		Store:              store,
		Foundation:         index,
		CheckFoundationCSV: true,
		AutoAddMaintainers: false,
		Provenance:         provenance.NewResolver(client),
	}

	summary, err := adder.ProcessProject(context.Background(), model.Project{Model: gorm.Model{ID: 1}, Name: "Kubernetes"}, &DiscoveryResult{
		MaintainersFile: FileDiscovery{
			Exists:    true,
			Path:      ".project/maintainers.yaml",
			BlobURL:   "https://github.com/example-org/kubernetes/blob/main/.project/maintainers.yaml",
			CommitSHA: "def456",
			Body: `maintainers:
  - teams:
      - name: project-maintainers
        members:
          - AliceExample
          - missing-handle
`,
		},
	})
	require.NoError(t, err, "a failed provenance lookup is not a project error")
	assert.Equal(t, 2, summary.Candidates)

	// Only missing-handle's foundation-csv "unmatched" row survives: its
	// lookupPerformed=false path never consults the resolver. AliceExample's
	// matched foundation row and both dot-project rows hit the failing
	// resolver and are skipped rather than blanking stored evidence - and
	// each skip must surface as an audit failure, since production loggers
	// are no-op and a silent skip would report a fully successful run.
	assert.Equal(t, 3, summary.AuditFailures)
	require.Len(t, store.observed, 1)
	assert.Equal(t, FoundationCSVSource, store.observed[0].Source)
	assert.Equal(t, "unmatched", store.observed[0].MatchStatus)
	assert.Equal(t, "github:missing-handle", store.observed[0].SourceRef)
}
