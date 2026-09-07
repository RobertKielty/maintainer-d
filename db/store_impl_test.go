package db

import (
	"maintainerd/model"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// setupTestDB creates an in-memory SQLite database for testing
func setupTestDB(t *testing.T) *gorm.DB {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	require.NoError(t, err)

	require.NoError(t, db.SetupJoinTable(&model.Maintainer{}, "Projects", &model.MaintainerProject{}))
	require.NoError(t, db.SetupJoinTable(&model.Project{}, "Maintainers", &model.MaintainerProject{}))

	err = db.AutoMigrate(
		&model.Company{},
		&model.Project{},
		&model.Maintainer{},
		&model.MaintainerProject{},
		&model.DotProjectSyncState{},
		&model.MaintainerIdentityObservation{},
		&model.AuditLog{},
		&model.Service{},
		&model.RemoteTeam{},
		&model.RemoteUser{},
		&model.RemoteTeamUser{},
		&model.ServiceInvitation{},
	)
	require.NoError(t, err)

	return db
}

// seedTestData creates test fixtures in the database
func seedTestData(t *testing.T, db *gorm.DB) (company model.Company, project1, project2 model.Project, maintainer1, maintainer2, maintainer3 model.Maintainer) {
	// Create a company
	company = model.Company{Name: "Test Company"}
	require.NoError(t, db.Create(&company).Error)

	// Create projects
	project1 = model.Project{Name: "kubernetes", Maturity: model.Graduated}
	require.NoError(t, db.Create(&project1).Error)

	project2 = model.Project{Name: "prometheus", Maturity: model.Graduated}
	require.NoError(t, db.Create(&project2).Error)

	// Create maintainers
	maintainer1 = model.Maintainer{
		Name:             "Alice Developer",
		Email:            "alice@example.com",
		GitHubAccount:    "alice",
		MaintainerStatus: model.ActiveMaintainer,
		CompanyID:        &company.ID,
	}
	require.NoError(t, db.Create(&maintainer1).Error)

	maintainer2 = model.Maintainer{
		Name:             "Bob Engineer",
		Email:            "bob@example.com",
		GitHubAccount:    "bob",
		MaintainerStatus: model.ActiveMaintainer,
		CompanyID:        &company.ID,
	}
	require.NoError(t, db.Create(&maintainer2).Error)

	maintainer3 = model.Maintainer{
		Name:             "Charlie Contributor",
		Email:            "charlie@example.com",
		GitHubAccount:    "charlie",
		MaintainerStatus: model.EmeritusMaintainer,
		CompanyID:        &company.ID,
	}
	require.NoError(t, db.Create(&maintainer3).Error)

	// Associate maintainers with projects
	// project1 has maintainer1 and maintainer2
	require.NoError(t, db.Model(&project1).Association("Maintainers").Append(&maintainer1, &maintainer2))

	// project2 has maintainer2 and maintainer3
	require.NoError(t, db.Model(&project2).Association("Maintainers").Append(&maintainer2, &maintainer3))

	return
}

func TestUpsertMaintainerWithIdentity_SetsLFXUserID(t *testing.T) {
	db := setupTestDB(t)
	store := NewSQLStore(db)

	project := model.Project{Name: "kubernetes", Maturity: model.Graduated}
	require.NoError(t, db.Create(&project).Error)

	maintainer, created, linked, err := store.UpsertMaintainerWithIdentity(project.ID, "Alice Example", "alice@example.com", "AliceExample", "Acme", "003-test")
	require.NoError(t, err)
	require.NotNil(t, maintainer)
	assert.True(t, created)
	assert.True(t, linked)
	assert.Equal(t, "003-test", maintainer.LFXUserID)
	assert.Equal(t, "GITHUB_EMAIL_MISSING", maintainer.GitHubEmail)
	require.NotNil(t, maintainer.CompanyID)
	assert.Equal(t, "Acme", maintainer.Company.Name)

	var refreshed model.Maintainer
	require.NoError(t, db.Preload("Company").First(&refreshed, maintainer.ID).Error)
	assert.Equal(t, "003-test", refreshed.LFXUserID)
	assert.Equal(t, "GITHUB_EMAIL_MISSING", refreshed.GitHubEmail)
	require.NotNil(t, refreshed.CompanyID)
	assert.Equal(t, "Acme", refreshed.Company.Name)
}

func TestUpsertMaintainerWithIdentity_IgnoresBlankCompany(t *testing.T) {
	db := setupTestDB(t)
	store := NewSQLStore(db)

	project := model.Project{Name: "kubernetes", Maturity: model.Graduated}
	require.NoError(t, db.Create(&project).Error)

	maintainer, created, linked, err := store.UpsertMaintainerWithIdentity(project.ID, "Alice Example", "alice@example.com", "AliceExample", "   ", "003-test")
	require.NoError(t, err)
	require.NotNil(t, maintainer)
	assert.True(t, created)
	assert.True(t, linked)
	assert.Nil(t, maintainer.CompanyID)

	var companyCount int64
	require.NoError(t, db.Model(&model.Company{}).Count(&companyCount).Error)
	assert.Equal(t, int64(0), companyCount)
}

func TestGetMaintainersByProject(t *testing.T) {
	db := setupTestDB(t)
	company, project1, project2, maintainer1, maintainer2, maintainer3 := seedTestData(t, db)
	store := NewSQLStore(db)

	t.Run("returns maintainers for project with multiple maintainers", func(t *testing.T) {
		maintainers, err := store.GetMaintainersByProject(project1.ID)
		require.NoError(t, err)
		require.Len(t, maintainers, 2)

		// Verify maintainer data
		maintainerIDs := []uint{maintainers[0].ID, maintainers[1].ID}
		assert.Contains(t, maintainerIDs, maintainer1.ID)
		assert.Contains(t, maintainerIDs, maintainer2.ID)

		// Verify Company is preloaded
		for _, m := range maintainers {
			assert.NotNil(t, m.CompanyID)
			assert.Equal(t, company.ID, m.Company.ID)
			assert.Equal(t, "Test Company", m.Company.Name)
		}
	})

	t.Run("returns different maintainers for different project", func(t *testing.T) {
		maintainers, err := store.GetMaintainersByProject(project2.ID)
		require.NoError(t, err)
		require.Len(t, maintainers, 2)

		maintainerIDs := []uint{maintainers[0].ID, maintainers[1].ID}
		assert.Contains(t, maintainerIDs, maintainer2.ID)
		assert.Contains(t, maintainerIDs, maintainer3.ID)
	})

	t.Run("returns empty slice for project with no maintainers", func(t *testing.T) {
		emptyProject := model.Project{Name: "empty-project", Maturity: model.Sandbox}
		require.NoError(t, db.Create(&emptyProject).Error)

		maintainers, err := store.GetMaintainersByProject(emptyProject.ID)
		require.NoError(t, err)
		assert.Empty(t, maintainers)
	})

	t.Run("returns empty slice for non-existent project", func(t *testing.T) {
		maintainers, err := store.GetMaintainersByProject(99999)
		require.Error(t, err)
		assert.Equal(t, ErrProjectNotFound, err)
		assert.Nil(t, maintainers)
	})

	t.Run("maintainers have correct fields populated", func(t *testing.T) {
		maintainers, err := store.GetMaintainersByProject(project1.ID)
		require.NoError(t, err)
		require.NotEmpty(t, maintainers)

		m := maintainers[0]
		assert.NotEmpty(t, m.Name)
		assert.NotEmpty(t, m.Email)
		assert.NotEmpty(t, m.GitHubAccount)
		assert.NotNil(t, m.CompanyID)

		// Projects field should NOT be populated (not preloaded)
		assert.Empty(t, m.Projects)
	})
}

func TestUpdateMaintainerProjectStatus_IsolatedPerProject(t *testing.T) {
	db := setupTestDB(t)
	_, project1, project2, _, maintainer2, _ := seedTestData(t, db)
	store := NewSQLStore(db)

	// maintainer2 belongs to both project1 and project2. Setting their status on
	// project1 alone must not change their status on project2.
	require.NoError(t, store.UpdateMaintainerProjectStatus(maintainer2.ID, project1.ID, model.EmeritusMaintainer))

	statusOnProject1, err := store.GetMaintainerProjectStatus(maintainer2.ID, project1.ID)
	require.NoError(t, err)
	assert.Equal(t, model.EmeritusMaintainer, statusOnProject1)

	statusOnProject2, err := store.GetMaintainerProjectStatus(maintainer2.ID, project2.ID)
	require.NoError(t, err)
	assert.Equal(t, model.ActiveMaintainer, statusOnProject2)
}

func TestUpdateMaintainersProjectStatus_IsolatedPerProject(t *testing.T) {
	db := setupTestDB(t)
	_, project1, project2, maintainer1, maintainer2, _ := seedTestData(t, db)
	store := NewSQLStore(db)

	require.NoError(t, store.UpdateMaintainersProjectStatus([]uint{maintainer1.ID, maintainer2.ID}, project1.ID, model.RetiredMaintainer))

	statuses, err := store.ListMaintainerProjectStatuses(project1.ID)
	require.NoError(t, err)
	assert.Equal(t, model.RetiredMaintainer, statuses[maintainer1.ID])
	assert.Equal(t, model.RetiredMaintainer, statuses[maintainer2.ID])

	statusOnProject2, err := store.GetMaintainerProjectStatus(maintainer2.ID, project2.ID)
	require.NoError(t, err)
	assert.Equal(t, model.ActiveMaintainer, statusOnProject2)
}

func TestListMaintainersActiveOnAnyProject(t *testing.T) {
	db := setupTestDB(t)
	_, project1, _, maintainer1, maintainer2, maintainer3 := seedTestData(t, db)
	store := NewSQLStore(db)

	// maintainer1 is only on project1; archive them there, leaving no active projects.
	require.NoError(t, store.UpdateMaintainerProjectStatus(maintainer1.ID, project1.ID, model.ArchivedMaintainer))

	active, err := store.ListMaintainersActiveOnAnyProject([]uint{maintainer1.ID, maintainer2.ID, maintainer3.ID})
	require.NoError(t, err)
	assert.False(t, active[maintainer1.ID])
	assert.True(t, active[maintainer2.ID])
	// maintainer3 is Active by DB default on project2 even though the deprecated
	// global field was seeded as Emeritus.
	assert.True(t, active[maintainer3.ID])
}

func TestGetMaintainerProjectStatuses(t *testing.T) {
	db := setupTestDB(t)
	_, project1, project2, _, maintainer2, _ := seedTestData(t, db)
	store := NewSQLStore(db)

	require.NoError(t, store.UpdateMaintainerProjectStatus(maintainer2.ID, project1.ID, model.EmeritusMaintainer))

	statuses, err := store.GetMaintainerProjectStatuses(maintainer2.ID)
	require.NoError(t, err)
	assert.Equal(t, model.EmeritusMaintainer, statuses[project1.ID])
	assert.Equal(t, model.ActiveMaintainer, statuses[project2.ID])
}

func TestDotProjectSyncState(t *testing.T) {
	db := setupTestDB(t)
	_, project1, _, _, _, _ := seedTestData(t, db)
	store := NewSQLStore(db)

	t.Run("returns nil when no sync state exists", func(t *testing.T) {
		state, err := store.GetDotProjectSyncState(project1.ID)
		require.NoError(t, err)
		assert.Nil(t, state)
	})

	t.Run("upserts and reloads sync state", func(t *testing.T) {
		now := time.Now().UTC().Truncate(time.Second)
		syncErr := "repo probe failed"
		parseErr := "unsupported schema version"
		state := &model.DotProjectSyncState{
			ProjectID:               project1.ID,
			RepoExists:              true,
			ProjectFileExists:       true,
			MaintainersFileExists:   true,
			SecurityFileExists:      true,
			ContributingFileExists:  false,
			GovernanceFileExists:    true,
			DefaultBranch:           "main",
			MaintainersFilename:     "MAINTAINERS.yaml",
			SchemaVersion:           "1.0.0",
			ImporterVersion:         "dot-project-sync/v1",
			ProjectFileETag:         "\"project-etag\"",
			MaintainersFileETag:     "\"maintainers-etag\"",
			ProjectFileBodyHash:     "abc123",
			MaintainersFileBodyHash: "def456",
			MaintainersFileBody:     strPtr("maintainers:\n  - teams: []\n"),
			SecurityFileBodyHash:    "ghi789",
			GovernanceFileBodyHash:  "jkl012",
			LastCheckedAt:           &now,
			SyncError:               &syncErr,
			ParseError:              &parseErr,
		}

		require.NoError(t, store.UpsertDotProjectSyncState(state))

		reloaded, err := store.GetDotProjectSyncState(project1.ID)
		require.NoError(t, err)
		require.NotNil(t, reloaded)
		assert.True(t, reloaded.RepoExists)
		assert.True(t, reloaded.ProjectFileExists)
		assert.True(t, reloaded.MaintainersFileExists)
		assert.False(t, reloaded.ContributingFileExists)
		assert.Equal(t, "MAINTAINERS.yaml", reloaded.MaintainersFilename)
		assert.Equal(t, "1.0.0", reloaded.SchemaVersion)
		assert.Equal(t, "dot-project-sync/v1", reloaded.ImporterVersion)
		require.NotNil(t, reloaded.MaintainersFileBody)
		assert.Equal(t, "maintainers:\n  - teams: []\n", *reloaded.MaintainersFileBody)
		require.NotNil(t, reloaded.LastCheckedAt)
		assert.True(t, reloaded.LastCheckedAt.Equal(now))
		require.NotNil(t, reloaded.SyncError)
		assert.Equal(t, syncErr, *reloaded.SyncError)
		require.NotNil(t, reloaded.ParseError)
		assert.Equal(t, parseErr, *reloaded.ParseError)
	})
}

func TestUpdateProjectDotProjectMetadata(t *testing.T) {
	db := setupTestDB(t)
	_, project1, _, _, _, _ := seedTestData(t, db)
	store := NewSQLStore(db)

	now := time.Now().UTC().Truncate(time.Second)
	count := uint(7)
	err := store.UpdateProjectDotProjectMetadata(project1.ID, model.Project{
		DotProjectRepoRef:         "https://github.com/example-org/.project",
		DotProjectProjectRef:      "https://github.com/example-org/.project/blob/main/project.yaml",
		DotProjectMaintainerRef:   "https://github.com/example-org/.project/blob/main/MAINTAINERS.yaml",
		DotProjectSecurityRef:     "https://github.com/example-org/.project/blob/main/SECURITY.md",
		DotProjectContributingRef: "https://github.com/example-org/.project/blob/main/CONTRIBUTING.md",
		DotProjectGovernanceRef:   "https://github.com/example-org/.project/blob/main/GOVERNANCE.md",
		DotProjectSchemaVersion:   "1.0.0",
		DotProjectMaintainerCount: &count,
		DotProjectLastSyncedAt:    &now,
		DotProjectAdoptionStatus:  "adopted",
	})
	require.NoError(t, err)

	reloaded, err := store.GetProjectByID(project1.ID)
	require.NoError(t, err)
	require.NotNil(t, reloaded)
	assert.Equal(t, "https://github.com/example-org/.project", reloaded.DotProjectRepoRef)
	assert.Equal(t, "https://github.com/example-org/.project/blob/main/project.yaml", reloaded.DotProjectProjectRef)
	assert.Equal(t, "https://github.com/example-org/.project/blob/main/MAINTAINERS.yaml", reloaded.DotProjectMaintainerRef)
	assert.Equal(t, "1.0.0", reloaded.DotProjectSchemaVersion)
	require.NotNil(t, reloaded.DotProjectMaintainerCount)
	assert.Equal(t, uint(7), *reloaded.DotProjectMaintainerCount)
	require.NotNil(t, reloaded.DotProjectLastSyncedAt)
	assert.True(t, reloaded.DotProjectLastSyncedAt.Equal(now))
	assert.Equal(t, "adopted", reloaded.DotProjectAdoptionStatus)

	err = store.UpdateProjectDotProjectMetadata(project1.ID, model.Project{
		DotProjectAdoptionStatus: "not_found",
	})
	require.NoError(t, err)

	reloaded, err = store.GetProjectByID(project1.ID)
	require.NoError(t, err)
	require.NotNil(t, reloaded)
	assert.Equal(t, "", reloaded.DotProjectRepoRef)
	assert.Nil(t, reloaded.DotProjectMaintainerCount)
	assert.Nil(t, reloaded.DotProjectLastSyncedAt)
	assert.Equal(t, "not_found", reloaded.DotProjectAdoptionStatus)
}

func TestGetProjectsUsingService(t *testing.T) {
	db := setupTestDB(t)
	store := NewSQLStore(db)

	service := model.Service{Name: "FOSSA"}
	require.NoError(t, db.Create(&service).Error)
	otherService := model.Service{Name: "Snyk"}
	require.NoError(t, db.Create(&otherService).Error)

	onboarded := model.Project{Name: "tekton", Maturity: model.Incubating}
	require.NoError(t, db.Create(&onboarded).Error)
	notOnboarded := model.Project{Name: "flux", Maturity: model.Incubating}
	require.NoError(t, db.Create(&notOnboarded).Error)

	require.NoError(t, db.Create(&model.RemoteTeam{
		ProjectID:    onboarded.ID,
		ServiceID:    service.ID,
		RemoteTeamID: 123,
	}).Error)
	require.NoError(t, db.Create(&model.RemoteTeam{
		ProjectID:    notOnboarded.ID,
		ServiceID:    otherService.ID,
		RemoteTeamID: 456,
	}).Error)

	maintainer := model.Maintainer{Name: "Riley Maintainer", Email: "riley@example.com", MaintainerStatus: model.ActiveMaintainer}
	require.NoError(t, db.Create(&maintainer).Error)
	require.NoError(t, db.Create(&model.MaintainerProject{
		MaintainerID: maintainer.ID,
		ProjectID:    onboarded.ID,
		Status:       model.ActiveMaintainer,
	}).Error)

	projects, err := store.GetProjectsUsingService(service.ID)
	require.NoError(t, err)
	require.Len(t, projects, 1)
	assert.Equal(t, onboarded.ID, projects[0].ID)
	require.Len(t, projects[0].Maintainers, 1)
	assert.Equal(t, maintainer.ID, projects[0].Maintainers[0].ID)
}

func TestUpsertServiceInvitation_RestoresSoftDeletedRow(t *testing.T) {
	db := setupTestDB(t)
	store := NewSQLStore(db)

	service := model.Service{Name: "FOSSA"}
	require.NoError(t, db.Create(&service).Error)
	project := model.Project{Name: "tekton", Maturity: model.Incubating}
	require.NoError(t, db.Create(&project).Error)
	maintainer := model.Maintainer{Name: "Riley Maintainer", Email: "riley@example.com"}
	require.NoError(t, db.Create(&maintainer).Error)

	invite := &model.ServiceInvitation{
		ProjectID:    project.ID,
		ServiceID:    service.ID,
		MaintainerID: &maintainer.ID,
		ServiceEmail: "riley@example.com",
		RemoteTeamID: 186460,
		Status:       "pending",
	}
	created, err := store.UpsertServiceInvitation(invite)
	require.NoError(t, err)
	require.NoError(t, db.Delete(created).Error)

	var deletedCount int64
	require.NoError(t, db.Unscoped().Model(&model.ServiceInvitation{}).
		Where("id = ? AND deleted_at IS NOT NULL", created.ID).Count(&deletedCount).Error)
	require.Equal(t, int64(1), deletedCount, "row must be soft-deleted before the re-upsert")

	rediscovered := &model.ServiceInvitation{
		ProjectID:    project.ID,
		ServiceID:    service.ID,
		MaintainerID: &maintainer.ID,
		ServiceEmail: "riley@example.com",
		RemoteTeamID: 186460,
		Status:       "pending",
	}
	_, err = store.UpsertServiceInvitation(rediscovered)
	require.NoError(t, err)

	invites, err := store.ListServiceInvitations(project.ID, service.ID)
	require.NoError(t, err)
	require.Len(t, invites, 1, "re-upserting over a soft-deleted invite must make it visible again")
	assert.Equal(t, "pending", invites[0].Status)
}

func TestUpsertMaintainer_FillsMissingFields(t *testing.T) {
	db := setupTestDB(t)
	store := NewSQLStore(db)

	project := model.Project{Name: "cedar", Maturity: model.Sandbox}
	require.NoError(t, db.Create(&project).Error)

	// Existing maintainer with missing name and GitHub account.
	existing := model.Maintainer{
		Name:             "",
		Email:            "adrian@example.com",
		GitHubAccount:    "GITHUB_MISSING",
		MaintainerStatus: model.ActiveMaintainer,
	}
	require.NoError(t, db.Create(&existing).Error)

	maintainer, err := store.UpsertMaintainer(project.ID, "Adrian Palacios", "adrian@example.com", "adpaco-aws", "Amazon")
	require.NoError(t, err)
	require.NotNil(t, maintainer)
	assert.Equal(t, existing.ID, maintainer.ID)

	var refreshed model.Maintainer
	require.NoError(t, db.First(&refreshed, existing.ID).Error)
	assert.Equal(t, "Adrian Palacios", refreshed.Name)
	assert.Equal(t, "adpaco-aws", refreshed.GitHubAccount)
	assert.Equal(t, "adrian@example.com", refreshed.Email)
	assert.NotNil(t, refreshed.CompanyID)
	assert.NotEqual(t, uint(0), *refreshed.CompanyID)

	var count int64
	require.NoError(t, db.Model(&model.MaintainerProject{}).
		Where("maintainer_id = ? AND project_id = ?", existing.ID, project.ID).
		Count(&count).Error)
	assert.Equal(t, int64(1), count)
}

func TestUpsertRemoteUser(t *testing.T) {
	db := setupTestDB(t)
	store := NewSQLStore(db)

	service := model.Service{Name: "FOSSA"}
	require.NoError(t, db.Create(&service).Error)

	user := &model.RemoteUser{
		ServiceID:    service.ID,
		RemoteUserID: 101,
		ServiceEmail: "alice@example.com",
		RemoteRef:    "alice-ref",
	}
	created, err := store.UpsertRemoteUser(user)
	require.NoError(t, err)
	require.NotNil(t, created)

	userUpdate := &model.RemoteUser{
		ServiceID:    service.ID,
		RemoteUserID: 101,
		ServiceEmail: "alice+updated@example.com",
		RemoteRef:    "alice-ref-updated",
	}
	updated, err := store.UpsertRemoteUser(userUpdate)
	require.NoError(t, err)
	require.NotNil(t, updated)
	assert.Equal(t, "alice+updated@example.com", updated.ServiceEmail)
	assert.Equal(t, "alice-ref-updated", updated.RemoteRef)

	var count int64
	require.NoError(t, db.Model(&model.RemoteUser{}).Count(&count).Error)
	assert.Equal(t, int64(1), count)
}

func TestUpsertRemoteUserTeam(t *testing.T) {
	db := setupTestDB(t)
	store := NewSQLStore(db)

	service := model.Service{Name: "FOSSA"}
	require.NoError(t, db.Create(&service).Error)
	project := model.Project{Name: "kubernetes", Maturity: model.Sandbox}
	require.NoError(t, db.Create(&project).Error)
	maintainer := model.Maintainer{
		Name:             "Alice Developer",
		Email:            "alice@example.com",
		GitHubAccount:    "alice",
		MaintainerStatus: model.ActiveMaintainer,
	}
	require.NoError(t, db.Create(&maintainer).Error)

	remoteTeam := model.RemoteTeam{
		ServiceID:      service.ID,
		ProjectID:      project.ID,
		RemoteTeamID:   999001,
		RemoteTeamName: strPtr("Team Demo"),
	}
	require.NoError(t, db.Create(&remoteTeam).Error)

	remoteUser := model.RemoteUser{
		ServiceID:    service.ID,
		RemoteUserID: 101,
		ServiceEmail: "alice@example.com",
		RemoteRef:    "alice-ref",
	}
	require.NoError(t, db.Create(&remoteUser).Error)

	link := &model.RemoteTeamUser{
		ServiceID:    service.ID,
		TeamID:       remoteTeam.ID,
		UserID:       remoteUser.ID,
		MaintainerID: &maintainer.ID,
	}
	created, err := store.UpsertRemoteUserTeam(link)
	require.NoError(t, err)
	require.NotNil(t, created)

	updatedLink := &model.RemoteTeamUser{
		ServiceID:    service.ID,
		TeamID:       remoteTeam.ID,
		UserID:       remoteUser.ID,
		MaintainerID: &maintainer.ID,
	}
	updated, err := store.UpsertRemoteUserTeam(updatedLink)
	require.NoError(t, err)
	require.NotNil(t, updated)

	var count int64
	require.NoError(t, db.Model(&model.RemoteTeamUser{}).Count(&count).Error)
	assert.Equal(t, int64(1), count)
}

func strPtr(value string) *string {
	return &value
}

// The update branch of the upsert must carry every column a caller can set;
// dropping fields there silently freezes existing production rows at their
// pre-migration values while fresh rows (which go through Create) look fine.
func TestUpsertMaintainerIdentityObservationUpdatesProfileAndProvenanceFields(t *testing.T) {
	db := setupTestDB(t)
	store := NewSQLStore(db)

	maintainerID := uint(1)
	projectID := uint(1)
	created, err := store.UpsertMaintainerIdentityObservation(&model.MaintainerIdentityObservation{
		MaintainerID: &maintainerID,
		ProjectID:    &projectID,
		Source:       "lfx",
		SourceRef:    "github:example-handle",
		SourceUserID: "user-1",
		MatchStatus:  "matched",
		Confidence:   "weak",
		ObservedAt:   time.Now().UTC(),
	})
	require.NoError(t, err)

	lastModified := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	identityCount := 2
	updated, err := store.UpsertMaintainerIdentityObservation(&model.MaintainerIdentityObservation{
		MaintainerID:         &maintainerID,
		ProjectID:            &projectID,
		Source:               "lfx",
		SourceRef:            "github:example-handle",
		SourceUserID:         "user-1",
		MatchStatus:          "chosen",
		Confidence:           "strong",
		ObservedAt:           time.Now().UTC(),
		SourceUserType:       "contact",
		SourceGitHubID:       "example-handle",
		SourceLastModifiedAt: &lastModified,
		IdentityCount:        &identityCount,
		SourceFilePath:       "maintainers.yaml",
		SourceLine:           12,
		SourceCommitSHA:      "abc123",
		SourceLineURL:        "https://github.com/example-org/.project/blob/abc123/maintainers.yaml#L12",
		SourcePRNumber:       42,
		SourcePRURL:          "https://github.com/example-org/.project/pull/42",
		SourceReviewState:    "approved",
	})
	require.NoError(t, err)

	assert.Equal(t, created.ID, updated.ID, "second upsert must update the existing row, not create a new one")
	assert.Equal(t, "chosen", updated.MatchStatus)
	assert.Equal(t, "contact", updated.SourceUserType)
	assert.Equal(t, "example-handle", updated.SourceGitHubID)
	require.NotNil(t, updated.SourceLastModifiedAt)
	assert.True(t, updated.SourceLastModifiedAt.Equal(lastModified))
	require.NotNil(t, updated.IdentityCount, "a measured identity count must persist; NULL means never measured")
	assert.Equal(t, 2, *updated.IdentityCount)
	assert.Equal(t, "maintainers.yaml", updated.SourceFilePath)
	assert.Equal(t, 12, updated.SourceLine)
	assert.Equal(t, "abc123", updated.SourceCommitSHA)
	assert.Equal(t, "https://github.com/example-org/.project/blob/abc123/maintainers.yaml#L12", updated.SourceLineURL)
	assert.Equal(t, 42, updated.SourcePRNumber)
	assert.Equal(t, "https://github.com/example-org/.project/pull/42", updated.SourcePRURL)
	assert.Equal(t, "approved", updated.SourceReviewState)
}

// An observation recorded before its maintainer existed carries a NULL
// maintainer_id. A later upsert for the same source/project/ref that does
// know the maintainer must adopt that row, not insert a duplicate next to it.
func TestUpsertMaintainerIdentityObservationAdoptsOrphanRow(t *testing.T) {
	db := setupTestDB(t)
	store := NewSQLStore(db)

	projectID := uint(7)
	orphan, err := store.UpsertMaintainerIdentityObservation(&model.MaintainerIdentityObservation{
		ProjectID:   &projectID,
		Source:      "dot-project",
		SourceRef:   "github:example-handle",
		MatchStatus: "matched",
		ObservedAt:  time.Now().UTC(),
	})
	require.NoError(t, err)
	require.Nil(t, orphan.MaintainerID)

	maintainerID := uint(31)
	adopted, err := store.UpsertMaintainerIdentityObservation(&model.MaintainerIdentityObservation{
		MaintainerID: &maintainerID,
		ProjectID:    &projectID,
		Source:       "dot-project",
		SourceRef:    "github:example-handle",
		MatchStatus:  "matched",
		ObservedAt:   time.Now().UTC(),
	})
	require.NoError(t, err)
	assert.Equal(t, orphan.ID, adopted.ID, "upsert must adopt the orphan row, not create a duplicate")
	require.NotNil(t, adopted.MaintainerID)
	assert.Equal(t, maintainerID, *adopted.MaintainerID)

	var count int64
	require.NoError(t, db.Model(&model.MaintainerIdentityObservation{}).Count(&count).Error)
	assert.Equal(t, int64(1), count)
}

func TestUpsertMaintainerIdentityObservationDoesNotAdoptOrphanForDifferentHandleSharingProfile(t *testing.T) {
	db := setupTestDB(t)
	store := NewSQLStore(db)

	// Two GitHub handles resolved to the same LFX profile in the same
	// project - an orphan row for the first handle must not be adopted by
	// the second handle's upsert just because source_user_id matches.
	projectID := uint(7)
	orphan, err := store.UpsertMaintainerIdentityObservation(&model.MaintainerIdentityObservation{
		ProjectID:    &projectID,
		Source:       "lfx",
		SourceRef:    "github:handle-one",
		SourceUserID: "sfid-shared",
		MatchStatus:  "matched",
		ObservedAt:   time.Now().UTC(),
	})
	require.NoError(t, err)
	require.Nil(t, orphan.MaintainerID)

	maintainerID := uint(31)
	adopted, err := store.UpsertMaintainerIdentityObservation(&model.MaintainerIdentityObservation{
		MaintainerID: &maintainerID,
		ProjectID:    &projectID,
		Source:       "lfx",
		SourceRef:    "github:handle-two",
		SourceUserID: "sfid-shared",
		MatchStatus:  "matched",
		ObservedAt:   time.Now().UTC(),
	})
	require.NoError(t, err)
	assert.NotEqual(t, orphan.ID, adopted.ID, "upsert must not adopt a different handle's orphan row")

	var refetchedOrphan model.MaintainerIdentityObservation
	require.NoError(t, db.First(&refetchedOrphan, orphan.ID).Error)
	assert.Nil(t, refetchedOrphan.MaintainerID, "the first handle's orphan row must remain unadopted")

	var count int64
	require.NoError(t, db.Model(&model.MaintainerIdentityObservation{}).Count(&count).Error)
	assert.Equal(t, int64(2), count, "each handle must keep its own row")
}

func TestAdoptMaintainerIdentityObservations(t *testing.T) {
	db := setupTestDB(t)
	store := NewSQLStore(db)

	projectID := uint(7)
	for _, source := range []string{"dot-project", "foundation-csv"} {
		_, err := store.UpsertMaintainerIdentityObservation(&model.MaintainerIdentityObservation{
			ProjectID:   &projectID,
			Source:      source,
			SourceRef:   "github:example-handle",
			MatchStatus: "matched",
			ObservedAt:  time.Now().UTC(),
		})
		require.NoError(t, err)
	}
	_, err := store.UpsertMaintainerIdentityObservation(&model.MaintainerIdentityObservation{
		ProjectID:   &projectID,
		Source:      "dot-project",
		SourceRef:   "github:other-handle",
		MatchStatus: "matched",
		ObservedAt:  time.Now().UTC(),
	})
	require.NoError(t, err)

	adopted, err := store.AdoptMaintainerIdentityObservations(31, projectID, "github:example-handle")
	require.NoError(t, err)
	assert.Equal(t, int64(2), adopted, "both sources for the handle must be adopted")

	var remaining int64
	require.NoError(t, db.Model(&model.MaintainerIdentityObservation{}).Where("maintainer_id IS NULL").Count(&remaining).Error)
	assert.Equal(t, int64(1), remaining, "the unrelated handle's row must stay unadopted")
}

func TestRetireStaleMaintainerIdentityObservationsDeletesProfilesAbsentFromLatestLookup(t *testing.T) {
	db := setupTestDB(t)
	store := NewSQLStore(db)

	projectID := uint(7)
	older := time.Now().UTC().Add(-time.Hour)
	for _, userID := range []string{"sfid-a", "sfid-b"} {
		_, err := store.UpsertMaintainerIdentityObservation(&model.MaintainerIdentityObservation{
			ProjectID:    &projectID,
			Source:       "lfx",
			SourceRef:    "github:example-handle",
			SourceUserID: userID,
			MatchStatus:  "duplicate",
			ObservedAt:   older,
		})
		require.NoError(t, err)
	}
	// An unmatched row (no SourceUserID) for the same candidate must never be
	// touched by retirement - it isn't part of the profile set.
	_, err := store.UpsertMaintainerIdentityObservation(&model.MaintainerIdentityObservation{
		ProjectID:   &projectID,
		Source:      "lfx",
		SourceRef:   "github:example-handle",
		MatchStatus: "unmatched",
		ObservedAt:  older,
	})
	require.NoError(t, err)

	now := time.Now().UTC()
	deleted, err := store.RetireStaleMaintainerIdentityObservations("lfx", &projectID, "github:example-handle", []string{"sfid-a"}, now)
	require.NoError(t, err)
	assert.Equal(t, int64(1), deleted, "only the profile absent from the keep list must be retired")

	var remaining []model.MaintainerIdentityObservation
	require.NoError(t, db.Where("source = ? AND source_ref = ?", "lfx", "github:example-handle").Find(&remaining).Error)
	require.Len(t, remaining, 2, "the kept profile and the unmatched row must survive")
	var sourceUserIDs []string
	for _, obs := range remaining {
		sourceUserIDs = append(sourceUserIDs, obs.SourceUserID)
	}
	assert.Contains(t, sourceUserIDs, "sfid-a")
	assert.Contains(t, sourceUserIDs, "")
	assert.NotContains(t, sourceUserIDs, "sfid-b")
}

func TestRetireStaleMaintainerIdentityObservationsWithEmptyKeepListRetiresEverything(t *testing.T) {
	db := setupTestDB(t)
	store := NewSQLStore(db)

	projectID := uint(9)
	older := time.Now().UTC().Add(-time.Hour)
	for _, userID := range []string{"sfid-a", "sfid-b"} {
		_, err := store.UpsertMaintainerIdentityObservation(&model.MaintainerIdentityObservation{
			ProjectID:    &projectID,
			Source:       "lfx",
			SourceRef:    "github:example-handle",
			SourceUserID: userID,
			MatchStatus:  "duplicate",
			ObservedAt:   older,
		})
		require.NoError(t, err)
	}

	now := time.Now().UTC()
	deleted, err := store.RetireStaleMaintainerIdentityObservations("lfx", &projectID, "github:example-handle", nil, now)
	require.NoError(t, err)
	assert.Equal(t, int64(2), deleted, "an empty keep list (the lookup found no profiles) must retire every prior profile row")
}
