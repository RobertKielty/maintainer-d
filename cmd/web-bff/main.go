package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"maintainerd/db"
	"maintainerd/model"
	"maintainerd/onboarding"
	"maintainerd/refparse"

	"github.com/google/go-github/v55/github"
	"golang.org/x/oauth2"
	ghoauth "golang.org/x/oauth2/github"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func redactPostgresDSN(dsn string) (string, error) {
	parts := strings.Fields(dsn)
	kv := make(map[string]string, len(parts))
	for _, part := range parts {
		split := strings.SplitN(part, "=", 2)
		if len(split) != 2 {
			continue
		}
		kv[split[0]] = split[1]
	}
	user := kv["user"]
	host := kv["host"]
	port := kv["port"]
	dbname := kv["dbname"]
	if host == "" || dbname == "" {
		return "", fmt.Errorf("missing host/dbname in DSN")
	}
	if port == "" {
		port = "5432"
	}
	return fmt.Sprintf("user=%s host=%s port=%s dbname=%s password=***", user, host, port, dbname), nil
}

const (
	defaultAddr              = ":8000"
	defaultSessionCookieName = "md_session"
	defaultStateCookieName   = "md_oauth_state"
	defaultSessionTTL        = 8 * time.Hour
	defaultStateTTL          = 10 * time.Minute
	defaultDBPath            = "/data/onboarding.db"
	defaultWebBaseURL        = "http://localhost:3000"
	defaultRedirectCallback  = "http://localhost:8000/auth/callback"
	loginRedirectParam       = "next"
	headerContentType        = "Content-Type"
	contentTypeJSON          = "application/json"
	roleStaff                = "staff"
	roleMaintainer           = "maintainer"
	onboardingIssueCacheTTL  = 15 * time.Minute
)

type server struct {
	oauthConfig     *oauth2.Config
	store           *db.SQLStore
	sessions        *sessionStore
	oauthStates     *stateStore
	cookieName      string
	stateCookie     string
	webBaseURL      string
	cookieDomain    string
	cookieSecure    bool
	sessionTTL      time.Duration
	webOrigin       string
	testMode        bool
	logger          *log.Logger
	githubToken     string
	fetchIssueTitle func(ctx context.Context, owner, repo string, number int) (string, error)
	onboardingCache *onboardingIssueCache
	fetchIssues     func(ctx context.Context) ([]onboardingIssueSummary, error)
}

type session struct {
	ID        string
	Login     string
	Role      string
	CreatedAt time.Time
	ExpiresAt time.Time
}

type sessionStore struct {
	mu       sync.RWMutex
	sessions map[string]session
	logger   *log.Logger
}

type stateEntry struct {
	Redirect string
	Expires  time.Time
}

type stateStore struct {
	mu     sync.RWMutex
	states map[string]stateEntry
	ttl    time.Duration
}

type onboardingIssueSummary struct {
	Number      int    `json:"number"`
	Title       string `json:"title"`
	URL         string `json:"url"`
	ProjectName string `json:"projectName,omitempty"`
}

type onboardingIssueCache struct {
	mu      sync.RWMutex
	expires time.Time
	raw     []onboardingIssueSummary
	issues  []onboardingIssueSummary
}

func main() {
	logger := log.New(os.Stdout, "", log.LstdFlags)

	addr := envOr("BFF_ADDR", defaultAddr)
	dbDriver := envOr("MD_DB_DRIVER", "sqlite")
	dbDSN := envOr("MD_DB_DSN", "")
	dbPath := envOr("MD_DB_PATH", defaultDBPath)
	webBaseURL := envOr("WEB_APP_BASE_URL", defaultWebBaseURL)
	redirectURL := envOr("GITHUB_OAUTH_REDIRECT_URL", defaultRedirectCallback)
	cookieName := envOr("SESSION_COOKIE_NAME", defaultSessionCookieName)
	cookieDomain := os.Getenv("SESSION_COOKIE_DOMAIN")
	stateCookie := envOr("OAUTH_STATE_COOKIE_NAME", defaultStateCookieName)
	sessionTTL := parseDuration(envOr("SESSION_TTL", ""), defaultSessionTTL)
	cookieSecure := envOr("SESSION_COOKIE_SECURE", "") == "true"
	testMode := envOr("BFF_TEST_MODE", "") == "true"

	clientID := os.Getenv("GITHUB_OAUTH_CLIENT_ID")
	clientSecret := os.Getenv("GITHUB_OAUTH_CLIENT_SECRET")
	githubToken := strings.TrimSpace(os.Getenv("GITHUB_API_TOKEN"))
	if dbDriver == "sqlite" {
		logger.Printf(
			"web-bff: config addr=%s dbDriver=%s dbPath=%s webBaseURL=%s redirectURL=%s cookieName=%s cookieDomain=%s stateCookie=%s sessionTTL=%s cookieSecure=%t testMode=%t clientID=%s",
			addr,
			dbDriver,
			dbPath,
			webBaseURL,
			redirectURL,
			cookieName,
			cookieDomain,
			stateCookie,
			sessionTTL,
			cookieSecure,
			testMode,
			clientID,
		)
	} else {
		logger.Printf(
			"web-bff: config addr=%s dbDriver=%s dbDSNSet=%t webBaseURL=%s redirectURL=%s cookieName=%s cookieDomain=%s stateCookie=%s sessionTTL=%s cookieSecure=%t testMode=%t clientID=%s",
			addr,
			dbDriver,
			dbDSN != "",
			webBaseURL,
			redirectURL,
			cookieName,
			cookieDomain,
			stateCookie,
			sessionTTL,
			cookieSecure,
			testMode,
			clientID,
		)
	}
	if !testMode && (clientID == "" || clientSecret == "") {
		logger.Fatal("web-bff: GITHUB_OAUTH_CLIENT_ID and GITHUB_OAUTH_CLIENT_SECRET must be set")
	}
	if testMode && clientID == "" {
		clientID = "test-client"
	}
	if testMode && clientSecret == "" {
		clientSecret = "test-secret"
	}
	if dbDriver == "postgres" && dbDSN == "" {
		logger.Fatal("web-bff: MD_DB_DSN is required when MD_DB_DRIVER=postgres")
	}

	redirectURLParsed, err := url.Parse(redirectURL)
	if err != nil {
		logger.Fatalf("web-bff: invalid GITHUB_OAUTH_REDIRECT_URL: %v", err)
	}
	if !cookieSecure {
		cookieSecure = redirectURLParsed.Scheme == "https"
	}

	dsn := dbPath
	if dbDriver == "postgres" {
		dsn = dbDSN
	}
	if dbDriver == "postgres" && dbDSN != "" {
		logDB, err := redactPostgresDSN(dbDSN)
		if err != nil {
			logger.Printf("web-bff: using postgres DSN (failed to parse details): %v", err)
		} else {
			logger.Printf("web-bff: using postgres DSN %s", logDB)
		}
	}
	store, err := openStore(dbDriver, dsn)
	if err != nil {
		logger.Fatalf("web-bff: failed to open database: %v", err)
	}

	s := &server{
		oauthConfig: &oauth2.Config{
			ClientID:     clientID,
			ClientSecret: clientSecret,
			RedirectURL:  redirectURL,
			Scopes:       []string{"read:user"},
			Endpoint:     ghoauth.Endpoint,
		},
		store:        store,
		sessions:     newSessionStore(logger),
		oauthStates:  newStateStore(defaultStateTTL),
		cookieName:   cookieName,
		stateCookie:  stateCookie,
		webBaseURL:   strings.TrimRight(webBaseURL, "/"),
		cookieDomain: cookieDomain,
		cookieSecure: cookieSecure,
		sessionTTL:   sessionTTL,
		webOrigin:    originFromBaseURL(webBaseURL),
		testMode:     testMode,
		logger:       logger,
		githubToken:  githubToken,
		onboardingCache: &onboardingIssueCache{
			expires: time.Time{},
		},
	}
	s.fetchIssueTitle = s.fetchIssueTitleFromGitHub
	s.fetchIssues = s.fetchOnboardingIssuesFromGitHub

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/auth/login", s.handleLogin)
	mux.HandleFunc("/auth/callback", s.handleCallback)
	mux.Handle("/auth/test-login", s.withCORS(http.HandlerFunc(s.handleTestLogin)))
	mux.Handle("/auth/logout", s.withCORS(http.HandlerFunc(s.handleLogout)))
	mux.Handle("/api/me", s.withCORS(s.requireSession(http.HandlerFunc(s.handleMe))))
	mux.Handle("/api/projects", s.withCORS(s.requireSession(http.HandlerFunc(s.handleProjects))))
	mux.Handle("/api/projects/recent", s.withCORS(s.requireSession(http.HandlerFunc(s.handleRecentProjects))))
	mux.Handle("/api/projects/", s.withCORS(s.requireSession(http.HandlerFunc(s.handleProject))))
	mux.Handle("/api/maintainers/status", s.withCORS(s.requireSession(http.HandlerFunc(s.handleMaintainerStatusUpdate))))
	mux.Handle("/api/maintainers/from-ref", s.withCORS(s.requireSession(http.HandlerFunc(s.handleMaintainerFromRef))))
	mux.Handle("/api/maintainers/", s.withCORS(s.requireSession(http.HandlerFunc(s.handleMaintainer))))
	mux.Handle("/api/audit", s.withCORS(s.requireSession(http.HandlerFunc(s.handleAudit))))
	mux.Handle("/api/companies/merge", s.withCORS(s.requireSession(http.HandlerFunc(s.handleCompanyMerge))))
	mux.Handle("/api/companies", s.withCORS(s.requireSession(http.HandlerFunc(s.handleCompanies))))
	mux.Handle("/api/onboarding/resolve", s.withCORS(s.requireSession(http.HandlerFunc(s.handleResolveOnboarding))))
	mux.Handle("/api/onboarding/issues", s.withCORS(s.requireSession(http.HandlerFunc(s.handleOnboardingIssues))))
	mux.Handle("/api/", s.withCORS(s.requireSession(http.HandlerFunc(s.handleAPINotImplemented))))

	server := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	logger.Printf("web-bff: starting on %s", addr)
	if err := server.ListenAndServe(); err != nil {
		logger.Fatalf("web-bff: server error: %v", err)
	}
}

func openStore(driver, dsn string) (*db.SQLStore, error) {
	gormDB, err := db.OpenGorm(driver, dsn, &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		return nil, err
	}
	return db.NewSQLStore(gormDB), nil
}

func (s *server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func (s *server) handleLogin(w http.ResponseWriter, r *http.Request) {
	state, err := randomToken(32)
	if err != nil {
		http.Error(w, "failed to start login", http.StatusInternalServerError)
		return
	}

	redirectPath := sanitizeRedirect(r.URL.Query().Get(loginRedirectParam))
	s.oauthStates.Set(state, stateEntry{
		Redirect: redirectPath,
		Expires:  time.Now().Add(s.oauthStates.ttl),
	})

	http.SetCookie(w, &http.Cookie{
		Name:     s.stateCookie,
		Value:    state,
		Path:     "/",
		MaxAge:   int(s.oauthStates.ttl.Seconds()),
		HttpOnly: true,
		Secure:   s.cookieSecure,
		SameSite: http.SameSiteLaxMode,
	})

	url := s.oauthConfig.AuthCodeURL(state, oauth2.AccessTypeOnline)
	http.Redirect(w, r, url, http.StatusFound)
}

func (s *server) handleCallback(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	code := r.URL.Query().Get("code")
	if state == "" || code == "" {
		http.Error(w, "missing oauth parameters", http.StatusBadRequest)
		return
	}

	stateCookie, err := r.Cookie(s.stateCookie)
	if err != nil || stateCookie.Value != state {
		http.Error(w, "invalid oauth state", http.StatusBadRequest)
		return
	}

	entry, ok := s.oauthStates.Consume(state)
	if !ok {
		http.Error(w, "oauth state expired", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	token, err := s.oauthConfig.Exchange(ctx, code)
	if err != nil {
		http.Error(w, "failed to exchange oauth code", http.StatusBadRequest)
		return
	}

	ghUser, err := fetchGitHubUser(ctx, token)
	if err != nil {
		http.Error(w, "failed to fetch github user", http.StatusBadGateway)
		return
	}

	login := strings.ToLower(ghUser.GetLogin())
	role, authorized := s.authorizeLogin(login)
	attemptRole := role
	if !authorized {
		attemptRole = "unauthorized"
	}
	s.logger.Printf("web-bff: login attempt user=%s role=%s ip=%s", login, attemptRole, clientIP(r))
	if !authorized {
		s.logger.Printf("web-bff: unauthorized login attempt from github user %q ip=%s", login, clientIP(r))
		http.Error(w, "unauthorized", http.StatusForbidden)
		return
	}

	if err := s.createSession(login, role, w); err != nil {
		http.Error(w, "failed to establish session", http.StatusInternalServerError)
		return
	}

	redirectURL := s.webBaseURL
	if entry.Redirect != "" {
		redirectURL = s.webBaseURL + entry.Redirect
	}
	http.Redirect(w, r, redirectURL, http.StatusFound)
}

func (s *server) handleTestLogin(w http.ResponseWriter, r *http.Request) {
	if !s.testMode {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	login := strings.TrimSpace(r.URL.Query().Get("login"))
	if login == "" {
		http.Error(w, "missing login", http.StatusBadRequest)
		return
	}
	role, authorized := s.authorizeLogin(login)
	attemptRole := role
	if !authorized {
		attemptRole = "unauthorized"
	}
	s.logger.Printf("web-bff: login attempt user=%s role=%s ip=%s", login, attemptRole, clientIP(r))
	if !authorized {
		http.Error(w, "unauthorized", http.StatusForbidden)
		return
	}

	if err := s.createSession(login, role, w); err != nil {
		http.Error(w, "failed to create session", http.StatusInternalServerError)
		return
	}

	w.Header().Set(headerContentType, contentTypeJSON)
	if err := json.NewEncoder(w).Encode(map[string]string{"status": "ok"}); err != nil {
		s.logger.Printf("web-bff: handleTestLogin encode error: %v", err)
	}
}

func (s *server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	sessionCookie, err := r.Cookie(s.cookieName)
	if err == nil {
		if sess, ok := s.sessions.Delete(sessionCookie.Value); ok {
			duration := time.Since(sess.CreatedAt).Truncate(time.Second)
			s.logger.Printf("web-bff: logout user=%s role=%s session_duration=%s", sess.Login, sess.Role, duration)
		}
	}

	http.SetCookie(w, &http.Cookie{
		Name:     s.cookieName,
		Value:    "",
		Path:     "/",
		Domain:   s.cookieDomain,
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   s.cookieSecure,
		SameSite: http.SameSiteLaxMode,
	})
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) createSession(login, role string, w http.ResponseWriter) error {
	sessionID, err := randomToken(48)
	if err != nil {
		return err
	}

	now := time.Now()
	s.sessions.Set(session{
		ID:        sessionID,
		Login:     login,
		Role:      role,
		CreatedAt: now,
		ExpiresAt: now.Add(s.sessionTTL),
	})

	s.logger.Printf("web-bff: login success user=%s role=%s", login, role)

	http.SetCookie(w, &http.Cookie{
		Name:     s.cookieName,
		Value:    sessionID,
		Path:     "/",
		Domain:   s.cookieDomain,
		MaxAge:   int(s.sessionTTL.Seconds()),
		HttpOnly: true,
		Secure:   s.cookieSecure,
		SameSite: http.SameSiteLaxMode,
	})

	return nil
}

func (s *server) handleMe(w http.ResponseWriter, r *http.Request) {
	session := sessionFromContext(r.Context())
	if session == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	response := map[string]any{
		"login": session.Login,
		"role":  session.Role,
	}
	if session.Role == roleMaintainer {
		if maintainer, err := s.getMaintainerByLogin(session.Login); err == nil {
			response["maintainerId"] = maintainer.ID
		}
	}
	w.Header().Set(headerContentType, contentTypeJSON)
	if err := json.NewEncoder(w).Encode(response); err != nil {
		s.logger.Printf("web-bff: handleMe encode error: %v", err)
	}
}

type projectSummary struct {
	ID          uint                `json:"id"`
	Name        string              `json:"name"`
	Maturity    string              `json:"maturity"`
	Maintainers []maintainerSummary `json:"maintainers"`
}

type projectsResponse struct {
	Total    int64            `json:"total"`
	Projects []projectSummary `json:"projects"`
}

type recentProjectSummary struct {
	ID                   uint                `json:"id"`
	Name                 string              `json:"name"`
	Maturity             string              `json:"maturity"`
	AddedBy              string              `json:"addedBy"`
	OnboardingIssue      string              `json:"onboardingIssue,omitempty"`
	OnboardingIssueState string              `json:"onboardingIssueStatus,omitempty"`
	LegacyMaintainerRef  string              `json:"legacyMaintainerRef,omitempty"`
	GitHubOrg            string              `json:"githubOrg,omitempty"`
	DotProjectYamlRef    string              `json:"dotProjectYamlRef,omitempty"`
	CreatedAt            string              `json:"createdAt,omitempty"`
	Maintainers          []maintainerSummary `json:"maintainers,omitempty"`
}

type recentProjectsResponse struct {
	Total    int64                  `json:"total"`
	Projects []recentProjectSummary `json:"projects"`
}

type recentProjectRow struct {
	ID              uint      `gorm:"column:id"`
	Name            string    `gorm:"column:name"`
	OnboardingIssue *string   `gorm:"column:onboarding_issue"`
	CreatedAt       time.Time `gorm:"column:created_at"`
}

type projectIDRow struct {
	ID   uint   `gorm:"column:id"`
	Name string `gorm:"column:name"`
}

type projectCreateRequest struct {
	OnboardingIssue     string `json:"onboardingIssue"`
	ProjectName         string `json:"projectName,omitempty"`
	GitHubOrg           string `json:"githubOrg"`
	ParentProjectID     *uint  `json:"parentProjectId,omitempty"`
	LegacyMaintainerRef string `json:"legacyMaintainerRef,omitempty"`
	DotProjectYamlRef   string `json:"dotProjectYamlRef,omitempty"`
	Maturity            string `json:"maturity,omitempty"`
}

type projectCreateResponse struct {
	ID        uint      `json:"id"`
	Name      string    `json:"name"`
	Maturity  string    `json:"maturity"`
	GitHubOrg string    `json:"githubOrg"`
	CreatedAt time.Time `json:"createdAt"`
}

type maintainerSummary struct {
	ID     uint   `json:"id"`
	Name   string `json:"name"`
	GitHub string `json:"github"`
}

type projectMaintainerDetail struct {
	ID              uint   `json:"id"`
	Name            string `json:"name"`
	GitHub          string `json:"github"`
	InMaintainerRef bool   `json:"inMaintainerRef"`
	Status          string `json:"status"`
	Company         string `json:"company,omitempty"`
}

type serviceSummary struct {
	ID          uint   `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

type maintainerRefStatus struct {
	URL       string     `json:"url,omitempty"`
	Status    string     `json:"status"`
	CheckedAt *time.Time `json:"checkedAt,omitempty"`
}

type projectDetailResponse struct {
	ID                      uint                      `json:"id"`
	Name                    string                    `json:"name"`
	Maturity                string                    `json:"maturity"`
	ParentProjectID         *uint                     `json:"parentProjectId,omitempty"`
	LegacyMaintainerRef     string                    `json:"legacyMaintainerRef,omitempty"`
	DotProjectYamlRef       string                    `json:"dotProjectYamlRef,omitempty"`
	RefStatus               maintainerRefStatus       `json:"maintainerRefStatus"`
	LegacyMaintainerRefBody string                    `json:"legacyMaintainerRefBody,omitempty"`
	RefOnlyGitHub           []string                  `json:"refOnlyGitHub"`
	RefLines                map[string]string         `json:"refLines,omitempty"`
	OnboardingIssue         string                    `json:"onboardingIssue,omitempty"`
	MailingList             string                    `json:"mailingList,omitempty"`
	Maintainers             []projectMaintainerDetail `json:"maintainers"`
	Services                []serviceSummary          `json:"services"`
	CreatedAt               time.Time                 `json:"createdAt"`
	UpdatedAt               time.Time                 `json:"updatedAt"`
	DeletedAt               *time.Time                `json:"deletedAt,omitempty"`
	UpdatedBy               string                    `json:"updatedBy,omitempty"`
	UpdatedAuditID          *uint                     `json:"updatedAuditId,omitempty"`
}

func (s *server) handleProjects(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		s.handleProjectCreate(w, r)
		return
	}
	session := sessionFromContext(r.Context())
	if session == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var maintainerID uint
	if session.Role == roleMaintainer {
		maintainer, err := s.getMaintainerByLogin(session.Login)
		if err != nil {
			s.logger.Printf("web-bff: access denied projects user=%s role=%s reason=maintainer_lookup_failed err=%v", session.Login, session.Role, err)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		maintainerID = maintainer.ID
		s.logger.Printf("web-bff: projects visible to maintainer user=%s id=%d", session.Login, maintainerID)
	} else if session.Role != roleStaff {
		s.logger.Printf("web-bff: access denied projects user=%s role=%s reason=role_not_allowed", session.Login, session.Role)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	query := strings.TrimSpace(r.URL.Query().Get("query"))
	namePrefix := strings.TrimSpace(r.URL.Query().Get("namePrefix"))
	limit := parseIntParam(r, "limit", 20, 1, 100)
	offset := parseIntParam(r, "offset", 0, 0, 10_000_000)
	sortBy := r.URL.Query().Get("sort")
	if sortBy == "" {
		sortBy = "name"
	}
	if sortBy != "name" {
		sortBy = "name"
	}
	direction := strings.ToLower(r.URL.Query().Get("direction"))
	if direction != "desc" {
		direction = "asc"
	}

	maturityFilters := parseCSVParam(r, "maturity")

	base := s.store.DB().Model(&model.Project{})
	if len(maturityFilters) > 0 {
		base = base.Where("projects.maturity IN ?", maturityFilters)
	}
	if namePrefix != "" {
		base = base.Where("LOWER(projects.name) LIKE ?", strings.ToLower(namePrefix)+"%")
	}
	if query != "" {
		like := "%" + strings.ToLower(query) + "%"
		compactQuery := strings.NewReplacer(" ", "", "-", "", "_", "").Replace(strings.ToLower(query))
		compactLike := "%" + compactQuery + "%"
		base = base.
			Joins("LEFT JOIN maintainer_projects mp ON mp.project_id = projects.id").
			Joins("LEFT JOIN maintainers maint ON maint.id = mp.maintainer_id").
			Joins("LEFT JOIN companies comp ON comp.id = maint.company_id").
			Where(
				"LOWER(projects.name) LIKE ? OR LOWER(projects.maintainer_ref) LIKE ? OR LOWER(maint.name) LIKE ? OR LOWER(maint.git_hub_account) LIKE ? OR LOWER(comp.name) LIKE ? OR REPLACE(REPLACE(REPLACE(LOWER(comp.name), ' ', ''), '-', ''), '_', '') LIKE ?",
				like, like, like, like, like, compactLike,
			)
	}

	var total int64
	if err := base.Distinct("projects.id").Count(&total).Error; err != nil {
		s.logger.Printf("web-bff: handleProjects count error: %v", err)
		http.Error(w, "failed to count projects", http.StatusInternalServerError)
		return
	}
	if session.Role == roleMaintainer && total == 0 {
		s.logger.Printf("web-bff: projects empty for maintainer user=%s id=%d query=%q maturity=%v", session.Login, maintainerID, query, maturityFilters)
	}

	order := "projects." + sortBy + " " + direction
	var rows []projectIDRow
	if err := base.
		Select("projects.id, projects.name").
		Distinct().
		Order(order).
		Limit(limit).
		Offset(offset).
		Scan(&rows).Error; err != nil {
		s.logger.Printf("web-bff: handleProjects list ids error: %v", err)
		http.Error(w, "failed to load projects", http.StatusInternalServerError)
		return
	}
	ids := make([]uint, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}

	projects := make([]projectSummary, 0, len(ids))
	if len(ids) > 0 {
		var results []model.Project
		if err := s.store.DB().
			Preload("Maintainers").
			Where("projects.id IN ?", ids).
			Find(&results).Error; err != nil {
			s.logger.Printf("web-bff: handleProjects load projects error: %v", err)
			http.Error(w, "failed to load projects", http.StatusInternalServerError)
			return
		}

		projectByID := make(map[uint]model.Project, len(results))
		for _, project := range results {
			projectByID[project.ID] = project
		}

		for _, id := range ids {
			project, ok := projectByID[id]
			if !ok {
				continue
			}
			maintainers := summarizeMaintainers(project.Maintainers)
			projects = append(projects, projectSummary{
				ID:          project.ID,
				Name:        project.Name,
				Maturity:    string(project.Maturity),
				Maintainers: maintainers,
			})
		}
	}

	w.Header().Set(headerContentType, contentTypeJSON)
	if namePrefix != "" {
		names := make([]string, 0, len(projects))
		for _, project := range projects {
			names = append(names, project.Name)
		}
		s.logger.Printf(
			"web-bff: projects list namePrefix=%q query=%q total=%d returned=%d names=%v",
			namePrefix,
			query,
			total,
			len(projects),
			names,
		)
	}
	if err := json.NewEncoder(w).Encode(projectsResponse{
		Total:    total,
		Projects: projects,
	}); err != nil {
		s.logger.Printf("web-bff: handleProjects encode error: %v", err)
	}
}

func (s *server) handleRecentProjects(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	session := sessionFromContext(r.Context())
	if session == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if session.Role != roleStaff && session.Role != roleMaintainer {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	limit := parseIntParam(r, "limit", 10, 1, 50)
	offset := parseIntParam(r, "offset", 0, 0, 10_000_000)
	sortBy := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("sort")))
	if sortBy == "" {
		sortBy = "created"
	}
	direction := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("direction")))
	if direction != "asc" {
		direction = "desc"
	}
	maturityFilters := parseCSVParam(r, "maturity")
	nameFilter := strings.TrimSpace(r.URL.Query().Get("projectName"))
	maintainerFilter := strings.TrimSpace(r.URL.Query().Get("maintainer"))
	maintainerFileFilter := strings.TrimSpace(r.URL.Query().Get("maintainerFile"))

	base := s.store.DB().Model(&model.Project{})
	if len(maturityFilters) > 0 {
		base = base.Where("projects.maturity IN ?", maturityFilters)
	}
	if nameFilter != "" {
		base = base.Where("LOWER(projects.name) LIKE ?", "%"+strings.ToLower(nameFilter)+"%")
	}
	if maintainerFileFilter != "" {
		base = base.Where(
			"LOWER(projects.maintainer_ref) LIKE ?",
			"%"+strings.ToLower(maintainerFileFilter)+"%",
		)
	}
	if maintainerFilter != "" {
		like := "%" + strings.ToLower(maintainerFilter) + "%"
		base = base.
			Joins("LEFT JOIN maintainer_projects mp ON mp.project_id = projects.id").
			Joins("LEFT JOIN maintainers maint ON maint.id = mp.maintainer_id").
			Where("LOWER(maint.name) LIKE ? OR LOWER(maint.git_hub_account) LIKE ?", like, like)
	}
	var total int64
	if err := base.Distinct("projects.id").Count(&total).Error; err != nil {
		s.logger.Printf("web-bff: recent projects count error: %v", err)
		http.Error(w, "failed to load projects", http.StatusInternalServerError)
		return
	}

	var rows []recentProjectRow
	if err := base.
		Select("projects.id, projects.name, projects.onboarding_issue, projects.created_at").
		Distinct("projects.id, projects.name, projects.onboarding_issue, projects.created_at").
		Find(&rows).Error; err != nil {
		s.logger.Printf("web-bff: recent projects list error: %v", err)
		http.Error(w, "failed to load projects", http.StatusInternalServerError)
		return
	}

	sort.Slice(rows, func(i, j int) bool {
		left := rows[i]
		right := rows[j]
		switch sortBy {
		case "name":
			if direction == "asc" {
				return strings.ToLower(left.Name) < strings.ToLower(right.Name)
			}
			return strings.ToLower(left.Name) > strings.ToLower(right.Name)
		case "obissue":
			leftNum, leftHas := issueNumberFromURL(left.OnboardingIssue)
			rightNum, rightHas := issueNumberFromURL(right.OnboardingIssue)
			if leftHas != rightHas {
				return leftHas && !rightHas
			}
			if leftNum == rightNum {
				if direction == "asc" {
					return left.ID < right.ID
				}
				return left.ID > right.ID
			}
			if direction == "asc" {
				return leftNum < rightNum
			}
			return leftNum > rightNum
		default:
			if direction == "asc" {
				return left.CreatedAt.Before(right.CreatedAt)
			}
			return left.CreatedAt.After(right.CreatedAt)
		}
	})

	start := offset
	if start > len(rows) {
		start = len(rows)
	}
	end := offset + limit
	if end > len(rows) {
		end = len(rows)
	}
	ids := make([]uint, 0, end-start)
	for _, row := range rows[start:end] {
		ids = append(ids, row.ID)
	}

	var projects []model.Project
	if len(ids) > 0 {
		if err := s.store.DB().
			Preload("Maintainers").
			Where("projects.id IN ?", ids).
			Find(&projects).Error; err != nil {
			s.logger.Printf("web-bff: recent projects load error: %v", err)
			http.Error(w, "failed to load projects", http.StatusInternalServerError)
			return
		}
	}

	addedBy := make(map[uint]string, len(ids))
	if len(ids) > 0 {
		var audits []model.AuditLog
		if err := s.store.DB().
			Preload("Staff").
			Where("project_id IN ? AND action = ?", ids, "PROJECT_CREATE").
			Order("created_at desc").
			Find(&audits).Error; err != nil {
			s.logger.Printf("web-bff: recent projects audit lookup error: %v", err)
		} else {
			for _, audit := range audits {
				if audit.ProjectID == nil {
					continue
				}
				if _, exists := addedBy[*audit.ProjectID]; exists {
					continue
				}
				label := ""
				if audit.Staff != nil {
					label = strings.TrimSpace(audit.Staff.Name)
					if label == "" {
						label = strings.TrimSpace(audit.Staff.GitHubAccount)
					}
				}
				if label == "" && audit.StaffID != nil {
					label = fmt.Sprintf("Staff #%d", *audit.StaffID)
				}
				if label == "" {
					label = "—"
				}
				addedBy[*audit.ProjectID] = label
			}
		}
	}

	openIssues := map[string]struct{}{}
	if rawIssues, err := s.getOnboardingIssuesRaw(r.Context()); err == nil {
		for _, issue := range rawIssues {
			openIssues[strings.ToLower(issue.URL)] = struct{}{}
		}
	}

	response := recentProjectsResponse{
		Total:    total,
		Projects: make([]recentProjectSummary, 0, len(projects)),
	}
	projectByID := make(map[uint]model.Project, len(projects))
	for _, project := range projects {
		projectByID[project.ID] = project
	}
	for _, id := range ids {
		project, ok := projectByID[id]
		if !ok {
			continue
		}
		entry := recentProjectSummary{
			ID:                  project.ID,
			Name:                project.Name,
			Maturity:            string(project.Maturity),
			AddedBy:             addedBy[project.ID],
			LegacyMaintainerRef: strings.TrimSpace(project.LegacyMaintainerRef),
			GitHubOrg:           strings.TrimSpace(project.GitHubOrg),
			DotProjectYamlRef:   strings.TrimSpace(project.DotProjectYamlRef),
			CreatedAt:           project.CreatedAt.Format(time.RFC3339),
			Maintainers:         summarizeMaintainers(project.Maintainers),
		}
		if entry.AddedBy == "" {
			entry.AddedBy = "—"
		}
		if project.OnboardingIssue != nil {
			entry.OnboardingIssue = strings.TrimSpace(*project.OnboardingIssue)
			if entry.OnboardingIssue != "" {
				if _, ok := openIssues[strings.ToLower(entry.OnboardingIssue)]; ok {
					entry.OnboardingIssueState = "open"
				} else {
					entry.OnboardingIssueState = "closed"
				}
			}
		}
		response.Projects = append(response.Projects, entry)
	}
	w.Header().Set(headerContentType, contentTypeJSON)
	if err := json.NewEncoder(w).Encode(response); err != nil {
		s.logger.Printf("web-bff: recent projects encode error: %v", err)
	}
}

func (s *server) handleProject(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPatch {
		if strings.HasSuffix(r.URL.Path, "/parent") {
			s.handleProjectParentUpdate(w, r)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/maturity") {
			s.handleProjectMaturityUpdate(w, r)
			return
		}
		s.handleProjectMaintainerRefUpdate(w, r)
		return
	}
	id, err := parseIDParam(r.URL.Path, "/api/projects/")
	if err != nil {
		http.Error(w, "invalid project id", http.StatusBadRequest)
		return
	}
	session := sessionFromContext(r.Context())
	if session == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	login := session.Login
	role := session.Role
	s.logger.Printf("web-bff: project lookup id=%d path=%s user=%s role=%s", id, r.URL.Path, login, role)

	project, err := s.store.GetProjectByID(id)
	if err != nil {
		if errors.Is(err, db.ErrProjectNotFound) {
			s.logger.Printf("web-bff: project not found id=%d path=%s user=%s role=%s", id, r.URL.Path, login, role)
			http.Error(w, "project not found", http.StatusNotFound)
			return
		}
		s.logger.Printf("web-bff: failed to load project id=%d path=%s user=%s role=%s err=%v", id, r.URL.Path, login, role, err)
		http.Error(w, "failed to load project", http.StatusInternalServerError)
		return
	}
	if session.Role == roleMaintainer {
		if _, err := s.getMaintainerByLogin(session.Login); err != nil {
			s.logger.Printf("web-bff: maintainer access denied project=%d user=%s role=%s reason=%v", id, session.Login, session.Role, err)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
	} else if session.Role != roleStaff {
		s.logger.Printf("web-bff: access denied project=%d user=%s role=%s reason=role_not_allowed", id, session.Login, session.Role)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	refStatus := maintainerRefStatus{Status: "missing"}
	refMatches := make(map[uint]bool)
	refOnlyGitHub := []string{}
	refLines := map[string]string{}
	refURL := strings.TrimSpace(project.LegacyMaintainerRef)
	refBody := ""
	if refURL != "" {
		refStatus.URL = refURL
		body, fetchErr := fetchMaintainerRef(r.Context(), refURL)
		if fetchErr != nil {
			refStatus.Status = "error"
		} else {
			refStatus.Status = "fetched"
			checkedAt := time.Now()
			refStatus.CheckedAt = &checkedAt
			refBody = body
			refMatches = buildMaintainerRefMatches(body, project.Maintainers)
			refOnlyGitHub = buildMaintainerRefOnly(body, project.Maintainers)
			refLines = buildMaintainerRefLines(body)
		}
	}
	if role != roleStaff && role != roleMaintainer {
		refBody = ""
		refLines = nil
	}
	if refOnlyGitHub == nil {
		refOnlyGitHub = []string{}
	}

	maintainers := summarizeMaintainerDetails(project.Maintainers, refMatches)
	services := make([]serviceSummary, 0, len(project.Services))
	for _, service := range project.Services {
		services = append(services, serviceSummary{
			ID:          service.ID,
			Name:        service.Name,
			Description: service.Description,
		})
	}

	var deletedAt *time.Time
	if project.DeletedAt.Valid {
		ts := project.DeletedAt.Time
		deletedAt = &ts
	}
	var updatedBy string
	var updatedAuditID *uint
	var audit model.AuditLog
	if err := s.store.DB().
		Where("project_id = ? AND action IN ?", id, []string{"PROJECT_MAINTAINER_REF_UPDATE", "PROJECT_MATURITY_UPDATE"}).
		Order("created_at desc").
		First(&audit).Error; err == nil {
		if audit.StaffID != nil {
			var staff model.StaffMember
			if err := s.store.DB().First(&staff, *audit.StaffID).Error; err == nil {
				updatedBy = staff.Name
			}
		}
		if updatedBy == "" {
			updatedBy = "Staff"
		}
		updatedAuditID = &audit.ID
	}

	response := projectDetailResponse{
		ID:                      project.ID,
		Name:                    project.Name,
		Maturity:                string(project.Maturity),
		ParentProjectID:         project.ParentProjectID,
		RefStatus:               refStatus,
		LegacyMaintainerRefBody: refBody,
		RefOnlyGitHub:           refOnlyGitHub,
		RefLines:                refLines,
		Maintainers:             maintainers,
		Services:                services,
		CreatedAt:               project.CreatedAt,
		UpdatedAt:               project.UpdatedAt,
		DeletedAt:               deletedAt,
		UpdatedBy:               updatedBy,
		UpdatedAuditID:          updatedAuditID,
	}

	maintainerRef := strings.TrimSpace(project.LegacyMaintainerRef)
	if maintainerRef != "" {
		response.LegacyMaintainerRef = maintainerRef
	}
	if project.DotProjectYamlRef != "" {
		response.DotProjectYamlRef = strings.TrimSpace(project.DotProjectYamlRef)
	}
	if project.OnboardingIssue != nil {
		onboardingIssue := strings.TrimSpace(*project.OnboardingIssue)
		if onboardingIssue != "" {
			response.OnboardingIssue = onboardingIssue
		}
	}
	if project.MailingList != nil {
		mailingList := strings.TrimSpace(normalizeValue(*project.MailingList, "MML_MISSING"))
		if mailingList != "" {
			response.MailingList = mailingList
		}
	}

	w.Header().Set(headerContentType, contentTypeJSON)
	if err := json.NewEncoder(w).Encode(response); err != nil {
		s.logger.Printf("web-bff: handleProject encode error: %v", err)
	}
}

func (s *server) handleProjectCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	session := sessionFromContext(r.Context())
	if session == nil || session.Role != roleStaff {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var req projectCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	onboardingIssue := strings.TrimSpace(req.OnboardingIssue)
	if onboardingIssue == "" {
		http.Error(w, "onboardingIssue is required", http.StatusBadRequest)
		return
	}
	owner, repo, number, err := parseGitHubIssueURL(onboardingIssue)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if owner != "cncf" || repo != "sandbox" {
		http.Error(w, "onboardingIssue must be from github.com/cncf/sandbox", http.StatusBadRequest)
		return
	}
	if s.githubToken == "" {
		http.Error(w, "github api token not configured", http.StatusInternalServerError)
		return
	}
	title, err := s.fetchIssueTitle(r.Context(), owner, repo, number)
	if err != nil {
		s.logger.Printf("web-bff: create project issue fetch error owner=%s repo=%s issue=%d err=%v", owner, repo, number, err)
		http.Error(w, "failed to fetch onboarding issue", http.StatusBadGateway)
		return
	}
	projectName, err := onboarding.GetProjectNameFromProjectTitle(title)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.ProjectName != "" && strings.TrimSpace(req.ProjectName) != projectName {
		http.Error(w, "projectName must match onboarding issue title", http.StatusBadRequest)
		return
	}
	githubOrg := strings.TrimSpace(req.GitHubOrg)
	legacyRef := strings.TrimSpace(req.LegacyMaintainerRef)
	dotProjectRef := strings.TrimSpace(req.DotProjectYamlRef)
	if legacyRef == "" && dotProjectRef == "" {
		http.Error(w, "legacyMaintainerRef or dotProjectYamlRef is required", http.StatusBadRequest)
		return
	}
	inferredOrg := ""
	if legacyRef != "" {
		org, err := parseGitHubOrgFromURL(legacyRef)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		inferredOrg = org
	}
	if dotProjectRef != "" {
		org, err := parseGitHubOrgFromURL(dotProjectRef)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if inferredOrg != "" && !strings.EqualFold(inferredOrg, org) {
			http.Error(w, "legacyMaintainerRef and dotProjectYamlRef must reference the same GitHub org", http.StatusBadRequest)
			return
		}
		inferredOrg = org
	}
	if githubOrg != "" && !strings.EqualFold(githubOrg, inferredOrg) {
		http.Error(w, "githubOrg must match the GitHub org in the maintainer file URLs", http.StatusBadRequest)
		return
	}
	if inferredOrg == "" {
		http.Error(w, "failed to infer github org from maintainer file URLs", http.StatusBadRequest)
		return
	}
	githubOrg = inferredOrg
	if req.ParentProjectID != nil {
		if _, err := s.store.GetProjectByID(*req.ParentProjectID); err != nil {
			if errors.Is(err, db.ErrProjectNotFound) {
				http.Error(w, "parent project not found", http.StatusBadRequest)
				return
			}
			s.logger.Printf("web-bff: create project parent lookup error: %v", err)
			http.Error(w, "failed to validate parent project", http.StatusInternalServerError)
			return
		}
	}
	maturity := model.Sandbox
	if req.Maturity != "" {
		maturity = model.Maturity(strings.TrimSpace(req.Maturity))
		if !maturity.IsValid() {
			http.Error(w, "invalid maturity", http.StatusBadRequest)
			return
		}
	}
	if err := ensureProjectNameAvailable(s.store, projectName); err != nil {
		if errors.Is(err, db.ErrProjectExists) {
			http.Error(w, "project already exists", http.StatusConflict)
			return
		}
		s.logger.Printf("web-bff: create project lookup error: %v", err)
		http.Error(w, "failed to validate project", http.StatusInternalServerError)
		return
	}
	project := model.Project{
		Name:                projectName,
		Maturity:            maturity,
		GitHubOrg:           githubOrg,
		ParentProjectID:     req.ParentProjectID,
		LegacyMaintainerRef: legacyRef,
		DotProjectYamlRef:   dotProjectRef,
		OnboardingIssue:     &onboardingIssue,
	}
	if err := s.store.DB().Create(&project).Error; err != nil {
		s.logger.Printf("web-bff: create project error: %v", err)
		http.Error(w, "failed to create project", http.StatusInternalServerError)
		return
	}
	staffID := lookupStaffID(s.store, session.Login)
	actorName := session.Login
	if staffID != nil {
		var staff model.StaffMember
		if err := s.store.DB().First(&staff, *staffID).Error; err == nil && staff.Name != "" {
			actorName = staff.Name
		}
	}
	changes := map[string]map[string]string{
		"projectName":     {"to": projectName},
		"githubOrg":       {"to": githubOrg},
		"maturity":        {"to": string(maturity)},
		"onboardingIssue": {"to": onboardingIssue},
	}
	if metadataJSON, err := json.Marshal(changes); err == nil {
		event := model.AuditLog{
			StaffID:   staffID,
			Action:    "PROJECT_CREATE",
			Message:   fmt.Sprintf("Project created by %s", actorName),
			Metadata:  string(metadataJSON),
			ProjectID: &project.ID,
		}
		if err := s.store.DB().Create(&event).Error; err != nil {
			s.logger.Printf("web-bff: create project audit log failed: %v", err)
		}
	}
	s.invalidateOnboardingCache()
	w.Header().Set(headerContentType, contentTypeJSON)
	if err := json.NewEncoder(w).Encode(projectCreateResponse{
		ID:        project.ID,
		Name:      project.Name,
		Maturity:  string(project.Maturity),
		GitHubOrg: project.GitHubOrg,
		CreatedAt: project.CreatedAt,
	}); err != nil {
		s.logger.Printf("web-bff: create project encode error: %v", err)
	}
}

func (s *server) invalidateOnboardingCache() {
	if s.onboardingCache == nil {
		return
	}
	s.onboardingCache.mu.Lock()
	s.onboardingCache.expires = time.Time{}
	s.onboardingCache.issues = nil
	s.onboardingCache.raw = nil
	s.onboardingCache.mu.Unlock()
}

func ensureProjectNameAvailable(store *db.SQLStore, name string) error {
	var project model.Project
	if err := store.DB().Where("name = ?", name).First(&project).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		return err
	}
	return db.ErrProjectExists
}

func lookupStaffID(store *db.SQLStore, login string) *uint {
	if login == "" {
		return nil
	}
	var staff model.StaffMember
	if err := store.DB().Where("LOWER(git_hub_account) = ?", strings.ToLower(login)).First(&staff).Error; err != nil {
		return nil
	}
	return &staff.ID
}

type projectMaintainerRefUpdateRequest struct {
	LegacyMaintainerRef string `json:"legacyMaintainerRef"`
}

type projectParentUpdateRequest struct {
	ParentProjectID *uint `json:"parentProjectId"`
}

type projectMaturityUpdateRequest struct {
	Maturity string `json:"maturity"`
}

func (s *server) handleProjectParentUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	trimmed := strings.TrimSuffix(r.URL.Path, "/parent")
	id, err := parseIDParam(trimmed, "/api/projects/")
	if err != nil {
		http.Error(w, "invalid project id", http.StatusBadRequest)
		return
	}
	session := sessionFromContext(r.Context())
	if session == nil || session.Role != roleStaff {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var req projectParentUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if req.ParentProjectID != nil && *req.ParentProjectID == id {
		http.Error(w, "parent project cannot reference itself", http.StatusBadRequest)
		return
	}
	beforeProject, beforeErr := s.store.GetProjectByID(id)
	if beforeErr != nil {
		if errors.Is(beforeErr, db.ErrProjectNotFound) {
			http.Error(w, "project not found", http.StatusNotFound)
			return
		}
		s.logger.Printf("web-bff: load project before parent update failed id=%d err=%v", id, beforeErr)
		http.Error(w, "failed to update project", http.StatusInternalServerError)
		return
	}
	if req.ParentProjectID != nil {
		if _, err := s.store.GetProjectByID(*req.ParentProjectID); err != nil {
			if errors.Is(err, db.ErrProjectNotFound) {
				http.Error(w, "parent project not found", http.StatusBadRequest)
				return
			}
			s.logger.Printf("web-bff: parent lookup failed id=%d parent=%d err=%v", id, *req.ParentProjectID, err)
			http.Error(w, "failed to update project", http.StatusInternalServerError)
			return
		}
	}
	if err := s.store.UpdateProjectParentProjectID(id, req.ParentProjectID); err != nil {
		if errors.Is(err, db.ErrProjectNotFound) {
			http.Error(w, "project not found", http.StatusNotFound)
			return
		}
		s.logger.Printf("web-bff: update parent failed id=%d err=%v", id, err)
		http.Error(w, "failed to update project", http.StatusInternalServerError)
		return
	}
	var staffID *uint
	staffName := ""
	if session.Login != "" {
		var staff model.StaffMember
		if err := s.store.DB().
			Where("LOWER(git_hub_account) = ?", strings.ToLower(session.Login)).
			First(&staff).Error; err == nil {
			staffID = &staff.ID
			staffName = staff.Name
		}
	}
	if staffName == "" {
		staffName = session.Login
	}
	changes := map[string]map[string]string{
		"parentProjectId": {
			"from": fmt.Sprintf("%v", beforeProject.ParentProjectID),
			"to":   fmt.Sprintf("%v", req.ParentProjectID),
		},
	}
	if metadataJSON, err := json.Marshal(changes); err == nil {
		event := model.AuditLog{
			StaffID:   staffID,
			Action:    "PROJECT_PARENT_UPDATE",
			Message:   fmt.Sprintf("Parent project updated by %s", staffName),
			Metadata:  string(metadataJSON),
			ProjectID: &beforeProject.ID,
		}
		if err := s.store.DB().Create(&event).Error; err != nil {
			s.logger.Printf("web-bff: parent update audit log failed: %v", err)
		}
	}
	w.Header().Set(headerContentType, contentTypeJSON)
	if err := json.NewEncoder(w).Encode(map[string]any{
		"parentProjectId": req.ParentProjectID,
	}); err != nil {
		s.logger.Printf("web-bff: parent update encode error: %v", err)
	}
}

func (s *server) handleProjectMaintainerRefUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	id, err := parseIDParam(r.URL.Path, "/api/projects/")
	if err != nil {
		http.Error(w, "invalid project id", http.StatusBadRequest)
		return
	}
	session := sessionFromContext(r.Context())
	if session == nil || session.Role != roleStaff {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var req projectMaintainerRefUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	ref := strings.TrimSpace(req.LegacyMaintainerRef)
	if ref != "" && !strings.HasPrefix(ref, "http://") && !strings.HasPrefix(ref, "https://") {
		http.Error(w, "maintainerRef must be a URL", http.StatusBadRequest)
		return
	}
	beforeProject, beforeErr := s.store.GetProjectByID(id)
	if beforeErr != nil {
		if errors.Is(beforeErr, db.ErrProjectNotFound) {
			http.Error(w, "project not found", http.StatusNotFound)
			return
		}
		s.logger.Printf("web-bff: load project before maintainerRef update failed id=%d err=%v", id, beforeErr)
		http.Error(w, "failed to update project", http.StatusInternalServerError)
		return
	}
	if err := s.store.UpdateProjectLegacyMaintainerRef(id, ref); err != nil {
		if errors.Is(err, db.ErrProjectNotFound) {
			http.Error(w, "project not found", http.StatusNotFound)
			return
		}
		s.logger.Printf("web-bff: update maintainerRef failed id=%d err=%v", id, err)
		http.Error(w, "failed to update project", http.StatusInternalServerError)
		return
	}
	var staffID *uint
	staffName := ""
	if session.Login != "" {
		var staff model.StaffMember
		if err := s.store.DB().
			Where("LOWER(git_hub_account) = ?", strings.ToLower(session.Login)).
			First(&staff).Error; err == nil {
			staffID = &staff.ID
			staffName = staff.Name
		}
	}
	if staffName == "" {
		staffName = session.Login
	}
	changes := map[string]map[string]string{
		"maintainerRef": {
			"from": strings.TrimSpace(beforeProject.LegacyMaintainerRef),
			"to":   ref,
		},
	}
	metadata := map[string]any{
		"actor": map[string]string{
			"login": session.Login,
			"role":  session.Role,
		},
		"changes": changes,
	}
	if metadataJSON, err := json.Marshal(metadata); err != nil {
		s.logger.Printf("web-bff: update maintainerRef audit metadata encode error: %v", err)
	} else {
		event := model.AuditLog{
			ProjectID: &id,
			StaffID:   staffID,
			Action:    "PROJECT_MAINTAINER_REF_UPDATE",
			Message:   fmt.Sprintf("Project maintainer ref updated by %s", staffName),
			Metadata:  string(metadataJSON),
		}
		if err := s.store.DB().Create(&event).Error; err != nil {
			s.logger.Printf("web-bff: update maintainerRef audit log failed: %v", err)
		}
	}
	w.Header().Set(headerContentType, contentTypeJSON)
	if err := json.NewEncoder(w).Encode(map[string]string{"status": "ok"}); err != nil {
		s.logger.Printf("web-bff: handleProject update encode error: %v", err)
	}
}

func (s *server) handleProjectMaturityUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	trimmed := strings.TrimSuffix(r.URL.Path, "/maturity")
	id, err := parseIDParam(trimmed, "/api/projects/")
	if err != nil {
		http.Error(w, "invalid project id", http.StatusBadRequest)
		return
	}
	session := sessionFromContext(r.Context())
	if session == nil || session.Role != roleStaff {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var req projectMaturityUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	next, ok := parseMaturity(req.Maturity)
	if !ok {
		http.Error(w, "invalid maturity", http.StatusBadRequest)
		return
	}
	project, err := s.store.GetProjectByID(id)
	if err != nil {
		if errors.Is(err, db.ErrProjectNotFound) {
			http.Error(w, "project not found", http.StatusNotFound)
			return
		}
		s.logger.Printf("web-bff: load project before maturity update failed id=%d err=%v", id, err)
		http.Error(w, "failed to update project", http.StatusInternalServerError)
		return
	}
	if err := s.store.UpdateProjectMaturity(id, next); err != nil {
		if errors.Is(err, db.ErrProjectNotFound) {
			http.Error(w, "project not found", http.StatusNotFound)
			return
		}
		s.logger.Printf("web-bff: update maturity failed id=%d err=%v", id, err)
		http.Error(w, "failed to update project", http.StatusInternalServerError)
		return
	}

	var staffID *uint
	staffName := ""
	if session.Login != "" {
		var staff model.StaffMember
		if err := s.store.DB().
			Where("LOWER(git_hub_account) = ?", strings.ToLower(session.Login)).
			First(&staff).Error; err == nil {
			staffID = &staff.ID
			staffName = staff.Name
		}
	}
	if staffName == "" {
		staffName = session.Login
	}
	metadata := map[string]any{
		"actor": map[string]string{
			"login": session.Login,
			"role":  session.Role,
		},
		"changes": map[string]map[string]string{
			"maturity": {
				"from": string(project.Maturity),
				"to":   string(next),
			},
		},
	}
	if metadataJSON, err := json.Marshal(metadata); err != nil {
		s.logger.Printf("web-bff: update maturity audit metadata encode error: %v", err)
	} else {
		event := model.AuditLog{
			ProjectID: &id,
			StaffID:   staffID,
			Action:    "PROJECT_MATURITY_UPDATE",
			Message:   fmt.Sprintf("Project maturity updated by %s", staffName),
			Metadata:  string(metadataJSON),
		}
		if err := s.store.DB().Create(&event).Error; err != nil {
			s.logger.Printf("web-bff: update maturity audit log failed: %v", err)
		}
	}

	w.Header().Set(headerContentType, contentTypeJSON)
	if err := json.NewEncoder(w).Encode(map[string]string{"status": "ok"}); err != nil {
		s.logger.Printf("web-bff: handleProject update encode error: %v", err)
	}
}

type maintainerDetailResponse struct {
	ID          uint                        `json:"id"`
	Name        string                      `json:"name"`
	Email       string                      `json:"email"`
	GitHub      string                      `json:"github"`
	GitHubEmail string                      `json:"githubEmail"`
	Status      string                      `json:"status"`
	CompanyID   *uint                       `json:"companyId,omitempty"`
	Company     string                      `json:"company,omitempty"`
	Projects    []maintainerProjectResponse `json:"projects"`
	CreatedAt   time.Time                   `json:"createdAt"`
	UpdatedAt   time.Time                   `json:"updatedAt"`
	DeletedAt   *time.Time                  `json:"deletedAt,omitempty"`
	UpdatedBy   string                      `json:"updatedBy,omitempty"`
}

type maintainerProjectResponse struct {
	ID   uint   `json:"id"`
	Name string `json:"name"`
}

type auditLogResponse struct {
	ID           uint      `json:"id"`
	Action       string    `json:"action"`
	Message      string    `json:"message"`
	Metadata     string    `json:"metadata,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
	ProjectID    *uint     `json:"projectId,omitempty"`
	MaintainerID *uint     `json:"maintainerId,omitempty"`
	ServiceID    *uint     `json:"serviceId,omitempty"`
	StaffID      *uint     `json:"staffId,omitempty"`
	StaffName    string    `json:"staffName,omitempty"`
	StaffLogin   string    `json:"staffLogin,omitempty"`
}

type auditListResponse struct {
	Total int64              `json:"total"`
	Logs  []auditLogResponse `json:"logs"`
}

func (s *server) handleMaintainer(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	id, err := parseIDParam(r.URL.Path, "/api/maintainers/")
	if err != nil {
		http.Error(w, "invalid maintainer id", http.StatusBadRequest)
		return
	}
	session := sessionFromContext(r.Context())
	if session == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	login := session.Login
	role := session.Role
	s.logger.Printf("web-bff: maintainer lookup id=%d path=%s user=%s role=%s", id, r.URL.Path, login, role)
	var requester *model.Maintainer
	if session.Role == roleMaintainer {
		maintainer, err := s.getMaintainerByLogin(session.Login)
		if err != nil {
			s.logger.Printf("web-bff: maintainer access denied target=%d user=%s role=%s reason=%v", id, session.Login, session.Role, err)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		requester = maintainer
	} else if session.Role != roleStaff {
		s.logger.Printf("web-bff: access denied target=%d user=%s role=%s reason=role_not_allowed", id, session.Login, session.Role)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	switch r.Method {
	case http.MethodGet:
		var maintainer model.Maintainer
		if err := s.store.DB().
			Preload("Company").
			Preload("Projects").
			First(&maintainer, id).Error; err != nil {
			s.logger.Printf("web-bff: maintainer not found id=%d path=%s user=%s role=%s err=%v", id, r.URL.Path, login, role, err)
			http.Error(w, "maintainer not found", http.StatusNotFound)
			return
		}

		projects := make([]maintainerProjectResponse, 0, len(maintainer.Projects))
		for _, project := range maintainer.Projects {
			projects = append(projects, maintainerProjectResponse{
				ID:   project.ID,
				Name: project.Name,
			})
		}

		response := maintainerDetailResponse{
			ID:          maintainer.ID,
			Name:        maintainer.Name,
			Email:       normalizeValue(maintainer.Email, "EMAIL_MISSING"),
			GitHub:      normalizeValue(maintainer.GitHubAccount, "GITHUB_MISSING"),
			GitHubEmail: normalizeValue(maintainer.GitHubEmail, "GITHUB_MISSING"),
			Status:      string(maintainer.MaintainerStatus),
			Projects:    projects,
			CreatedAt:   maintainer.CreatedAt,
			UpdatedAt:   maintainer.UpdatedAt,
		}
		if maintainer.DeletedAt.Valid {
			deleted := maintainer.DeletedAt.Time
			response.DeletedAt = &deleted
		}
		if maintainer.CompanyID != nil {
			response.CompanyID = maintainer.CompanyID
		}
		if maintainer.Company.Name != "" {
			response.Company = maintainer.Company.Name
		}

		var audit model.AuditLog
		if err := s.store.DB().
			Where("maintainer_id = ? AND action = ?", id, "MAINTAINER_UPDATE").
			Order("created_at desc").
			First(&audit).Error; err == nil && audit.StaffID != nil {
			var staff model.StaffMember
			if err := s.store.DB().First(&staff, *audit.StaffID).Error; err == nil {
				if staff.Name != "" {
					response.UpdatedBy = staff.Name
				}
			}
		}

		w.Header().Set(headerContentType, contentTypeJSON)
		isSelf := requester != nil && requester.ID == id
		if session.Role != roleStaff && !isSelf {
			response.Email = ""
			response.GitHubEmail = ""
		}
		if err := json.NewEncoder(w).Encode(response); err != nil {
			s.logger.Printf("web-bff: handleMaintainer encode error: %v", err)
		}
		return
	case http.MethodPatch, http.MethodPut:
		maintainerEditSelf := false
		if session.Role == roleMaintainer {
			requester, err := s.getMaintainerByLogin(session.Login)
			if err != nil || requester.ID != id {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			maintainerEditSelf = true
		} else if session.Role != roleStaff {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		var req maintainerUpdateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		var before model.Maintainer
		if err := s.store.DB().First(&before, id).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				http.Error(w, "maintainer not found", http.StatusNotFound)
				return
			}
			s.logger.Printf("web-bff: update maintainer failed id=%d err=%v", id, err)
			http.Error(w, "failed to update maintainer", http.StatusInternalServerError)
			return
		}
		status := model.MaintainerStatus(strings.TrimSpace(req.Status))
		if maintainerEditSelf {
			status = before.MaintainerStatus
			req.Name = before.Name
			req.GitHub = before.GitHubAccount
		}
		if !status.IsValid() {
			http.Error(w, "invalid status", http.StatusBadRequest)
			return
		}
		updated, err := s.store.UpdateMaintainerDetails(id, req.Name, req.Email, req.GitHub, status, req.CompanyID)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				http.Error(w, "maintainer not found", http.StatusNotFound)
				return
			}
			s.logger.Printf("web-bff: update maintainer failed id=%d err=%v", id, err)
			http.Error(w, "failed to update maintainer", http.StatusInternalServerError)
			return
		}

		var staffID *uint
		staffName := ""
		if session.Login != "" {
			var staff model.StaffMember
			if err := s.store.DB().
				Where("LOWER(git_hub_account) = ?", strings.ToLower(session.Login)).
				First(&staff).Error; err == nil {
				staffID = &staff.ID
				staffName = staff.Name
			}
		}
		if staffName == "" {
			staffName = session.Login
		}

		changes := make(map[string]map[string]string)
		beforeName := strings.TrimSpace(before.Name)
		afterName := strings.TrimSpace(updated.Name)
		if beforeName == "" {
			beforeName = "NAME_MISSING"
		}
		if afterName == "" {
			afterName = "NAME_MISSING"
		}
		if beforeName != afterName {
			changes["name"] = map[string]string{"from": beforeName, "to": afterName}
		}
		beforeEmail := normalizeValue(before.Email, "EMAIL_MISSING")
		afterEmail := normalizeValue(updated.Email, "EMAIL_MISSING")
		if beforeEmail != afterEmail {
			changes["email"] = map[string]string{"from": beforeEmail, "to": afterEmail}
		}
		beforeGitHub := normalizeValue(before.GitHubAccount, "GITHUB_MISSING")
		afterGitHub := normalizeValue(updated.GitHubAccount, "GITHUB_MISSING")
		if beforeGitHub != afterGitHub {
			changes["github"] = map[string]string{"from": beforeGitHub, "to": afterGitHub}
		}
		if before.MaintainerStatus != updated.MaintainerStatus {
			changes["status"] = map[string]string{
				"from": string(before.MaintainerStatus),
				"to":   string(updated.MaintainerStatus),
			}
		}
		beforeCompany := ""
		if before.CompanyID != nil {
			beforeCompany = fmt.Sprintf("%d", *before.CompanyID)
		}
		afterCompany := ""
		if updated.CompanyID != nil {
			afterCompany = fmt.Sprintf("%d", *updated.CompanyID)
		}
		if beforeCompany != afterCompany {
			beforeCompanyName := ""
			if before.CompanyID != nil {
				var company model.Company
				if err := s.store.DB().First(&company, *before.CompanyID).Error; err == nil {
					beforeCompanyName = strings.TrimSpace(company.Name)
				}
			}
			afterCompanyName := strings.TrimSpace(updated.Company.Name)
			if beforeCompanyName == "" {
				beforeCompanyName = "COMPANY_MISSING"
			}
			if afterCompanyName == "" {
				afterCompanyName = "COMPANY_MISSING"
			}
			changes["company"] = map[string]string{"from": beforeCompanyName, "to": afterCompanyName}
		}

		metadata := map[string]any{
			"actor": map[string]string{
				"login": session.Login,
				"role":  session.Role,
			},
			"changes": changes,
		}
		fieldNames := make([]string, 0, len(changes))
		for field := range changes {
			fieldNames = append(fieldNames, field)
		}
		sort.Strings(fieldNames)
		message := fmt.Sprintf("Maintainer updated by %s", staffName)
		if len(fieldNames) > 0 {
			message = fmt.Sprintf("Maintainer [%s] updated by %s", strings.Join(fieldNames, ", "), staffName)
		}
		if metadataJSON, err := json.Marshal(metadata); err != nil {
			s.logger.Printf("web-bff: update maintainer audit metadata encode error: %v", err)
		} else {
			event := model.AuditLog{
				MaintainerID: &id,
				StaffID:      staffID,
				Action:       "MAINTAINER_UPDATE",
				Message:      message,
				Metadata:     string(metadataJSON),
			}
			if err := s.store.DB().Create(&event).Error; err != nil {
				s.logger.Printf("web-bff: update maintainer audit log failed: %v", err)
			}
		}

		projects := make([]maintainerProjectResponse, 0, len(updated.Projects))
		for _, project := range updated.Projects {
			projects = append(projects, maintainerProjectResponse{
				ID:   project.ID,
				Name: project.Name,
			})
		}

		response := maintainerDetailResponse{
			ID:          updated.ID,
			Name:        updated.Name,
			Email:       normalizeValue(updated.Email, "EMAIL_MISSING"),
			GitHub:      normalizeValue(updated.GitHubAccount, "GITHUB_MISSING"),
			GitHubEmail: normalizeValue(updated.GitHubEmail, "GITHUB_MISSING"),
			Status:      string(updated.MaintainerStatus),
			Projects:    projects,
			CreatedAt:   updated.CreatedAt,
			UpdatedAt:   updated.UpdatedAt,
		}
		if updated.DeletedAt.Valid {
			deleted := updated.DeletedAt.Time
			response.DeletedAt = &deleted
		}
		if updated.CompanyID != nil {
			response.CompanyID = updated.CompanyID
		}
		if updated.Company.Name != "" {
			response.Company = updated.Company.Name
		}
		if staffName != "" {
			response.UpdatedBy = staffName
		}

		w.Header().Set(headerContentType, contentTypeJSON)
		if err := json.NewEncoder(w).Encode(response); err != nil {
			s.logger.Printf("web-bff: handleMaintainer update encode error: %v", err)
		}
		return
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
}

func (s *server) handleAudit(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	session := sessionFromContext(r.Context())
	if session == nil || session.Role != roleStaff {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	limit := parseIntParam(r, "limit", 20, 1, 200)
	offset := parseIntParam(r, "offset", 0, 0, 10_000_000)

	base := s.store.DB().Model(&model.AuditLog{})
	var total int64
	if err := base.Count(&total).Error; err != nil {
		s.logger.Printf("web-bff: handleAudit count error: %v", err)
		http.Error(w, "failed to load audit logs", http.StatusInternalServerError)
		return
	}

	var logs []model.AuditLog
	if err := base.
		Preload("Staff").
		Order("created_at desc").
		Limit(limit).
		Offset(offset).
		Find(&logs).Error; err != nil {
		s.logger.Printf("web-bff: handleAudit list error: %v", err)
		http.Error(w, "failed to load audit logs", http.StatusInternalServerError)
		return
	}

	response := auditListResponse{
		Total: total,
		Logs:  make([]auditLogResponse, 0, len(logs)),
	}
	for _, logEntry := range logs {
		item := auditLogResponse{
			ID:           logEntry.ID,
			Action:       logEntry.Action,
			Message:      logEntry.Message,
			Metadata:     logEntry.Metadata,
			CreatedAt:    logEntry.CreatedAt,
			ProjectID:    logEntry.ProjectID,
			MaintainerID: logEntry.MaintainerID,
			ServiceID:    logEntry.ServiceID,
			StaffID:      logEntry.StaffID,
		}
		if logEntry.Staff != nil {
			item.StaffName = logEntry.Staff.Name
			item.StaffLogin = logEntry.Staff.GitHubAccount
		}
		response.Logs = append(response.Logs, item)
	}

	w.Header().Set(headerContentType, contentTypeJSON)
	if err := json.NewEncoder(w).Encode(response); err != nil {
		s.logger.Printf("web-bff: handleAudit encode error: %v", err)
	}
}

type maintainerUpdateRequest struct {
	Name      string `json:"name"`
	Email     string `json:"email"`
	GitHub    string `json:"github"`
	Status    string `json:"status"`
	CompanyID *uint  `json:"companyId"`
}

type maintainerStatusUpdateRequest struct {
	IDs    []uint `json:"ids"`
	Status string `json:"status"`
}

func (s *server) handleMaintainerStatusUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	session := sessionFromContext(r.Context())
	if session == nil || session.Role != roleStaff {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	var req maintainerStatusUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if len(req.IDs) == 0 {
		http.Error(w, "no maintainer ids provided", http.StatusBadRequest)
		return
	}
	status := model.MaintainerStatus(strings.TrimSpace(req.Status))
	if !status.IsValid() {
		http.Error(w, "invalid status", http.StatusBadRequest)
		return
	}

	if err := s.store.UpdateMaintainersStatus(req.IDs, status); err != nil {
		s.logger.Printf("web-bff: maintainer status update failed ids=%v status=%s err=%v", req.IDs, status, err)
		http.Error(w, "failed to update maintainers", http.StatusInternalServerError)
		return
	}

	w.Header().Set(headerContentType, contentTypeJSON)
	if err := json.NewEncoder(w).Encode(map[string]string{"status": "ok"}); err != nil {
		s.logger.Printf("web-bff: handleMaintainerStatusUpdate encode error: %v", err)
	}
}

type addMaintainerRequest struct {
	ProjectID    uint   `json:"projectId"`
	Name         string `json:"name"`
	GitHubHandle string `json:"githubHandle"`
	Email        string `json:"email"`
	Company      string `json:"company"`
}

type addMaintainerResponse struct {
	ID      uint   `json:"id"`
	Name    string `json:"name"`
	GitHub  string `json:"github"`
	Email   string `json:"email,omitempty"`
	Company string `json:"company,omitempty"`
}

func (s *server) handleMaintainerFromRef(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	session := sessionFromContext(r.Context())
	if session == nil || session.Role != roleStaff {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var req addMaintainerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.GitHubHandle = strings.TrimSpace(req.GitHubHandle)
	req.Email = strings.TrimSpace(req.Email)
	req.Company = strings.TrimSpace(req.Company)
	if req.ProjectID == 0 || req.GitHubHandle == "" {
		http.Error(w, "missing required fields", http.StatusBadRequest)
		return
	}
	maintainer, err := s.store.CreateMaintainer(req.ProjectID, req.Name, req.Email, req.GitHubHandle, req.Company)
	if err != nil {
		if errors.Is(err, db.ErrProjectNotFound) {
			http.Error(w, "project not found", http.StatusNotFound)
			return
		}
		http.Error(w, "failed to create maintainer", http.StatusInternalServerError)
		return
	}
	response := addMaintainerResponse{
		ID:     maintainer.ID,
		Name:   maintainer.Name,
		GitHub: normalizeValue(maintainer.GitHubAccount, "GITHUB_MISSING"),
		Email:  normalizeValue(maintainer.Email, "EMAIL_MISSING"),
	}
	if maintainer.Company.Name != "" {
		response.Company = maintainer.Company.Name
	}
	w.Header().Set(headerContentType, contentTypeJSON)
	if err := json.NewEncoder(w).Encode(response); err != nil {
		s.logger.Printf("web-bff: handleCompanies encode error: %v", err)
	}
}

type companyResponse struct {
	ID   uint   `json:"id"`
	Name string `json:"name"`
}

type createCompanyRequest struct {
	Name string `json:"name"`
}

type mergeCompanyRequest struct {
	FromID uint `json:"fromId"`
	ToID   uint `json:"toId"`
}

type companyDetailResponse struct {
	ID              uint   `json:"id"`
	Name            string `json:"name"`
	MaintainerCount int64  `json:"maintainerCount"`
}

type companyDuplicateGroup struct {
	Canonical string                  `json:"canonical"`
	Variants  []companyDetailResponse `json:"variants"`
}

func (s *server) handleCompanies(w http.ResponseWriter, r *http.Request) {
	session := sessionFromContext(r.Context())
	if session == nil || (session.Role != roleStaff && session.Role != roleMaintainer) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	switch r.Method {
	case http.MethodGet:
		companies, err := s.store.ListCompanies()
		if err != nil {
			http.Error(w, "failed to load companies", http.StatusInternalServerError)
			return
		}

		// Maintainer counts
		type companyWithCount struct {
			model.Company
			MCount int64
		}
		var withCounts []companyWithCount
		if err := s.store.DB().
			Table("companies").
			Select("companies.*, COUNT(m.id) as m_count").
			Joins("LEFT JOIN maintainers m ON m.company_id = companies.id").
			Group("companies.id").
			Scan(&withCounts).Error; err != nil {
			s.logger.Printf("web-bff: handleCompanies counts error: %v", err)
			http.Error(w, "failed to load companies", http.StatusInternalServerError)
			return
		}
		countMap := make(map[uint]int64, len(withCounts))
		for _, c := range withCounts {
			countMap[c.ID] = c.MCount
		}

		resp := make([]companyDetailResponse, 0, len(companies))
		for _, company := range companies {
			if strings.TrimSpace(company.Name) == "" {
				continue
			}
			resp = append(resp, companyDetailResponse{
				ID:              company.ID,
				Name:            company.Name,
				MaintainerCount: countMap[company.ID],
			})
		}

		if strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("duplicates")), "true") {
			dups := groupCompanyDuplicates(resp)
			w.Header().Set(headerContentType, contentTypeJSON)
			if err := json.NewEncoder(w).Encode(dups); err != nil {
				s.logger.Printf("web-bff: handleCompanies encode error: %v", err)
			}
			return
		}

		w.Header().Set(headerContentType, contentTypeJSON)
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			s.logger.Printf("web-bff: handleCompanies encode error: %v", err)
		}
	case http.MethodPost:
		if session.Role != roleStaff && session.Role != roleMaintainer {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		var req createCompanyRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		company, err := s.store.CreateCompany(req.Name)
		if err != nil {
			if errors.Is(err, db.ErrCompanyExists) {
				http.Error(w, "company already exists", http.StatusConflict)
				return
			}
			http.Error(w, "failed to create company", http.StatusBadRequest)
			return
		}
		var staffID *uint
		var maintainerID *uint
		actorName := session.Login
		if session.Role == roleStaff && session.Login != "" {
			var staff model.StaffMember
			if err := s.store.DB().
				Where("LOWER(git_hub_account) = ?", strings.ToLower(session.Login)).
				First(&staff).Error; err == nil {
				staffID = &staff.ID
				if staff.Name != "" {
					actorName = staff.Name
				}
			}
		}
		if session.Role == roleMaintainer && session.Login != "" {
			if maintainer, err := s.getMaintainerByLogin(session.Login); err == nil {
				maintainerID = &maintainer.ID
				if strings.TrimSpace(maintainer.Name) != "" {
					actorName = maintainer.Name
				}
			}
		}
		metadata := map[string]any{
			"actor": map[string]string{
				"login": session.Login,
				"role":  session.Role,
			},
			"company": map[string]any{
				"id":   company.ID,
				"name": company.Name,
			},
		}
		if metadataJSON, err := json.Marshal(metadata); err != nil {
			s.logger.Printf("web-bff: create company audit metadata encode error: %v", err)
		} else {
			event := model.AuditLog{
				StaffID:      staffID,
				MaintainerID: maintainerID,
				Action:       "COMPANY_CREATE",
				Message:      fmt.Sprintf("Company created by %s", actorName),
				Metadata:     string(metadataJSON),
			}
			if err := s.store.DB().Create(&event).Error; err != nil {
				s.logger.Printf("web-bff: create company audit log failed: %v", err)
			}
		}
		w.Header().Set(headerContentType, contentTypeJSON)
		if err := json.NewEncoder(w).Encode(companyResponse{ID: company.ID, Name: company.Name}); err != nil {
			s.logger.Printf("web-bff: handleCompanies encode error: %v", err)
		}
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *server) handleAPINotImplemented(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, "api not implemented", http.StatusNotImplemented)
}

func (s *server) handleCompanyMerge(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	session := sessionFromContext(r.Context())
	if session == nil || session.Role != roleStaff {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var req mergeCompanyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if req.FromID == 0 || req.ToID == 0 || req.FromID == req.ToID {
		http.Error(w, "invalid ids", http.StatusBadRequest)
		return
	}
	if err := s.store.MergeCompanies(req.FromID, req.ToID); err != nil {
		s.logger.Printf("web-bff: merge companies error: %v", err)
		http.Error(w, "failed to merge companies", http.StatusBadRequest)
		return
	}
	w.Header().Set(headerContentType, contentTypeJSON)
	if err := json.NewEncoder(w).Encode(map[string]string{"status": "ok"}); err != nil {
		s.logger.Printf("web-bff: handleCompanyMerge encode error: %v", err)
	}
}

type resolveOnboardingRequest struct {
	IssueURL string `json:"issueUrl"`
}

type resolveOnboardingResponse struct {
	Title       string `json:"title"`
	ProjectName string `json:"projectName"`
}

func (s *server) handleResolveOnboarding(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	session := sessionFromContext(r.Context())
	if session == nil || session.Role != roleStaff {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var req resolveOnboardingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	issueURL := strings.TrimSpace(req.IssueURL)
	if issueURL == "" {
		http.Error(w, "issueUrl is required", http.StatusBadRequest)
		return
	}
	if s.githubToken == "" {
		http.Error(w, "github api token not configured", http.StatusInternalServerError)
		return
	}
	owner, repo, number, err := parseGitHubIssueURL(issueURL)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	title, err := s.fetchIssueTitle(r.Context(), owner, repo, number)
	if err != nil {
		s.logger.Printf("web-bff: resolve onboarding error owner=%s repo=%s issue=%d err=%v", owner, repo, number, err)
		http.Error(w, "failed to fetch onboarding issue", http.StatusBadGateway)
		return
	}
	projectName, err := onboarding.GetProjectNameFromProjectTitle(title)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set(headerContentType, contentTypeJSON)
	if err := json.NewEncoder(w).Encode(resolveOnboardingResponse{
		Title:       title,
		ProjectName: projectName,
	}); err != nil {
		s.logger.Printf("web-bff: resolve onboarding encode error: %v", err)
	}
}

type onboardingIssuesResponse struct {
	Issues []onboardingIssueSummary `json:"issues"`
}

func (s *server) handleOnboardingIssues(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	session := sessionFromContext(r.Context())
	if session == nil || session.Role != roleStaff {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if s.githubToken == "" {
		s.logger.Printf("web-bff: onboarding issues error: github api token not configured")
		http.Error(w, "github api token not configured", http.StatusInternalServerError)
		return
	}
	issues, err := s.getOnboardingIssues(r.Context())
	if err != nil {
		s.logger.Printf("web-bff: onboarding issues error: %v", err)
		http.Error(w, "failed to fetch onboarding issues", http.StatusBadGateway)
		return
	}
	s.logger.Printf("web-bff: onboarding issues total=%d", len(issues))
	w.Header().Set(headerContentType, contentTypeJSON)
	if err := json.NewEncoder(w).Encode(onboardingIssuesResponse{Issues: issues}); err != nil {
		s.logger.Printf("web-bff: onboarding issues encode error: %v", err)
	}
}

func (s *server) getOnboardingIssues(ctx context.Context) ([]onboardingIssueSummary, error) {
	if s.fetchIssues == nil {
		return nil, fmt.Errorf("onboarding issue fetcher not configured")
	}
	if s.onboardingCache == nil {
		raw, filtered, err := s.fetchAndFilterOnboardingIssues(ctx)
		if err != nil {
			return nil, err
		}
		_ = raw
		return filtered, nil
	}
	now := time.Now()
	s.onboardingCache.mu.RLock()
	if now.Before(s.onboardingCache.expires) && len(s.onboardingCache.issues) > 0 {
		cached := make([]onboardingIssueSummary, len(s.onboardingCache.issues))
		copy(cached, s.onboardingCache.issues)
		s.onboardingCache.mu.RUnlock()
		return cached, nil
	}
	s.onboardingCache.mu.RUnlock()

	raw, filtered, err := s.fetchAndFilterOnboardingIssues(ctx)
	if err != nil {
		return nil, err
	}
	s.onboardingCache.mu.Lock()
	s.onboardingCache.raw = raw
	s.onboardingCache.issues = filtered
	s.onboardingCache.expires = now.Add(onboardingIssueCacheTTL)
	s.onboardingCache.mu.Unlock()
	return filtered, nil
}

func (s *server) getOnboardingIssuesRaw(ctx context.Context) ([]onboardingIssueSummary, error) {
	if s.fetchIssues == nil {
		return nil, fmt.Errorf("onboarding issue fetcher not configured")
	}
	if s.onboardingCache == nil {
		raw, _, err := s.fetchAndFilterOnboardingIssues(ctx)
		return raw, err
	}
	now := time.Now()
	s.onboardingCache.mu.RLock()
	if now.Before(s.onboardingCache.expires) && len(s.onboardingCache.raw) > 0 {
		cached := make([]onboardingIssueSummary, len(s.onboardingCache.raw))
		copy(cached, s.onboardingCache.raw)
		s.onboardingCache.mu.RUnlock()
		return cached, nil
	}
	s.onboardingCache.mu.RUnlock()

	raw, filtered, err := s.fetchAndFilterOnboardingIssues(ctx)
	if err != nil {
		return nil, err
	}
	s.onboardingCache.mu.Lock()
	s.onboardingCache.raw = raw
	s.onboardingCache.issues = filtered
	s.onboardingCache.expires = now.Add(onboardingIssueCacheTTL)
	s.onboardingCache.mu.Unlock()
	return raw, nil
}

func (s *server) fetchAndFilterOnboardingIssues(ctx context.Context) ([]onboardingIssueSummary, []onboardingIssueSummary, error) {
	raw, err := s.fetchIssues(ctx)
	if err != nil {
		return nil, nil, err
	}
	if s.store == nil {
		return raw, raw, nil
	}
	filtered := make([]onboardingIssueSummary, 0, len(raw))
	for _, issue := range raw {
		var count int64
		query := s.store.DB().Model(&model.Project{})
		if issue.URL != "" {
			query = query.Where("LOWER(onboarding_issue) = ?", strings.ToLower(issue.URL))
		}
		if issue.ProjectName != "" {
			query = query.Or("LOWER(name) = ?", strings.ToLower(issue.ProjectName))
		}
		if err := query.Count(&count).Error; err != nil {
			return nil, nil, err
		}
		if count == 0 {
			filtered = append(filtered, issue)
			continue
		}
		s.logger.Printf(
			"web-bff: onboarding issue filtered url=%s projectName=%q",
			issue.URL,
			issue.ProjectName,
		)
	}
	s.logger.Printf(
		"web-bff: onboarding issues remaining=%d filteredOut=%d",
		len(filtered),
		len(raw)-len(filtered),
	)
	return raw, filtered, nil
}

func (s *server) fetchOnboardingIssuesFromGitHub(ctx context.Context) ([]onboardingIssueSummary, error) {
	client := github.NewClient(oauth2.NewClient(ctx, oauth2.StaticTokenSource(&oauth2.Token{
		AccessToken: s.githubToken,
	})))
	query := `repo:cncf/sandbox is:issue state:open label:"project onboarding"`
	options := &github.SearchOptions{ListOptions: github.ListOptions{PerPage: 100}}
	issues := make([]onboardingIssueSummary, 0, 128)
	for {
		result, resp, err := client.Search.Issues(ctx, query, options)
		if err != nil {
			return nil, err
		}
		for _, issue := range result.Issues {
			title := issue.GetTitle()
			projectName, err := onboarding.GetProjectNameFromProjectTitle(title)
			if err != nil {
				projectName = ""
			}
			issues = append(issues, onboardingIssueSummary{
				Number:      issue.GetNumber(),
				Title:       title,
				URL:         issue.GetHTMLURL(),
				ProjectName: projectName,
			})
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		options.Page = resp.NextPage
	}
	s.logger.Printf("web-bff: onboarding issues fetched=%d", len(issues))
	return issues, nil
}

func parseGitHubIssueURL(raw string) (string, string, int, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", "", 0, fmt.Errorf("invalid issue url")
	}
	if parsed.Host != "github.com" && parsed.Host != "www.github.com" {
		return "", "", 0, fmt.Errorf("issue url must be github.com")
	}
	path := strings.Trim(parsed.Path, "/")
	parts := strings.Split(path, "/")
	if len(parts) < 4 || parts[2] != "issues" {
		return "", "", 0, fmt.Errorf("issue url must be in form https://github.com/org/repo/issues/123")
	}
	number, err := strconv.Atoi(parts[3])
	if err != nil || number <= 0 {
		return "", "", 0, fmt.Errorf("invalid issue number")
	}
	return parts[0], parts[1], number, nil
}

func issueNumberFromURL(raw *string) (int, bool) {
	if raw == nil {
		return 0, false
	}
	value := strings.TrimSpace(*raw)
	if value == "" {
		return 0, false
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return 0, false
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) < 4 || parts[2] != "issues" {
		return 0, false
	}
	number, err := strconv.Atoi(parts[3])
	if err != nil || number <= 0 {
		return 0, false
	}
	return number, true
}

func parseGitHubOrgFromURL(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid github url")
	}
	host := parsed.Host
	if host != "github.com" && host != "www.github.com" && host != "raw.githubusercontent.com" {
		return "", fmt.Errorf("maintainer url must be on github.com or raw.githubusercontent.com")
	}
	path := strings.Trim(parsed.Path, "/")
	parts := strings.Split(path, "/")
	if len(parts) < 2 {
		return "", fmt.Errorf("maintainer url must include org and repo")
	}
	return parts[0], nil
}

func (s *server) fetchIssueTitleFromGitHub(ctx context.Context, owner, repo string, number int) (string, error) {
	client := github.NewClient(oauth2.NewClient(ctx, oauth2.StaticTokenSource(&oauth2.Token{
		AccessToken: s.githubToken,
	})))
	issue, _, err := client.Issues.Get(ctx, owner, repo, number)
	if err != nil {
		return "", err
	}
	return issue.GetTitle(), nil
}

func groupCompanyDuplicates(companies []companyDetailResponse) []companyDuplicateGroup {
	buckets := make(map[string][]companyDetailResponse)
	for _, c := range companies {
		key := strings.ToLower(strings.TrimSpace(c.Name))
		buckets[key] = append(buckets[key], c)
	}
	out := []companyDuplicateGroup{}
	for key, variants := range buckets {
		if len(variants) < 2 {
			continue
		}
		out = append(out, companyDuplicateGroup{
			Canonical: key,
			Variants:  variants,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Canonical < out[j].Canonical
	})
	return out
}

func (s *server) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.webOrigin != "" {
			w.Header().Set("Access-Control-Allow-Origin", s.webOrigin)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Vary", "Origin")
		}
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", "GET,POST,PATCH,OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *server) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sessionCookie, err := r.Cookie(s.cookieName)
		if err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		session, ok := s.sessions.Get(sessionCookie.Value)
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		ctx := context.WithValue(r.Context(), sessionKey{}, session)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func clientIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		parts := strings.Split(forwarded, ",")
		if len(parts) > 0 {
			return strings.TrimSpace(parts[0])
		}
	}
	if realIP := strings.TrimSpace(r.Header.Get("X-Real-IP")); realIP != "" {
		return realIP
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

func (s *server) authorizeLogin(login string) (string, bool) {
	if login == "" {
		return "", false
	}

	staff, err := s.store.ListStaffMembers()
	if err != nil {
		s.logger.Printf("web-bff: failed to load staff: %v", err)
		return "", false
	}
	for _, member := range staff {
		if strings.EqualFold(member.GitHubAccount, login) {
			return roleStaff, true
		}
	}

	maintainers, err := s.store.GetMaintainerMapByGitHubAccount()
	if err != nil {
		s.logger.Printf("web-bff: failed to load maintainers: %v", err)
		return "", false
	}
	for ghLogin, maintainer := range maintainers {
		if maintainer.GitHubAccount == "" || maintainer.GitHubAccount == "GITHUB_MISSING" {
			continue
		}
		if strings.EqualFold(ghLogin, login) {
			return roleMaintainer, true
		}
	}

	return "", false
}

func (s *server) getMaintainerByLogin(login string) (*model.Maintainer, error) {
	if strings.TrimSpace(login) == "" {
		return nil, fmt.Errorf("missing login")
	}
	var maintainer model.Maintainer
	if err := s.store.DB().
		Where("LOWER(git_hub_account) = ?", strings.ToLower(login)).
		First(&maintainer).Error; err != nil {
		return nil, err
	}
	if strings.TrimSpace(maintainer.GitHubAccount) == "" || maintainer.GitHubAccount == "GITHUB_MISSING" {
		return nil, fmt.Errorf("maintainer has no github account")
	}
	return &maintainer, nil
}

func fetchGitHubUser(ctx context.Context, token *oauth2.Token) (*github.User, error) {
	if token == nil {
		return nil, errors.New("missing oauth token")
	}
	client := github.NewClient(oauth2.NewClient(ctx, oauth2.StaticTokenSource(token)))
	user, _, err := client.Users.Get(ctx, "")
	if err != nil {
		return nil, err
	}
	return user, nil
}

type sessionKey struct{}

func sessionFromContext(ctx context.Context) *session {
	if ctx == nil {
		return nil
	}
	if value := ctx.Value(sessionKey{}); value != nil {
		if s, ok := value.(session); ok {
			return &s
		}
	}
	return nil
}

func newSessionStore(logger *log.Logger) *sessionStore {
	return &sessionStore{
		sessions: make(map[string]session),
		logger:   logger,
	}
}

func (s *sessionStore) Set(sess session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[sess.ID] = sess
}

func (s *sessionStore) Get(id string) (session, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sess, ok := s.sessions[id]
	if !ok {
		return session{}, false
	}
	if time.Now().After(sess.ExpiresAt) {
		if s.logger != nil {
			duration := time.Since(sess.CreatedAt).Truncate(time.Second)
			s.logger.Printf("web-bff: session expired user=%s role=%s session_duration=%s", sess.Login, sess.Role, duration)
		}
		return session{}, false
	}
	return sess, true
}

func (s *sessionStore) Delete(id string) (session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if ok {
		delete(s.sessions, id)
		return sess, true
	}
	return session{}, false
}

func newStateStore(ttl time.Duration) *stateStore {
	return &stateStore{
		states: make(map[string]stateEntry),
		ttl:    ttl,
	}
}

func (s *stateStore) Set(state string, entry stateEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.states[state] = entry
}

func (s *stateStore) Consume(state string) (stateEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.states[state]
	if !ok || time.Now().After(entry.Expires) {
		delete(s.states, state)
		return stateEntry{}, false
	}
	delete(s.states, state)
	return entry, true
}

func sanitizeRedirect(raw string) string {
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, "/") && !strings.HasPrefix(raw, "//") {
		return raw
	}
	return ""
}

func originFromBaseURL(baseURL string) string {
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ""
	}
	return fmt.Sprintf("%s://%s", parsed.Scheme, parsed.Host)
}

func randomToken(length int) (string, error) {
	bytes := make([]byte, length)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(bytes), nil
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func parseDuration(raw string, fallback time.Duration) time.Duration {
	if raw == "" {
		return fallback
	}
	value, err := time.ParseDuration(raw)
	if err != nil {
		return fallback
	}
	return value
}

func parseIntParam(r *http.Request, key string, fallback, min, max int) int {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}

func parseCSVParam(r *http.Request, key string) []string {
	values := r.URL.Query()[key]
	if len(values) == 0 {
		if raw := r.URL.Query().Get(key); raw != "" {
			values = []string{raw}
		}
	}
	var out []string
	for _, value := range values {
		parts := strings.Split(value, ",")
		for _, part := range parts {
			item := strings.TrimSpace(part)
			if item != "" {
				out = append(out, item)
			}
		}
	}
	return out
}

func parseIDParam(path, prefix string) (uint, error) {
	trimmed := strings.TrimPrefix(path, prefix)
	trimmed = strings.Trim(trimmed, "/")
	if trimmed == "" {
		return 0, fmt.Errorf("missing id")
	}
	value, err := strconv.Atoi(trimmed)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("invalid id")
	}
	return uint(value), nil
}

func parseMaturity(value string) (model.Maturity, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "sandbox":
		return model.Sandbox, true
	case "incubating":
		return model.Incubating, true
	case "graduated":
		return model.Graduated, true
	case "archived":
		return model.Archived, true
	default:
		return "", false
	}
}

func normalizeValue(value, sentinel string) string {
	if value == sentinel {
		return ""
	}
	return value
}

func summarizeMaintainers(maintainers []model.Maintainer) []maintainerSummary {
	seen := make(map[string]struct{})
	result := make([]maintainerSummary, 0, len(maintainers))
	for _, maintainer := range maintainers {
		name := strings.TrimSpace(maintainer.Name)
		github := strings.TrimSpace(maintainer.GitHubAccount)
		if github == "GITHUB_MISSING" {
			github = ""
		}
		key := fmt.Sprintf("%s|%s", name, github)
		if key == "|" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, maintainerSummary{
			ID:     maintainer.ID,
			Name:   name,
			GitHub: github,
		})
	}
	return result
}

func summarizeMaintainerDetails(maintainers []model.Maintainer, refMatches map[uint]bool) []projectMaintainerDetail {
	seen := make(map[string]struct{})
	result := make([]projectMaintainerDetail, 0, len(maintainers))
	for _, maintainer := range maintainers {
		name := strings.TrimSpace(maintainer.Name)
		github := strings.TrimSpace(maintainer.GitHubAccount)
		if github == "GITHUB_MISSING" {
			github = ""
		}
		key := fmt.Sprintf("%s|%s", name, github)
		if key == "|" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, projectMaintainerDetail{
			ID:              maintainer.ID,
			Name:            name,
			GitHub:          github,
			InMaintainerRef: refMatches[maintainer.ID],
			Status:          string(maintainer.MaintainerStatus),
			Company:         strings.TrimSpace(maintainer.Company.Name),
		})
	}
	return result
}

func fetchMaintainerRef(ctx context.Context, refURL string) (string, error) {
	rewritten, err := rewriteMaintainerRefURL(refURL)
	if err != nil {
		return "", fmt.Errorf("invalid maintainer ref url")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rewritten, nil)
	if err != nil {
		return "", err
	}
	client := &http.Client{
		Timeout: 5 * time.Second,
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func rewriteMaintainerRefURL(refURL string) (string, error) {
	parsed, err := url.Parse(refURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", fmt.Errorf("invalid maintainer ref url")
	}
	if strings.EqualFold(parsed.Host, "github.com") {
		parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
		if len(parts) >= 5 && parts[2] == "blob" {
			org := parts[0]
			repo := parts[1]
			branch := parts[3]
			filePath := strings.Join(parts[4:], "/")
			parsed.Host = "raw.githubusercontent.com"
			parsed.Path = fmt.Sprintf("/%s/%s/%s/%s", org, repo, branch, filePath)
		}
	}
	return parsed.String(), nil
}

func buildMaintainerRefMatches(refBody string, maintainers []model.Maintainer) map[uint]bool {
	matches := make(map[uint]bool)
	if refBody == "" {
		return matches
	}
	for _, maintainer := range maintainers {
		handle := strings.TrimSpace(maintainer.GitHubAccount)
		if handle == "" || handle == "GITHUB_MISSING" {
			continue
		}
		ok, err := refparse.MaintainerRefContains(refBody, handle)
		if err != nil {
			log.Printf("maintainer ref parse error (maintainer=%d handle=%q): %v", maintainer.ID, handle, err)
			continue
		}
		if ok {
			matches[maintainer.ID] = true
		}
	}
	return matches
}

func buildMaintainerRefOnly(refBody string, maintainers []model.Maintainer) []string {
	handles := refparse.ExtractGitHubHandles(refBody)
	if len(handles) == 0 {
		return nil
	}
	internal := make(map[string]struct{}, len(maintainers))
	for _, maintainer := range maintainers {
		handle := strings.TrimSpace(maintainer.GitHubAccount)
		if handle == "" || handle == "GITHUB_MISSING" {
			continue
		}
		internal[strings.ToLower(handle)] = struct{}{}
	}
	out := make([]string, 0, len(handles))
	for handle := range handles {
		if _, ok := internal[handle]; !ok {
			out = append(out, handle)
		}
	}
	sort.Strings(out)
	return out
}

func buildMaintainerRefLines(refBody string) map[string]string {
	lines := strings.Split(refBody, "\n")
	result := make(map[string]string)
	if len(lines) == 0 {
		return result
	}
	atRe := regexp.MustCompile(`(?i)(^|[^a-z0-9_-])@([a-z0-9-]{1,39})`)
	urlRe := regexp.MustCompile(`(?i)github\.com/([a-z0-9-]{1,39})`)
	listItemRe := regexp.MustCompile(`(?i)^\s*[-*]\s*([a-z0-9][a-z0-9-]{0,38})\b`)
	keyRe := regexp.MustCompile(`(?i)^\s*github\s*:\s*([a-z0-9][a-z0-9-]{0,38})\b`)

	headerMatch := func(header string) bool {
		normalized := strings.ToLower(strings.TrimSpace(header))
		switch normalized {
		case "github", "github id", "github username", "github handle", "github account":
			return true
		}
		return false
	}
	isSeparatorRow := func(cells []string) bool {
		if len(cells) == 0 {
			return false
		}
		for _, cell := range cells {
			trimmed := strings.TrimSpace(cell)
			if trimmed == "" {
				continue
			}
			for _, ch := range trimmed {
				if ch != '-' && ch != ':' {
					return false
				}
			}
		}
		return true
	}
	parseRow := func(line string) []string {
		if !strings.Contains(line, "|") {
			return nil
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			return nil
		}
		trimmed = strings.TrimPrefix(trimmed, "|")
		trimmed = strings.TrimSuffix(trimmed, "|")
		parts := strings.Split(trimmed, "|")
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}
		return parts
	}
	isValidHandle := func(handle string) bool {
		handle = strings.ToLower(strings.TrimSpace(handle))
		if handle == "" || handle == "organizations" || handle == "orgs" || handle == "repos" {
			return false
		}
		if len(handle) > 39 {
			return false
		}
		for i, r := range handle {
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
				continue
			}
			if r == '_' && i == 0 {
				return false
			}
			return false
		}
		return true
	}

	for i := 0; i+1 < len(lines); i++ {
		headerCells := parseRow(lines[i])
		if len(headerCells) == 0 {
			continue
		}
		separatorCells := parseRow(lines[i+1])
		if len(separatorCells) == 0 || !isSeparatorRow(separatorCells) {
			continue
		}
		githubIndex := -1
		for idx, cell := range headerCells {
			if headerMatch(cell) {
				githubIndex = idx
				break
			}
		}
		if githubIndex < 0 {
			continue
		}
		for row := i + 2; row < len(lines); row++ {
			rowLine := lines[row]
			rowCells := parseRow(rowLine)
			if len(rowCells) == 0 {
				break
			}
			if isSeparatorRow(rowCells) {
				break
			}
			if githubIndex >= len(rowCells) {
				continue
			}
			cell := strings.TrimSpace(rowCells[githubIndex])
			if cell == "" {
				continue
			}
			cell = strings.Trim(cell, "`")
			cell = strings.TrimPrefix(cell, "@")
			if !isValidHandle(cell) {
				continue
			}
			handle := strings.ToLower(cell)
			if _, ok := result[handle]; !ok {
				result[handle] = strings.TrimSpace(rowLine)
			}
		}
		i++
	}

	for _, line := range lines {
		for _, match := range atRe.FindAllStringSubmatch(line, -1) {
			if len(match) < 3 {
				continue
			}
			handle := strings.ToLower(match[2])
			if _, ok := result[handle]; !ok {
				result[handle] = strings.TrimSpace(line)
			}
		}
		for _, match := range urlRe.FindAllStringSubmatchIndex(line, -1) {
			if len(match) < 4 {
				continue
			}
			handle := strings.ToLower(line[match[2]:match[3]])
			if handle == "organizations" || handle == "orgs" || handle == "repos" {
				continue
			}
			if match[1] < len(line) && line[match[1]] == '/' {
				continue
			}
			if _, ok := result[handle]; !ok {
				result[handle] = strings.TrimSpace(line)
			}
		}
		if match := listItemRe.FindStringSubmatch(line); len(match) > 1 {
			handle := strings.ToLower(match[1])
			if handle != "organizations" && handle != "orgs" && handle != "repos" {
				if _, ok := result[handle]; !ok {
					result[handle] = strings.TrimSpace(line)
				}
			}
		}
		if match := keyRe.FindStringSubmatch(line); len(match) > 1 {
			handle := strings.ToLower(match[1])
			if handle != "organizations" && handle != "orgs" && handle != "repos" {
				if _, ok := result[handle]; !ok {
					result[handle] = strings.TrimSpace(line)
				}
			}
		}
	}
	return result
}
