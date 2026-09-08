package model

import (
	"database/sql/driver"
	"fmt"
	"net/url"
	"time"

	"gorm.io/gorm"
)

type MaintainerStatus string

const (
	ActiveMaintainer   MaintainerStatus = "Active"
	EmeritusMaintainer MaintainerStatus = "Emeritus"
	RetiredMaintainer  MaintainerStatus = "Retired"
	ArchivedMaintainer MaintainerStatus = "Archived"
)

// IsValid returns true id MaintainerStatus is known
func (s MaintainerStatus) IsValid() bool {
	switch s {
	case ActiveMaintainer, EmeritusMaintainer, RetiredMaintainer, ArchivedMaintainer:
		return true
	}
	return false
}

// OrDefault returns s if it is a known status, otherwise fallback. Use this when
// reading a per-project status that may be empty (join row never wrote a status)
// or otherwise invalid, so callers don't silently treat a maintainer as inactive.
func (s MaintainerStatus) OrDefault(fallback MaintainerStatus) MaintainerStatus {
	if s.IsValid() {
		return s
	}
	return fallback
}

func (s *MaintainerStatus) Scan(value interface{ any }) error {
	v, ok := value.(string)
	if !ok {
		return fmt.Errorf("cannot scan %T into MaintainerStatus", value)
	}
	*s = MaintainerStatus(v)
	return nil
}

func (s MaintainerStatus) Value() (driver.Value, error) {
	if !s.IsValid() {
		return nil, fmt.Errorf("invalid MaintainerStatus %q", s)
	}
	return string(s), nil
}

func (m *Maturity) Scan(value interface{ any }) error {
	v, ok := value.(string)
	if !ok {
		return fmt.Errorf("cannot scan %T into Maturity", value)
	}
	*m = Maturity(v)
	return nil
}

type Maturity string

const (
	Sandbox    Maturity = "Sandbox"
	Incubating Maturity = "Incubating"
	Graduated  Maturity = "Graduated"
	Archived   Maturity = "Archived"
)

func (m Maturity) Value() (driver.Value, error) {
	if !m.IsValid() {
		return nil, fmt.Errorf("invalid Maturity %q", m)
	}
	return string(m), nil
}

func (m Maturity) IsValid() bool {
	switch m {
	case Sandbox, Incubating, Graduated, Archived:
		return true
	}
	return false
}

// A Maintainer is a leader that can speak for a Project
//
// At registration, an email needs to be provided
// Optionally, a Maintainer
//
//		has a Company Affiliation
//	  	For kubernetes specifically, a maintainer may or may not have voting rights on a Project,
//	    has a status of Active, Emeritus, Retired or Archived, tracked per-project on MaintainerProject.Status
type Maintainer struct {
	gorm.Model
	Name          string
	Email         string `gorm:"size:254;default:EMAIL_MISSING"` // Primary/Work Email
	GitHubAccount string `gorm:"size:100;default:GITHUB_MISSING"`
	GitHubEmail   string `gorm:"size:100;default:GITHUB_MISSING"` // Email used for Git Commits on GitHub
	LFXUserID     string `gorm:"size:128;index"`
	// Deprecated: status is per-project now (see MaintainerProject.Status). This field is
	// retained only as a migration safety net and is no longer read or written.
	MaintainerStatus MaintainerStatus `gorm:"type:text;default:Active"`
	ImportWarnings   string
	Location         *string   `gorm:"size:255"`
	Country          *string   `gorm:"size:2"`
	Timezone         *string   `gorm:"size:64"`
	Projects         []Project `gorm:"many2many:maintainer_projects;joinForeignKey:MaintainerID;joinReferences:ProjectID"`
	RegisteredAt     *time.Time
	CompanyID        *uint
	Company          Company
}

// MaintainerRefCache stores fetch metadata for a project's maintainer reference file.
type MaintainerRefCache struct {
	ProjectID    uint   `gorm:"primaryKey"`
	ETag         string `gorm:"size:255"`
	LastModified *time.Time
	BodyHash     string `gorm:"size:128"` // sha256 hex
	Body         string `gorm:"type:text"`
	LastChecked  *time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// SanitizeRunStatus is a singleton row (ID=1) recording when the sanitize
// reconciliation job (which periodically re-checks each project's maintainer
// reference file and flips per-project maintainer status) last completed, so
// the UI can tell users how fresh that status is and when it will next be
// re-checked.
type SanitizeRunStatus struct {
	ID              uint `gorm:"primaryKey"`
	LastRunAt       time.Time
	IntervalSeconds int
}

// DotProjectSyncState stores sync metadata for the files discovered in a
// project's .project repository.
type DotProjectSyncState struct {
	ProjectID uint `gorm:"primaryKey"`

	RepoExists             bool `gorm:"index"`
	ProjectFileExists      bool
	MaintainersFileExists  bool
	SecurityFileExists     bool
	ContributingFileExists bool
	GovernanceFileExists   bool

	DefaultBranch       string `gorm:"size:255"`
	MaintainersFilename string `gorm:"size:255"`
	SchemaVersion       string `gorm:"size:64"`
	ImporterVersion     string `gorm:"size:64"`

	ProjectFileETag          string  `gorm:"size:255"`
	MaintainersFileETag      string  `gorm:"size:255"`
	SecurityFileETag         string  `gorm:"size:255"`
	ContributingFileETag     string  `gorm:"size:255"`
	GovernanceFileETag       string  `gorm:"size:255"`
	ProjectFileBodyHash      string  `gorm:"size:128"` // sha256 hex
	MaintainersFileBodyHash  string  `gorm:"size:128"` // sha256 hex
	MaintainersFileBody      *string `gorm:"type:text"`
	SecurityFileBodyHash     string  `gorm:"size:128"` // sha256 hex
	ContributingFileBodyHash string  `gorm:"size:128"` // sha256 hex
	GovernanceFileBodyHash   string  `gorm:"size:128"` // sha256 hex

	LastCheckedAt *time.Time `gorm:"index"`
	SyncError     *string    `gorm:"type:text"`
	ParseError    *string    `gorm:"type:text"`
	CreatedAt     time.Time
	UpdatedAt     time.Time
}
type Collaborator struct {
	gorm.Model
	Name          string
	Email         string  `gorm:"size:254;default:EMAIL_MISSING"`
	GitHubEmail   *string `gorm:"size:254;default:GITHUB_EMAIL_MISSING"`
	GitHubAccount *string `gorm:"size:100;default:GITHUB_MISSING"`
	LastLogin     time.Time
	RegisteredAt  time.Time
}
type Project struct {
	gorm.Model
	Name                string `gorm:"uniqueIndex,not null;check:name <> ''"`
	ParentProjectID     *uint  `gorm:"index"`
	Maturity            Maturity
	GitHubOrg           string `gorm:"size:255"`
	LegacyMaintainerRef string `gorm:"column:maintainer_ref"`
	// Keep the legacy column name for the first migration phase while the
	// logical field name moves to dot-project maintainer terminology.
	DotProjectRepoRef    string `gorm:"size:1024"`
	DotProjectProjectRef string `gorm:"size:1024"`
	// Keep this legacy DB column as text so migrations do not fight the
	// generated search_tsv column on refreshed production snapshots.
	DotProjectMaintainerRef   string `gorm:"column:dot_project_yaml_ref;type:text"`
	DotProjectSecurityRef     string `gorm:"size:1024"`
	DotProjectContributingRef string `gorm:"size:1024"`
	DotProjectGovernanceRef   string `gorm:"size:1024"`
	DotProjectSchemaVersion   string `gorm:"size:64"`
	DotProjectMaintainerCount *uint
	DotProjectLastSyncedAt    *time.Time `gorm:"index"`
	DotProjectAdoptionStatus  string     `gorm:"size:64"`
	OnboardingIssue           *string
	MailingList               *string      `gorm:"size:254;default:MML_MISSING"`
	Maintainers               []Maintainer `gorm:"many2many:maintainer_projects;joinForeignKey:ProjectID;joinReferences:MaintainerID"`
	Services                  []Service    `gorm:"many2many:service_projects;joinForeignKey:ProjectID;joinReferences:ServiceID"`
}

type MaintainerProject struct {
	MaintainerID uint             `gorm:"primaryKey;index"` // FK + index
	ProjectID    uint             `gorm:"primaryKey;index"` // FK + index
	Status       MaintainerStatus `gorm:"type:text;default:Active"`
	JoinedAt     time.Time        `gorm:"autoCreateTime"`
	Maintainer   Maintainer       `gorm:"foreignKey:MaintainerID;constraint:OnDelete:CASCADE"`
	Project      Project          `gorm:"foreignKey:ProjectID;constraint:OnDelete:CASCADE"`
}

type MaintainerIdentityObservation struct {
	gorm.Model
	MaintainerID *uint `gorm:"index"`
	ProjectID    *uint `gorm:"index"`

	Source string `gorm:"size:64;index"`
	// SourceRef is the stable identity key an observation is upserted on
	// (db.UpsertMaintainerIdentityObservation falls back to matching on
	// source_ref whenever SourceUserID is empty - e.g. "github:<handle>" for
	// foundation-csv). It must never encode a location such as a line number:
	// doing so would mint a new row every time the underlying file reorders.
	// Use the SourceLine/SourceLineURL fields below for location instead.
	SourceRef    string `gorm:"size:512;index"`
	SourceUserID string `gorm:"size:128;index"`

	Name        string `gorm:"size:255"`
	Email       string `gorm:"size:254"`
	GitHubUser  string `gorm:"size:100"`
	LFID        string `gorm:"size:100"`
	CompanyName string `gorm:"size:255"`
	CompanyRef  string `gorm:"size:128"`

	// SourceUserType, SourceGitHubID, SourceLastModifiedAt, and IdentityCount
	// let a maintainer's route present every LFX profile bound to their GitHub
	// handle side by side, since LFX has no 1:1 mapping between profile IDs
	// and GitHub identities (see lfx/LFX-USER-API-NOTES.MD finding 8).
	SourceUserType       string     `gorm:"size:32;index"` // lead | contact
	SourceGitHubID       string     `gorm:"size:100"`
	SourceLastModifiedAt *time.Time `gorm:"index"`
	// IdentityCount is a pointer so rows recorded before the column existed
	// stay NULL (never measured) instead of scanning as a measured zero -
	// zero linked identities is itself a signal in the UI.
	IdentityCount *int

	// SourceFilePath, SourceLine, SourceCommitSHA, and SourceLineURL let an
	// investigator navigate from an observation to the exact reviewed line
	// that produced it. SourcePRNumber/SourcePRURL/SourceReviewState record
	// whether a human gatekeeper reviewed that line before it was accepted -
	// the actual evidentiary signal behind a file-based match, as opposed to
	// mere presence in the file (see provenance package).
	SourceFilePath    string `gorm:"size:512"`
	SourceLine        int
	SourceCommitSHA   string `gorm:"size:64"`
	SourceLineURL     string `gorm:"size:1024"`
	SourcePRNumber    int
	SourcePRURL       string `gorm:"size:512"`
	SourceReviewState string `gorm:"size:32;index"` // approved | unreviewed | direct-push | unknown

	MatchStatus string    `gorm:"size:32;index"`
	MatchReason string    `gorm:"size:255"`
	Confidence  string    `gorm:"size:32"`
	RawPayload  string    `gorm:"type:jsonb"`
	ObservedAt  time.Time `gorm:"index"`
}

type Company struct {
	gorm.Model
	Name string `gorm:"uniqueIndex"`
}

// A Foundation represents an organization that employs Staff members working with
// CNCF projects (e.g., CNCF and LF).
type Foundation struct {
	gorm.Model
	Name string `gorm:"uniqueIndex"`
}

// A StaffMember is a CNCF/LF staff member who may perform operations across projects.
// Staff members are associated with a Foundation (not a Project).
type StaffMember struct {
	gorm.Model
	Name          string
	Email         string `gorm:"size:254;default:EMAIL_MISSING"`
	GitHubAccount string `gorm:"size:100;default:GITHUB_MISSING"`
	GitHubEmail   string `gorm:"size:254;default:GITHUB_EMAIL_MISSING"`
	RegisteredAt  *time.Time

	FoundationID *uint `gorm:"index"`
	Foundation   Foundation
}
type Service struct {
	gorm.Model
	Name        string `gorm:"uniqueIndex"`
	Description string
}

type RemoteTeamUser struct {
	gorm.Model

	ServiceID uint `gorm:"index"` // This may be redundant — if already tracked via foreign keys below

	TeamID uint `gorm:"index"` // FK to RemoteTeam (local DB ID)
	UserID uint `gorm:"index"` // FK to RemoteUser (local DB ID)

	MaintainerID   *uint `gorm:"index"` // nullable FK to Maintainer
	CollaboratorID *uint `gorm:"index"` // nullable FK to Collaborator
}

type RemoteTeam struct {
	gorm.Model
	ProjectID      uint `gorm:"index"` // FK to project
	ServiceID      uint `gorm:"index"` // FK to service
	RemoteTeamID   uint // ID on the remote service (e.g., FOSSA team ID)
	RemoteTeamName *string
	ProjectName    *string // De-normalised for debugging purposes
}

type RemoteUser struct {
	gorm.Model
	ServiceID         uint   `gorm:"index"` // FK to Service
	RemoteUserID      uint   `gorm:"index"` // ID on the remote service
	ServiceEmail      string `gorm:"size:254;default:EMAIL_MISSING"`
	RemoteRef         string `gorm:"size:512"`
	ServiceGitHubName *string
}

// Orchestrates Inviations maintainer-d sends to Maintaienrs to join services
type ServiceInvitation struct {
	gorm.Model
	ServiceID    uint   `gorm:"index;uniqueIndex:idx_service_invite_project_email"` // FK to Service
	RemoteTeamID uint   `gorm:"not null"`                                           // ID on the remote service (e.g., FOSSA team ID)
	ProjectID    uint   `gorm:"index;uniqueIndex:idx_service_invite_project_email"` // FK to project
	MaintainerID *uint  `gorm:"index"`
	ServiceEmail string `gorm:"size:254;not null;uniqueIndex:idx_service_invite_project_email"`
	Status       string `gorm:"size:32;index"` // pending, accepted, expired, error
	// TeamAssignmentStatus tracks membership status after invite acceptance: pending, done, error.
	TeamAssignmentStatus *string    `gorm:"size:32;index"`
	TeamAddAttempts      int        `gorm:"default:0"`
	NextTeamAddAt        *time.Time `gorm:"index"`
	SentAt               *time.Time `gorm:"index"`
	LastCheckedAt        *time.Time `gorm:"index"`
	LastError            *string    `gorm:"type:text"`
}

// A FoundationOfficer is a person who has elevated access to
// Services to carry out Maintainer Operations on behalf of the
// Foundation that governs projects.
type FoundationOfficer struct {
	gorm.Model
	Name          string
	Email         string `gorm:"size:254;default:EMAIL_MISSING"`
	GitHubAccount string `gorm:"size:100;default:GITHUB_MISSING"`
	RegisteredAt  *time.Time
	CompanyID     *uint
	// Services represent user identities on external services (e.g., FOSSA) that
	// this officer can operate as. This is a many-to-many relationship because a
	// service user could (in theory) be shared across officers.
	Services []RemoteUser `gorm:"many2many:foundation_officer_service_users;constraint:OnDelete:CASCADE"`
}

type ReconciliationResult struct {
	gorm.Model
	Service              Service
	ProjectID            *uint
	MissingMaintainerIDs []*uint
}

// ProjectInfo is an in-memory cache. TODO Review this
type ProjectInfo struct {
	Project     Project
	Maintainers []Maintainer
	Services    []Service
}

type AuditLog struct {
	gorm.Model
	ProjectID    *uint        `gorm:"index"`
	MaintainerID *uint        `gorm:"index"`
	ServiceID    *uint        `gorm:"index"`
	StaffID      *uint        `gorm:"index"`
	Staff        *StaffMember `gorm:"foreignKey:StaffID"`
	Action       string       `gorm:"index"` // e.g. "ADD_MEMBER", "REMOVE_MEMBER", "INVITE_SENT"
	Message      string       // human-readable message, optional
	Metadata     string       // optional JSON blob for advanced inspection
}

type OnboardingTask struct {
	Name        string    `json:"name"`
	Owner       string    `json:"owner"`
	Number      int       `json:"number"`
	Complete    bool      `json:"competed"`
	Issue       url.URL   `json:"issue"`
	CollectedAt time.Time `json:"collected_at"`
}
