package main

import (
	"flag"
	"fmt"
	"log"
	"strings"
	"time"

	"maintainerd/db"
	"maintainerd/geo"
	"maintainerd/model"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func main() {
	dbPath := flag.String("db", "testdata/maintainerd_test.db", "Path to SQLite database file")
	dbDriver := flag.String("driver", "sqlite", "Database driver: sqlite or postgres")
	dbDSN := flag.String("dsn", "", "Postgres DSN (required when driver=postgres)")
	flag.Parse()

	driver := strings.TrimSpace(*dbDriver)
	dsn := strings.TrimSpace(*dbDSN)
	if driver == "postgres" && dsn == "" {
		log.Fatal("seed: driver=postgres requires -dsn")
	}
	if driver != "postgres" && strings.TrimSpace(*dbPath) == "" {
		log.Fatal("seed: sqlite requires -db path")
	}
	if driver != "postgres" {
		dsn = *dbPath
	}

	dbConn, err := db.OpenGorm(driver, dsn, &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		log.Fatalf("seed: failed to open db: %v", err)
	}

	if err := dbConn.AutoMigrate(
		&model.AuditLog{},
		&model.Company{},
		&model.Foundation{},
		&model.Project{},
		&model.Maintainer{},
		&model.StaffMember{},
		&model.FoundationOfficer{},
		&model.Collaborator{},
		&model.MaintainerProject{},
		&model.MaintainerRefCache{},
		&model.DotProjectSyncState{},
		&model.Service{},
		&model.RemoteTeam{},
		&model.RemoteUser{},
		&model.RemoteTeamUser{},
		&model.ServiceInvitation{},
	); err != nil {
		log.Fatalf("seed: auto-migrate failed: %v", err)
	}

	company := model.Company{Name: "Example Labs"}
	foundation := model.Foundation{Name: "CNCF"}
	if err := dbConn.FirstOrCreate(&company, model.Company{Name: company.Name}).Error; err != nil {
		log.Fatalf("seed: company insert failed: %v", err)
	}
	if err := dbConn.FirstOrCreate(&foundation, model.Foundation{Name: foundation.Name}).Error; err != nil {
		log.Fatalf("seed: foundation insert failed: %v", err)
	}

	staff := model.StaffMember{
		Name:          "Staff Tester",
		Email:         "staff.tester@example.test",
		GitHubAccount: "staff-tester",
		GitHubEmail:   "staff.tester@github.test",
		FoundationID:  &foundation.ID,
		RegisteredAt:  timePtr(time.Now()),
	}
	if err := dbConn.FirstOrCreate(&staff, model.StaffMember{GitHubAccount: staff.GitHubAccount}).Error; err != nil {
		log.Fatalf("seed: staff insert failed: %v", err)
	}

	maintainers := []model.Maintainer{
		withLocation("Madrid, Spain", model.Maintainer{
			Name:          "Antonio Example",
			Email:         "antonio.example@test.dev",
			GitHubAccount: "antonio-example",
			GitHubEmail:   "antonio@example.dev",
			CompanyID:     &company.ID,
			RegisteredAt:  timePtr(time.Now()),
		}),
		withLocation("Portland, Oregon, USA", model.Maintainer{
			Name:          "Renee Sample",
			Email:         "renee.sample@example.dev",
			GitHubAccount: "renee-sample",
			GitHubEmail:   "renee@example.dev",
			CompanyID:     &company.ID,
			RegisteredAt:  timePtr(time.Now()),
		}),
		withLocation("London, UK", model.Maintainer{
			Name:          "Alex Example",
			Email:         "alex@example.dev",
			GitHubAccount: "alex-example",
			GitHubEmail:   "alex@example.dev",
			CompanyID:     &company.ID,
			RegisteredAt:  timePtr(time.Now()),
		}),
		withLocation("São Paulo, Brazil", model.Maintainer{
			Name:          "Diego Placeholder",
			Email:         "diego.placeholder@test.dev",
			GitHubAccount: "diego-placeholder",
			GitHubEmail:   "diego@example.dev",
			CompanyID:     &company.ID,
			RegisteredAt:  timePtr(time.Now()),
		}),
		withLocation("Tokyo, Japan", model.Maintainer{
			Name:          "Jun Example",
			Email:         "jun.example@test.dev",
			GitHubAccount: "jun-example",
			GitHubEmail:   "jun@example.dev",
			CompanyID:     &company.ID,
			RegisteredAt:  timePtr(time.Now()),
		}),
		withLocation("Bengaluru, India", model.Maintainer{
			Name:          "Priya Demo",
			Email:         "priya.demo@test.dev",
			GitHubAccount: "priya-demo",
			GitHubEmail:   "priya@example.dev",
			CompanyID:     &company.ID,
			RegisteredAt:  timePtr(time.Now()),
		}),
		withLocation("Yerevan, Armenia", model.Maintainer{
			Name:          "Sam NoEmail",
			Email:         "EMAIL_MISSING",
			GitHubAccount: "sam-noemail",
			GitHubEmail:   "sam@example.dev",
			CompanyID:     &company.ID,
			RegisteredAt:  timePtr(time.Now()),
		}),
	}

	for i := range maintainers {
		if err := dbConn.FirstOrCreate(&maintainers[i], model.Maintainer{GitHubAccount: maintainers[i].GitHubAccount}).Error; err != nil {
			log.Fatalf("seed: maintainer insert failed: %v", err)
		}
	}

	atlasSyncedAt := timePtr(time.Now())

	projects := []model.Project{
		{
			Name:                      "Project Atlas",
			Maturity:                  model.Graduated,
			GitHubOrg:                 "project-atlas",
			DotProjectRepoRef:         "https://github.com/project-atlas/.project",
			DotProjectProjectRef:      "https://github.com/project-atlas/.project/blob/main/project.yaml",
			DotProjectMaintainerRef:   "https://github.com/project-atlas/.project/blob/main/MAINTAINERS.yaml",
			DotProjectSecurityRef:     "https://github.com/project-atlas/.project/blob/main/SECURITY.md",
			DotProjectContributingRef: "https://github.com/project-atlas/.project/blob/main/CONTRIBUTING.md",
			DotProjectGovernanceRef:   "https://github.com/project-atlas/.project/blob/main/GOVERNANCE.md",
			DotProjectSchemaVersion:   "1.0.0",
			DotProjectMaintainerCount: uintPtr(7),
			DotProjectLastSyncedAt:    atlasSyncedAt,
			DotProjectAdoptionStatus:  "adopted",
		},
		{Name: "Project Beacon", Maturity: model.Incubating, DotProjectAdoptionStatus: "not_found"},
		{Name: "Project Comet", Maturity: model.Sandbox},
		{Name: "Project Fossa Full", Maturity: model.Sandbox},
		{Name: "Project Fossa Partial", Maturity: model.Sandbox},
		{Name: "Project Fossa Invites", Maturity: model.Sandbox},
		{Name: "Project Fossa Missing Email", Maturity: model.Sandbox},
		{Name: "Project Snyk", Maturity: model.Sandbox},
		{Name: "Project No License", Maturity: model.Sandbox},
	}

	for i := range projects {
		if err := dbConn.FirstOrCreate(&projects[i], model.Project{Name: projects[i].Name}).Error; err != nil {
			log.Fatalf("seed: project insert failed: %v", err)
		}
	}

	if err := dbConn.Model(&projects[0]).Association("Maintainers").Replace(
		&maintainers[0],
		&maintainers[1],
		&maintainers[2],
	); err != nil {
		log.Fatalf("seed: association failed: %v", err)
	}
	if err := dbConn.Model(&projects[1]).Association("Maintainers").Replace(
		&maintainers[0],
		&maintainers[3],
	); err != nil {
		log.Fatalf("seed: association failed: %v", err)
	}
	if err := dbConn.Model(&projects[2]).Association("Maintainers").Replace(
		&maintainers[0],
		&maintainers[4],
		&maintainers[5],
	); err != nil {
		log.Fatalf("seed: association failed: %v", err)
	}
	// Antonio Example is Active on Project Atlas but Emeritus on Project Comet,
	// giving the maintainer detail page a mixed-status example to render.
	if err := dbConn.Model(&model.MaintainerProject{}).
		Where("maintainer_id = ? AND project_id = ?", maintainers[0].ID, projects[2].ID).
		Update("status", model.EmeritusMaintainer).Error; err != nil {
		log.Fatalf("seed: maintainer project status update failed: %v", err)
	}

	fossa := model.Service{Name: "FOSSA", Description: "License compliance"}
	snyk := model.Service{Name: "Snyk", Description: "License compliance"}
	if err := dbConn.FirstOrCreate(&fossa, model.Service{Name: fossa.Name}).Error; err != nil {
		log.Fatalf("seed: service insert failed: %v", err)
	}
	if err := dbConn.FirstOrCreate(&snyk, model.Service{Name: snyk.Name}).Error; err != nil {
		log.Fatalf("seed: service insert failed: %v", err)
	}

	projectMap := map[string]*model.Project{}
	for i := range projects {
		projectMap[projects[i].Name] = &projects[i]
	}
	atlasSyncState := model.DotProjectSyncState{
		ProjectID:               projectMap["Project Atlas"].ID,
		RepoExists:              true,
		ProjectFileExists:       true,
		MaintainersFileExists:   true,
		SecurityFileExists:      true,
		ContributingFileExists:  false,
		GovernanceFileExists:    false,
		DefaultBranch:           "main",
		MaintainersFilename:     "MAINTAINERS.yaml",
		SchemaVersion:           "1.0.0",
		ImporterVersion:         "seed",
		LastCheckedAt:           atlasSyncedAt,
		ProjectFileETag:         "seed-project",
		MaintainersFileETag:     "seed-maintainers",
		SecurityFileETag:        "seed-security",
		ContributingFileETag:    "seed-contributing",
		GovernanceFileETag:      "seed-governance",
		ProjectFileBodyHash:     "seed-project-hash",
		MaintainersFileBodyHash: "seed-maintainers-hash",
		// Projects now organize maintainers.yaml teams however they see fit
		// (cncf/automation#602), so the fixture mixes the legacy
		// "project-maintainers" name with upstream's "maintainers" scaffold
		// name and other team names, including emeritus/community teams. Until
		// maintainer-d#172 models upstream's `managed` flag, every team here is
		// treated as a maintainer team.
		MaintainersFileBody: strPtr(`# Project Atlas dot-project maintainers
maintainers:
  - teams:
      - name: project-maintainers
        members:
          - antonio-example
          - unmapped-dotproject
      - name: maintainers
        members:
          - renee-sample
      - name: release-team
        members:
          - atlas-release-lead
  - teams:
      - name: emeritus
        members:
          - atlas-emeritus
      - name: emeritus-maintainers
        members:
          - atlas-emeritus-maintainer
      - name: community
        members:
          - atlas-community-member
`),
		SecurityFileBodyHash:     "seed-security-hash",
		ContributingFileBodyHash: "seed-contributing-hash",
		GovernanceFileBodyHash:   "seed-governance-hash",
	}
	if err := dbConn.Where(model.DotProjectSyncState{ProjectID: atlasSyncState.ProjectID}).Assign(atlasSyncState).FirstOrCreate(&atlasSyncState).Error; err != nil {
		log.Fatalf("seed: dot-project sync state insert failed: %v", err)
	}
	if err := dbConn.Model(projectMap["Project Fossa Full"]).Association("Maintainers").Replace(
		&maintainers[0],
		&maintainers[1],
	); err != nil {
		log.Fatalf("seed: association failed: %v", err)
	}
	if err := dbConn.Model(projectMap["Project Fossa Partial"]).Association("Maintainers").Replace(
		&maintainers[0],
		&maintainers[1],
		&maintainers[2],
		&maintainers[3],
	); err != nil {
		log.Fatalf("seed: association failed: %v", err)
	}
	if err := dbConn.Model(projectMap["Project Fossa Invites"]).Association("Maintainers").Replace(
		&maintainers[4],
		&maintainers[5],
	); err != nil {
		log.Fatalf("seed: association failed: %v", err)
	}
	if err := dbConn.Model(projectMap["Project Fossa Missing Email"]).Association("Maintainers").Replace(
		&maintainers[6],
	); err != nil {
		log.Fatalf("seed: association failed: %v", err)
	}
	if err := dbConn.Model(projectMap["Project Snyk"]).Association("Maintainers").Replace(
		&maintainers[0],
	); err != nil {
		log.Fatalf("seed: association failed: %v", err)
	}
	if err := dbConn.Model(projectMap["Project No License"]).Association("Maintainers").Replace(
		&maintainers[1],
	); err != nil {
		log.Fatalf("seed: association failed: %v", err)
	}

	if err := dbConn.Model(projectMap["Project Fossa Full"]).Association("Services").Append(&fossa); err != nil {
		log.Fatalf("seed: service association failed: %v", err)
	}
	if err := dbConn.Model(projectMap["Project Fossa Partial"]).Association("Services").Append(&fossa); err != nil {
		log.Fatalf("seed: service association failed: %v", err)
	}
	if err := dbConn.Model(projectMap["Project Fossa Invites"]).Association("Services").Append(&fossa); err != nil {
		log.Fatalf("seed: service association failed: %v", err)
	}
	if err := dbConn.Model(projectMap["Project Fossa Missing Email"]).Association("Services").Append(&fossa); err != nil {
		log.Fatalf("seed: service association failed: %v", err)
	}
	if err := dbConn.Model(projectMap["Project Snyk"]).Association("Services").Append(&snyk); err != nil {
		log.Fatalf("seed: service association failed: %v", err)
	}

	teamNameFull := "Project Fossa Full"
	teamNamePartial := "Project Fossa Partial"
	teamNameInvites := "Project Fossa Invites"
	teamNameMissing := "Project Fossa Missing Email"

	teams := []model.RemoteTeam{
		{ProjectID: projectMap[teamNameFull].ID, ServiceID: fossa.ID, RemoteTeamID: 101, RemoteTeamName: &teamNameFull, ProjectName: &teamNameFull},
		{ProjectID: projectMap[teamNamePartial].ID, ServiceID: fossa.ID, RemoteTeamID: 102, RemoteTeamName: &teamNamePartial, ProjectName: &teamNamePartial},
		{ProjectID: projectMap[teamNameInvites].ID, ServiceID: fossa.ID, RemoteTeamID: 103, RemoteTeamName: &teamNameInvites, ProjectName: &teamNameInvites},
		{ProjectID: projectMap[teamNameMissing].ID, ServiceID: fossa.ID, RemoteTeamID: 104, RemoteTeamName: &teamNameMissing, ProjectName: &teamNameMissing},
	}
	for i := range teams {
		if err := dbConn.Where("remote_team_id = ?", teams[i].RemoteTeamID).FirstOrCreate(&teams[i]).Error; err != nil {
			log.Fatalf("seed: service team insert failed: %v", err)
		}
	}

	remoteUsers := []model.RemoteUser{
		{ServiceID: fossa.ID, RemoteUserID: 1, ServiceEmail: maintainers[0].Email},
		{ServiceID: fossa.ID, RemoteUserID: 2, ServiceEmail: maintainers[1].GitHubEmail},
		{ServiceID: fossa.ID, RemoteUserID: 3, ServiceEmail: maintainers[2].Email},
		{ServiceID: fossa.ID, RemoteUserID: 4, ServiceEmail: maintainers[5].Email},
	}
	for i := range remoteUsers {
		if err := dbConn.Where("remote_user_id = ?", remoteUsers[i].RemoteUserID).FirstOrCreate(&remoteUsers[i]).Error; err != nil {
			log.Fatalf("seed: remote user insert failed: %v", err)
		}
	}

	serviceUserTeams := []model.RemoteTeamUser{
		{ServiceID: fossa.ID, TeamID: teams[0].ID, UserID: remoteUsers[0].ID, MaintainerID: &maintainers[0].ID},
		{ServiceID: fossa.ID, TeamID: teams[0].ID, UserID: remoteUsers[1].ID, MaintainerID: &maintainers[1].ID},
		{ServiceID: fossa.ID, TeamID: teams[2].ID, UserID: remoteUsers[3].ID, MaintainerID: &maintainers[5].ID},
	}
	for i := range serviceUserTeams {
		if err := dbConn.Where("team_id = ? AND maintainer_id = ?", serviceUserTeams[i].TeamID, serviceUserTeams[i].MaintainerID).
			FirstOrCreate(&serviceUserTeams[i]).Error; err != nil {
			log.Fatalf("seed: service user team insert failed: %v", err)
		}
	}

	now := time.Now().UTC()
	errMsg := "FOSSA check failed"
	invites := []model.ServiceInvitation{
		{
			ServiceID:     fossa.ID,
			RemoteTeamID:  teams[2].RemoteTeamID,
			ProjectID:     projectMap[teamNameInvites].ID,
			MaintainerID:  &maintainers[4].ID,
			ServiceEmail:  maintainers[4].Email,
			Status:        "pending",
			SentAt:        &now,
			LastCheckedAt: &now,
		},
		{
			ServiceID:     fossa.ID,
			RemoteTeamID:  teams[2].RemoteTeamID,
			ProjectID:     projectMap[teamNameInvites].ID,
			MaintainerID:  &maintainers[5].ID,
			ServiceEmail:  maintainers[5].Email,
			Status:        "accepted",
			LastCheckedAt: &now,
		},
		{
			ServiceID:     fossa.ID,
			RemoteTeamID:  teams[2].RemoteTeamID,
			ProjectID:     projectMap[teamNameInvites].ID,
			MaintainerID:  &maintainers[2].ID,
			ServiceEmail:  maintainers[2].Email,
			Status:        "expired",
			SentAt:        &now,
			LastCheckedAt: &now,
		},
		{
			ServiceID:     fossa.ID,
			RemoteTeamID:  teams[2].RemoteTeamID,
			ProjectID:     projectMap[teamNameInvites].ID,
			MaintainerID:  &maintainers[3].ID,
			ServiceEmail:  maintainers[3].Email,
			Status:        "error",
			LastCheckedAt: &now,
			LastError:     &errMsg,
		},
	}
	for i := range invites {
		if err := dbConn.Where("project_id = ? AND service_id = ? AND service_email = ?",
			invites[i].ProjectID, invites[i].ServiceID, invites[i].ServiceEmail).
			FirstOrCreate(&invites[i]).Error; err != nil {
			log.Fatalf("seed: service invite insert failed: %v", err)
		}
	}

	if driver == "postgres" {
		fmt.Printf("seed: wrote test data to postgres\n")
	} else {
		fmt.Printf("seed: wrote test db to %s\n", *dbPath)
	}
}

func timePtr(t time.Time) *time.Time {
	return &t
}

func uintPtr(v uint) *uint {
	return &v
}

func strPtr(s string) *string { return &s }

// withLocation sets Location, Country, and Timezone on m using the geo lookup.
func withLocation(rawLocation string, m model.Maintainer) model.Maintainer {
	m.Location = strPtr(rawLocation)
	if country, tz, ok := geo.DeriveCountryAndTimezone(rawLocation); ok {
		m.Country = strPtr(country)
		m.Timezone = strPtr(tz)
	}
	return m
}
