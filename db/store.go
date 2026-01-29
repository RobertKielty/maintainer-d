package db

import (
	"errors"

	"maintainerd/model"

	"go.uber.org/zap"
)

var ErrProjectNotFound = errors.New("project not found")
var ErrProjectExists = errors.New("project already exists")
var ErrCompanyExists = errors.New("company already exists")

type Store interface {
	GetProjectsUsingService(serviceID uint) ([]model.Project, error)
	GetProjectByID(projectID uint) (*model.Project, error)
	GetProjectMapByName() (map[string]model.Project, error)
	GetMaintainersByProject(projectID uint) ([]model.Maintainer, error)
	GetProjectServiceTeamMap(serviceName string) (map[uint]*model.ServiceTeam, error)
	GetMaintainerMapByEmail() (map[string]model.Maintainer, error)
	GetServiceTeamByProject(projectID uint, serviceID uint) (*model.ServiceTeam, error)
	LogAuditEvent(logger *zap.SugaredLogger, event model.AuditLog) error
	GetMaintainerMapByGitHubAccount() (map[string]model.Maintainer, error)
	CreateServiceTeamForUser(interface{ any }) (*model.ServiceTeam, error)
	CreateMaintainer(projectID uint, name, email, githubHandle, company string) (*model.Maintainer, error)
	CreateCompany(name string) (*model.Company, error)
	UpdateProjectMaturity(projectID uint, maturity model.Maturity) error
	UpdateProjectLegacyMaintainerRef(projectID uint, ref string) error
	UpdateProjectParentProjectID(projectID uint, parentProjectID *uint) error
	UpdateMaintainerStatus(maintainerID uint, status model.MaintainerStatus) error
	UpdateMaintainersStatus(ids []uint, status model.MaintainerStatus) error
	UpdateMaintainerGitHubEmail(maintainerID uint, githubEmail string) error
	UpdateMaintainerDetails(maintainerID uint, name, email, github string, status model.MaintainerStatus, companyID *uint) (*model.Maintainer, error)
	ListCompanies() ([]model.Company, error)
	ListStaffMembers() ([]model.StaffMember, error)
	GetMaintainerRefCache(projectID uint) (*model.MaintainerRefCache, error)
	UpsertMaintainerRefCache(cache *model.MaintainerRefCache) error
	MergeCompanies(fromID, toID uint) error
}
