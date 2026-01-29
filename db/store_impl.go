package db

import (
	"context"
	"errors"
	"fmt"
	"log"
	"maintainerd/model"
	"strings"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"
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
		Joins("JOIN service_teams st ON st.project_id = projects.id").
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

// ListProjectsWithMaintainers returns all projects with maintainer associations preloaded.
func (s *SQLStore) ListProjectsWithMaintainers() ([]model.Project, error) {
	var projects []model.Project
	err := s.db.Preload("Maintainers").Preload("Maintainers.Company").Find(&projects).Error
	return projects, err
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

func (s *SQLStore) UpdateProjectParentProjectID(projectID uint, parentProjectID *uint) error {
	result := s.db.Model(&model.Project{}).
		Where("id = ?", projectID).
		Update("parent_project_id", parentProjectID)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrProjectNotFound
	}
	return nil
}

func (s *SQLStore) CreateMaintainer(projectID uint, name, email, githubHandle, company string) (*model.Maintainer, error) {
	tx := s.db.Begin()
	if tx.Error != nil {
		return nil, tx.Error
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
			return nil, ErrProjectNotFound
		}
		return nil, err
	}

	var maintainer model.Maintainer
	if githubHandle != "" {
		if err := tx.Where("LOWER(git_hub_account) = ?", strings.ToLower(githubHandle)).
			First(&maintainer).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			tx.Rollback()
			return nil, err
		}
	}
	if maintainer.ID == 0 && email != "" {
		if err := tx.Where("LOWER(email) = ?", strings.ToLower(email)).
			First(&maintainer).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			tx.Rollback()
			return nil, err
		}
	}

	var companyModel *model.Company
	if company != "" {
		var c model.Company
		if err := tx.Where("name = ?", company).FirstOrCreate(&c).Error; err != nil {
			tx.Rollback()
			return nil, err
		}
		companyModel = &c
	}

	if maintainer.ID == 0 {
		maintainer = model.Maintainer{
			Name:             name,
			Email:            normalizeOrSentinel(email, "EMAIL_MISSING"),
			GitHubAccount:    normalizeOrSentinel(githubHandle, "GITHUB_MISSING"),
			GitHubEmail:      "GITHUB_MISSING",
			MaintainerStatus: model.ActiveMaintainer,
		}
		if companyModel != nil {
			maintainer.CompanyID = &companyModel.ID
			maintainer.Company = *companyModel
		}
		if err := tx.Create(&maintainer).Error; err != nil {
			tx.Rollback()
			return nil, err
		}
	} else if companyModel != nil && maintainer.CompanyID == nil {
		maintainer.CompanyID = &companyModel.ID
		maintainer.Company = *companyModel
		if err := tx.Save(&maintainer).Error; err != nil {
			tx.Rollback()
			return nil, err
		}
	}

	if err := tx.Model(&maintainer).Association("Projects").Append(&project); err != nil {
		tx.Rollback()
		return nil, err
	}

	if err := tx.Commit().Error; err != nil {
		return nil, err
	}
	return &maintainer, nil
}

func normalizeOrSentinel(value, sentinel string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return sentinel
	}
	return trimmed
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

// UpdateMaintainerStatus updates the MaintainerStatus for a given maintainer.
func (s *SQLStore) UpdateMaintainerStatus(maintainerID uint, status model.MaintainerStatus) error {
	if !status.IsValid() {
		return fmt.Errorf("invalid maintainer status %q", status)
	}
	result := s.db.Model(&model.Maintainer{}).
		Where("id = ?", maintainerID).
		Update("maintainer_status", status)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

// UpdateMaintainersStatus updates multiple maintainers to the given status.
func (s *SQLStore) UpdateMaintainersStatus(ids []uint, status model.MaintainerStatus) error {
	if len(ids) == 0 {
		return nil
	}
	if !status.IsValid() {
		return fmt.Errorf("invalid maintainer status %q", status)
	}
	return s.db.Model(&model.Maintainer{}).
		Where("id IN ?", ids).
		Update("maintainer_status", status).Error
}

// UpdateMaintainerDetails updates a maintainer's editable fields and returns the updated record.
func (s *SQLStore) UpdateMaintainerDetails(maintainerID uint, name, email, github string, status model.MaintainerStatus, companyID *uint) (*model.Maintainer, error) {
	if !status.IsValid() {
		return nil, fmt.Errorf("invalid maintainer status %q", status)
	}

	if companyID != nil {
		var company model.Company
		if err := s.db.First(&company, *companyID).Error; err != nil {
			return nil, err
		}
	}

	updates := map[string]interface{}{
		"name":              strings.TrimSpace(name),
		"email":             normalizeOrSentinel(email, "EMAIL_MISSING"),
		"git_hub_account":   normalizeOrSentinel(github, "GITHUB_MISSING"),
		"maintainer_status": status,
		"company_id":        companyID,
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

func (s *SQLStore) GetServiceTeamByProject(projectID, serviceID uint) (*model.ServiceTeam, error) {
	var st model.ServiceTeam
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

// GetMaintainerMapByEmail returns a map of Maintainers keyed by email address
func (s *SQLStore) GetMaintainerMapByEmail() (map[string]model.Maintainer, error) {
	var maintainers []model.Maintainer
	err := s.db.Preload("Company").Find(&maintainers).Error
	if err != nil {
		return nil, err
	}
	m := make(map[string]model.Maintainer)
	for _, maintainer := range maintainers {
		m[maintainer.Email] = maintainer
	}
	return m, nil
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
		m[maintainer.GitHubAccount] = maintainer
	}
	return m, nil
}

// GetProjectServiceTeamMap returns a map of projectID to ServiceTeams
// for every Project that uses the service identified by serviceId
func (s *SQLStore) GetProjectServiceTeamMap(serviceName string) (map[uint]*model.ServiceTeam, error) {
	var serviceTeams []model.ServiceTeam
	service, err := s.getServiceByName(serviceName)
	if err != nil {
		return nil, fmt.Errorf("failed to get service, %s, by name: %v", serviceName, err)
	}
	// Preload the many-to-many relationship
	err = s.db.
		Where("service_id = ? ", service.ID).
		Find(&serviceTeams).Error
	if err != nil {
		return nil, fmt.Errorf("querying ServiceTeam for service_id %d: %w", service.ID, err)
	}

	result := make(map[uint]*model.ServiceTeam, len(serviceTeams))

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

// CreateServiceTeam creates or retrieves a service team entry in the database based on the provided project and service details.
// It accepts a project ID, project name, service ID, and service name as input and returns the service team or an error.
func (s *SQLStore) CreateServiceTeam(
	projectID uint, projectName string,
	serviceID int, serviceName string) (*model.ServiceTeam, error) {

	var errMessages []string

	st := &model.ServiceTeam{
		ServiceTeamID:   serviceID,
		ServiceID:       1, // TODO : Hardcoded to FOSSA for now
		ServiceTeamName: &serviceName,
		ProjectID:       projectID,
		ProjectName:     &projectName,
	}
	err := s.db.Where("service_team_id = ?", serviceID).FirstOrCreate(st).Error
	if err != nil {
		msg := fmt.Sprintf("CreateServiceTeamsForUser: failed for team %d (%s): %v", serviceID, serviceName, err)
		log.Println(msg)
		return nil, fmt.Errorf("CreateServiceTeamsForUser had partial errors:\n%s", strings.Join(errMessages, "\n"))
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
