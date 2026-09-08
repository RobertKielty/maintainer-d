package db

import (
	"errors"
	"time"

	"maintainerd/model"

	"go.uber.org/zap"
)

var ErrProjectNotFound = errors.New("project not found")
var ErrProjectExists = errors.New("project already exists")
var ErrCompanyExists = errors.New("company already exists")

type Store interface {
	GetProjectsUsingService(serviceID uint) ([]model.Project, error)
	ListProjects() ([]model.Project, error)
	GetProjectByID(projectID uint) (*model.Project, error)
	GetProjectMapByName() (map[string]model.Project, error)
	ListMaintainers() ([]model.Maintainer, error)
	ListMaintainersWithoutIdentityObservation(source string) ([]model.Maintainer, error)
	GetMaintainersByProject(projectID uint) ([]model.Maintainer, error)
	ListMaintainerProjectStatuses(projectID uint) (map[uint]model.MaintainerStatus, error)
	GetMaintainerProjectStatuses(maintainerID uint) (map[uint]model.MaintainerStatus, error)
	ListMaintainersActiveOnAnyProject(maintainerIDs []uint) (map[uint]bool, error)
	GetProjectRemoteTeamMap(serviceName string) (map[uint]*model.RemoteTeam, error)
	GetRemoteTeamByProject(projectID uint, serviceID uint) (*model.RemoteTeam, error)
	LogAuditEvent(logger *zap.SugaredLogger, event model.AuditLog) error
	GetMaintainerMapByGitHubAccount() (map[string]model.Maintainer, error)
	CreateRemoteTeam(projectID uint, projectName string, serviceID uint, remoteTeamID uint, remoteTeamName string) (*model.RemoteTeam, error)
	UpsertMaintainer(projectID uint, name, email, githubHandle, company string) (*model.Maintainer, error)
	UpsertMaintainerWithIdentity(projectID uint, name, email, githubHandle, company, lfxUserID string) (*model.Maintainer, bool, bool, error)
	CreateCompany(name string) (*model.Company, error)
	UpdateProjectMaturity(projectID uint, maturity model.Maturity) error
	UpdateProjectLegacyMaintainerRef(projectID uint, ref string) error
	UpdateProjectDotProjectMetadata(projectID uint, patch model.Project) error
	PersistDotProjectSync(projectID uint, patch model.Project, state *model.DotProjectSyncState) error
	UpdateMaintainerProjectStatus(maintainerID, projectID uint, status model.MaintainerStatus) error
	UpdateMaintainersProjectStatus(maintainerIDs []uint, projectID uint, status model.MaintainerStatus) error
	GetMaintainerProjectStatus(maintainerID, projectID uint) (model.MaintainerStatus, error)
	UpdateMaintainerGitHubEmail(maintainerID uint, githubEmail string) error
	UpdateMaintainerDetails(maintainerID uint, name, email, github, githubEmail string, location *string, companyID *uint) (*model.Maintainer, error)
	ListCompanies() ([]model.Company, error)
	ListStaffMembers() ([]model.StaffMember, error)
	ListServiceInvitations(projectID uint, serviceID uint) ([]model.ServiceInvitation, error)
	ListServiceInvitationsByStatus(serviceID uint, statuses []string) ([]model.ServiceInvitation, error)
	UpsertServiceInvitation(invite *model.ServiceInvitation) (*model.ServiceInvitation, error)
	GetServiceInvitationByID(id uint) (*model.ServiceInvitation, error)
	DeleteServiceInvitation(id uint) error
	ListRemoteTeamMaintainers(teamID uint) ([]model.Maintainer, error)
	UpsertRemoteUser(user *model.RemoteUser) (*model.RemoteUser, error)
	UpsertRemoteUserTeam(link *model.RemoteTeamUser) (*model.RemoteTeamUser, error)
	GetMaintainerRefCache(projectID uint) (*model.MaintainerRefCache, error)
	UpsertMaintainerRefCache(cache *model.MaintainerRefCache) error
	GetSanitizeRunStatus() (*model.SanitizeRunStatus, error)
	UpsertSanitizeRunStatus(status *model.SanitizeRunStatus) error
	GetDotProjectSyncState(projectID uint) (*model.DotProjectSyncState, error)
	UpsertDotProjectSyncState(state *model.DotProjectSyncState) error
	UpsertMaintainerIdentityObservation(observation *model.MaintainerIdentityObservation) (*model.MaintainerIdentityObservation, error)
	AdoptMaintainerIdentityObservations(maintainerID, projectID uint, sourceRef string) (int64, error)
	RetireStaleMaintainerIdentityObservations(source string, projectID *uint, sourceRef string, keepSourceUserIDs []string, observedBefore time.Time) (int64, error)
	GetLatestMaintainerIdentityObservation(source string, maintainerID uint) (*model.MaintainerIdentityObservation, error)
	GetLatestMaintainerIdentityObservationByRef(source string, projectID uint, sourceRef string) (*model.MaintainerIdentityObservation, error)
	ListMaintainerIdentityObservations(maintainerID uint) ([]model.MaintainerIdentityObservation, error)
	MergeCompanies(fromID, toID uint) error
}
