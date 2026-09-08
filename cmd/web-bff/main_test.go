package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"maintainerd/db"
	"maintainerd/lfx"
	"maintainerd/model"

	"github.com/google/go-github/v55/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func setupPostgresTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	testcontainers.SkipIfProviderIsNotHealthy(t)
	ctx := context.Background()
	container, err := postgres.Run(
		ctx,
		"postgres:16-alpine",
		postgres.WithDatabase("maintainerd_test"),
		postgres.WithUsername("maintainerd"),
		postgres.WithPassword("maintainerd"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithStartupTimeout(30*time.Second),
		),
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = container.Terminate(ctx)
	})

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	var gormDB *gorm.DB
	var lastErr error
	for attempt := 0; attempt < 10; attempt++ {
		gormDB, lastErr = db.OpenGorm("postgres", dsn, &gorm.Config{
			Logger:                                   logger.Default.LogMode(logger.Silent),
			DisableForeignKeyConstraintWhenMigrating: true,
		})
		if lastErr != nil {
			time.Sleep(time.Duration(attempt+1) * 200 * time.Millisecond)
			continue
		}
		sqlDB, err := gormDB.DB()
		if err == nil {
			pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			err = sqlDB.PingContext(pingCtx)
			cancel()
		}
		if err == nil {
			lastErr = nil
			break
		}
		lastErr = err
		time.Sleep(time.Duration(attempt+1) * 200 * time.Millisecond)
	}
	require.NoError(t, lastErr)

	err = gormDB.AutoMigrate(
		&model.AuditLog{},
		&model.Company{},
		&model.Foundation{},
		&model.Project{},
		&model.Maintainer{},
		&model.StaffMember{},
		&model.FoundationOfficer{},
		&model.Collaborator{},
		&model.MaintainerProject{},
		&model.MaintainerIdentityObservation{},
		&model.Service{},
		&model.RemoteTeam{},
		&model.RemoteUser{},
		&model.RemoteTeamUser{},
		&model.ServiceInvitation{},
		&model.DotProjectSyncState{},
	)
	require.NoError(t, err)

	return gormDB
}

func githubTestClient(t *testing.T, handler http.Handler) *github.Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client := github.NewClient(server.Client())
	baseURL, err := url.Parse(server.URL + "/")
	require.NoError(t, err)
	client.BaseURL = baseURL
	return client
}

func performMaintainerGet(t *testing.T, s *server, maintainerID uint, sessionID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/maintainers/%d", maintainerID), nil)
	req.AddCookie(&http.Cookie{Name: s.cookieName, Value: sessionID})
	rec := httptest.NewRecorder()
	handler := s.requireSession(http.HandlerFunc(s.handleMaintainer))
	handler.ServeHTTP(rec, req)
	return rec
}

func performMaintainerAction(t *testing.T, s *server, maintainerID uint, action string, sessionID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/maintainers/%d/services/fossa/%s", maintainerID, action), nil)
	req.AddCookie(&http.Cookie{Name: s.cookieName, Value: sessionID})
	rec := httptest.NewRecorder()
	handler := s.requireSession(http.HandlerFunc(s.handleMaintainer))
	handler.ServeHTTP(rec, req)
	return rec
}

func TestMaintainerEmailRedaction(t *testing.T) {
	dbConn := setupPostgresTestDB(t)
	store := db.NewSQLStore(dbConn)
	now := time.Now()

	company := model.Company{Name: "Test Co"}
	require.NoError(t, dbConn.Create(&company).Error)

	project := model.Project{Name: "Project A", Maturity: model.Sandbox}
	require.NoError(t, dbConn.Create(&project).Error)

	alice := model.Maintainer{
		Name:             "Alice Example",
		Email:            "alice@example.org",
		GitHubAccount:    "alice-example",
		GitHubEmail:      "alice@github.example",
		MaintainerStatus: model.ActiveMaintainer,
		CompanyID:        &company.ID,
	}
	require.NoError(t, dbConn.Create(&alice).Error)

	bob := model.Maintainer{
		Name:             "Bob Example",
		Email:            "bob@example.org",
		GitHubAccount:    "bob-example",
		GitHubEmail:      "bob@github.example",
		MaintainerStatus: model.ActiveMaintainer,
		CompanyID:        &company.ID,
	}
	require.NoError(t, dbConn.Create(&bob).Error)

	require.NoError(t, dbConn.Model(&project).Association("Maintainers").Append(&alice, &bob))

	staff := model.StaffMember{
		Name:          "Staff Tester",
		GitHubAccount: "staff-tester",
		Email:         "staff@example.org",
	}
	require.NoError(t, dbConn.Create(&staff).Error)

	s := &server{
		store:      store,
		sessions:   newSessionStore(log.New(io.Discard, "", 0)),
		cookieName: defaultSessionCookieName,
		logger:     log.New(io.Discard, "", 0),
	}

	staffSessionID := "staff-session"
	s.sessions.Set(session{
		ID:        staffSessionID,
		Login:     staff.GitHubAccount,
		Role:      roleStaff,
		CreatedAt: now,
		ExpiresAt: now.Add(time.Hour),
	})

	aliceSessionID := "alice-session"
	s.sessions.Set(session{
		ID:        aliceSessionID,
		Login:     alice.GitHubAccount,
		Role:      roleMaintainer,
		CreatedAt: now,
		ExpiresAt: now.Add(time.Hour),
	})

	t.Run("staff sees all maintainer emails", func(t *testing.T) {
		rec := performMaintainerGet(t, s, bob.ID, staffSessionID)
		require.Equal(t, http.StatusOK, rec.Code)
		var response maintainerDetailResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&response))
		assert.Equal(t, bob.Email, response.Email)
		assert.Equal(t, bob.GitHubEmail, response.GitHubEmail)
	})

	t.Run("maintainer sees own email", func(t *testing.T) {
		rec := performMaintainerGet(t, s, alice.ID, aliceSessionID)
		require.Equal(t, http.StatusOK, rec.Code)
		var response maintainerDetailResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&response))
		assert.Equal(t, alice.Email, response.Email)
		assert.Equal(t, alice.GitHubEmail, response.GitHubEmail)
	})

	t.Run("maintainer cannot see other maintainer email", func(t *testing.T) {
		rec := performMaintainerGet(t, s, bob.ID, aliceSessionID)
		require.Equal(t, http.StatusOK, rec.Code)
		var response maintainerDetailResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&response))
		assert.Empty(t, response.Email)
		assert.Empty(t, response.GitHubEmail)
	})
}

func TestMaintainerDetailSurfacesLinkedLFXProfile(t *testing.T) {
	dbConn := setupPostgresTestDB(t)
	store := db.NewSQLStore(dbConn)
	now := time.Now()

	project := model.Project{Name: "LFX Linked Project", Maturity: model.Sandbox}
	require.NoError(t, dbConn.Create(&project).Error)

	// The maintainer record carries the linked profile even though no lfx
	// identity observation corroborates it - the observation rows can stay
	// stale-unmatched indefinitely (enrichment skips maintainers that already
	// have any lfx row), so the response must expose the record's own link.
	maintainer := model.Maintainer{
		Name:             "Carol Example",
		Email:            "carol@example.org",
		GitHubAccount:    "carol-example",
		MaintainerStatus: model.ActiveMaintainer,
		LFXUserID:        "  lf-carol-profile-id  ",
	}
	require.NoError(t, dbConn.Create(&maintainer).Error)
	require.NoError(t, dbConn.Model(&project).Association("Maintainers").Append(&maintainer))

	staff := model.StaffMember{
		Name:          "Staff Tester",
		GitHubAccount: "staff-tester",
		Email:         "staff@example.org",
	}
	require.NoError(t, dbConn.Create(&staff).Error)

	s := &server{
		store:      store,
		sessions:   newSessionStore(log.New(io.Discard, "", 0)),
		cookieName: defaultSessionCookieName,
		logger:     log.New(io.Discard, "", 0),
	}
	staffSessionID := "staff-session"
	s.sessions.Set(session{
		ID:        staffSessionID,
		Login:     staff.GitHubAccount,
		Role:      roleStaff,
		CreatedAt: now,
		ExpiresAt: now.Add(time.Hour),
	})

	rec := performMaintainerGet(t, s, maintainer.ID, staffSessionID)
	require.Equal(t, http.StatusOK, rec.Code)
	var response maintainerDetailResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&response))
	assert.Equal(t, "lf-carol-profile-id", response.LFXUserID)
}

func TestMaintainerCanAccessAllProjectsAndMaintainers(t *testing.T) {
	dbConn := setupPostgresTestDB(t)
	store := db.NewSQLStore(dbConn)
	now := time.Now()

	company := model.Company{Name: "Test Co"}
	require.NoError(t, dbConn.Create(&company).Error)

	projectA := model.Project{Name: "Project A", Maturity: model.Sandbox}
	projectB := model.Project{Name: "Project B", Maturity: model.Graduated}
	require.NoError(t, dbConn.Create(&projectA).Error)
	require.NoError(t, dbConn.Create(&projectB).Error)

	alice := model.Maintainer{
		Name:             "Alice Example",
		Email:            "alice@example.org",
		GitHubAccount:    "alice-example",
		MaintainerStatus: model.ActiveMaintainer,
		CompanyID:        &company.ID,
	}
	bob := model.Maintainer{
		Name:             "Bob Example",
		Email:            "bob@example.org",
		GitHubAccount:    "bob-example",
		MaintainerStatus: model.ActiveMaintainer,
		CompanyID:        &company.ID,
	}
	require.NoError(t, dbConn.Create(&alice).Error)
	require.NoError(t, dbConn.Create(&bob).Error)
	require.NoError(t, dbConn.Model(&projectA).Association("Maintainers").Append(&alice))

	s := &server{
		store:      store,
		sessions:   newSessionStore(log.New(io.Discard, "", 0)),
		cookieName: defaultSessionCookieName,
		logger:     log.New(io.Discard, "", 0),
		githubClient: func(context.Context, string) *github.Client {
			return githubTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/repos/project-cache/.project/pulls", r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`[]`))
			}))
		},
	}

	maintainerSessionID := "alice-session"
	s.sessions.Set(session{
		ID:        maintainerSessionID,
		Login:     alice.GitHubAccount,
		Role:      roleMaintainer,
		CreatedAt: now,
		ExpiresAt: now.Add(time.Hour),
	})

	t.Run("maintainer can access any project", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/projects/%d", projectB.ID), nil)
		req.AddCookie(&http.Cookie{Name: s.cookieName, Value: maintainerSessionID})
		rec := httptest.NewRecorder()
		handler := s.requireSession(http.HandlerFunc(s.handleProject))
		handler.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code)
	})

	t.Run("maintainer can list all projects", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/projects", nil)
		req.AddCookie(&http.Cookie{Name: s.cookieName, Value: maintainerSessionID})
		rec := httptest.NewRecorder()
		handler := s.requireSession(http.HandlerFunc(s.handleProjects))
		handler.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code)
		var response projectsResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&response))
		assert.GreaterOrEqual(t, response.Total, int64(2))
	})

	t.Run("maintainer can access other maintainer profiles", func(t *testing.T) {
		rec := performMaintainerGet(t, s, bob.ID, maintainerSessionID)
		require.Equal(t, http.StatusOK, rec.Code)
	})
}

func TestProjectDetailIncludesDotProjectMaintainerCache(t *testing.T) {
	dbConn := setupPostgresTestDB(t)
	store := db.NewSQLStore(dbConn)
	now := time.Now().UTC().Truncate(time.Second)

	staff := model.StaffMember{
		Name:          "Staff Tester",
		GitHubAccount: "staff-tester",
		Email:         "staff@example.org",
	}
	require.NoError(t, dbConn.Create(&staff).Error)

	project := model.Project{
		Name:                    "Project Cache",
		Maturity:                model.Graduated,
		GitHubOrg:               "project-cache",
		DotProjectRepoRef:       "https://github.com/project-cache/.project",
		DotProjectMaintainerRef: "https://github.com/project-cache/.project/blob/main/Maintainers.YAML",
	}
	require.NoError(t, dbConn.Create(&project).Error)

	body := "maintainers:\n  - teams:\n      - name: project-maintainers\n        members:\n          - alice-example\n"
	syncState := model.DotProjectSyncState{
		ProjectID:               project.ID,
		RepoExists:              true,
		MaintainersFileExists:   true,
		MaintainersFilename:     "Maintainers.YAML",
		MaintainersFileETag:     "\"maintainers-etag\"",
		MaintainersFileBodyHash: "abc123def456",
		MaintainersFileBody:     &body,
		LastCheckedAt:           &now,
	}
	require.NoError(t, dbConn.Create(&syncState).Error)

	s := &server{
		store:      store,
		sessions:   newSessionStore(log.New(io.Discard, "", 0)),
		cookieName: defaultSessionCookieName,
		logger:     log.New(io.Discard, "", 0),
		githubClient: func(context.Context, string) *github.Client {
			return githubTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/repos/project-cache/.project/pulls", r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`[]`))
			}))
		},
	}

	staffSessionID := "staff-session"
	s.sessions.Set(session{
		ID:        staffSessionID,
		Login:     staff.GitHubAccount,
		Role:      roleStaff,
		CreatedAt: now,
		ExpiresAt: now.Add(time.Hour),
	})

	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/projects/%d", project.ID), nil)
	req.AddCookie(&http.Cookie{Name: s.cookieName, Value: staffSessionID})
	rec := httptest.NewRecorder()
	handler := s.requireSession(http.HandlerFunc(s.handleProject))
	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var response projectDetailResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&response))
	require.NotNil(t, response.DotProjectSyncState)
	assert.Equal(t, "Maintainers.YAML", response.DotProjectSyncState.MaintainersFilename)
	require.NotNil(t, response.DotProjectMaintainerCache)
	assert.Equal(t, "Maintainers.YAML", response.DotProjectMaintainerCache.Filename)
	assert.Equal(t, "\"maintainers-etag\"", response.DotProjectMaintainerCache.ETag)
	assert.Equal(t, "abc123def456", response.DotProjectMaintainerCache.BodyHash)
	assert.Equal(t, body, response.DotProjectMaintainerCache.Body)
	require.NotNil(t, response.DotProjectMaintainerCache.LastCheckedAt)
	assert.True(t, response.DotProjectMaintainerCache.LastCheckedAt.Equal(now))
}

func TestProjectDetailGeneratesMaintainersYamlWhenNoDotProjectFile(t *testing.T) {
	dbConn := setupPostgresTestDB(t)
	store := db.NewSQLStore(dbConn)
	now := time.Now().UTC().Truncate(time.Second)

	staff := model.StaffMember{
		Name:          "Staff Tester",
		GitHubAccount: "staff-tester",
		Email:         "staff@example.org",
	}
	require.NoError(t, dbConn.Create(&staff).Error)

	project := model.Project{
		Name:                     "Project Nofile",
		Maturity:                 model.Sandbox,
		GitHubOrg:                "project-nofile",
		DotProjectAdoptionStatus: "not_found",
	}
	require.NoError(t, dbConn.Create(&project).Error)

	alice := model.Maintainer{
		Name:          "Alice Example",
		Email:         "alice@example.org",
		GitHubAccount: "alice-example",
	}
	require.NoError(t, dbConn.Create(&alice).Error)

	archived := model.Maintainer{
		Name:          "Departed Maintainer",
		Email:         "departed@example.org",
		GitHubAccount: "departed-example",
	}
	require.NoError(t, dbConn.Create(&archived).Error)

	require.NoError(t, dbConn.Model(&project).Association("Maintainers").Append(&alice, &archived))
	require.NoError(t, store.UpdateMaintainerProjectStatus(archived.ID, project.ID, model.ArchivedMaintainer))

	s := &server{
		store:      store,
		sessions:   newSessionStore(log.New(io.Discard, "", 0)),
		cookieName: defaultSessionCookieName,
		logger:     log.New(io.Discard, "", 0),
	}

	staffSessionID := "staff-session"
	s.sessions.Set(session{
		ID:        staffSessionID,
		Login:     staff.GitHubAccount,
		Role:      roleStaff,
		CreatedAt: now,
		ExpiresAt: now.Add(time.Hour),
	})

	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/projects/%d", project.ID), nil)
	req.AddCookie(&http.Cookie{Name: s.cookieName, Value: staffSessionID})
	rec := httptest.NewRecorder()
	handler := s.requireSession(http.HandlerFunc(s.handleProject))
	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var response projectDetailResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&response))
	require.Nil(t, response.DotProjectMaintainerCache)
	require.NotEmpty(t, response.DotProjectGeneratedMaintainersYaml)
	assert.Contains(t, response.DotProjectGeneratedMaintainersYaml, `name: "project-maintainers"`)
	assert.Contains(t, response.DotProjectGeneratedMaintainersYaml, "- alice-example")
	assert.NotContains(t, response.DotProjectGeneratedMaintainersYaml, "departed-example")
}

func TestProjectDetailIncludesOpenDotProjectMaintainerPullRequestFromGitHub(t *testing.T) {
	dbConn := setupPostgresTestDB(t)
	store := db.NewSQLStore(dbConn)
	now := time.Now().UTC().Truncate(time.Second)

	staff := model.StaffMember{
		Name:          "Staff Tester",
		GitHubAccount: "staff-tester",
		Email:         "staff@example.org",
	}
	require.NoError(t, dbConn.Create(&staff).Error)

	project := model.Project{
		Name:                    "Project Cache",
		Maturity:                model.Graduated,
		GitHubOrg:               "project-cache",
		DotProjectRepoRef:       "https://github.com/project-cache/.project",
		DotProjectMaintainerRef: "https://github.com/project-cache/.project/blob/main/Maintainers.YAML",
	}
	require.NoError(t, dbConn.Create(&project).Error)

	body := "maintainers:\n  - teams:\n      - name: project-maintainers\n        members:\n          - alice-example\n"
	syncState := model.DotProjectSyncState{
		ProjectID:             project.ID,
		RepoExists:            true,
		MaintainersFileExists: true,
		MaintainersFilename:   "Maintainers.YAML",
		MaintainersFileBody:   &body,
		LastCheckedAt:         &now,
	}
	require.NoError(t, dbConn.Create(&syncState).Error)

	s := &server{
		store:      store,
		sessions:   newSessionStore(log.New(io.Discard, "", 0)),
		cookieName: defaultSessionCookieName,
		logger:     log.New(io.Discard, "", 0),
		githubClient: func(context.Context, string) *github.Client {
			return githubTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/repos/project-cache/.project/pulls":
					_, _ = w.Write([]byte(`[{"number":7,"state":"open","html_url":"https://github.com/project-cache/.project/pull/7","title":"Manual maintainer update","user":{"login":"outside-user"},"created_at":"2026-05-21T09:00:00Z","updated_at":"2026-05-21T09:05:00Z"}]`))
				case "/repos/project-cache/.project/pulls/7/files":
					_, _ = w.Write([]byte(`[{"filename":"Maintainers.YAML"}]`))
				default:
					http.NotFound(w, r)
				}
			}))
		},
	}

	staffSessionID := "staff-session"
	s.sessions.Set(session{
		ID:        staffSessionID,
		Login:     staff.GitHubAccount,
		Role:      roleStaff,
		CreatedAt: now,
		ExpiresAt: now.Add(time.Hour),
	})

	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/projects/%d", project.ID), nil)
	req.AddCookie(&http.Cookie{Name: s.cookieName, Value: staffSessionID})
	rec := httptest.NewRecorder()
	handler := s.requireSession(http.HandlerFunc(s.handleProject))
	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var response projectDetailResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&response))
	require.NotNil(t, response.DotProjectMaintainerPR)
	assert.Equal(t, "open", response.DotProjectMaintainerPR.Status)
	assert.Equal(t, 7, response.DotProjectMaintainerPR.Number)
	assert.Equal(t, "github", response.DotProjectMaintainerPR.Source)
	assert.Equal(t, "https://github.com/project-cache/.project/pull/7", response.DotProjectMaintainerPR.URL)
}

func TestHandleDotProjectPullRequestCreatesAuditLog(t *testing.T) {
	dbConn := setupPostgresTestDB(t)
	store := db.NewSQLStore(dbConn)
	now := time.Now().UTC().Truncate(time.Second)

	staff := model.StaffMember{
		Name:          "Staff Tester",
		GitHubAccount: "staff-tester",
		Email:         "staff@example.org",
	}
	require.NoError(t, dbConn.Create(&staff).Error)

	project := model.Project{
		Name:                    "Project PR",
		Maturity:                model.Sandbox,
		GitHubOrg:               "project-pr",
		DotProjectMaintainerRef: "https://github.com/project-pr/.project/blob/main/MAINTAINERS.yaml",
	}
	require.NoError(t, dbConn.Create(&project).Error)

	alice := model.Maintainer{
		Name:             "Alice Example",
		Email:            "alice@example.org",
		GitHubAccount:    "alice",
		MaintainerStatus: model.ActiveMaintainer,
	}
	bob := model.Maintainer{
		Name:             "Bob Example",
		Email:            "bob@example.org",
		GitHubAccount:    "bob",
		MaintainerStatus: model.ActiveMaintainer,
	}
	require.NoError(t, dbConn.Create(&alice).Error)
	require.NoError(t, dbConn.Create(&bob).Error)
	require.NoError(t, dbConn.Model(&project).Association("Maintainers").Append(&alice, &bob))

	body := `maintainers:
  - teams:
      - name: project-maintainers
        members:
          # TODO: Add maintainer GitHub handles
          - github-handle
          - alice
`
	syncState := model.DotProjectSyncState{
		ProjectID:               project.ID,
		RepoExists:              true,
		MaintainersFileExists:   true,
		MaintainersFilename:     "MAINTAINERS.yaml",
		MaintainersFileBodyHash: "body-hash",
		MaintainersFileBody:     &body,
		DefaultBranch:           "main",
		LastCheckedAt:           &now,
	}
	require.NoError(t, dbConn.Create(&syncState).Error)

	var captured dotProjectPullRequestInput
	s := &server{
		store:      store,
		sessions:   newSessionStore(log.New(io.Discard, "", 0)),
		cookieName: defaultSessionCookieName,
		logger:     log.New(io.Discard, "", 0),
		createDotProjectPullRequest: func(_ context.Context, input dotProjectPullRequestInput, _ string) (*dotProjectPullRequestResponse, error) {
			captured = input
			return &dotProjectPullRequestResponse{
				URL:        "https://github.com/project-pr/.project/pull/42",
				Number:     42,
				Branch:     input.HeadBranch,
				BaseBranch: input.BaseBranch,
				ForkOwner:  input.ForkOwner,
				ForkRepo:   input.ForkRepo,
				FilePath:   input.FilePath,
				CommitSHA:  "abc123",
			}, nil
		},
	}

	staffSessionID := "staff-session"
	s.sessions.Set(session{
		ID:        staffSessionID,
		Login:     staff.GitHubAccount,
		Role:      roleStaff,
		CreatedAt: now,
		ExpiresAt: now.Add(time.Hour),
	})

	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/projects/%d/dot-project/pull-request", project.ID), nil)
	req.AddCookie(&http.Cookie{Name: s.cookieName, Value: staffSessionID})
	rec := httptest.NewRecorder()
	handler := s.requireSession(http.HandlerFunc(s.handleProject))
	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "project-pr", captured.Owner)
	assert.Equal(t, ".project", captured.Repo)
	assert.Equal(t, "staff-tester", captured.ForkOwner)
	assert.Equal(t, "project-pr.project", captured.ForkRepo)
	assert.Equal(t, "MAINTAINERS.yaml", captured.FilePath)
	assert.Equal(t, []string{"bob"}, captured.AddedHandles)
	assert.Equal(t, []string{"# TODO: Add maintainer GitHub handles", "- github-handle"}, captured.RemovedPlaceholders)
	assert.Contains(t, captured.Proposed, "          - bob")
	assert.NotContains(t, captured.Proposed, "github-handle")
	assert.NotContains(t, captured.Proposed, "TODO: Add maintainer")

	var response dotProjectPullRequestResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&response))
	assert.Equal(t, "https://github.com/project-pr/.project/pull/42", response.URL)
	assert.Equal(t, 42, response.Number)
	assert.Equal(t, "staff-tester", response.ForkOwner)
	assert.Equal(t, "project-pr.project", response.ForkRepo)
	assert.Equal(t, []string{"bob"}, response.AddedHandles)

	var audit model.AuditLog
	require.NoError(t, dbConn.Where("project_id = ? AND action = ?", project.ID, "DOT_PROJECT_MAINTAINER_PR_CREATE").First(&audit).Error)
	assert.Equal(t, staff.ID, *audit.StaffID)
	assert.Contains(t, audit.Message, "Staff Tester")
	assert.Contains(t, audit.Metadata, "https://github.com/project-pr/.project/pull/42")
	assert.Contains(t, audit.Metadata, "bob")
}

func TestDotProjectForkRepoNamePrefixesSourceRepoWithProjectSlug(t *testing.T) {
	assert.Equal(t, "cohdi.project", dotProjectForkRepoName("CoHDI", ".project"))
	assert.Equal(t, "cadence-workflow.project", dotProjectForkRepoName("Cadence Workflow", ".project"))
	assert.Equal(t, "project.project", dotProjectForkRepoName("...", ".project"))
}

func TestDotProjectMaintainerCommitMessageIncludesDCOSignoff(t *testing.T) {
	message := dotProjectMaintainerCommitMessage("CoHDI", &github.CommitAuthor{
		Name:  github.String("Robert Kielty"),
		Email: github.String("123+RobertKielty@users.noreply.github.com"),
	})

	assert.Equal(t, "Update CoHDI maintainers\n\nSigned-off-by: Robert Kielty <123+RobertKielty@users.noreply.github.com>", message)
}

func TestBuildDotProjectPullRequestBodyMentionsAddedMaintainers(t *testing.T) {
	body := buildDotProjectPullRequestBody(dotProjectPullRequestInput{
		FilePath:            "MAINTAINERS.yaml",
		AddedHandles:        []string{"alice", "@Bob", "alice"},
		RemovedPlaceholders: []string{"# TODO: Add maintainer handles"},
		SubmittedByName:     "Staff Tester",
		SubmittedByLogin:    "staff-tester",
	})

	assert.Contains(t, body, "Changes:\n- Add active maintainer-d handles missing from `MAINTAINERS.yaml`: @alice, @bob")
	assert.Contains(t, body, "- Remove placeholder maintainer lines: # TODO: Add maintainer handles")
	assert.NotContains(t, body, "@@")
}

func TestDotProjectMaintainerMentionHandlesNormalizesHandles(t *testing.T) {
	assert.Equal(t, []string{"alice", "bob"}, dotProjectMaintainerMentionHandles([]string{"@Alice", "bob", "alice", ""}))
}

func TestMaintainerServiceAssociationsForStaff(t *testing.T) {
	dbConn := setupPostgresTestDB(t)
	store := db.NewSQLStore(dbConn)
	now := time.Now()

	staff := model.StaffMember{
		Name:          "Staff Tester",
		GitHubAccount: "staff-tester",
		Email:         "staff@example.org",
	}
	require.NoError(t, dbConn.Create(&staff).Error)

	fossa := model.Service{Name: "FOSSA", Description: "License compliance"}
	require.NoError(t, dbConn.Create(&fossa).Error)

	maintainer := model.Maintainer{
		Name:             "Alice Example",
		Email:            "alice@example.org",
		GitHubAccount:    "alice-example",
		GitHubEmail:      "alice@github.example",
		MaintainerStatus: model.ActiveMaintainer,
	}
	require.NoError(t, dbConn.Create(&maintainer).Error)

	projectA := model.Project{Name: "Project A", Maturity: model.Sandbox}
	projectB := model.Project{Name: "Project B", Maturity: model.Graduated}
	require.NoError(t, dbConn.Create(&projectA).Error)
	require.NoError(t, dbConn.Create(&projectB).Error)
	require.NoError(t, dbConn.Model(&projectA).Association("Maintainers").Append(&maintainer))
	require.NoError(t, dbConn.Model(&projectB).Association("Maintainers").Append(&maintainer))
	require.NoError(t, dbConn.Model(&projectA).Association("Services").Append(&fossa))
	require.NoError(t, dbConn.Model(&projectB).Association("Services").Append(&fossa))

	teamAName := "Project A Team"
	teamBName := "Project B Team"
	teamA := model.RemoteTeam{
		ProjectID:      projectA.ID,
		ServiceID:      fossa.ID,
		RemoteTeamID:   101,
		RemoteTeamName: &teamAName,
		ProjectName:    &projectA.Name,
	}
	teamB := model.RemoteTeam{
		ProjectID:      projectB.ID,
		ServiceID:      fossa.ID,
		RemoteTeamID:   102,
		RemoteTeamName: &teamBName,
		ProjectName:    &projectB.Name,
	}
	require.NoError(t, dbConn.Create(&teamA).Error)
	require.NoError(t, dbConn.Create(&teamB).Error)

	remoteUser := model.RemoteUser{
		ServiceID:    fossa.ID,
		RemoteUserID: 9001,
		ServiceEmail: maintainer.Email,
		RemoteRef:    "alice-fossa",
	}
	require.NoError(t, dbConn.Create(&remoteUser).Error)

	link := model.RemoteTeamUser{
		ServiceID:    fossa.ID,
		TeamID:       teamA.ID,
		UserID:       remoteUser.ID,
		MaintainerID: &maintainer.ID,
	}
	require.NoError(t, dbConn.Create(&link).Error)

	invite := model.ServiceInvitation{
		ServiceID:            fossa.ID,
		RemoteTeamID:         teamB.RemoteTeamID,
		ProjectID:            projectB.ID,
		MaintainerID:         &maintainer.ID,
		ServiceEmail:         maintainer.Email,
		Status:               "pending",
		TeamAssignmentStatus: nil,
		SentAt:               &now,
		LastCheckedAt:        &now,
	}
	require.NoError(t, dbConn.Create(&invite).Error)

	s := &server{
		store:      store,
		sessions:   newSessionStore(log.New(io.Discard, "", 0)),
		cookieName: defaultSessionCookieName,
		logger:     log.New(io.Discard, "", 0),
	}
	staffSessionID := "staff-session"
	s.sessions.Set(session{
		ID:        staffSessionID,
		Login:     staff.GitHubAccount,
		Role:      roleStaff,
		CreatedAt: now,
		ExpiresAt: now.Add(time.Hour),
	})

	rec := performMaintainerGet(t, s, maintainer.ID, staffSessionID)
	require.Equal(t, http.StatusOK, rec.Code)

	var response maintainerDetailResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&response))
	require.Len(t, response.Services, 1)

	service := response.Services[0]
	assert.Equal(t, "fossa", service.Kind)
	assert.Equal(t, "CNCF FOSSA", service.Label)
	assert.Equal(t, "registered", service.Account.State)
	assert.Equal(t, "maintainer_email", service.Account.MatchedBy)
	if assert.NotNil(t, service.Account.RemoteUserID) {
		assert.Equal(t, uint(9001), *service.Account.RemoteUserID)
	}
	assert.Equal(t, "alice-fossa", service.Account.RemoteRef)
	assert.Equal(t, maintainer.Email, service.Account.EmailUsed)
	require.Len(t, service.Targets, 2)

	targetsByProject := map[uint]maintainerServiceTargetResponse{}
	for _, target := range service.Targets {
		targetsByProject[target.ProjectID] = target
	}

	assert.Equal(t, "member", targetsByProject[projectA.ID].State)
	assert.Equal(t, "pending", targetsByProject[projectB.ID].State)
	assert.True(t, targetsByProject[projectB.ID].PendingInvite)
}

func TestMaintainerFossaRefreshAction(t *testing.T) {
	dbConn := setupPostgresTestDB(t)
	store := db.NewSQLStore(dbConn)
	now := time.Now()

	staff := model.StaffMember{
		Name:          "Staff Tester",
		GitHubAccount: "staff-tester",
		Email:         "staff@example.org",
	}
	require.NoError(t, dbConn.Create(&staff).Error)

	maintainer := model.Maintainer{
		Name:             "Alice Example",
		Email:            "alice@example.org",
		GitHubAccount:    "alice-example",
		GitHubEmail:      "alice@github.example",
		MaintainerStatus: model.ActiveMaintainer,
	}
	require.NoError(t, dbConn.Create(&maintainer).Error)

	project := model.Project{Name: "Project Refresh", Maturity: model.Sandbox}
	require.NoError(t, dbConn.Create(&project).Error)
	require.NoError(t, dbConn.Model(&project).Association("Maintainers").Append(&maintainer))

	fossaService := model.Service{Name: "FOSSA", Description: "License scanning"}
	require.NoError(t, dbConn.Create(&fossaService).Error)
	require.NoError(t, dbConn.Model(&project).Association("Services").Append(&fossaService))

	teamName := "Project Refresh Team"
	team := model.RemoteTeam{
		ProjectID:      project.ID,
		ServiceID:      fossaService.ID,
		RemoteTeamID:   501,
		RemoteTeamName: &teamName,
		ProjectName:    &project.Name,
	}
	require.NoError(t, dbConn.Create(&team).Error)

	invite := model.ServiceInvitation{
		ServiceID:     fossaService.ID,
		RemoteTeamID:  team.RemoteTeamID,
		ProjectID:     project.ID,
		MaintainerID:  &maintainer.ID,
		ServiceEmail:  maintainer.Email,
		Status:        "pending",
		LastCheckedAt: &now,
	}
	require.NoError(t, dbConn.Create(&invite).Error)

	fossaAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/users"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"id":9001,"username":"alice-fossa","email":"alice@example.org","github":{"name":"alice-example","email":"alice@github.example"},"bitbucketCloud":{"name":null,"email":null},"teamUsers":[{"roleId":3,"team":{"id":501,"name":"Project Refresh Team"}}]}]`))
		case r.Method == http.MethodGet && r.URL.Path == "/user-invitations":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"email":"alice@example.org"}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer fossaAPI.Close()
	t.Setenv("FOSSA_API_BASE", fossaAPI.URL)

	s := &server{
		store:      store,
		sessions:   newSessionStore(log.New(io.Discard, "", 0)),
		cookieName: defaultSessionCookieName,
		logger:     log.New(io.Discard, "", 0),
		fossaToken: "test-token",
	}
	staffSessionID := "staff-session"
	s.sessions.Set(session{
		ID:        staffSessionID,
		Login:     staff.GitHubAccount,
		Role:      roleStaff,
		CreatedAt: now,
		ExpiresAt: now.Add(time.Hour),
	})

	rec := performMaintainerAction(t, s, maintainer.ID, "refresh", staffSessionID)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var response maintainerDetailResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&response))
	require.Len(t, response.Services, 1)
	assert.Equal(t, "registered", response.Services[0].Account.State)
	assert.Equal(t, "member", response.Services[0].Targets[0].State)

	var remoteUser model.RemoteUser
	require.NoError(t, dbConn.Where("service_id = ? AND remote_user_id = ?", fossaService.ID, 9001).First(&remoteUser).Error)
	assert.Equal(t, "alice-fossa", remoteUser.RemoteRef)

	var link model.RemoteTeamUser
	require.NoError(t, dbConn.Where("service_id = ? AND team_id = ? AND maintainer_id = ?", fossaService.ID, team.ID, maintainer.ID).First(&link).Error)

	var updatedInvite model.ServiceInvitation
	require.NoError(t, dbConn.First(&updatedInvite, invite.ID).Error)
	assert.Equal(t, "pending", updatedInvite.Status)
}

func TestMaintainerFossaReconcileAction(t *testing.T) {
	dbConn := setupPostgresTestDB(t)
	store := db.NewSQLStore(dbConn)
	now := time.Now()

	staff := model.StaffMember{
		Name:          "Staff Tester",
		GitHubAccount: "staff-tester",
		Email:         "staff@example.org",
	}
	require.NoError(t, dbConn.Create(&staff).Error)

	maintainer := model.Maintainer{
		Name:             "Alice Example",
		Email:            "alice@example.org",
		GitHubAccount:    "alice-example",
		GitHubEmail:      "alice@github.example",
		MaintainerStatus: model.ActiveMaintainer,
	}
	require.NoError(t, dbConn.Create(&maintainer).Error)

	projectA := model.Project{Name: "Project A", Maturity: model.Sandbox}
	projectB := model.Project{Name: "Project B", Maturity: model.Graduated}
	require.NoError(t, dbConn.Create(&projectA).Error)
	require.NoError(t, dbConn.Create(&projectB).Error)
	require.NoError(t, dbConn.Model(&projectA).Association("Maintainers").Append(&maintainer))
	require.NoError(t, dbConn.Model(&projectB).Association("Maintainers").Append(&maintainer))

	fossaService := model.Service{Name: "FOSSA", Description: "License scanning"}
	require.NoError(t, dbConn.Create(&fossaService).Error)
	require.NoError(t, dbConn.Model(&projectA).Association("Services").Append(&fossaService))
	require.NoError(t, dbConn.Model(&projectB).Association("Services").Append(&fossaService))

	teamAName := "Project A Team"
	teamBName := "Project B Team"
	teamA := model.RemoteTeam{
		ProjectID:      projectA.ID,
		ServiceID:      fossaService.ID,
		RemoteTeamID:   601,
		RemoteTeamName: &teamAName,
		ProjectName:    &projectA.Name,
	}
	teamB := model.RemoteTeam{
		ProjectID:      projectB.ID,
		ServiceID:      fossaService.ID,
		RemoteTeamID:   602,
		RemoteTeamName: &teamBName,
		ProjectName:    &projectB.Name,
	}
	require.NoError(t, dbConn.Create(&teamA).Error)
	require.NoError(t, dbConn.Create(&teamB).Error)

	putBodies := make([]string, 0, 2)
	fossaAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/users"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"id":9001,"username":"alice-fossa","email":"alice@example.org","github":{"name":"alice-example","email":"alice@github.example"},"bitbucketCloud":{"name":null,"email":null},"teamUsers":[]}]`))
		case r.Method == http.MethodGet && r.URL.Path == "/roles":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"id":4,"scope":"team","name":"Team Admin"}]`))
		case r.Method == http.MethodPut && r.URL.Path == "/teams/601/users":
			body, _ := io.ReadAll(r.Body)
			putBodies = append(putBodies, string(body))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodPut && r.URL.Path == "/teams/602/users":
			body, _ := io.ReadAll(r.Body)
			putBodies = append(putBodies, string(body))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer fossaAPI.Close()
	t.Setenv("FOSSA_API_BASE", fossaAPI.URL)

	s := &server{
		store:      store,
		sessions:   newSessionStore(log.New(io.Discard, "", 0)),
		cookieName: defaultSessionCookieName,
		logger:     log.New(io.Discard, "", 0),
		fossaToken: "test-token",
	}
	staffSessionID := "staff-session"
	s.sessions.Set(session{
		ID:        staffSessionID,
		Login:     staff.GitHubAccount,
		Role:      roleStaff,
		CreatedAt: now,
		ExpiresAt: now.Add(time.Hour),
	})

	rec := performMaintainerAction(t, s, maintainer.ID, "reconcile", staffSessionID)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var response maintainerDetailResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&response))
	require.Len(t, response.Services, 1)
	for _, target := range response.Services[0].Targets {
		assert.Equal(t, "member", target.State)
	}

	require.Len(t, putBodies, 2)
	assert.Contains(t, putBodies[0], `"userId":9001`)
	assert.Contains(t, putBodies[0], `"roleId":4`)
	assert.Contains(t, putBodies[1], `"userId":9001`)
	assert.Contains(t, putBodies[1], `"roleId":4`)

	var links int64
	require.NoError(t, dbConn.Model(&model.RemoteTeamUser{}).
		Where("service_id = ? AND maintainer_id = ?", fossaService.ID, maintainer.ID).
		Count(&links).Error)
	assert.Equal(t, int64(2), links)

	var invites []model.ServiceInvitation
	require.NoError(t, dbConn.Where("service_id = ? AND maintainer_id = ?", fossaService.ID, maintainer.ID).Find(&invites).Error)
	require.Len(t, invites, 2)
	for _, invite := range invites {
		assert.Equal(t, "accepted", invite.Status)
		require.NotNil(t, invite.TeamAssignmentStatus)
		assert.Equal(t, "done", *invite.TeamAssignmentStatus)
	}
}

func TestFossaInviteRefreshDoesNotRemoveAcceptedInvitations(t *testing.T) {
	dbConn := setupPostgresTestDB(t)
	store := db.NewSQLStore(dbConn)
	now := time.Now()

	staff := model.StaffMember{
		Name:          "Staff Tester",
		GitHubAccount: "staff-tester",
		Email:         "staff@example.org",
	}
	require.NoError(t, dbConn.Create(&staff).Error)

	maintainer := model.Maintainer{
		Name:             "Vincent Example",
		Email:            "vincent@example.org",
		GitHubAccount:    "vincent-example",
		MaintainerStatus: model.ActiveMaintainer,
	}
	require.NoError(t, dbConn.Create(&maintainer).Error)

	project := model.Project{Name: "Project Tekton", Maturity: model.Graduated}
	require.NoError(t, dbConn.Create(&project).Error)
	require.NoError(t, dbConn.Model(&project).Association("Maintainers").Append(&maintainer))

	fossaService := model.Service{Name: "FOSSA", Description: "License scanning"}
	require.NoError(t, dbConn.Create(&fossaService).Error)
	require.NoError(t, dbConn.Model(&project).Association("Services").Append(&fossaService))

	teamName := "Project Tekton Team"
	team := model.RemoteTeam{
		ProjectID:      project.ID,
		ServiceID:      fossaService.ID,
		RemoteTeamID:   701,
		RemoteTeamName: &teamName,
		ProjectName:    &project.Name,
	}
	require.NoError(t, dbConn.Create(&team).Error)

	done := "done"
	invite := model.ServiceInvitation{
		ProjectID:            project.ID,
		MaintainerID:         &maintainer.ID,
		ServiceID:            fossaService.ID,
		ServiceEmail:         "vincent@example.org",
		RemoteTeamID:         team.RemoteTeamID,
		Status:               "accepted",
		TeamAssignmentStatus: &done,
		LastCheckedAt:        &now,
	}
	require.NoError(t, dbConn.Create(&invite).Error)

	fossaAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/user-invitations":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer fossaAPI.Close()
	t.Setenv("FOSSA_API_BASE", fossaAPI.URL)

	s := &server{
		store:      store,
		sessions:   newSessionStore(log.New(io.Discard, "", 0)),
		cookieName: defaultSessionCookieName,
		logger:     log.New(io.Discard, "", 0),
		fossaToken: "test-token",
	}
	staffSessionID := "staff-session"
	s.sessions.Set(session{
		ID:        staffSessionID,
		Login:     staff.GitHubAccount,
		Role:      roleStaff,
		CreatedAt: now,
		ExpiresAt: now.Add(time.Hour),
	})

	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/services/fossa/invites/refresh?projectId=%d", project.ID), nil)
	req.AddCookie(&http.Cookie{Name: s.cookieName, Value: staffSessionID})
	rec := httptest.NewRecorder()
	handler := s.requireSession(http.HandlerFunc(s.handleFossaInviteRefresh))
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var refreshed model.ServiceInvitation
	require.NoError(t, dbConn.Where("id = ?", invite.ID).First(&refreshed).Error)
	assert.Equal(t, "accepted", refreshed.Status)
}

// TestFossaInviteRefreshDoesNotRemoveDoneTeamAssignments covers a row that reflects a
// confirmed FOSSA team assignment (TeamAssignmentStatus="done") but whose Status was later
// clobbered to "error" by an unrelated transient API failure on a subsequent poll — this
// happens because the poller's generic error branches (cmd/fossa-poller/main.go) only ever
// touch Status/LastError, never TeamAssignmentStatus. Status=="accepted" alone is not a
// reliable membership signal once that has happened.
func TestFossaInviteRefreshDoesNotRemoveDoneTeamAssignments(t *testing.T) {
	dbConn := setupPostgresTestDB(t)
	store := db.NewSQLStore(dbConn)
	now := time.Now()

	staff := model.StaffMember{
		Name:          "Staff Tester",
		GitHubAccount: "staff-tester",
		Email:         "staff@example.org",
	}
	require.NoError(t, dbConn.Create(&staff).Error)

	maintainer := model.Maintainer{
		Name:             "Tiger Example",
		Email:            "tiger@example.org",
		GitHubAccount:    "tiger-example",
		MaintainerStatus: model.ActiveMaintainer,
	}
	require.NoError(t, dbConn.Create(&maintainer).Error)

	project := model.Project{Name: "Project Velero", Maturity: model.Graduated}
	require.NoError(t, dbConn.Create(&project).Error)
	require.NoError(t, dbConn.Model(&project).Association("Maintainers").Append(&maintainer))

	fossaService := model.Service{Name: "FOSSA", Description: "License scanning"}
	require.NoError(t, dbConn.Create(&fossaService).Error)
	require.NoError(t, dbConn.Model(&project).Association("Services").Append(&fossaService))

	teamName := "Project Velero Team"
	team := model.RemoteTeam{
		ProjectID:      project.ID,
		ServiceID:      fossaService.ID,
		RemoteTeamID:   702,
		RemoteTeamName: &teamName,
		ProjectName:    &project.Name,
	}
	require.NoError(t, dbConn.Create(&team).Error)

	done := "done"
	fetchErr := "FetchUserInvitations failed: 502 Bad Gateway"
	invite := model.ServiceInvitation{
		ProjectID:            project.ID,
		MaintainerID:         &maintainer.ID,
		ServiceID:            fossaService.ID,
		ServiceEmail:         "tiger@example.org",
		RemoteTeamID:         team.RemoteTeamID,
		Status:               "error",
		TeamAssignmentStatus: &done,
		LastError:            &fetchErr,
		LastCheckedAt:        &now,
	}
	require.NoError(t, dbConn.Create(&invite).Error)

	fossaAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/user-invitations":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer fossaAPI.Close()
	t.Setenv("FOSSA_API_BASE", fossaAPI.URL)

	s := &server{
		store:      store,
		sessions:   newSessionStore(log.New(io.Discard, "", 0)),
		cookieName: defaultSessionCookieName,
		logger:     log.New(io.Discard, "", 0),
		fossaToken: "test-token",
	}
	staffSessionID := "staff-session"
	s.sessions.Set(session{
		ID:        staffSessionID,
		Login:     staff.GitHubAccount,
		Role:      roleStaff,
		CreatedAt: now,
		ExpiresAt: now.Add(time.Hour),
	})

	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/services/fossa/invites/refresh?projectId=%d", project.ID), nil)
	req.AddCookie(&http.Cookie{Name: s.cookieName, Value: staffSessionID})
	rec := httptest.NewRecorder()
	handler := s.requireSession(http.HandlerFunc(s.handleFossaInviteRefresh))
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var refreshed model.ServiceInvitation
	require.NoError(t, dbConn.Where("id = ?", invite.ID).First(&refreshed).Error)
	require.NotNil(t, refreshed.TeamAssignmentStatus)
	assert.Equal(t, "done", *refreshed.TeamAssignmentStatus)
}

func TestMaintainerFossaInviteAction(t *testing.T) {
	dbConn := setupPostgresTestDB(t)
	store := db.NewSQLStore(dbConn)
	now := time.Now()

	staff := model.StaffMember{
		Name:          "Staff Tester",
		GitHubAccount: "staff-tester",
		Email:         "staff@example.org",
	}
	require.NoError(t, dbConn.Create(&staff).Error)

	maintainer := model.Maintainer{
		Name:             "Alice Example",
		Email:            "alice@example.org",
		GitHubAccount:    "alice-example",
		GitHubEmail:      "alice@github.example",
		MaintainerStatus: model.ActiveMaintainer,
	}
	require.NoError(t, dbConn.Create(&maintainer).Error)

	projectA := model.Project{Name: "Project A", Maturity: model.Sandbox}
	projectB := model.Project{Name: "Project B", Maturity: model.Incubating}
	require.NoError(t, dbConn.Create(&projectA).Error)
	require.NoError(t, dbConn.Create(&projectB).Error)
	require.NoError(t, dbConn.Model(&projectA).Association("Maintainers").Append(&maintainer))
	require.NoError(t, dbConn.Model(&projectB).Association("Maintainers").Append(&maintainer))

	fossaService := model.Service{Name: "FOSSA", Description: "License scanning"}
	require.NoError(t, dbConn.Create(&fossaService).Error)
	require.NoError(t, dbConn.Model(&projectA).Association("Services").Append(&fossaService))
	require.NoError(t, dbConn.Model(&projectB).Association("Services").Append(&fossaService))

	teamAName := "Project A Team"
	teamBName := "Project B Team"
	teamA := model.RemoteTeam{
		ProjectID:      projectA.ID,
		ServiceID:      fossaService.ID,
		RemoteTeamID:   701,
		RemoteTeamName: &teamAName,
		ProjectName:    &projectA.Name,
	}
	teamB := model.RemoteTeam{
		ProjectID:      projectB.ID,
		ServiceID:      fossaService.ID,
		RemoteTeamID:   702,
		RemoteTeamName: &teamBName,
		ProjectName:    &projectB.Name,
	}
	require.NoError(t, dbConn.Create(&teamA).Error)
	require.NoError(t, dbConn.Create(&teamB).Error)

	fossaAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/organizations/162/invite":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer fossaAPI.Close()
	t.Setenv("FOSSA_API_BASE", fossaAPI.URL)

	s := &server{
		store:      store,
		sessions:   newSessionStore(log.New(io.Discard, "", 0)),
		cookieName: defaultSessionCookieName,
		logger:     log.New(io.Discard, "", 0),
		fossaToken: "test-token",
	}
	staffSessionID := "staff-session"
	s.sessions.Set(session{
		ID:        staffSessionID,
		Login:     staff.GitHubAccount,
		Role:      roleStaff,
		CreatedAt: now,
		ExpiresAt: now.Add(time.Hour),
	})

	rec := performMaintainerAction(t, s, maintainer.ID, "invite", staffSessionID)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var response maintainerDetailResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&response))
	require.Len(t, response.Services, 1)
	assert.Equal(t, "invited", response.Services[0].Account.State)

	var invites []model.ServiceInvitation
	require.NoError(t, dbConn.Where("service_id = ? AND maintainer_id = ?", fossaService.ID, maintainer.ID).Order("project_id asc").Find(&invites).Error)
	require.Len(t, invites, 2)
	assert.Equal(t, projectA.ID, invites[0].ProjectID)
	assert.Equal(t, projectB.ID, invites[1].ProjectID)
	for _, invite := range invites {
		assert.Equal(t, "pending", invite.Status)
		assert.Equal(t, maintainer.Email, invite.ServiceEmail)
	}
}

func TestParseGitHubIssueURL(t *testing.T) {
	t.Run("parses github issue url", func(t *testing.T) {
		owner, repo, number, err := parseGitHubIssueURL("https://github.com/cncf/sandbox/issues/1234")
		require.NoError(t, err)
		assert.Equal(t, "cncf", owner)
		assert.Equal(t, "sandbox", repo)
		assert.Equal(t, 1234, number)
	})

	t.Run("rejects non-github host", func(t *testing.T) {
		_, _, _, err := parseGitHubIssueURL("https://example.com/cncf/sandbox/issues/1")
		require.Error(t, err)
	})

	t.Run("rejects non-issue urls", func(t *testing.T) {
		_, _, _, err := parseGitHubIssueURL("https://github.com/cncf/sandbox/pulls/12")
		require.Error(t, err)
	})

	t.Run("rejects missing number", func(t *testing.T) {
		_, _, _, err := parseGitHubIssueURL("https://github.com/cncf/sandbox/issues/")
		require.Error(t, err)
	})
}

func TestHandleProjectCreate(t *testing.T) {
	dbConn := setupPostgresTestDB(t)
	store := db.NewSQLStore(dbConn)
	now := time.Now()

	staff := model.StaffMember{
		Name:          "Staff Tester",
		GitHubAccount: "staff-tester",
		Email:         "staff@example.org",
	}
	require.NoError(t, dbConn.Create(&staff).Error)

	s := &server{
		store:       store,
		sessions:    newSessionStore(log.New(io.Discard, "", 0)),
		cookieName:  defaultSessionCookieName,
		logger:      log.New(io.Discard, "", 0),
		githubToken: "test-token",
	}

	staffSessionID := "staff-session"
	s.sessions.Set(session{
		ID:        staffSessionID,
		Login:     staff.GitHubAccount,
		Role:      roleStaff,
		CreatedAt: now,
		ExpiresAt: now.Add(time.Hour),
	})

	s.fetchIssueTitle = func(_ context.Context, _, _ string, _ int) (string, error) {
		return "[PROJECT ONBOARDING] Example Project", nil
	}

	body := `{"onboardingIssue":"https://github.com/cncf/sandbox/issues/123","legacyMaintainerRef":"https://github.com/exampleorg/example/blob/main/OWNERS"}`
	req := httptest.NewRequest(http.MethodPost, "/api/projects", strings.NewReader(body))
	req.AddCookie(&http.Cookie{Name: s.cookieName, Value: staffSessionID})
	rec := httptest.NewRecorder()
	handler := s.requireSession(http.HandlerFunc(s.handleProjects))
	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var response projectCreateResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&response))
	assert.Equal(t, "Example Project", response.Name)
	assert.Equal(t, "Sandbox", response.Maturity)
	assert.Equal(t, "exampleorg", response.GitHubOrg)
}

func TestRewriteMaintainerRefURL(t *testing.T) {
	tests := []struct {
		name    string
		refURL  string
		want    string
		wantErr bool
	}{
		{
			name:   "rewrites github blob urls",
			refURL: "https://github.com/etcd-io/etcd/blob/main/OWNERS",
			want:   "https://raw.githubusercontent.com/etcd-io/etcd/main/OWNERS",
		},
		{
			name:   "keeps raw github urls",
			refURL: "https://raw.githubusercontent.com/etcd-io/etcd/main/OWNERS",
			want:   "https://raw.githubusercontent.com/etcd-io/etcd/main/OWNERS",
		},
		{
			name:   "keeps raw gist urls",
			refURL: "https://gist.githubusercontent.com/RobertKielty/a00d784f6d501296b739846f3f17c430/raw/d7f9508f71d8a687883ffc5ae8f8ee0c3f81b51a/gistfile1.txt",
			want:   "https://gist.githubusercontent.com/RobertKielty/a00d784f6d501296b739846f3f17c430/raw/d7f9508f71d8a687883ffc5ae8f8ee0c3f81b51a/gistfile1.txt",
		},
		{
			name:    "rejects non-https raw github urls",
			refURL:  "http://raw.githubusercontent.com/etcd-io/etcd/main/OWNERS",
			wantErr: true,
		},
		{
			name:    "rejects unrelated hosts",
			refURL:  "https://example.com/OWNERS",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := rewriteMaintainerRefURL(tt.refURL)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestHandleProjectsNamePrefix(t *testing.T) {
	dbConn := setupPostgresTestDB(t)
	store := db.NewSQLStore(dbConn)
	now := time.Now()

	staff := model.StaffMember{
		Name:          "Staff Tester",
		GitHubAccount: "staff-tester",
		Email:         "staff@example.org",
	}
	require.NoError(t, dbConn.Create(&staff).Error)

	projectA := model.Project{Name: "KubeFlow", Maturity: model.Sandbox}
	projectB := model.Project{Name: "Argo", Maturity: model.Graduated}
	projectC := model.Project{Name: "KubeEdge", Maturity: model.Incubating}
	require.NoError(t, dbConn.Create(&projectA).Error)
	require.NoError(t, dbConn.Create(&projectB).Error)
	require.NoError(t, dbConn.Create(&projectC).Error)

	s := &server{
		store:      store,
		sessions:   newSessionStore(log.New(io.Discard, "", 0)),
		cookieName: defaultSessionCookieName,
		logger:     log.New(io.Discard, "", 0),
	}

	staffSessionID := "staff-session"
	s.sessions.Set(session{
		ID:        staffSessionID,
		Login:     staff.GitHubAccount,
		Role:      roleStaff,
		CreatedAt: now,
		ExpiresAt: now.Add(time.Hour),
	})

	req := httptest.NewRequest(http.MethodGet, "/api/projects?namePrefix=ku", nil)
	req.AddCookie(&http.Cookie{Name: s.cookieName, Value: staffSessionID})
	rec := httptest.NewRecorder()
	handler := s.requireSession(http.HandlerFunc(s.handleProjects))
	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var response projectsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&response))

	names := make([]string, 0, len(response.Projects))
	for _, project := range response.Projects {
		names = append(names, project.Name)
	}
	assert.ElementsMatch(t, []string{"KubeFlow", "KubeEdge"}, names)
}

func TestHandleOnboardingIssues(t *testing.T) {
	dbConn := setupPostgresTestDB(t)
	store := db.NewSQLStore(dbConn)
	now := time.Now()

	staff := model.StaffMember{
		Name:          "Staff Tester",
		GitHubAccount: "staff-tester",
		Email:         "staff@example.org",
	}
	require.NoError(t, dbConn.Create(&staff).Error)

	s := &server{
		store:           store,
		sessions:        newSessionStore(log.New(io.Discard, "", 0)),
		cookieName:      defaultSessionCookieName,
		logger:          log.New(io.Discard, "", 0),
		githubToken:     "token",
		onboardingCache: &onboardingIssueCache{},
	}

	s.fetchIssues = func(ctx context.Context) ([]onboardingIssueSummary, error) {
		return []onboardingIssueSummary{
			{
				Number:      101,
				Title:       "[PROJECT ONBOARDING] Sample",
				URL:         "https://github.com/cncf/sandbox/issues/101",
				ProjectName: "Sample",
			},
		}, nil
	}

	staffSessionID := "staff-session"
	s.sessions.Set(session{
		ID:        staffSessionID,
		Login:     staff.GitHubAccount,
		Role:      roleStaff,
		CreatedAt: now,
		ExpiresAt: now.Add(time.Hour),
	})

	req := httptest.NewRequest(http.MethodGet, "/api/onboarding/issues", nil)
	req.AddCookie(&http.Cookie{Name: s.cookieName, Value: staffSessionID})
	rec := httptest.NewRecorder()
	handler := s.requireSession(http.HandlerFunc(s.handleOnboardingIssues))
	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var response onboardingIssuesResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&response))
	require.Len(t, response.Issues, 1)
	assert.Equal(t, 101, response.Issues[0].Number)
	assert.Equal(t, "Sample", response.Issues[0].ProjectName)
}

func TestHandleMaintainerFromRef_AuditLog(t *testing.T) {
	dbConn := setupPostgresTestDB(t)
	store := db.NewSQLStore(dbConn)
	now := time.Now()

	staff := model.StaffMember{
		Name:          "Staff Tester",
		GitHubAccount: "staff-tester",
		Email:         "staff@example.org",
	}
	require.NoError(t, dbConn.Create(&staff).Error)

	project := model.Project{Name: "Cedar", Maturity: model.Sandbox}
	require.NoError(t, dbConn.Create(&project).Error)

	existing := model.Maintainer{
		Name:             "",
		Email:            "sam.quill@example.invalid",
		GitHubAccount:    "GITHUB_MISSING",
		MaintainerStatus: model.ActiveMaintainer,
	}
	require.NoError(t, dbConn.Create(&existing).Error)

	s := &server{
		store:      store,
		sessions:   newSessionStore(log.New(io.Discard, "", 0)),
		cookieName: defaultSessionCookieName,
		logger:     log.New(io.Discard, "", 0),
	}

	staffSessionID := "staff-session"
	s.sessions.Set(session{
		ID:        staffSessionID,
		Login:     staff.GitHubAccount,
		Role:      roleStaff,
		CreatedAt: now,
		ExpiresAt: now.Add(time.Hour),
	})

	body := `{"projectId":` + fmt.Sprintf("%d", project.ID) + `,"name":"Sam Quill","githubHandle":"samquill","email":"sam.quill@example.invalid","company":"Acme Co"}`
	req := httptest.NewRequest(http.MethodPost, "/api/maintainers/from-ref", strings.NewReader(body))
	req.AddCookie(&http.Cookie{Name: s.cookieName, Value: staffSessionID})
	rec := httptest.NewRecorder()
	handler := s.requireSession(http.HandlerFunc(s.handleMaintainerFromRef))
	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var audit model.AuditLog
	err := dbConn.Where("maintainer_id = ? AND project_id = ? AND action = ?", existing.ID, project.ID, "MAINTAINER_UPDATE").First(&audit).Error
	require.NoError(t, err)

	var metadata map[string]any
	require.NoError(t, json.Unmarshal([]byte(audit.Metadata), &metadata))
	changes, ok := metadata["changes"].(map[string]any)
	require.True(t, ok)
	assert.Contains(t, changes, "name")
	assert.Contains(t, changes, "github")
	assert.Contains(t, changes, "company")
}

func TestFossaInviteEndpointsContract(t *testing.T) {
	dbConn := setupPostgresTestDB(t)
	store := db.NewSQLStore(dbConn)
	now := time.Now().UTC()

	staff := model.StaffMember{
		Name:          "Staff Tester",
		GitHubAccount: "staff-tester",
		Email:         "staff@example.org",
	}
	require.NoError(t, dbConn.Create(&staff).Error)

	project := model.Project{Name: "Fossa Project", Maturity: model.Sandbox}
	require.NoError(t, dbConn.Create(&project).Error)

	service := model.Service{Name: "FOSSA", Description: "FOSSA"}
	require.NoError(t, dbConn.Create(&service).Error)

	serviceTeamName := "CNCF Test"
	serviceTeam := model.RemoteTeam{
		ProjectID:      project.ID,
		ServiceID:      service.ID,
		RemoteTeamID:   101,
		RemoteTeamName: &serviceTeamName,
	}
	require.NoError(t, dbConn.Create(&serviceTeam).Error)

	invite := model.ServiceInvitation{
		ProjectID:    project.ID,
		ServiceID:    service.ID,
		ServiceEmail: "maintainer@example.org",
		RemoteTeamID: 101,
		Status:       "error",
		SentAt:       &now,
	}
	require.NoError(t, dbConn.Create(&invite).Error)

	s := &server{
		store:      store,
		sessions:   newSessionStore(log.New(io.Discard, "", 0)),
		cookieName: defaultSessionCookieName,
		logger:     log.New(io.Discard, "", 0),
	}

	staffSessionID := "staff-session"
	s.sessions.Set(session{
		ID:        staffSessionID,
		Login:     staff.GitHubAccount,
		Role:      roleStaff,
		CreatedAt: now,
		ExpiresAt: now.Add(time.Hour),
	})

	t.Run("list invites returns expected shape", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/services/fossa/invites?projectId=%d", project.ID), nil)
		req.AddCookie(&http.Cookie{Name: s.cookieName, Value: staffSessionID})
		rec := httptest.NewRecorder()
		handler := s.requireSession(http.HandlerFunc(s.handleFossaInvites))
		handler.ServeHTTP(rec, req)

		require.Equal(t, http.StatusOK, rec.Code)
		var response []fossaInviteSummary
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&response))
		require.Len(t, response, 1)
		assert.Equal(t, invite.ID, response[0].ID)
		assert.Equal(t, invite.ServiceEmail, response[0].Email)
		assert.Equal(t, invite.RemoteTeamID, response[0].FossaTeamID)
		assert.Equal(t, serviceTeamName, response[0].FossaTeamName)
		assert.Equal(t, invite.Status, response[0].Status)
		assert.Nil(t, response[0].TeamAssignmentStatus)
		assert.Equal(t, 0, response[0].TeamAddAttempts)
		assert.Nil(t, response[0].NextTeamAddAt)
	})

	t.Run("invite requires project id", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/services/fossa/invite", strings.NewReader(`{}`))
		req.AddCookie(&http.Cookie{Name: s.cookieName, Value: staffSessionID})
		rec := httptest.NewRecorder()
		handler := s.requireSession(http.HandlerFunc(s.handleFossaInvite))
		handler.ServeHTTP(rec, req)
		require.Equal(t, http.StatusBadRequest, rec.Code)
	})

	t.Run("reissue without token returns server error", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/services/fossa/invites/%d/reissue", invite.ID), nil)
		req.AddCookie(&http.Cookie{Name: s.cookieName, Value: staffSessionID})
		rec := httptest.NewRecorder()
		handler := s.requireSession(http.HandlerFunc(s.handleFossaInviteAction))
		handler.ServeHTTP(rec, req)
		require.Equal(t, http.StatusInternalServerError, rec.Code)
		assert.Contains(t, rec.Body.String(), "FOSSA_API_TOKEN not set")
	})
}

func TestFossaChooseRequiresRemoteTeamIDFromFossa(t *testing.T) {
	dbConn := setupPostgresTestDB(t)
	store := db.NewSQLStore(dbConn)
	now := time.Now().UTC()

	staff := model.StaffMember{
		Name:          "Staff Tester",
		GitHubAccount: "staff-tester",
		Email:         "staff@example.org",
	}
	require.NoError(t, dbConn.Create(&staff).Error)

	project := model.Project{Name: "Fossa Project", Maturity: model.Sandbox}
	require.NoError(t, dbConn.Create(&project).Error)

	service := model.Service{Name: "FOSSA", Description: "FOSSA"}
	require.NoError(t, dbConn.Create(&service).Error)

	s := &server{
		store:      store,
		sessions:   newSessionStore(log.New(io.Discard, "", 0)),
		cookieName: defaultSessionCookieName,
		logger:     log.New(io.Discard, "", 0),
		testMode:   true,
	}

	staffSessionID := "staff-session"
	s.sessions.Set(session{
		ID:        staffSessionID,
		Login:     staff.GitHubAccount,
		Role:      roleStaff,
		CreatedAt: now,
		ExpiresAt: now.Add(time.Hour),
	})

	req := httptest.NewRequest(http.MethodPost, "/api/services/fossa/choose", strings.NewReader(
		fmt.Sprintf(`{"projectId":%d}`, project.ID),
	))
	req.AddCookie(&http.Cookie{Name: s.cookieName, Value: staffSessionID})
	rec := httptest.NewRecorder()
	handler := s.requireSession(http.HandlerFunc(s.handleFossaChoose))
	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "RemoteTeamID must come from FOSSA API")

	var count int64
	require.NoError(t, dbConn.Model(&model.RemoteTeam{}).Count(&count).Error)
	assert.Equal(t, int64(0), count)
}

type fakeLFXEnrichmentClient struct {
	checkErr error
}

func (f fakeLFXEnrichmentClient) CheckToken(_ context.Context) error {
	return f.checkErr
}

func (f fakeLFXEnrichmentClient) SearchUsers(_ context.Context, query lfx.UserSearch) ([]lfx.User, error) {
	if strings.EqualFold(query.GitHubID, "alice-example") {
		return []lfx.User{{
			ID:       "003-alice",
			Name:     "Alice Example",
			Email:    "alice@example.org",
			Username: "alice-lfid",
		}}, nil
	}
	return []lfx.User{}, nil
}

func (f fakeLFXEnrichmentClient) GetUserIdentities(_ context.Context, _ string) ([]lfx.Identity, error) {
	return []lfx.Identity{{
		Source:   "github",
		Username: "alice-example",
		Email:    "alice@example.org",
	}}, nil
}

func TestLFXEnrichmentAccessUsesEnvAllowlist(t *testing.T) {
	t.Setenv(lfxAdminLoginsEnv, "staff-tester, other-admin")
	now := time.Now().UTC()
	s := &server{
		sessions:   newSessionStore(log.New(io.Discard, "", 0)),
		cookieName: defaultSessionCookieName,
		logger:     log.New(io.Discard, "", 0),
	}
	s.sessions.Set(session{
		ID:        "staff-session",
		Login:     "staff-tester",
		Role:      roleStaff,
		CreatedAt: now,
		ExpiresAt: now.Add(time.Hour),
	})

	req := httptest.NewRequest(http.MethodGet, "/api/lfx/enrichment/access", nil)
	req.AddCookie(&http.Cookie{Name: s.cookieName, Value: "staff-session"})
	rec := httptest.NewRecorder()
	s.requireSession(http.HandlerFunc(s.handleLFXEnrichmentAccess)).ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var response lfxEnrichmentAccessResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&response))
	assert.True(t, response.CanRun)
	assert.Equal(t, []string{"other-admin", "staff-tester"}, response.AllowedLogins)
}

func TestLFXEnrichmentRunStartsAsyncAndRecordsProgress(t *testing.T) {
	t.Setenv(lfxAdminLoginsEnv, "staff-tester")
	dbConn := setupPostgresTestDB(t)
	store := db.NewSQLStore(dbConn)
	now := time.Now().UTC()

	staff := model.StaffMember{
		Name:          "Staff Tester",
		GitHubAccount: "staff-tester",
		Email:         "staff@example.org",
	}
	require.NoError(t, dbConn.Create(&staff).Error)
	alice := model.Maintainer{
		Name:          "Alice Example",
		Email:         "alice@example.org",
		GitHubAccount: "alice-example",
	}
	require.NoError(t, dbConn.Create(&alice).Error)

	project := model.Project{
		Name:      "Project Enrichment",
		Maturity:  model.Sandbox,
		GitHubOrg: "project-enrichment",
	}
	require.NoError(t, dbConn.Create(&project).Error)
	require.NoError(t, dbConn.Model(&project).Association("Maintainers").Append(&alice))

	s := &server{
		store:      store,
		sessions:   newSessionStore(log.New(io.Discard, "", 0)),
		cookieName: defaultSessionCookieName,
		logger:     log.New(io.Discard, "", 0),
		lfxRuns:    newLFXEnrichmentRunStore(),
		newLFXClient: func(_, _ string, _, _ time.Duration) lfxEnrichmentClient {
			return fakeLFXEnrichmentClient{}
		},
	}
	s.sessions.Set(session{
		ID:        "staff-session",
		Login:     staff.GitHubAccount,
		Role:      roleStaff,
		CreatedAt: now,
		ExpiresAt: now.Add(time.Hour),
	})

	req := httptest.NewRequest(http.MethodPost, "/api/lfx/enrichment/runs", strings.NewReader(`{"token":"short-lived","requestsPerSecond":4,"maxLookups":5,"checkFoundationCsv":false}`))
	req.AddCookie(&http.Cookie{Name: s.cookieName, Value: "staff-session"})
	rec := httptest.NewRecorder()
	s.requireSession(http.HandlerFunc(s.handleLFXEnrichmentRuns)).ServeHTTP(rec, req)

	require.Equal(t, http.StatusAccepted, rec.Code)
	var created lfxEnrichmentRun
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&created))
	require.NotEmpty(t, created.ID)
	assert.Equal(t, 4.0, created.RequestsPerSecond)
	assert.Equal(t, "250ms", created.RequestDelay)

	require.Eventually(t, func() bool {
		run, ok := s.lfxRuns.Get(created.ID)
		return ok && run.Status == lfxRunSucceeded && run.Matched == 1
	}, 2*time.Second, 20*time.Millisecond)

	run, ok := s.lfxRuns.Get(created.ID)
	require.True(t, ok)
	assert.Equal(t, 1, run.Total)
	assert.Equal(t, 1, run.Processed)
	assert.Equal(t, 1, run.Attempted)

	var observation model.MaintainerIdentityObservation
	require.NoError(t, dbConn.Where("source = ? AND maintainer_id = ?", "lfx", alice.ID).First(&observation).Error)
	assert.Equal(t, "003-alice", observation.SourceUserID)
	assert.Equal(t, "exact", observation.Confidence)

	var audit model.AuditLog
	require.NoError(t, dbConn.Where("action = ?", "LFX_ENRICHMENT_RUN_SUCCEEDED").First(&audit).Error)
	assert.NotContains(t, audit.Metadata, "short-lived")
}

func TestBuildDotProjectGistRowsUsesCachedSyncState(t *testing.T) {
	dbConn := setupPostgresTestDB(t)
	count := uint(2)
	project := model.Project{
		Name:                      "Example",
		Maturity:                  model.Sandbox,
		DotProjectProjectRef:      "https://github.com/example/.project/blob/main/project.yaml",
		DotProjectMaintainerRef:   "https://github.com/example/.project/blob/main/maintainers.yaml",
		DotProjectSecurityRef:     "https://github.com/example/.project/blob/main/SECURITY.md",
		DotProjectMaintainerCount: &count,
	}
	require.NoError(t, dbConn.Create(&project).Error)
	parseErr := "maintainers.yaml: maintainers must contain at least one entry"
	require.NoError(t, dbConn.Create(&model.DotProjectSyncState{
		ProjectID:              project.ID,
		RepoExists:             true,
		ProjectFileExists:      true,
		MaintainersFileExists:  true,
		MaintainersFilename:    "maintainers.yaml",
		SecurityFileExists:     true,
		ContributingFileExists: false,
		GovernanceFileExists:   false,
		ParseError:             &parseErr,
	}).Error)

	s := &server{store: db.NewSQLStore(dbConn)}
	rows, err := s.buildDotProjectGistRows()
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "Example", rows[0].ProjectName)
	assert.Equal(t, "https://github.com/example/.project/blob/main/project.yaml", rows[0].ProjectFileURL)
	assert.Equal(t, "https://github.com/example/.project/blob/main/maintainers.yaml", rows[0].MaintainersFileURL)
	require.NotNil(t, rows[0].MaintainerCount)
	assert.Equal(t, uint(2), *rows[0].MaintainerCount)
	assert.Contains(t, rows[0].Warning, "maintainers must contain at least one entry")
}

func TestLFXEnrichmentRunRejectsNonAllowedStaff(t *testing.T) {
	t.Setenv(lfxAdminLoginsEnv, "other-admin")
	now := time.Now().UTC()
	s := &server{
		sessions:   newSessionStore(log.New(io.Discard, "", 0)),
		cookieName: defaultSessionCookieName,
		logger:     log.New(io.Discard, "", 0),
		lfxRuns:    newLFXEnrichmentRunStore(),
		newLFXClient: func(_, _ string, _, _ time.Duration) lfxEnrichmentClient {
			return fakeLFXEnrichmentClient{}
		},
	}
	s.sessions.Set(session{
		ID:        "staff-session",
		Login:     "staff-tester",
		Role:      roleStaff,
		CreatedAt: now,
		ExpiresAt: now.Add(time.Hour),
	})

	req := httptest.NewRequest(http.MethodPost, "/api/lfx/enrichment/runs", strings.NewReader(`{"token":"short-lived"}`))
	req.AddCookie(&http.Cookie{Name: s.cookieName, Value: "staff-session"})
	rec := httptest.NewRecorder()
	s.requireSession(http.HandlerFunc(s.handleLFXEnrichmentRuns)).ServeHTTP(rec, req)

	require.Equal(t, http.StatusForbidden, rec.Code)
}

func TestHandleAuditFiltersAndExcludesSyncRunsByDefault(t *testing.T) {
	dbConn := setupPostgresTestDB(t)
	store := db.NewSQLStore(dbConn)
	now := time.Now()

	project := model.Project{Name: "Cedar", Maturity: model.Sandbox}
	require.NoError(t, dbConn.Create(&project).Error)

	other := model.Project{Name: "Other Project", Maturity: model.Sandbox}
	require.NoError(t, dbConn.Create(&other).Error)

	old := now.Add(-48 * time.Hour)
	recent := now.Add(-time.Hour)

	logs := []model.AuditLog{
		{Action: "PROJECT_MATURITY_UPDATE", ProjectID: &project.ID, Message: "old entry"},
		{Action: "PROJECT_MATURITY_UPDATE", ProjectID: &other.ID, Message: "other project"},
		{Action: actionDotProjectSyncRunStarted, Message: "sync started"},
		{Action: actionDotProjectSyncRunFinished, Message: "sync finished"},
	}
	for i := range logs {
		require.NoError(t, dbConn.Create(&logs[i]).Error)
	}
	require.NoError(t, dbConn.Model(&model.AuditLog{}).Where("id = ?", logs[0].ID).Update("created_at", old).Error)
	require.NoError(t, dbConn.Model(&model.AuditLog{}).Where("id IN ?", []uint{logs[1].ID, logs[2].ID, logs[3].ID}).Update("created_at", recent).Error)

	s := &server{
		store:      store,
		sessions:   newSessionStore(log.New(io.Discard, "", 0)),
		cookieName: defaultSessionCookieName,
		logger:     log.New(io.Discard, "", 0),
	}
	staffSessionID := "staff-session"
	s.sessions.Set(session{
		ID:        staffSessionID,
		Login:     "staff-tester",
		Role:      roleStaff,
		CreatedAt: now,
		ExpiresAt: now.Add(time.Hour),
	})

	doRequest := func(query string) auditListResponse {
		req := httptest.NewRequest(http.MethodGet, "/api/audit?"+query, nil)
		req.AddCookie(&http.Cookie{Name: s.cookieName, Value: staffSessionID})
		rec := httptest.NewRecorder()
		s.requireSession(http.HandlerFunc(s.handleAudit)).ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var response auditListResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))
		return response
	}

	t.Run("excludes sync run actions by default", func(t *testing.T) {
		response := doRequest("limit=50")
		assert.Equal(t, int64(2), response.Total)
		for _, entry := range response.Logs {
			assert.NotEqual(t, actionDotProjectSyncRunStarted, entry.Action)
			assert.NotEqual(t, actionDotProjectSyncRunFinished, entry.Action)
		}
	})

	t.Run("action filter can select sync run actions", func(t *testing.T) {
		response := doRequest("action=" + actionDotProjectSyncRunStarted)
		require.Equal(t, int64(1), response.Total)
		assert.Equal(t, actionDotProjectSyncRunStarted, response.Logs[0].Action)
	})

	t.Run("filters by start and end time", func(t *testing.T) {
		startTime := now.Add(-2 * time.Hour).Format(time.RFC3339)
		response := doRequest("startTime=" + url.QueryEscape(startTime))
		require.Equal(t, int64(1), response.Total)
		assert.Equal(t, "other project", response.Logs[0].Message)
	})

	t.Run("filters by target project name", func(t *testing.T) {
		response := doRequest("target=" + url.QueryEscape("Cedar"))
		require.Equal(t, int64(1), response.Total)
		assert.Equal(t, project.ID, *response.Logs[0].ProjectID)
	})

	t.Run("filters by numeric target matching project id", func(t *testing.T) {
		response := doRequest(fmt.Sprintf("target=%d", other.ID))
		require.Equal(t, int64(1), response.Total)
		assert.Equal(t, other.ID, *response.Logs[0].ProjectID)
	})
}
