package db

import (
	"context"
	"errors"
	"fmt"
	"log"
	"maintainerd/geo"
	"maintainerd/model"
	"strings"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type SQLStore struct {
	db *gorm.DB
}

func NewSQLStore(db *gorm.DB) *SQLStore {
	return &SQLStore{db: db}
}

// DB returns the underlying gorm DB handle for read-only queries.
func (s *SQLStore) DB() *gorm.DB {
	return s.db
}

// Ping verifies the underlying database connection is healthy.
func (s *SQLStore) Ping(ctx context.Context) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("sql store is not initialized")
	}
	sqlDB, err := s.db.DB()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return sqlDB.PingContext(ctx)
}

// getServiceByName returns a &Service the service identified by name
func (s *SQLStore) getServiceByName(name string) (*model.Service, error) {
	var svc model.Service
	err := s.db.Where("name = ?", name).First(&svc).Error
	return &svc, err
}
func (s *SQLStore) GetProjectsUsingService(serviceID uint) ([]model.Project, error) {
	var projects []model.Project
	err := s.db.
		Joins("JOIN remote_teams st ON st.project_id = projects.id").
		Where("st.service_id = ?", serviceID).
		Preload("Maintainers.Company").
		Find(&projects).Error
	return projects, err
}

func (s *SQLStore) GetProjectByID(projectID uint) (*model.Project, error) {
	var project model.Project
	err := s.db.
		Preload("Maintainers").
		Preload("Maintainers.Company").
		Preload("Services").
		First(&project, projectID).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrProjectNotFound
		}
		return nil, err
	}
	return &project, nil
}

// ListProjects returns all projects without preloading associations.
// ListProjects orders by dot_project_last_synced_at ascending (nulls/never-
// synced first). dot-project-sync iterates this list under a run-level
// deadline and stops early if it runs out of time (see Syncer.SyncAll) - a
// stable order here would let the same tail of projects starve every run;
// ordering by staleness means a run that stops early always skips the
// projects it most recently refreshed.
func (s *SQLStore) ListProjects() ([]model.Project, error) {
	var projects []model.Project
	err := s.db.Order("dot_project_last_synced_at ASC NULLS FIRST").Find(&projects).Error
	return projects, err
}

// ListProjectsWithMaintainers returns all projects with maintainer associations preloaded.
func (s *SQLStore) ListProjectsWithMaintainers() ([]model.Project, error) {
	var projects []model.Project
	err := s.db.Preload("Maintainers").Preload("Maintainers.Company").Find(&projects).Error
	return projects, err
}

func (s *SQLStore) ListMaintainers() ([]model.Maintainer, error) {
	var maintainers []model.Maintainer
	err := s.db.Preload("Company").Find(&maintainers).Error
	return maintainers, err
}

func (s *SQLStore) ListMaintainersWithoutIdentityObservation(source string) ([]model.Maintainer, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return nil, fmt.Errorf("identity observation source is required")
	}
	var maintainers []model.Maintainer
	err := s.db.
		Preload("Company").
		Where(`
			not exists (
				select 1
				from maintainer_identity_observations mio
				where mio.maintainer_id = maintainers.id
				  and mio.source = ?
				  and mio.deleted_at is null
			)
		`, source).
		Find(&maintainers).Error
	return maintainers, err
}

func (s *SQLStore) UpdateProjectLegacyMaintainerRef(projectID uint, ref string) error {
	result := s.db.Model(&model.Project{}).
		Where("id = ?", projectID).
		Update("maintainer_ref", ref)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrProjectNotFound
	}
	return nil
}

func (s *SQLStore) UpdateProjectMaturity(projectID uint, maturity model.Maturity) error {
	result := s.db.Model(&model.Project{}).
		Where("id = ?", projectID).
		Update("maturity", maturity)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrProjectNotFound
	}
	return nil
}

func (s *SQLStore) UpdateProjectDotProjectMetadata(projectID uint, patch model.Project) error {
	result := s.db.Model(&model.Project{}).
		Where("id = ?", projectID).
		Updates(dotProjectProjectUpdates(patch))
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrProjectNotFound
	}
	return nil
}

func (s *SQLStore) PersistDotProjectSync(projectID uint, patch model.Project, state *model.DotProjectSyncState) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&model.Project{}).
			Where("id = ?", projectID).
			Updates(dotProjectProjectUpdates(patch))
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return ErrProjectNotFound
		}
		if state != nil {
			if err := tx.Save(state).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

func dotProjectProjectUpdates(patch model.Project) map[string]interface{} {
	return map[string]interface{}{
		"dot_project_repo_ref":         strings.TrimSpace(patch.DotProjectRepoRef),
		"dot_project_project_ref":      strings.TrimSpace(patch.DotProjectProjectRef),
		"dot_project_yaml_ref":         strings.TrimSpace(patch.DotProjectMaintainerRef),
		"dot_project_security_ref":     strings.TrimSpace(patch.DotProjectSecurityRef),
		"dot_project_contributing_ref": strings.TrimSpace(patch.DotProjectContributingRef),
		"dot_project_governance_ref":   strings.TrimSpace(patch.DotProjectGovernanceRef),
		"dot_project_schema_version":   strings.TrimSpace(patch.DotProjectSchemaVersion),
		"dot_project_maintainer_count": patch.DotProjectMaintainerCount,
		"dot_project_last_synced_at":   patch.DotProjectLastSyncedAt,
		"dot_project_adoption_status":  strings.TrimSpace(patch.DotProjectAdoptionStatus),
	}
}

func (s *SQLStore) UpsertMaintainer(projectID uint, name, email, githubHandle, company string) (*model.Maintainer, error) {
	maintainer, _, _, err := s.UpsertMaintainerWithIdentity(projectID, name, email, githubHandle, company, "")
	return maintainer, err
}

func (s *SQLStore) UpsertMaintainerWithIdentity(projectID uint, name, email, githubHandle, company, lfxUserID string) (*model.Maintainer, bool, bool, error) {
	tx := s.db.Begin()
	if tx.Error != nil {
		return nil, false, false, tx.Error
	}
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
		}
	}()

	var project model.Project
	if err := tx.First(&project, projectID).Error; err != nil {
		tx.Rollback()
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, false, false, ErrProjectNotFound
		}
		return nil, false, false, err
	}

	var maintainer model.Maintainer
	if githubHandle != "" && !strings.EqualFold(githubHandle, "GITHUB_MISSING") {
		result := tx.Where("LOWER(git_hub_account) = ?", strings.ToLower(githubHandle)).
			Limit(1).
			Find(&maintainer)
		if result.Error != nil {
			tx.Rollback()
			return nil, false, false, result.Error
		}
	}
	if maintainer.ID == 0 && email != "" && !strings.EqualFold(email, "EMAIL_MISSING") {
		result := tx.Where("LOWER(email) = ?", strings.ToLower(email)).
			Limit(1).
			Find(&maintainer)
		if result.Error != nil {
			tx.Rollback()
			return nil, false, false, result.Error
		}
	}

	var companyModel *model.Company
	if company = strings.TrimSpace(company); company != "" {
		var c model.Company
		if err := tx.Where(model.Company{Name: company}).FirstOrCreate(&c).Error; err != nil {
			tx.Rollback()
			return nil, false, false, err
		}
		companyModel = &c
	}

	created := false
	if maintainer.ID == 0 {
		maintainer = model.Maintainer{
			Name:          name,
			Email:         normalizeOrSentinel(email, "EMAIL_MISSING"),
			GitHubAccount: normalizeOrSentinel(githubHandle, "GITHUB_MISSING"),
			GitHubEmail:   "GITHUB_EMAIL_MISSING",
			LFXUserID:     strings.TrimSpace(lfxUserID),
		}
		if companyModel != nil {
			maintainer.CompanyID = &companyModel.ID
			maintainer.Company = *companyModel
		}
		if err := tx.Create(&maintainer).Error; err != nil {
			tx.Rollback()
			return nil, false, false, err
		}
		created = true
	}

	var existingLink int64
	if err := tx.Model(&model.MaintainerProject{}).
		Where("maintainer_id = ? AND project_id = ?", maintainer.ID, project.ID).
		Count(&existingLink).Error; err != nil {
		tx.Rollback()
		return nil, false, false, err
	}
	linked := existingLink == 0
	if linked {
		if err := tx.Model(&maintainer).Association("Projects").Append(&project); err != nil {
			tx.Rollback()
			return nil, false, false, err
		}
	}

	if err := tx.Commit().Error; err != nil {
		return nil, false, false, err
	}

	finalName := maintainer.Name
	if strings.TrimSpace(finalName) == "" && strings.TrimSpace(name) != "" {
		finalName = name
	}
	finalGitHub := maintainer.GitHubAccount
	if finalGitHub == "" || finalGitHub == "GITHUB_MISSING" {
		if strings.TrimSpace(githubHandle) != "" {
			finalGitHub = githubHandle
		}
	}
	finalEmail := maintainer.Email
	if finalEmail == "" || finalEmail == "EMAIL_MISSING" {
		if strings.TrimSpace(email) != "" {
			finalEmail = email
		}
	}
	finalCompanyID := maintainer.CompanyID
	if finalCompanyID == nil && companyModel != nil {
		finalCompanyID = &companyModel.ID
	}
	updatedMaintainer, err := s.UpdateMaintainerDetails(maintainer.ID, finalName, finalEmail, finalGitHub, maintainer.GitHubEmail, maintainer.Location, finalCompanyID)
	if err != nil {
		return nil, false, false, err
	}
	lfxUserID = strings.TrimSpace(lfxUserID)
	if lfxUserID != "" && strings.TrimSpace(updatedMaintainer.LFXUserID) == "" {
		if err := s.db.Model(&model.Maintainer{}).
			Where("id = ?", updatedMaintainer.ID).
			Update("lfx_user_id", lfxUserID).Error; err != nil {
			return nil, false, false, err
		}
		updatedMaintainer.LFXUserID = lfxUserID
	}
	return updatedMaintainer, created, linked, nil
}

func normalizeOrSentinel(value, sentinel string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return sentinel
	}
	return trimmed
}

// stringPtrToUpdateValue converts a *string into the value a GORM Updates map needs: an
// explicit literal nil clears the column, while a typed-nil *string left in the map would not
// (GORM sees a non-nil interface{} wrapping a nil pointer, not an explicit NULL).
func stringPtrToUpdateValue(v *string) any {
	if v == nil {
		return nil
	}
	return *v
}

// CreateCompany creates or retrieves a company by name.
func (s *SQLStore) CreateCompany(name string) (*model.Company, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return nil, fmt.Errorf("company name is required")
	}
	var existing model.Company
	if err := s.db.Where("LOWER(name) = ?", strings.ToLower(trimmed)).First(&existing).Error; err == nil {
		return nil, ErrCompanyExists
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}

	company := model.Company{Name: trimmed}
	if err := s.db.Create(&company).Error; err != nil {
		return nil, err
	}
	return &company, nil
}

func (s *SQLStore) GetMaintainersByProject(projectID uint) ([]model.Maintainer, error) {
	var project model.Project
	err := s.db.
		Preload("Maintainers.Company").
		First(&project, projectID).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrProjectNotFound
		}
		return nil, err
	}
	return project.Maintainers, nil

}

// ListMaintainerProjectStatuses returns each maintainer's status on the given project, keyed
// by maintainer ID.
func (s *SQLStore) ListMaintainerProjectStatuses(projectID uint) (map[uint]model.MaintainerStatus, error) {
	var links []model.MaintainerProject
	if err := s.db.Where("project_id = ?", projectID).Find(&links).Error; err != nil {
		return nil, err
	}
	statuses := make(map[uint]model.MaintainerStatus, len(links))
	for _, link := range links {
		statuses[link.MaintainerID] = link.Status
	}
	return statuses, nil
}

// GetMaintainerProjectStatuses returns a maintainer's status on each project they belong to,
// keyed by project ID.
func (s *SQLStore) GetMaintainerProjectStatuses(maintainerID uint) (map[uint]model.MaintainerStatus, error) {
	var links []model.MaintainerProject
	if err := s.db.Where("maintainer_id = ?", maintainerID).Find(&links).Error; err != nil {
		return nil, err
	}
	statuses := make(map[uint]model.MaintainerStatus, len(links))
	for _, link := range links {
		statuses[link.ProjectID] = link.Status
	}
	return statuses, nil
}

// ListMaintainersActiveOnAnyProject reports, for each of the given maintainer IDs, whether
// they are Active on at least one project. This is the semantics used for project-agnostic
// views (search, LFX enrichment) where a single global status no longer exists.
func (s *SQLStore) ListMaintainersActiveOnAnyProject(maintainerIDs []uint) (map[uint]bool, error) {
	active := make(map[uint]bool, len(maintainerIDs))
	if len(maintainerIDs) == 0 {
		return active, nil
	}
	var activeIDs []uint
	if err := s.db.Model(&model.MaintainerProject{}).
		Distinct("maintainer_id").
		Where("maintainer_id IN ? AND status = ?", maintainerIDs, model.ActiveMaintainer).
		Pluck("maintainer_id", &activeIDs).Error; err != nil {
		return nil, err
	}
	for _, id := range activeIDs {
		active[id] = true
	}
	return active, nil
}

// UpdateMaintainerProjectStatus updates a maintainer's status on a single project, leaving
// their status on every other project untouched.
func (s *SQLStore) UpdateMaintainerProjectStatus(maintainerID, projectID uint, status model.MaintainerStatus) error {
	if !status.IsValid() {
		return fmt.Errorf("invalid maintainer status %q", status)
	}
	result := s.db.Model(&model.MaintainerProject{}).
		Where("maintainer_id = ? AND project_id = ?", maintainerID, projectID).
		Update("status", status)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

// UpdateMaintainersProjectStatus updates multiple maintainers' status on a single project.
func (s *SQLStore) UpdateMaintainersProjectStatus(maintainerIDs []uint, projectID uint, status model.MaintainerStatus) error {
	if len(maintainerIDs) == 0 {
		return nil
	}
	if !status.IsValid() {
		return fmt.Errorf("invalid maintainer status %q", status)
	}
	return s.db.Model(&model.MaintainerProject{}).
		Where("maintainer_id IN ? AND project_id = ?", maintainerIDs, projectID).
		Update("status", status).Error
}

// GetMaintainerProjectStatus returns a maintainer's status on a single project.
func (s *SQLStore) GetMaintainerProjectStatus(maintainerID, projectID uint) (model.MaintainerStatus, error) {
	var link model.MaintainerProject
	err := s.db.
		Where("maintainer_id = ? AND project_id = ?", maintainerID, projectID).
		First(&link).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", gorm.ErrRecordNotFound
	}
	if err != nil {
		return "", err
	}
	return link.Status, nil
}

// UpdateMaintainerDetails updates a maintainer's editable fields and returns the updated record.
func (s *SQLStore) UpdateMaintainerDetails(maintainerID uint, name, email, github, githubEmail string, location *string, companyID *uint) (*model.Maintainer, error) {
	if companyID != nil {
		var company model.Company
		if err := s.db.First(&company, *companyID).Error; err != nil {
			return nil, err
		}
	}

	var existing model.Maintainer
	if err := s.db.First(&existing, maintainerID).Error; err != nil {
		return nil, err
	}

	rawLocation := ""
	if location != nil {
		rawLocation = strings.TrimSpace(*location)
	}
	previous := geo.ResolvedLocation{Location: existing.Location, Country: existing.Country, Timezone: existing.Timezone}
	resolved := geo.ResolveLocation(rawLocation, previous)

	updates := map[string]interface{}{
		"name":            strings.TrimSpace(name),
		"email":           normalizeOrSentinel(email, "EMAIL_MISSING"),
		"git_hub_account": normalizeOrSentinel(github, "GITHUB_MISSING"),
		"git_hub_email":   normalizeOrSentinel(githubEmail, "GITHUB_MISSING"),
		"location":        stringPtrToUpdateValue(resolved.Location),
		"country":         stringPtrToUpdateValue(resolved.Country),
		"timezone":        stringPtrToUpdateValue(resolved.Timezone),
		"company_id":      companyID,
	}

	if err := s.db.Model(&model.Maintainer{}).
		Where("id = ?", maintainerID).
		Updates(updates).Error; err != nil {
		return nil, err
	}

	var maintainer model.Maintainer
	if err := s.db.Preload("Company").Preload("Projects").First(&maintainer, maintainerID).Error; err != nil {
		return nil, err
	}
	return &maintainer, nil
}

func (s *SQLStore) GetRemoteTeamByProject(projectID, serviceID uint) (*model.RemoteTeam, error) {
	var st model.RemoteTeam
	err := s.db.
		Where("project_id = ? AND service_id = ?", projectID, serviceID).
		First(&st).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &st, err
}

// GetMaintainerRefCache returns the cached metadata for a project's maintainer ref, or nil if none.
func (s *SQLStore) GetMaintainerRefCache(projectID uint) (*model.MaintainerRefCache, error) {
	var cache model.MaintainerRefCache
	err := s.db.Where("project_id = ?", projectID).First(&cache).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &cache, nil
}

// UpsertMaintainerRefCache inserts or updates maintainer ref cache metadata.
func (s *SQLStore) UpsertMaintainerRefCache(cache *model.MaintainerRefCache) error {
	if cache == nil {
		return nil
	}
	return s.db.Save(cache).Error
}

// GetSanitizeRunStatus returns the singleton sanitize run-status row, or nil
// if the sanitize job has never recorded a completed run.
func (s *SQLStore) GetSanitizeRunStatus() (*model.SanitizeRunStatus, error) {
	var status model.SanitizeRunStatus
	err := s.db.Where("id = ?", 1).First(&status).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &status, nil
}

// UpsertSanitizeRunStatus records the sanitize job's most recent completed
// run. It always writes to the singleton row (ID=1).
func (s *SQLStore) UpsertSanitizeRunStatus(status *model.SanitizeRunStatus) error {
	if status == nil {
		return nil
	}
	status.ID = 1
	return s.db.Save(status).Error
}

// GetDotProjectSyncState returns the cached sync metadata for a project's
// dot-project repository, or nil if none exists.
func (s *SQLStore) GetDotProjectSyncState(projectID uint) (*model.DotProjectSyncState, error) {
	var state model.DotProjectSyncState
	err := s.db.Where("project_id = ?", projectID).First(&state).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &state, nil
}

// UpsertDotProjectSyncState inserts or updates dot-project sync metadata.
func (s *SQLStore) UpsertDotProjectSyncState(state *model.DotProjectSyncState) error {
	if state == nil {
		return nil
	}
	return s.db.Save(state).Error
}

// MergeCompanies reassigns all maintainers from fromID to toID and deletes the source company.
func (s *SQLStore) MergeCompanies(fromID, toID uint) error {
	if fromID == toID {
		return fmt.Errorf("fromID and toID must differ")
	}
	return s.db.Transaction(func(tx *gorm.DB) error {
		// Ensure target exists.
		var target model.Company
		if err := tx.First(&target, toID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return fmt.Errorf("target company %d not found", toID)
			}
			return err
		}
		// Ensure source exists.
		var source model.Company
		if err := tx.First(&source, fromID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return fmt.Errorf("source company %d not found", fromID)
			}
			return err
		}

		if err := tx.Model(&model.Maintainer{}).Where("company_id = ?", fromID).Update("company_id", toID).Error; err != nil {
			return err
		}
		if err := tx.Delete(&model.Company{}, fromID).Error; err != nil {
			return err
		}
		return nil
	})
}

// GetMaintainerMapByGitHubAccount returns a map of Maintainers keyed by GitHub Account
func (s *SQLStore) GetMaintainerMapByGitHubAccount() (map[string]model.Maintainer, error) {
	var maintainers []model.Maintainer
	err := s.db.Preload("Company").Find(&maintainers).Error
	if err != nil {
		return nil, err
	}
	m := make(map[string]model.Maintainer)
	for _, maintainer := range maintainers {
		key := strings.ToLower(strings.TrimSpace(maintainer.GitHubAccount))
		if key == "" {
			continue
		}
		m[key] = maintainer
	}
	return m, nil
}

// GetProjectRemoteTeamMap returns a map of projectID to RemoteTeams
// for every Project that uses the service identified by serviceId
func (s *SQLStore) GetProjectRemoteTeamMap(serviceName string) (map[uint]*model.RemoteTeam, error) {
	var serviceTeams []model.RemoteTeam
	service, err := s.getServiceByName(serviceName)
	if err != nil {
		return nil, fmt.Errorf("failed to get service, %s, by name: %v", serviceName, err)
	}
	// Preload the many-to-many relationship
	err = s.db.
		Where("service_id = ? ", service.ID).
		Find(&serviceTeams).Error
	if err != nil {
		return nil, fmt.Errorf("querying RemoteTeam for service_id %d: %w", service.ID, err)
	}

	result := make(map[uint]*model.RemoteTeam, len(serviceTeams))

	for i := range serviceTeams {
		st := &serviceTeams[i]
		result[st.ProjectID] = st
	}

	return result, nil

}
func (s *SQLStore) GetProjectMapByName() (map[string]model.Project, error) {
	var projects []model.Project
	if err := s.db.
		Preload("Maintainers").
		Preload("Maintainers.Company").
		Find(&projects).Error; err != nil {
		return nil, err
	}

	projectsByName := make(map[string]model.Project)
	for _, p := range projects {
		projectsByName[p.Name] = p
	}
	return projectsByName, nil
}

func (s *SQLStore) LogAuditEvent(logger *zap.SugaredLogger, event model.AuditLog) error {
	if event.Message == "" {
		event.Message = event.Action
	}

	err := s.db.WithContext(context.Background()).Create(&event).Error
	if err != nil {
		logger.Errorf("failed to write %v audit log: %v", event, err)
	}
	return err
}

// CreateRemoteTeam creates or retrieves a service team entry in the database based on the provided project and service details.
// It accepts a project ID, project name, service ID, and service name as input and returns the service team or an error.
func (s *SQLStore) CreateRemoteTeam(
	projectID uint, projectName string,
	serviceID uint, remoteTeamID uint, remoteTeamName string) (*model.RemoteTeam, error) {

	var errMessages []string

	st := &model.RemoteTeam{
		RemoteTeamID:   remoteTeamID,
		ServiceID:      serviceID,
		RemoteTeamName: &remoteTeamName,
		ProjectID:      projectID,
		ProjectName:    &projectName,
	}
	err := s.db.Where("remote_team_id = ? AND service_id = ?", remoteTeamID, serviceID).FirstOrCreate(st).Error
	if err != nil {
		msg := fmt.Sprintf("CreateRemoteTeamsForUser: failed for team %d (%s): %v", remoteTeamID, remoteTeamName, err)
		log.Println(msg)
		return nil, fmt.Errorf("CreateRemoteTeamsForUser had partial errors:\n%s", strings.Join(errMessages, "\n"))
	}
	return st, nil
}

// ListCompanies returns all companies in the database.
func (s *SQLStore) ListCompanies() ([]model.Company, error) {
	var companies []model.Company
	if err := s.db.Find(&companies).Error; err != nil {
		return nil, err
	}
	return companies, nil
}

// ListStaffMembers returns all staff members in the database, including their foundations.
func (s *SQLStore) ListStaffMembers() ([]model.StaffMember, error) {
	var staffMembers []model.StaffMember
	if err := s.db.Preload("Foundation").Find(&staffMembers).Error; err != nil {
		return nil, err
	}
	return staffMembers, nil
}

// ListServiceInvitations returns service invitations for a project and service.
func (s *SQLStore) ListServiceInvitations(projectID uint, serviceID uint) ([]model.ServiceInvitation, error) {
	var invites []model.ServiceInvitation
	if err := s.db.
		Where("project_id = ? AND service_id = ?", projectID, serviceID).
		Order("created_at desc").
		Find(&invites).Error; err != nil {
		return nil, err
	}
	return invites, nil
}

// ListServiceInvitationsByStatus returns invite rows matching any of the provided statuses for a service.
func (s *SQLStore) ListServiceInvitationsByStatus(serviceID uint, statuses []string) ([]model.ServiceInvitation, error) {
	var invites []model.ServiceInvitation
	if len(statuses) == 0 {
		return invites, nil
	}
	if err := s.db.
		Where("service_id = ? AND status IN ?", serviceID, statuses).
		Order("created_at asc").
		Find(&invites).Error; err != nil {
		return nil, err
	}
	return invites, nil
}

// UpsertServiceInvitation inserts or updates a service invitation record.
func (s *SQLStore) UpsertServiceInvitation(invite *model.ServiceInvitation) (*model.ServiceInvitation, error) {
	if invite == nil {
		return nil, fmt.Errorf("invite is nil")
	}
	err := s.db.Unscoped().Clauses(clause.OnConflict{
		Columns: []clause.Column{
			{Name: "project_id"},
			{Name: "service_id"},
			{Name: "service_email"},
		},
		DoUpdates: clause.AssignmentColumns([]string{
			"maintainer_id",
			"remote_team_id",
			"status",
			"team_assignment_status",
			"team_add_attempts",
			"next_team_add_at",
			"last_error",
			"sent_at",
			"last_checked_at",
			"updated_at",
			"deleted_at",
		}),
	}).Create(invite).Error
	if err != nil {
		return nil, err
	}
	return invite, nil
}

// GetServiceInvitationByID loads a single invitation by ID.
func (s *SQLStore) GetServiceInvitationByID(id uint) (*model.ServiceInvitation, error) {
	var invite model.ServiceInvitation
	if err := s.db.First(&invite, id).Error; err != nil {
		return nil, err
	}
	return &invite, nil
}

// DeleteServiceInvitation removes a service invitation record.
func (s *SQLStore) DeleteServiceInvitation(id uint) error {
	return s.db.Delete(&model.ServiceInvitation{}, id).Error
}

// ListRemoteTeamMaintainers returns maintainers linked to the service team.
func (s *SQLStore) ListRemoteTeamMaintainers(teamID uint) ([]model.Maintainer, error) {
	var maintainers []model.Maintainer
	if err := s.db.
		Model(&model.Maintainer{}).
		Distinct("maintainers.*").
		Joins("JOIN remote_team_users rtu ON rtu.maintainer_id = maintainers.id").
		Where("rtu.team_id = ?", teamID).
		Order("maintainers.name asc").
		Find(&maintainers).Error; err != nil {
		return nil, err
	}
	return maintainers, nil
}

// UpsertRemoteUser inserts or updates a service user record keyed by service + remote_user_id.
func (s *SQLStore) UpsertRemoteUser(user *model.RemoteUser) (*model.RemoteUser, error) {
	if user == nil {
		return nil, fmt.Errorf("service user is nil")
	}
	var existing model.RemoteUser
	err := s.db.
		Where("service_id = ? AND remote_user_id = ?", user.ServiceID, user.RemoteUserID).
		First(&existing).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			if createErr := s.db.Create(user).Error; createErr != nil {
				return nil, createErr
			}
			return user, nil
		}
		return nil, err
	}
	existing.ServiceEmail = user.ServiceEmail
	existing.RemoteRef = user.RemoteRef
	existing.ServiceGitHubName = user.ServiceGitHubName
	if saveErr := s.db.Save(&existing).Error; saveErr != nil {
		return nil, saveErr
	}
	return &existing, nil
}

// UpsertRemoteUserTeam inserts or updates a service user team link.
func (s *SQLStore) UpsertRemoteUserTeam(link *model.RemoteTeamUser) (*model.RemoteTeamUser, error) {
	if link == nil {
		return nil, fmt.Errorf("service user team link is nil")
	}
	query := s.db.Where(
		"service_id = ? AND team_id = ? AND user_id = ? AND maintainer_id IS NOT DISTINCT FROM ?",
		link.ServiceID,
		link.TeamID,
		link.UserID,
		link.MaintainerID,
	)
	var existing model.RemoteTeamUser
	err := query.First(&existing).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			if createErr := s.db.Create(link).Error; createErr != nil {
				return nil, createErr
			}
			return link, nil
		}
		return nil, err
	}
	existing.CollaboratorID = link.CollaboratorID
	if saveErr := s.db.Save(&existing).Error; saveErr != nil {
		return nil, saveErr
	}
	return &existing, nil
}

func (s *SQLStore) UpsertMaintainerIdentityObservation(observation *model.MaintainerIdentityObservation) (*model.MaintainerIdentityObservation, error) {
	if observation == nil {
		return nil, fmt.Errorf("maintainer identity observation is required")
	}
	observation.Source = strings.TrimSpace(observation.Source)
	observation.SourceRef = strings.TrimSpace(observation.SourceRef)
	observation.SourceUserID = strings.TrimSpace(observation.SourceUserID)
	observation.Name = strings.TrimSpace(observation.Name)
	observation.Email = strings.TrimSpace(observation.Email)
	observation.GitHubUser = strings.TrimSpace(observation.GitHubUser)
	observation.LFID = strings.TrimSpace(observation.LFID)
	observation.CompanyName = strings.TrimSpace(observation.CompanyName)
	observation.CompanyRef = strings.TrimSpace(observation.CompanyRef)
	observation.MatchStatus = strings.TrimSpace(observation.MatchStatus)
	observation.MatchReason = strings.TrimSpace(observation.MatchReason)
	observation.Confidence = strings.TrimSpace(observation.Confidence)
	observation.RawPayload = strings.TrimSpace(observation.RawPayload)
	if observation.RawPayload == "" {
		observation.RawPayload = "{}"
	}
	if observation.Source == "" {
		return nil, fmt.Errorf("maintainer identity observation source is required")
	}
	if observation.ObservedAt.IsZero() {
		observation.ObservedAt = time.Now().UTC()
	}

	matchQuery := func(maintainerClause string, maintainerArgs ...interface{}) *gorm.DB {
		query := s.db.Where("source = ?", observation.Source).Where(maintainerClause, maintainerArgs...)
		if observation.ProjectID == nil {
			query = query.Where("project_id IS NULL")
		} else {
			query = query.Where("project_id = ?", *observation.ProjectID)
		}
		if observation.SourceUserID != "" {
			query = query.Where("source_user_id = ?", observation.SourceUserID)
		} else {
			query = query.Where("source_user_id = '' AND source_ref = ?", observation.SourceRef)
		}
		return query
	}

	var existing model.MaintainerIdentityObservation
	if observation.MaintainerID == nil {
		if err := matchQuery("maintainer_id IS NULL").Find(&existing).Error; err != nil {
			return nil, err
		}
	} else {
		if err := matchQuery("maintainer_id = ?", *observation.MaintainerID).Find(&existing).Error; err != nil {
			return nil, err
		}
		if existing.ID == 0 {
			// Adopt a row written before the maintainer existed (the
			// observation is recorded earlier in the sync loop than the
			// maintainer is created), instead of leaving it orphaned with a
			// NULL maintainer_id and inserting a duplicate.
			if err := matchQuery("maintainer_id IS NULL").Find(&existing).Error; err != nil {
				return nil, err
			}
		}
	}
	if existing.ID == 0 {
		if err := s.db.Create(observation).Error; err != nil {
			return nil, err
		}
		return observation, nil
	}

	existing.MaintainerID = observation.MaintainerID
	existing.SourceRef = observation.SourceRef
	existing.SourceUserID = observation.SourceUserID
	existing.Name = observation.Name
	existing.Email = observation.Email
	existing.GitHubUser = observation.GitHubUser
	existing.LFID = observation.LFID
	existing.CompanyName = observation.CompanyName
	existing.CompanyRef = observation.CompanyRef
	existing.MatchStatus = observation.MatchStatus
	existing.MatchReason = observation.MatchReason
	existing.Confidence = observation.Confidence
	existing.RawPayload = observation.RawPayload
	existing.ObservedAt = observation.ObservedAt
	existing.SourceUserType = observation.SourceUserType
	existing.SourceGitHubID = observation.SourceGitHubID
	existing.SourceLastModifiedAt = observation.SourceLastModifiedAt
	existing.IdentityCount = observation.IdentityCount
	existing.SourceFilePath = observation.SourceFilePath
	existing.SourceLine = observation.SourceLine
	existing.SourceCommitSHA = observation.SourceCommitSHA
	existing.SourceLineURL = observation.SourceLineURL
	existing.SourcePRNumber = observation.SourcePRNumber
	existing.SourcePRURL = observation.SourcePRURL
	existing.SourceReviewState = observation.SourceReviewState
	if err := s.db.Save(&existing).Error; err != nil {
		return nil, err
	}
	return &existing, nil
}

// AdoptMaintainerIdentityObservations attaches observation rows recorded
// before a maintainer existed (maintainer_id NULL) to the maintainer that was
// subsequently created for the same project and source ref, so the evidence
// that justified creating the maintainer is not orphaned.
func (s *SQLStore) AdoptMaintainerIdentityObservations(maintainerID, projectID uint, sourceRef string) (int64, error) {
	sourceRef = strings.TrimSpace(sourceRef)
	if maintainerID == 0 {
		return 0, fmt.Errorf("maintainer id is required")
	}
	if sourceRef == "" {
		return 0, fmt.Errorf("source ref is required")
	}
	result := s.db.Model(&model.MaintainerIdentityObservation{}).
		Where("maintainer_id IS NULL AND project_id = ? AND source_ref = ?", projectID, sourceRef).
		Update("maintainer_id", maintainerID)
	return result.RowsAffected, result.Error
}

// RetireStaleMaintainerIdentityObservations deletes source_user_id-keyed
// rows for one candidate (source, project, sourceRef) that are absent from
// the latest lookup and older than observedBefore. An enrichment upsert only
// ever touches the profiles a given search actually returned, so a profile
// that used to match and no longer does (LFX resolved a duplicate, a
// maintainer's identity changed) would otherwise linger forever, inflating
// the profile count and keeping a stale "chosen"/"duplicate" badge alive.
// keepSourceUserIDs lists the profiles the current lookup did return; pass
// an empty slice to retire every previously recorded profile for this
// candidate (the current lookup found none). Rows with no SourceUserID
// (unmatched/no-handle observations) are never touched - they aren't part
// of the profile set this retires.
func (s *SQLStore) RetireStaleMaintainerIdentityObservations(source string, projectID *uint, sourceRef string, keepSourceUserIDs []string, observedBefore time.Time) (int64, error) {
	source = strings.TrimSpace(source)
	sourceRef = strings.TrimSpace(sourceRef)
	if source == "" {
		return 0, fmt.Errorf("maintainer identity observation source is required")
	}
	if sourceRef == "" {
		return 0, fmt.Errorf("source ref is required")
	}
	query := s.db.Where("source = ? AND source_ref = ? AND source_user_id != '' AND observed_at < ?", source, sourceRef, observedBefore)
	if projectID == nil {
		query = query.Where("project_id IS NULL")
	} else {
		query = query.Where("project_id = ?", *projectID)
	}
	if len(keepSourceUserIDs) > 0 {
		query = query.Where("source_user_id NOT IN ?", keepSourceUserIDs)
	}
	result := query.Delete(&model.MaintainerIdentityObservation{})
	return result.RowsAffected, result.Error
}

func (s *SQLStore) GetLatestMaintainerIdentityObservation(source string, maintainerID uint) (*model.MaintainerIdentityObservation, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return nil, fmt.Errorf("maintainer identity observation source is required")
	}
	var observation model.MaintainerIdentityObservation
	err := s.db.
		Where("source = ? AND maintainer_id = ?", source, maintainerID).
		Order("observed_at DESC, id DESC").
		Find(&observation).Error
	if err != nil {
		return nil, err
	}
	if observation.ID == 0 {
		return nil, nil
	}
	return &observation, nil
}

func (s *SQLStore) GetLatestMaintainerIdentityObservationByRef(source string, projectID uint, sourceRef string) (*model.MaintainerIdentityObservation, error) {
	source = strings.TrimSpace(source)
	sourceRef = strings.TrimSpace(sourceRef)
	if source == "" {
		return nil, fmt.Errorf("maintainer identity observation source is required")
	}
	if sourceRef == "" {
		return nil, fmt.Errorf("maintainer identity observation source ref is required")
	}
	var observation model.MaintainerIdentityObservation
	err := s.db.
		Where("source = ? AND project_id = ? AND source_ref = ?", source, projectID, sourceRef).
		Order("observed_at DESC, id DESC").
		Find(&observation).Error
	if err != nil {
		return nil, err
	}
	if observation.ID == 0 {
		return nil, nil
	}
	return &observation, nil
}

func (s *SQLStore) ListMaintainerIdentityObservations(maintainerID uint) ([]model.MaintainerIdentityObservation, error) {
	var observations []model.MaintainerIdentityObservation
	err := s.db.
		Where("maintainer_id = ?", maintainerID).
		Order("observed_at DESC, id DESC").
		Find(&observations).Error
	return observations, err
}

// IsStaffGitHubAccount returns true if the GitHub account belongs to a staff member.
func (s *SQLStore) IsStaffGitHubAccount(githubAccount string) (bool, error) {
	if githubAccount == "" {
		return false, nil
	}
	var count int64
	err := s.db.
		Model(&model.StaffMember{}).
		Where("LOWER(git_hub_account) = ?", strings.ToLower(githubAccount)).
		Count(&count).Error
	return count > 0, err
}
