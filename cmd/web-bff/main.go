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
	pathpkg "path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"maintainerd/db"
	"maintainerd/dotproject"
	"maintainerd/model"
	"maintainerd/onboarding"
	"maintainerd/plugins/fossa"
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
	oauthConfig                 *oauth2.Config
	store                       *db.SQLStore
	sessions                    *sessionStore
	oauthStates                 *stateStore
	cookieName                  string
	stateCookie                 string
	webBaseURL                  string
	cookieDomain                string
	cookieSecure                bool
	sessionTTL                  time.Duration
	webOrigin                   string
	testMode                    bool
	allowLiveFossa              bool
	logger                      *log.Logger
	githubToken                 string
	fossaToken                  string
	fetchIssueTitle             func(ctx context.Context, owner, repo string, number int) (string, error)
	onboardingCache             *onboardingIssueCache
	fetchIssues                 func(ctx context.Context) ([]onboardingIssueSummary, error)
	createDotProjectPullRequest func(ctx context.Context, input dotProjectPullRequestInput, githubToken string) (*dotProjectPullRequestResponse, error)
	githubClient                func(ctx context.Context, token string) *github.Client
	fossaTeamCacheMu            sync.RWMutex
	fossaTeamCache              map[uint]cachedFossaTeam
}

type cachedFossaTeam struct {
	emails    []string
	fetchedAt time.Time
}

type session struct {
	ID          string
	Login       string
	Role        string
	AccessToken string
	CreatedAt   time.Time
	ExpiresAt   time.Time
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
	allowLiveFossa := envOr("BFF_ALLOW_LIVE_FOSSA", "") == "true"

	clientID := os.Getenv("GITHUB_OAUTH_CLIENT_ID")
	clientSecret := os.Getenv("GITHUB_OAUTH_CLIENT_SECRET")
	githubToken := strings.TrimSpace(os.Getenv("GITHUB_API_TOKEN"))
	fossaToken := strings.TrimSpace(os.Getenv("FOSSA_API_TOKEN"))
	if dbDriver == "sqlite" {
		logger.Printf(
			"web-bff: config addr=%s dbDriver=%s dbPath=%s webBaseURL=%s redirectURL=%s cookieName=%s cookieDomain=%s stateCookie=%s sessionTTL=%s cookieSecure=%t testMode=%t allowLiveFossa=%t clientID=%s",
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
			allowLiveFossa,
			clientID,
		)
	} else {
		logger.Printf(
			"web-bff: config addr=%s dbDriver=%s dbDSNSet=%t webBaseURL=%s redirectURL=%s cookieName=%s cookieDomain=%s stateCookie=%s sessionTTL=%s cookieSecure=%t testMode=%t allowLiveFossa=%t clientID=%s",
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
			allowLiveFossa,
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
			Scopes:       []string{"read:user", "public_repo"},
			Endpoint:     ghoauth.Endpoint,
		},
		store:          store,
		sessions:       newSessionStore(logger),
		oauthStates:    newStateStore(defaultStateTTL),
		cookieName:     cookieName,
		stateCookie:    stateCookie,
		webBaseURL:     strings.TrimRight(webBaseURL, "/"),
		cookieDomain:   cookieDomain,
		cookieSecure:   cookieSecure,
		sessionTTL:     sessionTTL,
		webOrigin:      originFromBaseURL(webBaseURL),
		testMode:       testMode,
		allowLiveFossa: allowLiveFossa,
		logger:         logger,
		githubToken:    githubToken,
		fossaToken:     fossaToken,
		onboardingCache: &onboardingIssueCache{
			expires: time.Time{},
		},
		fossaTeamCache: make(map[uint]cachedFossaTeam),
	}
	s.fetchIssueTitle = s.fetchIssueTitleFromGitHub
	s.fetchIssues = s.fetchOnboardingIssuesFromGitHub
	if testMode {
		// Avoid external GitHub calls in test mode to keep BDD tests deterministic.
		s.fetchIssueTitle = func(_ context.Context, _, _ string, _ int) (string, error) {
			return "[PROJECT ONBOARDING] KubeElasti", nil
		}
		s.fetchIssues = func(_ context.Context) ([]onboardingIssueSummary, error) {
			return []onboardingIssueSummary{
				{
					Number:      123,
					Title:       "[PROJECT ONBOARDING] KubeElasti",
					URL:         "https://github.com/cncf/sandbox/issues/123",
					ProjectName: "KubeElasti",
				},
			}, nil
		}
	}

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
	mux.Handle("/api/search", s.withCORS(s.requireSession(http.HandlerFunc(s.handleSearch))))
	mux.Handle("/api/maintainers/status", s.withCORS(s.requireSession(http.HandlerFunc(s.handleMaintainerStatusUpdate))))
	mux.Handle("/api/maintainers/from-ref", s.withCORS(s.requireSession(http.HandlerFunc(s.handleMaintainerFromRef))))
	mux.Handle("/api/maintainers/", s.withCORS(s.requireSession(http.HandlerFunc(s.handleMaintainer))))
	mux.Handle("/api/audit", s.withCORS(s.requireSession(http.HandlerFunc(s.handleAudit))))
	mux.Handle("/api/companies/merge", s.withCORS(s.requireSession(http.HandlerFunc(s.handleCompanyMerge))))
	mux.Handle("/api/companies", s.withCORS(s.requireSession(http.HandlerFunc(s.handleCompanies))))
	mux.Handle("/api/companies/", s.withCORS(s.requireSession(http.HandlerFunc(s.handleCompany))))
	mux.Handle("/api/onboarding/resolve", s.withCORS(s.requireSession(http.HandlerFunc(s.handleResolveOnboarding))))
	mux.Handle("/api/onboarding/issues", s.withCORS(s.requireSession(http.HandlerFunc(s.handleOnboardingIssues))))
	mux.Handle("/api/services/fossa/invite", s.withCORS(s.requireSession(http.HandlerFunc(s.handleFossaInvite))))
	mux.Handle("/api/services/fossa/choose", s.withCORS(s.requireSession(http.HandlerFunc(s.handleFossaChoose))))
	mux.Handle("/api/services/fossa/invites", s.withCORS(s.requireSession(http.HandlerFunc(s.handleFossaInvites))))
	mux.Handle("/api/services/fossa/invites/refresh", s.withCORS(s.requireSession(http.HandlerFunc(s.handleFossaInviteRefresh))))
	mux.Handle("/api/services/fossa/invites/", s.withCORS(s.requireSession(http.HandlerFunc(s.handleFossaInviteAction))))
	mux.Handle("/api/services/fossa/team/sync", s.withCORS(s.requireSession(http.HandlerFunc(s.handleFossaTeamSync))))
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

	//nolint:gosec // Secure is intentionally configurable for local HTTP development and test mode; HttpOnly and SameSite are set.
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

	if err := s.createSession(login, role, token.AccessToken, w); err != nil {
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

	if err := s.createSession(login, role, "", w); err != nil {
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

	//nolint:gosec // Secure is intentionally configurable for local HTTP development and test mode; HttpOnly and SameSite are set.
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

func (s *server) createSession(login, role, accessToken string, w http.ResponseWriter) error {
	sessionID, err := randomToken(48)
	if err != nil {
		return err
	}

	now := time.Now()
	s.sessions.Set(session{
		ID:          sessionID,
		Login:       login,
		Role:        role,
		AccessToken: accessToken,
		CreatedAt:   now,
		ExpiresAt:   now.Add(s.sessionTTL),
	})

	s.logger.Printf("web-bff: login success user=%s role=%s", login, role)

	//nolint:gosec // Secure is intentionally configurable for local HTTP development and test mode; HttpOnly and SameSite are set.
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
	ID                        uint                `json:"id"`
	Name                      string              `json:"name"`
	Maturity                  string              `json:"maturity"`
	AddedBy                   string              `json:"addedBy"`
	OnboardingIssue           string              `json:"onboardingIssue,omitempty"`
	OnboardingIssueState      string              `json:"onboardingIssueStatus,omitempty"`
	LegacyMaintainerRef       string              `json:"legacyMaintainerRef,omitempty"`
	GitHubOrg                 string              `json:"githubOrg,omitempty"`
	DotProjectRepoRef         string              `json:"dotProjectRepoRef,omitempty"`
	DotProjectProjectRef      string              `json:"dotProjectProjectRef,omitempty"`
	DotProjectMaintainerRef   string              `json:"dotProjectMaintainerRef,omitempty"`
	DotProjectSecurityRef     string              `json:"dotProjectSecurityRef,omitempty"`
	DotProjectContributingRef string              `json:"dotProjectContributingRef,omitempty"`
	DotProjectGovernanceRef   string              `json:"dotProjectGovernanceRef,omitempty"`
	DotProjectSchemaVersion   string              `json:"dotProjectSchemaVersion,omitempty"`
	DotProjectMaintainerCount *uint               `json:"dotProjectMaintainerCount,omitempty"`
	DotProjectLastSyncedAt    *time.Time          `json:"dotProjectLastSyncedAt,omitempty"`
	DotProjectAdoptionStatus  string              `json:"dotProjectAdoptionStatus,omitempty"`
	CreatedAt                 string              `json:"createdAt,omitempty"`
	Maintainers               []maintainerSummary `json:"maintainers,omitempty"`
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
	OnboardingIssue         string `json:"onboardingIssue"`
	ProjectName             string `json:"projectName,omitempty"`
	GitHubOrg               string `json:"githubOrg"`
	ParentProjectID         *uint  `json:"parentProjectId,omitempty"`
	LegacyMaintainerRef     string `json:"legacyMaintainerRef,omitempty"`
	DotProjectMaintainerRef string `json:"dotProjectMaintainerRef,omitempty"`
	Maturity                string `json:"maturity,omitempty"`
}

type projectCreateResponse struct {
	ID        uint      `json:"id"`
	Name      string    `json:"name"`
	Maturity  string    `json:"maturity"`
	GitHubOrg string    `json:"githubOrg"`
	CreatedAt time.Time `json:"createdAt"`
}

type maintainerSummary struct {
	ID       uint   `json:"id"`
	Name     string `json:"name"`
	GitHub   string `json:"github"`
	Country  string `json:"country,omitempty"`
	Location string `json:"location,omitempty"`
	Timezone string `json:"timezone,omitempty"`
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

type fossaTeamMemberSummary struct {
	ID     uint   `json:"id"`
	Name   string `json:"name"`
	GitHub string `json:"github"`
	Email  string `json:"email"`
}

type fossaInviteIneligibleSummary struct {
	ID     uint   `json:"id"`
	Name   string `json:"name"`
	GitHub string `json:"github"`
	Email  string `json:"email"`
	Reason string `json:"reason"`
}

type fossaInviteCandidateSummary struct {
	ID     uint   `json:"id"`
	Name   string `json:"name"`
	GitHub string `json:"github"`
	Email  string `json:"email"`
}

type maintainerRefStatus struct {
	URL       string     `json:"url,omitempty"`
	Status    string     `json:"status"`
	CheckedAt *time.Time `json:"checkedAt,omitempty"`
}

type projectDetailResponse struct {
	ID                        uint                           `json:"id"`
	Name                      string                         `json:"name"`
	Maturity                  string                         `json:"maturity"`
	ParentProjectID           *uint                          `json:"parentProjectId,omitempty"`
	LegacyMaintainerRef       string                         `json:"legacyMaintainerRef,omitempty"`
	DotProjectRepoRef         string                         `json:"dotProjectRepoRef,omitempty"`
	DotProjectProjectRef      string                         `json:"dotProjectProjectRef,omitempty"`
	DotProjectMaintainerRef   string                         `json:"dotProjectMaintainerRef,omitempty"`
	DotProjectSecurityRef     string                         `json:"dotProjectSecurityRef,omitempty"`
	DotProjectContributingRef string                         `json:"dotProjectContributingRef,omitempty"`
	DotProjectGovernanceRef   string                         `json:"dotProjectGovernanceRef,omitempty"`
	DotProjectSchemaVersion   string                         `json:"dotProjectSchemaVersion,omitempty"`
	DotProjectMaintainerCount *uint                          `json:"dotProjectMaintainerCount,omitempty"`
	DotProjectLastSyncedAt    *time.Time                     `json:"dotProjectLastSyncedAt,omitempty"`
	DotProjectAdoptionStatus  string                         `json:"dotProjectAdoptionStatus,omitempty"`
	DotProjectSyncState       *dotProjectSyncStateResponse   `json:"dotProjectSyncState,omitempty"`
	DotProjectMaintainerCache *dotProjectMaintainerCacheBody `json:"dotProjectMaintainerCache,omitempty"`
	DotProjectMaintainerPR    *dotProjectMaintainerPRStatus  `json:"dotProjectMaintainerPullRequest,omitempty"`
	RefStatus                 maintainerRefStatus            `json:"maintainerRefStatus"`
	LegacyMaintainerRefBody   string                         `json:"legacyMaintainerRefBody,omitempty"`
	RefOnlyGitHub             []string                       `json:"refOnlyGitHub"`
	RefLines                  map[string]string              `json:"refLines,omitempty"`
	OnboardingIssue           string                         `json:"onboardingIssue,omitempty"`
	MailingList               string                         `json:"mailingList,omitempty"`
	Maintainers               []projectMaintainerDetail      `json:"maintainers"`
	Services                  []serviceSummary               `json:"services"`
	FossaTeamID               *uint                          `json:"fossaTeamId,omitempty"`
	FossaTeamName             string                         `json:"fossaTeamName,omitempty"`
	FossaTeamMembers          []fossaTeamMemberSummary       `json:"fossaTeamMembers,omitempty"`
	FossaInviteIneligible     []fossaInviteIneligibleSummary `json:"fossaInviteIneligible,omitempty"`
	FossaInviteCandidates     []fossaInviteCandidateSummary  `json:"fossaInviteCandidates,omitempty"`
	CreatedAt                 time.Time                      `json:"createdAt"`
	UpdatedAt                 time.Time                      `json:"updatedAt"`
	DeletedAt                 *time.Time                     `json:"deletedAt,omitempty"`
	UpdatedBy                 string                         `json:"updatedBy,omitempty"`
	UpdatedAuditID            *uint                          `json:"updatedAuditId,omitempty"`
}

type dotProjectSyncStateResponse struct {
	RepoExists             bool       `json:"repoExists"`
	ProjectFileExists      bool       `json:"projectFileExists"`
	MaintainersFileExists  bool       `json:"maintainersFileExists"`
	SecurityFileExists     bool       `json:"securityFileExists"`
	ContributingFileExists bool       `json:"contributingFileExists"`
	GovernanceFileExists   bool       `json:"governanceFileExists"`
	DefaultBranch          string     `json:"defaultBranch,omitempty"`
	MaintainersFilename    string     `json:"maintainersFilename,omitempty"`
	SchemaVersion          string     `json:"schemaVersion,omitempty"`
	LastCheckedAt          *time.Time `json:"lastCheckedAt,omitempty"`
	SyncError              string     `json:"syncError,omitempty"`
	ParseError             string     `json:"parseError,omitempty"`
}

type dotProjectMaintainerCacheBody struct {
	Filename      string     `json:"filename,omitempty"`
	ETag          string     `json:"etag,omitempty"`
	BodyHash      string     `json:"bodyHash,omitempty"`
	Body          string     `json:"body,omitempty"`
	LastCheckedAt *time.Time `json:"lastCheckedAt,omitempty"`
}

type dotProjectMaintainerPRStatus struct {
	Status    string     `json:"status"`
	URL       string     `json:"url,omitempty"`
	Number    int        `json:"number,omitempty"`
	Title     string     `json:"title,omitempty"`
	Author    string     `json:"author,omitempty"`
	Source    string     `json:"source,omitempty"`
	CreatedAt *time.Time `json:"createdAt,omitempty"`
	UpdatedAt *time.Time `json:"updatedAt,omitempty"`
	Warning   string     `json:"warning,omitempty"`
}

type dotProjectPRAuditMetadata struct {
	PullRequest struct {
		URL    string `json:"url"`
		Number int    `json:"number"`
	} `json:"pull_request"`
	Repository struct {
		Owner string `json:"owner"`
		Repo  string `json:"repo"`
		Path  string `json:"path"`
	} `json:"repository"`
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
	dotProjectFilter := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("dotProject")))

	base := s.store.DB().Model(&model.Project{})
	if len(maturityFilters) > 0 {
		base = base.Where("projects.maturity IN ?", maturityFilters)
	}
	switch dotProjectFilter {
	case "with":
		base = base.Where("COALESCE(TRIM(projects.dot_project_repo_ref), '') <> ''")
	case "without":
		base = base.Where("COALESCE(TRIM(projects.dot_project_repo_ref), '') = ''")
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
				"LOWER(projects.name) LIKE ? OR LOWER(projects.maintainer_ref) LIKE ? OR LOWER(maint.name) LIKE ? OR LOWER(maint.git_hub_account) LIKE ? OR LOWER(maint.location) LIKE ? OR LOWER(maint.country) LIKE ? OR LOWER(maint.timezone) LIKE ? OR LOWER(comp.name) LIKE ? OR REPLACE(REPLACE(REPLACE(LOWER(comp.name), ' ', ''), '-', ''), '_', '') LIKE ?",
				like, like, like, like, like, like, like, like, compactLike,
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
	dotProjectFilter := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("dotProject")))
	nameFilter := strings.TrimSpace(r.URL.Query().Get("projectName"))
	maintainerFilter := strings.TrimSpace(r.URL.Query().Get("maintainer"))
	maintainerFileFilter := strings.TrimSpace(r.URL.Query().Get("maintainerFile"))

	base := s.store.DB().Model(&model.Project{})
	if len(maturityFilters) > 0 {
		base = base.Where("projects.maturity IN ?", maturityFilters)
	}
	switch dotProjectFilter {
	case "with":
		base = base.Where("COALESCE(TRIM(projects.dot_project_repo_ref), '') <> ''")
	case "without":
		base = base.Where("COALESCE(TRIM(projects.dot_project_repo_ref), '') = ''")
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
			Where(
				"LOWER(maint.name) LIKE ? OR LOWER(maint.git_hub_account) LIKE ? OR LOWER(maint.location) LIKE ? OR LOWER(maint.country) LIKE ? OR LOWER(maint.timezone) LIKE ?",
				like, like, like, like, like,
			)
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
			ID:                        project.ID,
			Name:                      project.Name,
			Maturity:                  string(project.Maturity),
			AddedBy:                   addedBy[project.ID],
			LegacyMaintainerRef:       strings.TrimSpace(project.LegacyMaintainerRef),
			GitHubOrg:                 strings.TrimSpace(project.GitHubOrg),
			DotProjectRepoRef:         strings.TrimSpace(project.DotProjectRepoRef),
			DotProjectProjectRef:      strings.TrimSpace(project.DotProjectProjectRef),
			DotProjectMaintainerRef:   strings.TrimSpace(project.DotProjectMaintainerRef),
			DotProjectSecurityRef:     strings.TrimSpace(project.DotProjectSecurityRef),
			DotProjectContributingRef: strings.TrimSpace(project.DotProjectContributingRef),
			DotProjectGovernanceRef:   strings.TrimSpace(project.DotProjectGovernanceRef),
			DotProjectSchemaVersion:   strings.TrimSpace(project.DotProjectSchemaVersion),
			DotProjectMaintainerCount: project.DotProjectMaintainerCount,
			DotProjectLastSyncedAt:    project.DotProjectLastSyncedAt,
			DotProjectAdoptionStatus:  strings.TrimSpace(project.DotProjectAdoptionStatus),
			CreatedAt:                 project.CreatedAt.Format(time.RFC3339),
			Maintainers:               summarizeMaintainers(project.Maintainers),
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
	if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/dot-project/pull-request") {
		s.handleDotProjectPullRequest(w, r)
		return
	}
	if r.Method == http.MethodPatch {
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

	dotProjectSyncState, err := s.store.GetDotProjectSyncState(project.ID)
	if err != nil {
		s.logger.Printf("web-bff: failed to load dot-project sync state project=%d path=%s user=%s role=%s err=%v", id, r.URL.Path, login, role, err)
		http.Error(w, "failed to load project", http.StatusInternalServerError)
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

	var fossaTeamID *uint
	var fossaTeamName string
	var fossaTeamMembers []fossaTeamMemberSummary
	var fossaTeamEmails []string
	var fossaInviteIneligible []fossaInviteIneligibleSummary
	var fossaInviteCandidates []fossaInviteCandidateSummary
	if serviceID, err := s.getFossaServiceID(); err == nil {
		if serviceTeam, err := s.store.GetRemoteTeamByProject(project.ID, serviceID); err == nil && serviceTeam != nil {
			idVal := serviceTeam.RemoteTeamID
			fossaTeamID = &idVal
			if serviceTeam.RemoteTeamName != nil {
				fossaTeamName = *serviceTeam.RemoteTeamName
			}
			if !s.testMode || s.allowLiveFossa {
				var fossaMembersErr error
				if cachedEmails, ok := s.getCachedFossaTeamEmails(serviceTeam.RemoteTeamID); ok {
					fossaTeamEmails = cachedEmails
					s.logger.Printf("web-bff: FOSSA team email cache hit project=%d remoteTeamID=%d count=%d", project.ID, serviceTeam.RemoteTeamID, len(fossaTeamEmails))
				} else if client, err := s.fossaClient(); err == nil {
					s.logger.Printf("web-bff: fetching FOSSA team state project=%d remoteTeamID=%d", project.ID, serviceTeam.RemoteTeamID)
					if emails, err := client.FetchTeamUserEmails(serviceTeam.RemoteTeamID); err == nil {
						fossaTeamEmails = emails
						s.setCachedFossaTeamEmails(serviceTeam.RemoteTeamID, emails)
					} else {
						fossaMembersErr = err
						s.logger.Printf("web-bff: failed to load FOSSA team emails from API project=%d err=%v", project.ID, err)
					}
				} else {
					fossaMembersErr = err
				}
				if len(fossaTeamEmails) > 0 {
					domainCounts := make(map[string]int)
					for _, email := range fossaTeamEmails {
						parts := strings.Split(strings.ToLower(strings.TrimSpace(email)), "@")
						if len(parts) == 2 && parts[1] != "" {
							domainCounts[parts[1]]++
						} else {
							domainCounts["(invalid)"]++
						}
					}
					domains := make([]string, 0, len(domainCounts))
					for domain := range domainCounts {
						domains = append(domains, domain)
					}
					sort.Strings(domains)
					parts := make([]string, 0, len(domains))
					for _, domain := range domains {
						parts = append(parts, fmt.Sprintf("%s=%d", domain, domainCounts[domain]))
					}
					s.logger.Printf("web-bff: FOSSA team email domains project=%d remoteTeamID=%d %s", project.ID, serviceTeam.RemoteTeamID, strings.Join(parts, ", "))
					emailToMaintainer := make(map[string]model.Maintainer, len(project.Maintainers))
					for _, maintainer := range project.Maintainers {
						normalized := strings.ToLower(strings.TrimSpace(maintainer.Email))
						if normalized != "" && !strings.EqualFold(normalized, "EMAIL_MISSING") {
							emailToMaintainer[normalized] = maintainer
						}
					}
					fossaTeamMembers = make([]fossaTeamMemberSummary, 0, len(fossaTeamEmails))
					for _, email := range fossaTeamEmails {
						normalized := strings.ToLower(strings.TrimSpace(email))
						name := strings.TrimSpace(email)
						var maintainerID uint
						var github string
						if matched, ok := emailToMaintainer[normalized]; ok {
							maintainerID = matched.ID
							if matched.Name != "" {
								name = matched.Name
							}
							github = matched.GitHubAccount
						}
						fossaTeamMembers = append(fossaTeamMembers, fossaTeamMemberSummary{
							ID:     maintainerID,
							Name:   name,
							GitHub: github,
							Email:  email,
						})
					}
					s.logger.Printf("web-bff: loaded FOSSA team emails project=%d remoteTeamID=%d count=%d", project.ID, serviceTeam.RemoteTeamID, len(fossaTeamEmails))
				}
				if fossaMembersErr != nil {
					if members, err := s.store.ListRemoteTeamMaintainers(serviceTeam.ID); err == nil {
						fossaTeamMembers = make([]fossaTeamMemberSummary, 0, len(members))
						for _, member := range members {
							fossaTeamMembers = append(fossaTeamMembers, fossaTeamMemberSummary{
								ID:     member.ID,
								Name:   member.Name,
								GitHub: member.GitHubAccount,
								Email:  member.Email,
							})
						}
						fossaTeamEmails = make([]string, 0, len(members))
						for _, member := range members {
							if member.Email != "" {
								fossaTeamEmails = append(fossaTeamEmails, member.Email)
							}
						}
					} else {
						s.logger.Printf("web-bff: failed to load FOSSA team members from db project=%d err=%v", project.ID, err)
					}
				}
			}
			if s.testMode && (!s.allowLiveFossa || len(fossaTeamEmails) == 0) {
				if members, err := s.store.ListRemoteTeamMaintainers(serviceTeam.ID); err == nil {
					fossaTeamMembers = make([]fossaTeamMemberSummary, 0, len(members))
					for _, member := range members {
						fossaTeamMembers = append(fossaTeamMembers, fossaTeamMemberSummary{
							ID:     member.ID,
							Name:   member.Name,
							GitHub: member.GitHubAccount,
							Email:  member.Email,
						})
					}
					fossaTeamEmails = make([]string, 0, len(members))
					for _, member := range members {
						if member.Email != "" {
							fossaTeamEmails = append(fossaTeamEmails, member.Email)
						}
					}
				}
			}

			fossaInviteIneligible = classifyIneligibleMaintainers(project.Maintainers)
			if role == roleStaff {
				pendingInviteEmails := make(map[string]struct{})
				if invites, err := s.store.ListServiceInvitations(project.ID, serviceID); err == nil {
					for _, invite := range invites {
						if strings.EqualFold(invite.Status, "pending") {
							normalized := strings.ToLower(strings.TrimSpace(invite.ServiceEmail))
							if normalized != "" {
								pendingInviteEmails[normalized] = struct{}{}
							}
						}
					}
				}
				s.logger.Printf("web-bff: build invite candidates project=%d remoteTeamID=%d fossaTeamEmails=%v pendingInviteEmails=%d",
					project.ID, serviceTeam.RemoteTeamID, fossaTeamEmails, len(pendingInviteEmails))
				fossaInviteCandidates = buildFossaInviteCandidates(project.Maintainers, fossaTeamEmails, pendingInviteEmails)
			}
		}
	}

	response := projectDetailResponse{
		ID:                        project.ID,
		Name:                      project.Name,
		Maturity:                  string(project.Maturity),
		ParentProjectID:           project.ParentProjectID,
		RefStatus:                 refStatus,
		LegacyMaintainerRefBody:   refBody,
		RefOnlyGitHub:             refOnlyGitHub,
		RefLines:                  refLines,
		Maintainers:               maintainers,
		Services:                  services,
		FossaTeamID:               fossaTeamID,
		FossaTeamName:             fossaTeamName,
		FossaTeamMembers:          fossaTeamMembers,
		FossaInviteIneligible:     fossaInviteIneligible,
		FossaInviteCandidates:     fossaInviteCandidates,
		DotProjectMaintainerCount: project.DotProjectMaintainerCount,
		DotProjectLastSyncedAt:    project.DotProjectLastSyncedAt,
		CreatedAt:                 project.CreatedAt,
		UpdatedAt:                 project.UpdatedAt,
		DeletedAt:                 deletedAt,
		UpdatedBy:                 updatedBy,
		UpdatedAuditID:            updatedAuditID,
	}

	maintainerRef := strings.TrimSpace(project.LegacyMaintainerRef)
	if maintainerRef != "" {
		response.LegacyMaintainerRef = maintainerRef
	}
	response.DotProjectRepoRef = strings.TrimSpace(project.DotProjectRepoRef)
	response.DotProjectProjectRef = strings.TrimSpace(project.DotProjectProjectRef)
	response.DotProjectMaintainerRef = strings.TrimSpace(project.DotProjectMaintainerRef)
	response.DotProjectSecurityRef = strings.TrimSpace(project.DotProjectSecurityRef)
	response.DotProjectContributingRef = strings.TrimSpace(project.DotProjectContributingRef)
	response.DotProjectGovernanceRef = strings.TrimSpace(project.DotProjectGovernanceRef)
	response.DotProjectSchemaVersion = strings.TrimSpace(project.DotProjectSchemaVersion)
	response.DotProjectAdoptionStatus = strings.TrimSpace(project.DotProjectAdoptionStatus)
	if dotProjectSyncState != nil {
		syncState := &dotProjectSyncStateResponse{
			RepoExists:             dotProjectSyncState.RepoExists,
			ProjectFileExists:      dotProjectSyncState.ProjectFileExists,
			MaintainersFileExists:  dotProjectSyncState.MaintainersFileExists,
			SecurityFileExists:     dotProjectSyncState.SecurityFileExists,
			ContributingFileExists: dotProjectSyncState.ContributingFileExists,
			GovernanceFileExists:   dotProjectSyncState.GovernanceFileExists,
			DefaultBranch:          strings.TrimSpace(dotProjectSyncState.DefaultBranch),
			MaintainersFilename:    strings.TrimSpace(dotProjectSyncState.MaintainersFilename),
			SchemaVersion:          strings.TrimSpace(dotProjectSyncState.SchemaVersion),
			LastCheckedAt:          dotProjectSyncState.LastCheckedAt,
		}
		if dotProjectSyncState.SyncError != nil {
			syncState.SyncError = strings.TrimSpace(*dotProjectSyncState.SyncError)
		}
		if dotProjectSyncState.ParseError != nil {
			syncState.ParseError = strings.TrimSpace(*dotProjectSyncState.ParseError)
		}
		response.DotProjectSyncState = syncState
		if dotProjectSyncState.MaintainersFileBody != nil || dotProjectSyncState.MaintainersFilename != "" || dotProjectSyncState.MaintainersFileETag != "" || dotProjectSyncState.MaintainersFileBodyHash != "" {
			maintainerCache := &dotProjectMaintainerCacheBody{
				Filename:      strings.TrimSpace(dotProjectSyncState.MaintainersFilename),
				ETag:          strings.TrimSpace(dotProjectSyncState.MaintainersFileETag),
				BodyHash:      strings.TrimSpace(dotProjectSyncState.MaintainersFileBodyHash),
				LastCheckedAt: dotProjectSyncState.LastCheckedAt,
			}
			if dotProjectSyncState.MaintainersFileBody != nil {
				maintainerCache.Body = *dotProjectSyncState.MaintainersFileBody
			}
			response.DotProjectMaintainerCache = maintainerCache
		}
		maintainerFilePath := strings.TrimSpace(dotProjectSyncState.MaintainersFilename)
		if maintainerFilePath == "" {
			maintainerFilePath = pathpkg.Base(strings.TrimSpace(project.DotProjectMaintainerRef))
		}
		owner := strings.TrimSpace(project.GitHubOrg)
		if owner == "" {
			if parsedOwner, err := parseGitHubOrgFromURL(project.DotProjectMaintainerRef); err == nil {
				owner = parsedOwner
			}
		}
		if status := s.dotProjectMaintainerPullRequestStatus(r.Context(), project.ID, owner, ".project", maintainerFilePath, session.AccessToken); status != nil {
			response.DotProjectMaintainerPR = status
		}
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
	if s.githubToken == "" && !s.testMode {
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
	dotProjectRef := strings.TrimSpace(req.DotProjectMaintainerRef)
	if legacyRef == "" && dotProjectRef == "" {
		http.Error(w, "legacyMaintainerRef or dotProjectMaintainerRef is required", http.StatusBadRequest)
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
			http.Error(w, "legacyMaintainerRef and dotProjectMaintainerRef must reference the same GitHub org", http.StatusBadRequest)
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
		Name:                    projectName,
		Maturity:                maturity,
		GitHubOrg:               githubOrg,
		ParentProjectID:         req.ParentProjectID,
		LegacyMaintainerRef:     legacyRef,
		DotProjectMaintainerRef: dotProjectRef,
		OnboardingIssue:         &onboardingIssue,
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

func staffIdentityForSession(store *db.SQLStore, session *session) (*uint, string) {
	if session == nil {
		return nil, "Staff"
	}
	staffName := strings.TrimSpace(session.Login)
	var staffID *uint
	if session.Login != "" {
		var staff model.StaffMember
		if err := store.DB().
			Where("LOWER(git_hub_account) = ?", strings.ToLower(session.Login)).
			First(&staff).Error; err == nil {
			staffID = &staff.ID
			staffName = strings.TrimSpace(staff.Name)
		}
	}
	if staffName == "" {
		staffName = "Staff"
	}
	return staffID, staffName
}

func activeProjectMaintainerGitHubHandles(maintainers []model.Maintainer) []string {
	handles := make([]string, 0, len(maintainers))
	seen := map[string]struct{}{}
	for _, maintainer := range maintainers {
		if maintainer.MaintainerStatus != model.ActiveMaintainer {
			continue
		}
		normalized := dotproject.NormalizeGitHubHandle(maintainer.GitHubAccount)
		if normalized == "" || normalized == "github_missing" || normalized == "github-missing" {
			continue
		}
		if _, ok := seen[normalized]; ok {
			continue
		}
		seen[normalized] = struct{}{}
		handles = append(handles, normalized)
	}
	return handles
}

type projectMaintainerRefUpdateRequest struct {
	LegacyMaintainerRef string `json:"legacyMaintainerRef"`
}

type projectMaturityUpdateRequest struct {
	Maturity string `json:"maturity"`
}

type dotProjectPullRequestInput struct {
	ProjectName          string
	Owner                string
	Repo                 string
	ForkOwner            string
	ForkRepo             string
	FilePath             string
	BaseBranch           string
	HeadBranch           string
	Original             string
	Proposed             string
	AddedHandles         []string
	RemovedPlaceholders  []string
	SubmittedByLogin     string
	SubmittedByName      string
	OriginalBodyHash     string
	ProjectMaintainerRef string
}

type dotProjectPullRequestResponse struct {
	URL                  string   `json:"url"`
	Number               int      `json:"number"`
	Branch               string   `json:"branch"`
	BaseBranch           string   `json:"baseBranch"`
	ForkOwner            string   `json:"forkOwner,omitempty"`
	ForkRepo             string   `json:"forkRepo,omitempty"`
	FilePath             string   `json:"filePath"`
	CommitSHA            string   `json:"commitSha,omitempty"`
	AddedHandles         []string `json:"addedHandles"`
	RemovedPlaceholders  []string `json:"removedPlaceholders"`
	SubmittedBy          string   `json:"submittedBy"`
	ProjectMaintainerRef string   `json:"projectMaintainerRef,omitempty"`
}

func (s *server) handleDotProjectPullRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	id, err := parseProjectSubresourceID(r.URL.Path, "/dot-project/pull-request")
	if err != nil {
		http.Error(w, "invalid project id", http.StatusBadRequest)
		return
	}
	session := sessionFromContext(r.Context())
	if session == nil || session.Role != roleStaff {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	project, err := s.store.GetProjectByID(id)
	if err != nil {
		if errors.Is(err, db.ErrProjectNotFound) {
			http.Error(w, "project not found", http.StatusNotFound)
			return
		}
		s.logger.Printf("web-bff: load project for dot-project PR failed id=%d err=%v", id, err)
		http.Error(w, "failed to load project", http.StatusInternalServerError)
		return
	}
	syncState, err := s.store.GetDotProjectSyncState(project.ID)
	if err != nil {
		s.logger.Printf("web-bff: load dot-project sync state for PR failed project=%d err=%v", project.ID, err)
		http.Error(w, "failed to load dot-project state", http.StatusInternalServerError)
		return
	}
	if syncState == nil || syncState.MaintainersFileBody == nil || strings.TrimSpace(*syncState.MaintainersFileBody) == "" {
		http.Error(w, "cached maintainers file body is required", http.StatusConflict)
		return
	}

	patch, err := dotproject.BuildMaintainerRosterPatch(*syncState.MaintainersFileBody, activeProjectMaintainerGitHubHandles(project.Maintainers))
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if patch.Proposed == *syncState.MaintainersFileBody {
		http.Error(w, "no dot-project maintainer file changes to submit", http.StatusConflict)
		return
	}

	staffID, staffName := staffIdentityForSession(s.store, session)
	filePath := strings.TrimSpace(syncState.MaintainersFilename)
	if filePath == "" {
		filePath = pathpkg.Base(strings.TrimSpace(project.DotProjectMaintainerRef))
	}
	if filePath == "." || filePath == "/" || filePath == "" {
		filePath = "MAINTAINERS.yaml"
	}
	baseBranch := strings.TrimSpace(syncState.DefaultBranch)
	if baseBranch == "" {
		baseBranch = "main"
	}
	owner := strings.TrimSpace(project.GitHubOrg)
	if owner == "" {
		if parsedOwner, err := parseGitHubOrgFromURL(project.DotProjectMaintainerRef); err == nil {
			owner = parsedOwner
		}
	}
	if owner == "" {
		http.Error(w, "project github org is required", http.StatusConflict)
		return
	}

	input := dotProjectPullRequestInput{
		ProjectName:          project.Name,
		Owner:                owner,
		Repo:                 ".project",
		ForkOwner:            session.Login,
		ForkRepo:             dotProjectForkRepoName(project.Name, ".project"),
		FilePath:             filePath,
		BaseBranch:           baseBranch,
		HeadBranch:           fmt.Sprintf("maintainer-d/%d/maintainers-%d", project.ID, time.Now().UTC().UnixNano()),
		Original:             *syncState.MaintainersFileBody,
		Proposed:             patch.Proposed,
		AddedHandles:         patch.AddedHandles,
		RemovedPlaceholders:  patch.RemovedPlaceholders,
		SubmittedByLogin:     session.Login,
		SubmittedByName:      staffName,
		OriginalBodyHash:     strings.TrimSpace(syncState.MaintainersFileBodyHash),
		ProjectMaintainerRef: strings.TrimSpace(project.DotProjectMaintainerRef),
	}

	submitter := s.createDotProjectPullRequest
	if submitter == nil {
		submitter = s.createDotProjectMaintainerPullRequest
	}
	if strings.TrimSpace(session.AccessToken) == "" && s.createDotProjectPullRequest == nil {
		http.Error(w, "github oauth token is required; sign out and sign in again to approve repository access", http.StatusUnauthorized)
		return
	}
	result, err := submitter(r.Context(), input, session.AccessToken)
	if err != nil {
		s.logger.Printf("web-bff: dot-project PR create failed project=%d owner=%s repo=%s file=%s err=%v", project.ID, owner, input.Repo, filePath, err)
		http.Error(w, "failed to create dot-project pull request", http.StatusBadGateway)
		return
	}
	if result.AddedHandles == nil {
		result.AddedHandles = patch.AddedHandles
	}
	if result.RemovedPlaceholders == nil {
		result.RemovedPlaceholders = patch.RemovedPlaceholders
	}
	if result.SubmittedBy == "" {
		result.SubmittedBy = staffName
	}
	if result.ForkOwner == "" {
		result.ForkOwner = input.ForkOwner
	}
	if result.ForkRepo == "" {
		result.ForkRepo = input.ForkRepo
	}
	result.ProjectMaintainerRef = input.ProjectMaintainerRef
	metadata := map[string]any{
		"actor": map[string]string{
			"login": session.Login,
			"role":  session.Role,
			"name":  staffName,
		},
		"pull_request": map[string]any{
			"url":         result.URL,
			"number":      result.Number,
			"branch":      result.Branch,
			"base_branch": result.BaseBranch,
			"commit_sha":  result.CommitSHA,
		},
		"fork": map[string]string{
			"owner": result.ForkOwner,
			"repo":  result.ForkRepo,
		},
		"repository": map[string]string{
			"owner": owner,
			"repo":  input.Repo,
			"path":  filePath,
		},
		"added_handles":        patch.AddedHandles,
		"removed_placeholders": patch.RemovedPlaceholders,
		"original_body_hash":   input.OriginalBodyHash,
	}
	if metadataJSON, err := json.Marshal(metadata); err != nil {
		s.logger.Printf("web-bff: dot-project PR audit metadata encode error: %v", err)
	} else {
		event := model.AuditLog{
			ProjectID: &project.ID,
			StaffID:   staffID,
			Action:    "DOT_PROJECT_MAINTAINER_PR_CREATE",
			Message:   fmt.Sprintf("Dot-project maintainer pull request submitted by %s", staffName),
			Metadata:  string(metadataJSON),
		}
		if err := s.store.DB().Create(&event).Error; err != nil {
			s.logger.Printf("web-bff: dot-project PR audit log failed: %v", err)
		}
	}

	w.Header().Set(headerContentType, contentTypeJSON)
	if err := json.NewEncoder(w).Encode(result); err != nil {
		s.logger.Printf("web-bff: dot-project PR encode error: %v", err)
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
	Location    string                      `json:"location,omitempty"`
	Country     string                      `json:"country,omitempty"`
	Timezone    string                      `json:"timezone,omitempty"`
	Projects    []maintainerProjectResponse `json:"projects"`
	Services    []maintainerServiceResponse `json:"services,omitempty"`
	CreatedAt   time.Time                   `json:"createdAt"`
	UpdatedAt   time.Time                   `json:"updatedAt"`
	DeletedAt   *time.Time                  `json:"deletedAt,omitempty"`
	UpdatedBy   string                      `json:"updatedBy,omitempty"`
}

type maintainerProjectResponse struct {
	ID   uint   `json:"id"`
	Name string `json:"name"`
}

type maintainerServiceResponse struct {
	Kind    string                            `json:"kind"`
	Label   string                            `json:"label"`
	Account maintainerServiceAccountResponse  `json:"account"`
	Targets []maintainerServiceTargetResponse `json:"targets"`
}

type maintainerServiceAccountResponse struct {
	State               string     `json:"state"`
	MatchedBy           string     `json:"matchedBy"`
	RemoteUserID        *uint      `json:"remoteUserId,omitempty"`
	RemoteRef           string     `json:"remoteRef,omitempty"`
	EmailUsed           string     `json:"emailUsed,omitempty"`
	LastCheckedAt       *time.Time `json:"lastCheckedAt,omitempty"`
	PendingInvitations  int        `json:"pendingInvitations,omitempty"`
	AcceptedInvitations int        `json:"acceptedInvitations,omitempty"`
	Error               string     `json:"error,omitempty"`
}

type maintainerServiceTargetResponse struct {
	ProjectID     uint       `json:"projectId"`
	ProjectName   string     `json:"projectName"`
	TargetKind    string     `json:"targetKind"`
	TargetID      *uint      `json:"targetId,omitempty"`
	TargetName    string     `json:"targetName"`
	Required      bool       `json:"required"`
	State         string     `json:"state"`
	PendingInvite bool       `json:"pendingInvite,omitempty"`
	LastCheckedAt *time.Time `json:"lastCheckedAt,omitempty"`
	Error         string     `json:"error,omitempty"`
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
	if maintainerID, serviceKind, action, ok := parseMaintainerServiceActionPath(r.URL.Path); ok {
		s.handleMaintainerServiceAction(w, r, maintainerID, serviceKind, action)
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
		maintainer, err := s.loadMaintainerWithRelations(id)
		if err != nil {
			s.logger.Printf("web-bff: maintainer not found id=%d path=%s user=%s role=%s err=%v", id, r.URL.Path, login, role, err)
			http.Error(w, "maintainer not found", http.StatusNotFound)
			return
		}

		response := s.buildMaintainerDetailResponse(*maintainer, session.Role == roleStaff)
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

func (s *server) buildMaintainerServices(maintainer model.Maintainer) []maintainerServiceResponse {
	services := make([]maintainerServiceResponse, 0, 1)

	fossaService, ok := s.buildMaintainerFossaService(maintainer)
	if ok {
		services = append(services, fossaService)
	}

	return services
}

func (s *server) loadMaintainerWithRelations(id uint) (*model.Maintainer, error) {
	var maintainer model.Maintainer
	if err := s.store.DB().
		Preload("Company").
		Preload("Projects").
		First(&maintainer, id).Error; err != nil {
		return nil, err
	}
	return &maintainer, nil
}

func (s *server) buildMaintainerDetailResponse(maintainer model.Maintainer, includeServices bool) maintainerDetailResponse {
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
	if includeServices {
		response.Services = s.buildMaintainerServices(maintainer)
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
	if maintainer.Location != nil {
		response.Location = *maintainer.Location
	}
	if maintainer.Country != nil {
		response.Country = *maintainer.Country
	}
	if maintainer.Timezone != nil {
		response.Timezone = *maintainer.Timezone
	}
	return response
}

func (s *server) buildMaintainerFossaService(maintainer model.Maintainer) (maintainerServiceResponse, bool) {
	serviceID, err := s.getFossaServiceID()
	if err != nil {
		return maintainerServiceResponse{}, false
	}

	projects, err := s.loadMaintainerServiceProjects(maintainer.ID, serviceID)
	if err != nil || len(projects) == 0 {
		return maintainerServiceResponse{}, false
	}

	projectIDs := make([]uint, 0, len(projects))
	for _, project := range projects {
		projectIDs = append(projectIDs, project.ID)
	}

	var remoteTeams []model.RemoteTeam
	if err := s.store.DB().
		Where("service_id = ? AND project_id IN ?", serviceID, projectIDs).
		Find(&remoteTeams).Error; err != nil {
		return maintainerServiceResponse{}, false
	}

	teamByProjectID := make(map[uint]model.RemoteTeam, len(remoteTeams))
	teamIDs := make([]uint, 0, len(remoteTeams))
	for _, team := range remoteTeams {
		teamByProjectID[team.ProjectID] = team
		teamIDs = append(teamIDs, team.ID)
	}

	candidateEmails := maintainerServiceCandidateEmails(maintainer)

	remoteUsers := make([]model.RemoteUser, 0)
	if len(candidateEmails) > 0 {
		if err := s.store.DB().
			Where("service_id = ? AND LOWER(service_email) IN ?", serviceID, candidateEmails).
			Find(&remoteUsers).Error; err != nil {
			return maintainerServiceResponse{}, false
		}
	}

	var invites []model.ServiceInvitation
	inviteQuery := s.store.DB().
		Where("service_id = ? AND project_id IN ?", serviceID, projectIDs).
		Where("maintainer_id = ?", maintainer.ID)
	if len(candidateEmails) > 0 {
		inviteQuery = inviteQuery.Or(
			"service_id = ? AND project_id IN ? AND LOWER(service_email) IN ?",
			serviceID,
			projectIDs,
			candidateEmails,
		)
	}
	if err := inviteQuery.Order("created_at desc").Find(&invites).Error; err != nil {
		return maintainerServiceResponse{}, false
	}

	membershipQuery := s.store.DB().Where("service_id = ?", serviceID)
	if len(teamIDs) > 0 {
		membershipQuery = membershipQuery.Where("team_id IN ?", teamIDs)
	}

	remoteUserIDs := make([]uint, 0, len(remoteUsers))
	for _, user := range remoteUsers {
		remoteUserIDs = append(remoteUserIDs, user.ID)
	}
	if len(remoteUserIDs) > 0 {
		membershipQuery = membershipQuery.Where("maintainer_id = ? OR user_id IN ?", maintainer.ID, remoteUserIDs)
	} else {
		membershipQuery = membershipQuery.Where("maintainer_id = ?", maintainer.ID)
	}

	var memberships []model.RemoteTeamUser
	if err := membershipQuery.Find(&memberships).Error; err != nil {
		return maintainerServiceResponse{}, false
	}

	selectedRemoteUser, matchedBy := selectMaintainerRemoteUser(maintainer, remoteUsers)
	account := buildMaintainerServiceAccount(selectedRemoteUser, matchedBy, invites)
	targets := buildMaintainerServiceTargets(projects, teamByProjectID, memberships, invites, account.State)

	return maintainerServiceResponse{
		Kind:    "fossa",
		Label:   "CNCF FOSSA",
		Account: account,
		Targets: targets,
	}, true
}

func (s *server) loadMaintainerServiceProjects(maintainerID, serviceID uint) ([]model.Project, error) {
	var projects []model.Project
	err := s.store.DB().
		Model(&model.Project{}).
		Distinct("projects.*").
		Joins("JOIN maintainer_projects mp ON mp.project_id = projects.id").
		Joins("JOIN service_projects sp ON sp.project_id = projects.id").
		Where("mp.maintainer_id = ? AND sp.service_id = ?", maintainerID, serviceID).
		Order("projects.name asc").
		Find(&projects).Error
	return projects, err
}

func maintainerServiceCandidateEmails(maintainer model.Maintainer) []string {
	values := []string{
		strings.ToLower(strings.TrimSpace(maintainer.Email)),
		strings.ToLower(strings.TrimSpace(maintainer.GitHubEmail)),
	}
	seen := make(map[string]struct{}, len(values))
	candidateEmails := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" || value == "email_missing" || value == "github_email_missing" || value == "github_missing" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		candidateEmails = append(candidateEmails, value)
	}
	return candidateEmails
}

func selectMaintainerRemoteUser(maintainer model.Maintainer, remoteUsers []model.RemoteUser) (*model.RemoteUser, string) {
	maintainerEmail := strings.ToLower(strings.TrimSpace(maintainer.Email))
	githubEmail := strings.ToLower(strings.TrimSpace(maintainer.GitHubEmail))

	for i := range remoteUsers {
		email := strings.ToLower(strings.TrimSpace(remoteUsers[i].ServiceEmail))
		if maintainerEmail != "" && email == maintainerEmail {
			return &remoteUsers[i], "maintainer_email"
		}
	}
	for i := range remoteUsers {
		email := strings.ToLower(strings.TrimSpace(remoteUsers[i].ServiceEmail))
		if githubEmail != "" && email == githubEmail {
			return &remoteUsers[i], "github_email"
		}
	}
	if len(remoteUsers) > 0 {
		return &remoteUsers[0], "unknown"
	}
	return nil, "none"
}

func buildMaintainerServiceAccount(selectedRemoteUser *model.RemoteUser, matchedBy string, invites []model.ServiceInvitation) maintainerServiceAccountResponse {
	account := maintainerServiceAccountResponse{
		State:     "unknown",
		MatchedBy: matchedBy,
	}

	var latestCheckedAt *time.Time
	pendingInvites := 0
	acceptedInvites := 0
	lastError := ""
	for _, invite := range invites {
		if invite.LastCheckedAt != nil && (latestCheckedAt == nil || invite.LastCheckedAt.After(*latestCheckedAt)) {
			latestCheckedAt = invite.LastCheckedAt
		}
		switch strings.ToLower(strings.TrimSpace(invite.Status)) {
		case "pending", "expired":
			pendingInvites++
		case "accepted":
			acceptedInvites++
		case "error":
			if invite.LastError != nil && strings.TrimSpace(*invite.LastError) != "" {
				lastError = strings.TrimSpace(*invite.LastError)
			}
		}
	}
	account.LastCheckedAt = latestCheckedAt
	account.PendingInvitations = pendingInvites
	account.AcceptedInvitations = acceptedInvites

	if selectedRemoteUser != nil {
		account.State = "registered"
		account.RemoteUserID = &selectedRemoteUser.RemoteUserID
		account.RemoteRef = strings.TrimSpace(selectedRemoteUser.RemoteRef)
		account.EmailUsed = strings.TrimSpace(selectedRemoteUser.ServiceEmail)
		return account
	}
	if pendingInvites > 0 {
		account.State = "invited"
		if len(invites) > 0 {
			account.EmailUsed = strings.TrimSpace(invites[0].ServiceEmail)
		}
		return account
	}
	if acceptedInvites > 0 {
		account.State = "registered"
		if len(invites) > 0 {
			account.EmailUsed = strings.TrimSpace(invites[0].ServiceEmail)
		}
		return account
	}
	if lastError != "" {
		account.State = "error"
		account.Error = lastError
		return account
	}

	account.State = "not_registered"
	return account
}

func buildMaintainerServiceTargets(
	projects []model.Project,
	teamByProjectID map[uint]model.RemoteTeam,
	memberships []model.RemoteTeamUser,
	invites []model.ServiceInvitation,
	accountState string,
) []maintainerServiceTargetResponse {
	membershipByTeamID := make(map[uint]model.RemoteTeamUser, len(memberships))
	for _, membership := range memberships {
		if _, exists := membershipByTeamID[membership.TeamID]; !exists {
			membershipByTeamID[membership.TeamID] = membership
		}
	}

	inviteByProjectID := make(map[uint]model.ServiceInvitation, len(invites))
	for _, invite := range invites {
		if _, exists := inviteByProjectID[invite.ProjectID]; !exists {
			inviteByProjectID[invite.ProjectID] = invite
		}
	}

	targets := make([]maintainerServiceTargetResponse, 0, len(projects))
	for _, project := range projects {
		target := maintainerServiceTargetResponse{
			ProjectID:   project.ID,
			ProjectName: project.Name,
			TargetKind:  "team",
			Required:    true,
			State:       "missing",
		}

		team, hasTeam := teamByProjectID[project.ID]
		invite, hasInvite := inviteByProjectID[project.ID]
		if hasInvite {
			target.LastCheckedAt = invite.LastCheckedAt
		}

		if !hasTeam {
			target.TargetName = "FOSSA team not assigned"
			target.State = "error"
			target.Error = "Project does not have a cached FOSSA team"
			targets = append(targets, target)
			continue
		}

		target.TargetID = &team.RemoteTeamID
		if team.RemoteTeamName != nil && strings.TrimSpace(*team.RemoteTeamName) != "" {
			target.TargetName = strings.TrimSpace(*team.RemoteTeamName)
		} else {
			target.TargetName = fmt.Sprintf("Team %d", team.RemoteTeamID)
		}

		if _, ok := membershipByTeamID[team.ID]; ok {
			target.State = "member"
			targets = append(targets, target)
			continue
		}

		if hasInvite {
			switch strings.ToLower(strings.TrimSpace(invite.Status)) {
			case "pending", "expired":
				target.State = "pending"
				target.PendingInvite = true
			case "error":
				target.State = "error"
				if invite.LastError != nil {
					target.Error = strings.TrimSpace(*invite.LastError)
				}
			case "accepted":
				if invite.TeamAssignmentStatus != nil && strings.EqualFold(strings.TrimSpace(*invite.TeamAssignmentStatus), "error") {
					target.State = "error"
					if invite.LastError != nil {
						target.Error = strings.TrimSpace(*invite.LastError)
					}
				} else if invite.TeamAssignmentStatus != nil && strings.EqualFold(strings.TrimSpace(*invite.TeamAssignmentStatus), "done") {
					target.State = "member"
				} else {
					target.State = "pending"
				}
			default:
				if accountState == "registered" {
					target.State = "missing"
				}
			}
			targets = append(targets, target)
			continue
		}

		switch accountState {
		case "registered":
			target.State = "missing"
		case "invited":
			target.State = "pending"
			target.PendingInvite = true
		case "not_registered":
			target.State = "missing"
		}

		targets = append(targets, target)
	}

	return targets
}

func (s *server) handleMaintainerServiceAction(w http.ResponseWriter, r *http.Request, maintainerID uint, serviceKind, action string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	session := sessionFromContext(r.Context())
	if session == nil || session.Role != roleStaff {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	maintainer, err := s.loadMaintainerWithRelations(maintainerID)
	if err != nil {
		http.Error(w, "maintainer not found", http.StatusNotFound)
		return
	}

	switch serviceKind {
	case "fossa":
		switch action {
		case "refresh":
			err = s.refreshMaintainerFossaState(*maintainer)
		case "invite":
			err = s.inviteMaintainerToFossa(*maintainer)
		case "reconcile":
			err = s.reconcileMaintainerFossaTeams(*maintainer)
		default:
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
	default:
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	if err != nil {
		s.logger.Printf("web-bff: maintainer service action failed maintainer=%d service=%s action=%s err=%v", maintainerID, serviceKind, action, err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	updatedMaintainer, err := s.loadMaintainerWithRelations(maintainerID)
	if err != nil {
		http.Error(w, "maintainer not found", http.StatusNotFound)
		return
	}
	response := s.buildMaintainerDetailResponse(*updatedMaintainer, true)
	w.Header().Set(headerContentType, contentTypeJSON)
	if err := json.NewEncoder(w).Encode(response); err != nil {
		s.logger.Printf("web-bff: handleMaintainerServiceAction encode error: %v", err)
	}
}

func (s *server) refreshMaintainerFossaState(maintainer model.Maintainer) error {
	serviceID, projects, serviceTeams, client, err := s.loadMaintainerFossaContext(maintainer, false)
	if err != nil {
		return err
	}

	now := time.Now().UTC()
	users, err := client.FetchUsers()
	if err != nil {
		return err
	}
	matchedUser, emailUsed := findFossaUserForMaintainer(users, maintainer)

	var remoteUser *model.RemoteUser
	if matchedUser != nil {
		remoteUser, err = s.upsertFossaRemoteUser(serviceID, *matchedUser, emailUsed)
		if err != nil {
			return err
		}
		for _, serviceTeam := range serviceTeams {
			if fossaUserHasTeam(*matchedUser, serviceTeam.RemoteTeamID) {
				if err := s.upsertFossaMembership(serviceID, *serviceTeam, remoteUser, maintainer.ID); err != nil {
					return err
				}
			}
		}
	}

	pendingEmails, err := client.FetchUserInvitationEmails()
	if err != nil {
		return err
	}
	pendingByEmail := normalizeEmailSet(pendingEmails)

	candidateEmails := maintainerServiceCandidateEmails(maintainer)
	var invites []model.ServiceInvitation
	if err := s.store.DB().
		Where("service_id = ? AND maintainer_id = ? AND project_id IN ?", serviceID, maintainer.ID, projectIDsForProjects(projects)).
		Order("created_at desc").
		Find(&invites).Error; err != nil {
		return err
	}
	inviteByProjectID := make(map[uint]model.ServiceInvitation, len(invites))
	for _, invite := range invites {
		if _, ok := inviteByProjectID[invite.ProjectID]; !ok {
			inviteByProjectID[invite.ProjectID] = invite
		}
	}

	for _, project := range projects {
		serviceTeam := serviceTeams[project.ID]
		invite, hasInvite := inviteByProjectID[project.ID]
		pendingEmail := firstMatchingPendingEmail(candidateEmails, pendingByEmail)
		hasMembership := matchedUser != nil && fossaUserHasTeam(*matchedUser, serviceTeam.RemoteTeamID)
		switch {
		case pendingEmail != "":
			if !hasInvite {
				invite = model.ServiceInvitation{
					ProjectID:    project.ID,
					MaintainerID: &maintainer.ID,
					ServiceID:    serviceID,
				}
			}
			invite.ServiceEmail = pendingEmail
			invite.RemoteTeamID = serviceTeam.RemoteTeamID
			invite.Status = "pending"
			invite.LastError = nil
			invite.LastCheckedAt = &now
			if _, err := s.store.UpsertServiceInvitation(&invite); err != nil {
				return err
			}
		case matchedUser != nil:
			if hasInvite {
				invite.ServiceEmail = firstNonEmpty(emailUsed, invite.ServiceEmail)
				invite.RemoteTeamID = serviceTeam.RemoteTeamID
				invite.Status = "accepted"
				invite.LastError = nil
				invite.LastCheckedAt = &now
				if hasMembership {
					done := "done"
					invite.TeamAssignmentStatus = &done
				} else {
					pending := "pending"
					invite.TeamAssignmentStatus = &pending
				}
				if _, err := s.store.UpsertServiceInvitation(&invite); err != nil {
					return err
				}
			}
		case hasInvite:
			invite.Status = "expired"
			invite.LastCheckedAt = &now
			if _, err := s.store.UpsertServiceInvitation(&invite); err != nil {
				return err
			}
		}
	}

	return nil
}

func (s *server) inviteMaintainerToFossa(maintainer model.Maintainer) error {
	serviceID, projects, serviceTeams, client, err := s.loadMaintainerFossaContext(maintainer, true)
	if err != nil {
		return err
	}

	email := preferredMaintainerServiceEmail(maintainer)
	if email == "" {
		return fmt.Errorf("maintainer is missing an email address")
	}

	now := time.Now().UTC()
	err = client.SendUserInvitation(email)
	if err == nil || errors.Is(err, fossa.ErrInviteAlreadyExists) {
		for _, project := range projects {
			serviceTeam := serviceTeams[project.ID]
			invite := &model.ServiceInvitation{
				ProjectID:     project.ID,
				MaintainerID:  &maintainer.ID,
				ServiceID:     serviceID,
				ServiceEmail:  email,
				RemoteTeamID:  serviceTeam.RemoteTeamID,
				Status:        "pending",
				SentAt:        &now,
				LastCheckedAt: &now,
				LastError:     nil,
			}
			if _, upsertErr := s.store.UpsertServiceInvitation(invite); upsertErr != nil {
				return upsertErr
			}
		}
		return nil
	}
	if errors.Is(err, fossa.ErrUserAlreadyMember) {
		return s.reconcileMaintainerFossaTeams(maintainer)
	}

	msg := err.Error()
	for _, project := range projects {
		serviceTeam := serviceTeams[project.ID]
		invite := &model.ServiceInvitation{
			ProjectID:     project.ID,
			MaintainerID:  &maintainer.ID,
			ServiceID:     serviceID,
			ServiceEmail:  email,
			RemoteTeamID:  serviceTeam.RemoteTeamID,
			Status:        "error",
			LastCheckedAt: &now,
			LastError:     &msg,
		}
		if _, upsertErr := s.store.UpsertServiceInvitation(invite); upsertErr != nil {
			return upsertErr
		}
	}
	return err
}

func (s *server) reconcileMaintainerFossaTeams(maintainer model.Maintainer) error {
	serviceID, projects, serviceTeams, client, err := s.loadMaintainerFossaContext(maintainer, true)
	if err != nil {
		return err
	}

	users, err := client.FetchUsers()
	if err != nil {
		return err
	}
	matchedUser, emailUsed := findFossaUserForMaintainer(users, maintainer)
	if matchedUser == nil {
		return fmt.Errorf("maintainer is not registered with CNCF FOSSA")
	}

	remoteUser, err := s.upsertFossaRemoteUser(serviceID, *matchedUser, emailUsed)
	if err != nil {
		return err
	}

	now := time.Now().UTC()
	for _, project := range projects {
		serviceTeam := serviceTeams[project.ID]
		if !fossaUserHasTeam(*matchedUser, serviceTeam.RemoteTeamID) {
			teamAdminRoleID, err := client.ResolveTeamAdminRoleID()
			if err != nil {
				return err
			}
			if err := client.AddUserToTeamByEmail(serviceTeam.RemoteTeamID, emailUsed, teamAdminRoleID); err != nil && !errors.Is(err, fossa.ErrUserAlreadyMember) {
				msg := err.Error()
				invite := &model.ServiceInvitation{
					ProjectID:     project.ID,
					MaintainerID:  &maintainer.ID,
					ServiceID:     serviceID,
					ServiceEmail:  emailUsed,
					RemoteTeamID:  serviceTeam.RemoteTeamID,
					Status:        "error",
					LastCheckedAt: &now,
					LastError:     &msg,
				}
				if _, upsertErr := s.store.UpsertServiceInvitation(invite); upsertErr != nil {
					return upsertErr
				}
				return err
			}
		}
		if err := s.upsertFossaMembership(serviceID, *serviceTeam, remoteUser, maintainer.ID); err != nil {
			return err
		}
		done := "done"
		invite := &model.ServiceInvitation{
			ProjectID:            project.ID,
			MaintainerID:         &maintainer.ID,
			ServiceID:            serviceID,
			ServiceEmail:         emailUsed,
			RemoteTeamID:         serviceTeam.RemoteTeamID,
			Status:               "accepted",
			TeamAssignmentStatus: &done,
			TeamAddAttempts:      1,
			LastCheckedAt:        &now,
			LastError:            nil,
		}
		if _, upsertErr := s.store.UpsertServiceInvitation(invite); upsertErr != nil {
			return upsertErr
		}
	}

	return nil
}

func (s *server) loadMaintainerFossaContext(maintainer model.Maintainer, ensureTeams bool) (uint, []model.Project, map[uint]*model.RemoteTeam, *fossa.Client, error) {
	serviceID, err := s.getFossaServiceID()
	if err != nil {
		return 0, nil, nil, nil, err
	}

	projects, err := s.loadMaintainerServiceProjects(maintainer.ID, serviceID)
	if err != nil {
		return 0, nil, nil, nil, err
	}
	if len(projects) == 0 {
		return 0, nil, nil, nil, fmt.Errorf("maintainer has no FOSSA-enabled projects")
	}

	client, err := s.fossaClient()
	if err != nil {
		return 0, nil, nil, nil, err
	}

	serviceTeams := make(map[uint]*model.RemoteTeam, len(projects))
	for _, project := range projects {
		serviceTeam, err := s.store.GetRemoteTeamByProject(project.ID, serviceID)
		if err != nil {
			return 0, nil, nil, nil, err
		}
		if serviceTeam == nil && ensureTeams {
			serviceTeam, _, err = s.ensureFossaTeam(project, client)
			if err != nil {
				return 0, nil, nil, nil, err
			}
		}
		if serviceTeam == nil {
			return 0, nil, nil, nil, fmt.Errorf("project %s does not have a cached FOSSA team", project.Name)
		}
		serviceTeams[project.ID] = serviceTeam
	}

	return serviceID, projects, serviceTeams, client, nil
}

func (s *server) upsertFossaRemoteUser(serviceID uint, user fossa.User, emailUsed string) (*model.RemoteUser, error) {
	serviceEmail := strings.TrimSpace(user.Email)
	if serviceEmail == "" {
		serviceEmail = emailUsed
	}
	var githubName *string
	if user.GitHub.Name != nil && strings.TrimSpace(*user.GitHub.Name) != "" {
		value := strings.TrimSpace(*user.GitHub.Name)
		githubName = &value
	}
	return s.store.UpsertRemoteUser(&model.RemoteUser{
		ServiceID:         serviceID,
		RemoteUserID:      user.ID,
		ServiceEmail:      serviceEmail,
		RemoteRef:         strings.TrimSpace(user.Username),
		ServiceGitHubName: githubName,
	})
}

func (s *server) upsertFossaMembership(serviceID uint, serviceTeam model.RemoteTeam, remoteUser *model.RemoteUser, maintainerID uint) error {
	if remoteUser == nil {
		return fmt.Errorf("missing remote user")
	}
	link := &model.RemoteTeamUser{
		ServiceID:    serviceID,
		TeamID:       serviceTeam.ID,
		UserID:       remoteUser.ID,
		MaintainerID: &maintainerID,
	}
	_, err := s.store.UpsertRemoteUserTeam(link)
	return err
}

func normalizeEmailSet(values map[string]struct{}) map[string]struct{} {
	normalized := make(map[string]struct{}, len(values))
	for value := range values {
		email := strings.ToLower(strings.TrimSpace(value))
		if email == "" {
			continue
		}
		normalized[email] = struct{}{}
	}
	return normalized
}

func firstMatchingPendingEmail(candidateEmails []string, pendingByEmail map[string]struct{}) string {
	for _, email := range candidateEmails {
		if _, ok := pendingByEmail[email]; ok {
			return email
		}
	}
	return ""
}

func preferredMaintainerServiceEmail(maintainer model.Maintainer) string {
	if email := strings.TrimSpace(normalizeValue(maintainer.Email, "EMAIL_MISSING")); email != "" {
		return email
	}
	if email := strings.TrimSpace(normalizeValue(maintainer.GitHubEmail, "GITHUB_MISSING")); email != "" {
		return email
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func projectIDsForProjects(projects []model.Project) []uint {
	projectIDs := make([]uint, 0, len(projects))
	for _, project := range projects {
		projectIDs = append(projectIDs, project.ID)
	}
	return projectIDs
}

func findFossaUserForMaintainer(users []fossa.User, maintainer model.Maintainer) (*fossa.User, string) {
	candidateEmails := maintainerServiceCandidateEmails(maintainer)
	if len(candidateEmails) == 0 {
		return nil, ""
	}

	for _, candidate := range candidateEmails {
		for i := range users {
			if fossaUserMatchesEmail(users[i], candidate) {
				return &users[i], candidate
			}
		}
	}

	return nil, ""
}

func fossaUserMatchesEmail(user fossa.User, email string) bool {
	normalized := strings.ToLower(strings.TrimSpace(email))
	if normalized == "" {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(user.Email), normalized) {
		return true
	}
	if user.GitHub.Email != nil && strings.EqualFold(strings.TrimSpace(*user.GitHub.Email), normalized) {
		return true
	}
	if user.Bitbucket.Email != nil && strings.EqualFold(strings.TrimSpace(*user.Bitbucket.Email), normalized) {
		return true
	}
	return false
}

func fossaUserHasTeam(user fossa.User, remoteTeamID uint) bool {
	for _, membership := range user.TeamUsers {
		if membership.Team.ID == remoteTeamID {
			return true
		}
	}
	return false
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

type fossaInviteRequest struct {
	ProjectID     uint   `json:"projectId"`
	MaintainerIDs []uint `json:"maintainerIds,omitempty"`
}

type fossaChooseRequest struct {
	ProjectID uint `json:"projectId"`
}

type fossaInviteSummary struct {
	ID                   uint       `json:"id"`
	ProjectID            uint       `json:"projectId"`
	MaintainerID         *uint      `json:"maintainerId,omitempty"`
	Email                string     `json:"email"`
	FossaTeamID          uint       `json:"fossaTeamId"`
	FossaTeamName        string     `json:"fossaTeamName"`
	Status               string     `json:"status"`
	TeamAssignmentStatus *string    `json:"teamAssignmentStatus,omitempty"`
	TeamAddAttempts      int        `json:"teamAddAttempts"`
	NextTeamAddAt        *time.Time `json:"nextTeamAddAt,omitempty"`
	LastError            *string    `json:"lastError,omitempty"`
	SentAt               *time.Time `json:"sentAt,omitempty"`
	LastCheckedAt        *time.Time `json:"lastCheckedAt,omitempty"`
}

type fossaInviteResponse struct {
	Invited []string          `json:"invited"`
	Skipped []string          `json:"skipped"`
	Errors  map[string]string `json:"errors"`
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

	var before *model.Maintainer
	if req.GitHubHandle != "" {
		var existing model.Maintainer
		err := s.store.DB().Where("LOWER(git_hub_account) = ?", strings.ToLower(req.GitHubHandle)).First(&existing).Error
		if err == nil {
			before = &existing
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			http.Error(w, "failed to load maintainer", http.StatusInternalServerError)
			return
		}
	}
	if before == nil && req.Email != "" {
		var existing model.Maintainer
		err := s.store.DB().Where("LOWER(email) = ?", strings.ToLower(req.Email)).First(&existing).Error
		if err == nil {
			before = &existing
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			http.Error(w, "failed to load maintainer", http.StatusInternalServerError)
			return
		}
	}

	maintainer, err := s.store.UpsertMaintainer(req.ProjectID, req.Name, req.Email, req.GitHubHandle, req.Company)
	if err != nil {
		if errors.Is(err, db.ErrProjectNotFound) {
			http.Error(w, "project not found", http.StatusNotFound)
			return
		}
		http.Error(w, "failed to create maintainer", http.StatusInternalServerError)
		return
	}

	staffID := lookupStaffID(s.store, session.Login)
	staffName := session.Login
	if staffID != nil {
		var staff model.StaffMember
		if err := s.store.DB().First(&staff, *staffID).Error; err == nil && staff.Name != "" {
			staffName = staff.Name
		}
	}

	changes := map[string]map[string]string{}
	if before == nil {
		if req.Name != "" {
			changes["name"] = map[string]string{"to": req.Name}
		}
		if req.Email != "" {
			changes["email"] = map[string]string{"to": req.Email}
		}
		if req.GitHubHandle != "" {
			changes["github"] = map[string]string{"to": req.GitHubHandle}
		}
		if req.Company != "" {
			changes["company"] = map[string]string{"to": req.Company}
		}
	} else {
		beforeName := strings.TrimSpace(before.Name)
		afterName := strings.TrimSpace(maintainer.Name)
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
		afterEmail := normalizeValue(maintainer.Email, "EMAIL_MISSING")
		if beforeEmail != afterEmail {
			changes["email"] = map[string]string{"from": beforeEmail, "to": afterEmail}
		}
		beforeGitHub := normalizeValue(before.GitHubAccount, "GITHUB_MISSING")
		afterGitHub := normalizeValue(maintainer.GitHubAccount, "GITHUB_MISSING")
		if beforeGitHub != afterGitHub {
			changes["github"] = map[string]string{"from": beforeGitHub, "to": afterGitHub}
		}
		beforeCompany := ""
		if before.CompanyID != nil {
			var company model.Company
			if err := s.store.DB().First(&company, *before.CompanyID).Error; err == nil {
				beforeCompany = strings.TrimSpace(company.Name)
			}
		}
		afterCompany := strings.TrimSpace(maintainer.Company.Name)
		if afterCompany == "" && maintainer.CompanyID != nil {
			var company model.Company
			if err := s.store.DB().First(&company, *maintainer.CompanyID).Error; err == nil {
				afterCompany = strings.TrimSpace(company.Name)
			}
		}
		if afterCompany == "" && strings.TrimSpace(req.Company) != "" {
			afterCompany = strings.TrimSpace(req.Company)
		}
		if beforeCompany == "" {
			beforeCompany = "COMPANY_MISSING"
		}
		if afterCompany == "" {
			afterCompany = "COMPANY_MISSING"
		}
		if beforeCompany != afterCompany {
			changes["company"] = map[string]string{"from": beforeCompany, "to": afterCompany}
		}
	}

	if len(changes) > 0 {
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
		action := "MAINTAINER_UPDATE"
		if before == nil {
			message = fmt.Sprintf("Maintainer created by %s", staffName)
			action = "MAINTAINER_CREATE"
		} else if len(fieldNames) > 0 {
			message = fmt.Sprintf("Maintainer [%s] updated by %s", strings.Join(fieldNames, ", "), staffName)
		}
		if metadataJSON, err := json.Marshal(metadata); err != nil {
			s.logger.Printf("web-bff: add maintainer audit metadata encode error: %v", err)
		} else {
			event := model.AuditLog{
				ProjectID:    &req.ProjectID,
				MaintainerID: &maintainer.ID,
				StaffID:      staffID,
				Action:       action,
				Message:      message,
				Metadata:     string(metadataJSON),
			}
			if err := s.store.DB().Create(&event).Error; err != nil {
				s.logger.Printf("web-bff: add maintainer audit log failed: %v", err)
			}
		}
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

type searchProjectResult struct {
	ID                        uint       `json:"id"`
	Name                      string     `json:"name"`
	GitHubOrg                 string     `json:"githubOrg,omitempty"`
	OnboardingIssue           *string    `json:"onboardingIssue,omitempty"`
	LegacyMaintainerRef       string     `json:"legacyMaintainerRef,omitempty"`
	DotProjectRepoRef         string     `json:"dotProjectRepoRef,omitempty"`
	DotProjectProjectRef      string     `json:"dotProjectProjectRef,omitempty"`
	DotProjectMaintainerRef   string     `json:"dotProjectMaintainerRef,omitempty"`
	DotProjectSecurityRef     string     `json:"dotProjectSecurityRef,omitempty"`
	DotProjectContributingRef string     `json:"dotProjectContributingRef,omitempty"`
	DotProjectGovernanceRef   string     `json:"dotProjectGovernanceRef,omitempty"`
	DotProjectSchemaVersion   string     `json:"dotProjectSchemaVersion,omitempty"`
	DotProjectMaintainerCount *uint      `json:"dotProjectMaintainerCount,omitempty"`
	DotProjectLastSyncedAt    *time.Time `json:"dotProjectLastSyncedAt,omitempty"`
	DotProjectAdoptionStatus  string     `json:"dotProjectAdoptionStatus,omitempty"`
}

type searchMaintainerResult struct {
	ID       uint                               `json:"id"`
	Name     string                             `json:"name"`
	GitHub   string                             `json:"github"`
	Email    string                             `json:"email,omitempty"`
	Company  string                             `json:"company,omitempty"`
	Projects []companyMaintainerProjectResponse `json:"projects,omitempty"`
}

type searchCompanyResult struct {
	ID   uint   `json:"id"`
	Name string `json:"name"`
}

type searchResponse struct {
	Query            string                   `json:"query"`
	Projects         []searchProjectResult    `json:"projects"`
	Maintainers      []searchMaintainerResult `json:"maintainers"`
	Companies        []searchCompanyResult    `json:"companies"`
	ProjectsTotal    int64                    `json:"projectsTotal"`
	MaintainersTotal int64                    `json:"maintainersTotal"`
	CompaniesTotal   int64                    `json:"companiesTotal"`
}

type companyMaintainerProjectResponse struct {
	ID   uint   `json:"id"`
	Name string `json:"name"`
}

type companyMaintainerResponse struct {
	ID       uint                               `json:"id"`
	Name     string                             `json:"name"`
	GitHub   string                             `json:"github"`
	Email    string                             `json:"email,omitempty"`
	Projects []companyMaintainerProjectResponse `json:"projects"`
}

type companyMaintainersResponse struct {
	ID          uint                        `json:"id"`
	Name        string                      `json:"name"`
	Maintainers []companyMaintainerResponse `json:"maintainers"`
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

func (s *server) handleCompany(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id, err := parseIDParam(r.URL.Path, "/api/companies/")
	if err != nil {
		http.Error(w, "invalid company id", http.StatusBadRequest)
		return
	}
	session := sessionFromContext(r.Context())
	if session == nil || session.Role != roleStaff {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	var company model.Company
	if err := s.store.DB().First(&company, id).Error; err != nil {
		http.Error(w, "company not found", http.StatusNotFound)
		return
	}

	var maintainers []model.Maintainer
	if err := s.store.DB().
		Preload("Projects").
		Where("company_id = ?", id).
		Order("name").
		Find(&maintainers).Error; err != nil {
		s.logger.Printf("web-bff: handleCompany maintainers error: %v", err)
		http.Error(w, "failed to load maintainers", http.StatusInternalServerError)
		return
	}

	maintainerResults := make([]companyMaintainerResponse, 0, len(maintainers))
	for _, maintainer := range maintainers {
		projects := make([]companyMaintainerProjectResponse, 0, len(maintainer.Projects))
		for _, project := range maintainer.Projects {
			projects = append(projects, companyMaintainerProjectResponse{
				ID:   project.ID,
				Name: project.Name,
			})
		}
		maintainerResults = append(maintainerResults, companyMaintainerResponse{
			ID:       maintainer.ID,
			Name:     strings.TrimSpace(maintainer.Name),
			GitHub:   normalizeValue(maintainer.GitHubAccount, "GITHUB_MISSING"),
			Email:    normalizeValue(maintainer.Email, "EMAIL_MISSING"),
			Projects: projects,
		})
	}

	w.Header().Set(headerContentType, contentTypeJSON)
	if err := json.NewEncoder(w).Encode(companyMaintainersResponse{
		ID:          company.ID,
		Name:        company.Name,
		Maintainers: maintainerResults,
	}); err != nil {
		s.logger.Printf("web-bff: handleCompany encode error: %v", err)
	}
}

func (s *server) handleSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	session := sessionFromContext(r.Context())
	if session == nil || session.Role != roleStaff {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("query"))
	if query == "" {
		http.Error(w, "query is required", http.StatusBadRequest)
		return
	}
	limit := 20
	projectsPage := parseIntParam(r, "projectsPage", 1, 1, 1000)
	maintainersPage := parseIntParam(r, "maintainersPage", 1, 1, 1000)
	companiesPage := parseIntParam(r, "companiesPage", 1, 1, 1000)
	projectsOffset := (projectsPage - 1) * limit
	maintainersOffset := (maintainersPage - 1) * limit
	companiesOffset := (companiesPage - 1) * limit
	if s.store.DB().Name() == "postgres" {
		s.handleSearchPostgres(w, query, limit, projectsOffset, maintainersOffset, companiesOffset)
		return
	}
	s.handleSearchFallback(w, query, limit, projectsOffset, maintainersOffset, companiesOffset)
}

func (s *server) handleSearchPostgres(w http.ResponseWriter, query string, limit int, projectsOffset int, maintainersOffset int, companiesOffset int) {
	like := "%" + query + "%"

	var projectsTotal int64
	if err := s.store.DB().Raw(`
		SELECT COUNT(*)
		FROM projects
		WHERE deleted_at IS NULL
		  AND search_tsv @@ websearch_to_tsquery('simple', unaccent(?))`, query).Scan(&projectsTotal).Error; err != nil {
		s.logger.Printf("web-bff: search projects total error: %v", err)
		http.Error(w, "failed to search projects", http.StatusInternalServerError)
		return
	}

	var projects []model.Project
	if err := s.store.DB().Raw(`
		SELECT id, name, git_hub_org, onboarding_issue, maintainer_ref, dot_project_repo_ref,
		       dot_project_project_ref, dot_project_yaml_ref, dot_project_security_ref,
		       dot_project_contributing_ref, dot_project_governance_ref,
		       dot_project_schema_version, dot_project_maintainer_count,
		       dot_project_last_synced_at, dot_project_adoption_status
		FROM projects
		WHERE deleted_at IS NULL
		  AND search_tsv @@ websearch_to_tsquery('simple', unaccent(?))
		ORDER BY ts_rank_cd(search_tsv, websearch_to_tsquery('simple', unaccent(?))) DESC, name
		LIMIT ? OFFSET ?`, query, query, limit, projectsOffset).Scan(&projects).Error; err != nil {
		s.logger.Printf("web-bff: search projects error: %v", err)
		http.Error(w, "failed to search projects", http.StatusInternalServerError)
		return
	}

	projectResults := make([]searchProjectResult, 0, len(projects))
	for _, project := range projects {
		projectResults = append(projectResults, searchProjectResult{
			ID:                        project.ID,
			Name:                      project.Name,
			GitHubOrg:                 strings.TrimSpace(project.GitHubOrg),
			OnboardingIssue:           project.OnboardingIssue,
			LegacyMaintainerRef:       strings.TrimSpace(project.LegacyMaintainerRef),
			DotProjectRepoRef:         strings.TrimSpace(project.DotProjectRepoRef),
			DotProjectProjectRef:      strings.TrimSpace(project.DotProjectProjectRef),
			DotProjectMaintainerRef:   strings.TrimSpace(project.DotProjectMaintainerRef),
			DotProjectSecurityRef:     strings.TrimSpace(project.DotProjectSecurityRef),
			DotProjectContributingRef: strings.TrimSpace(project.DotProjectContributingRef),
			DotProjectGovernanceRef:   strings.TrimSpace(project.DotProjectGovernanceRef),
			DotProjectSchemaVersion:   strings.TrimSpace(project.DotProjectSchemaVersion),
			DotProjectMaintainerCount: project.DotProjectMaintainerCount,
			DotProjectLastSyncedAt:    project.DotProjectLastSyncedAt,
			DotProjectAdoptionStatus:  strings.TrimSpace(project.DotProjectAdoptionStatus),
		})
	}

	type maintainerSearchRow struct {
		ID            uint
		Name          string
		Email         string
		GitHubAccount string `gorm:"column:git_hub_account"`
		CompanyName   string `gorm:"column:company_name"`
	}
	var maintainerRows []maintainerSearchRow
	var maintainersTotal int64
	if err := s.store.DB().Raw(`
		SELECT COUNT(*)
		FROM maintainers m
		WHERE m.deleted_at IS NULL
		  AND (m.search_tsv @@ websearch_to_tsquery('simple', unaccent(?))
		   OR unaccent(m.name) ILIKE unaccent(?)
		   OR unaccent(m.email) ILIKE unaccent(?)
		   OR unaccent(m.git_hub_account) ILIKE unaccent(?))`, query, like, like, like).Scan(&maintainersTotal).Error; err != nil {
		s.logger.Printf("web-bff: search maintainers total error: %v", err)
		http.Error(w, "failed to search maintainers", http.StatusInternalServerError)
		return
	}
	if err := s.store.DB().Raw(`
		SELECT m.id, m.name, m.email, m.git_hub_account, c.name AS company_name
		FROM maintainers m
		LEFT JOIN companies c ON c.id = m.company_id
		WHERE m.deleted_at IS NULL
		  AND (m.search_tsv @@ websearch_to_tsquery('simple', unaccent(?))
		   OR unaccent(m.name) ILIKE unaccent(?)
		   OR unaccent(m.email) ILIKE unaccent(?)
		   OR unaccent(m.git_hub_account) ILIKE unaccent(?))
		ORDER BY ts_rank_cd(m.search_tsv, websearch_to_tsquery('simple', unaccent(?))) DESC, m.name
		LIMIT ? OFFSET ?`, query, like, like, like, query, limit, maintainersOffset).Scan(&maintainerRows).Error; err != nil {
		s.logger.Printf("web-bff: search maintainers error: %v", err)
		http.Error(w, "failed to search maintainers", http.StatusInternalServerError)
		return
	}
	maintainerResults := make([]searchMaintainerResult, 0, len(maintainerRows))
	maintainerIDs := make([]uint, 0, len(maintainerRows))
	for _, maintainer := range maintainerRows {
		maintainerIDs = append(maintainerIDs, maintainer.ID)
		result := searchMaintainerResult{
			ID:     maintainer.ID,
			Name:   strings.TrimSpace(maintainer.Name),
			GitHub: normalizeValue(maintainer.GitHubAccount, "GITHUB_MISSING"),
			Email:  normalizeValue(maintainer.Email, "EMAIL_MISSING"),
		}
		if maintainer.CompanyName != "" {
			result.Company = maintainer.CompanyName
		}
		maintainerResults = append(maintainerResults, result)
	}

	if len(maintainerIDs) > 0 {
		type maintainerProjectRow struct {
			MaintainerID uint   `gorm:"column:maintainer_id"`
			ProjectID    uint   `gorm:"column:id"`
			ProjectName  string `gorm:"column:name"`
		}
		var projectRows []maintainerProjectRow
		if err := s.store.DB().Raw(`
			SELECT mp.maintainer_id, p.id, p.name
			FROM maintainer_projects mp
			JOIN projects p ON p.id = mp.project_id
			WHERE p.deleted_at IS NULL
			  AND mp.maintainer_id IN ?
			ORDER BY p.name`, maintainerIDs).Scan(&projectRows).Error; err != nil {
			s.logger.Printf("web-bff: search maintainer projects error: %v", err)
			http.Error(w, "failed to search maintainers", http.StatusInternalServerError)
			return
		}
		projectMap := make(map[uint][]companyMaintainerProjectResponse)
		for _, row := range projectRows {
			projectMap[row.MaintainerID] = append(projectMap[row.MaintainerID], companyMaintainerProjectResponse{
				ID:   row.ProjectID,
				Name: row.ProjectName,
			})
		}
		for i := range maintainerResults {
			maintainerResults[i].Projects = projectMap[maintainerResults[i].ID]
		}
	}

	var companies []model.Company
	var companiesTotal int64
	if err := s.store.DB().Raw(`
		SELECT COUNT(*)
		FROM companies
		WHERE deleted_at IS NULL
		  AND (unaccent(name) ILIKE unaccent(?)
		   OR similarity(unaccent(name), unaccent(?)) > 0.2)`, like, query).Scan(&companiesTotal).Error; err != nil {
		s.logger.Printf("web-bff: search companies total error: %v", err)
		http.Error(w, "failed to search companies", http.StatusInternalServerError)
		return
	}
	if err := s.store.DB().Raw(`
		SELECT id, name
		FROM companies
		WHERE deleted_at IS NULL
		  AND (unaccent(name) ILIKE unaccent(?)
		   OR similarity(unaccent(name), unaccent(?)) > 0.2)
		ORDER BY similarity(unaccent(name), unaccent(?)) DESC, name
		LIMIT ? OFFSET ?`, like, query, query, limit, companiesOffset).Scan(&companies).Error; err != nil {
		s.logger.Printf("web-bff: search companies error: %v", err)
		http.Error(w, "failed to search companies", http.StatusInternalServerError)
		return
	}
	companyResults := make([]searchCompanyResult, 0, len(companies))
	for _, company := range companies {
		companyResults = append(companyResults, searchCompanyResult{
			ID:   company.ID,
			Name: company.Name,
		})
	}

	w.Header().Set(headerContentType, contentTypeJSON)
	if err := json.NewEncoder(w).Encode(searchResponse{
		Query:            query,
		Projects:         projectResults,
		Maintainers:      maintainerResults,
		Companies:        companyResults,
		ProjectsTotal:    projectsTotal,
		MaintainersTotal: maintainersTotal,
		CompaniesTotal:   companiesTotal,
	}); err != nil {
		s.logger.Printf("web-bff: handleSearch encode error: %v", err)
	}
}

func (s *server) handleSearchFallback(w http.ResponseWriter, query string, limit int, projectsOffset int, maintainersOffset int, companiesOffset int) {
	like := "%" + strings.ToLower(query) + "%"

	var projectsTotal int64
	var projects []model.Project
	if err := s.store.DB().
		Model(&model.Project{}).
		Where(
			"LOWER(name) LIKE ? OR LOWER(maintainer_ref) LIKE ? OR LOWER(dot_project_yaml_ref) LIKE ? OR LOWER(git_hub_org) LIKE ?",
			like,
			like,
			like,
			like,
		).
		Count(&projectsTotal).Error; err != nil {
		s.logger.Printf("web-bff: search projects total error: %v", err)
		http.Error(w, "failed to search projects", http.StatusInternalServerError)
		return
	}
	if err := s.store.DB().
		Model(&model.Project{}).
		Select(`id, name, git_hub_org, onboarding_issue, maintainer_ref,
			dot_project_repo_ref, dot_project_project_ref, dot_project_yaml_ref,
			dot_project_security_ref, dot_project_contributing_ref, dot_project_governance_ref,
			dot_project_schema_version, dot_project_maintainer_count, dot_project_last_synced_at,
			dot_project_adoption_status`).
		Where(
			"LOWER(name) LIKE ? OR LOWER(maintainer_ref) LIKE ? OR LOWER(dot_project_yaml_ref) LIKE ? OR LOWER(git_hub_org) LIKE ?",
			like,
			like,
			like,
			like,
		).
		Order("name").
		Limit(limit).
		Offset(projectsOffset).
		Find(&projects).Error; err != nil {
		s.logger.Printf("web-bff: search projects error: %v", err)
		http.Error(w, "failed to search projects", http.StatusInternalServerError)
		return
	}

	projectResults := make([]searchProjectResult, 0, len(projects))
	for _, project := range projects {
		projectResults = append(projectResults, searchProjectResult{
			ID:                        project.ID,
			Name:                      project.Name,
			GitHubOrg:                 strings.TrimSpace(project.GitHubOrg),
			OnboardingIssue:           project.OnboardingIssue,
			LegacyMaintainerRef:       strings.TrimSpace(project.LegacyMaintainerRef),
			DotProjectRepoRef:         strings.TrimSpace(project.DotProjectRepoRef),
			DotProjectProjectRef:      strings.TrimSpace(project.DotProjectProjectRef),
			DotProjectMaintainerRef:   strings.TrimSpace(project.DotProjectMaintainerRef),
			DotProjectSecurityRef:     strings.TrimSpace(project.DotProjectSecurityRef),
			DotProjectContributingRef: strings.TrimSpace(project.DotProjectContributingRef),
			DotProjectGovernanceRef:   strings.TrimSpace(project.DotProjectGovernanceRef),
			DotProjectSchemaVersion:   strings.TrimSpace(project.DotProjectSchemaVersion),
			DotProjectMaintainerCount: project.DotProjectMaintainerCount,
			DotProjectLastSyncedAt:    project.DotProjectLastSyncedAt,
			DotProjectAdoptionStatus:  strings.TrimSpace(project.DotProjectAdoptionStatus),
		})
	}

	var maintainers []model.Maintainer
	var maintainersTotal int64
	if err := s.store.DB().
		Model(&model.Maintainer{}).
		Where(
			"LOWER(name) LIKE ? OR LOWER(email) LIKE ? OR LOWER(git_hub_account) LIKE ?",
			like,
			like,
			like,
		).
		Count(&maintainersTotal).Error; err != nil {
		s.logger.Printf("web-bff: search maintainers total error: %v", err)
		http.Error(w, "failed to search maintainers", http.StatusInternalServerError)
		return
	}
	if err := s.store.DB().
		Preload("Company").
		Preload("Projects").
		Where(
			"LOWER(name) LIKE ? OR LOWER(email) LIKE ? OR LOWER(git_hub_account) LIKE ?",
			like,
			like,
			like,
		).
		Order("name").
		Limit(limit).
		Offset(maintainersOffset).
		Find(&maintainers).Error; err != nil {
		s.logger.Printf("web-bff: search maintainers error: %v", err)
		http.Error(w, "failed to search maintainers", http.StatusInternalServerError)
		return
	}
	maintainerResults := make([]searchMaintainerResult, 0, len(maintainers))
	for _, maintainer := range maintainers {
		result := searchMaintainerResult{
			ID:     maintainer.ID,
			Name:   strings.TrimSpace(maintainer.Name),
			GitHub: normalizeValue(maintainer.GitHubAccount, "GITHUB_MISSING"),
			Email:  normalizeValue(maintainer.Email, "EMAIL_MISSING"),
		}
		if maintainer.Company.Name != "" {
			result.Company = maintainer.Company.Name
		}
		if len(maintainer.Projects) > 0 {
			projects := make([]companyMaintainerProjectResponse, 0, len(maintainer.Projects))
			for _, project := range maintainer.Projects {
				projects = append(projects, companyMaintainerProjectResponse{
					ID:   project.ID,
					Name: project.Name,
				})
			}
			result.Projects = projects
		}
		maintainerResults = append(maintainerResults, result)
	}

	var companies []model.Company
	var companiesTotal int64
	if err := s.store.DB().
		Model(&model.Company{}).
		Where("LOWER(name) LIKE ?", like).
		Count(&companiesTotal).Error; err != nil {
		s.logger.Printf("web-bff: search companies total error: %v", err)
		http.Error(w, "failed to search companies", http.StatusInternalServerError)
		return
	}
	if err := s.store.DB().
		Where("LOWER(name) LIKE ?", like).
		Order("name").
		Limit(limit).
		Offset(companiesOffset).
		Find(&companies).Error; err != nil {
		s.logger.Printf("web-bff: search companies error: %v", err)
		http.Error(w, "failed to search companies", http.StatusInternalServerError)
		return
	}
	companyResults := make([]searchCompanyResult, 0, len(companies))
	for _, company := range companies {
		companyResults = append(companyResults, searchCompanyResult{
			ID:   company.ID,
			Name: company.Name,
		})
	}

	w.Header().Set(headerContentType, contentTypeJSON)
	if err := json.NewEncoder(w).Encode(searchResponse{
		Query:            query,
		Projects:         projectResults,
		Maintainers:      maintainerResults,
		Companies:        companyResults,
		ProjectsTotal:    projectsTotal,
		MaintainersTotal: maintainersTotal,
		CompaniesTotal:   companiesTotal,
	}); err != nil {
		s.logger.Printf("web-bff: handleSearch encode error: %v", err)
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

type fossaChooseErrorResponse struct {
	Error   string `json:"error"`
	ErrorAt string `json:"errorAt"`
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
	if s.githubToken == "" && !s.testMode {
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

func (s *server) handleFossaChoose(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	session := sessionFromContext(r.Context())
	if session == nil || session.Role != roleStaff {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	writeChooseError := func(status int, err error) {
		w.Header().Set(headerContentType, contentTypeJSON)
		w.WriteHeader(status)
		payload := fossaChooseErrorResponse{
			Error:   err.Error(),
			ErrorAt: time.Now().UTC().Format(time.RFC3339),
		}
		if encodeErr := json.NewEncoder(w).Encode(payload); encodeErr != nil {
			s.logger.Printf("web-bff: handleFossaChoose encode error: %v", encodeErr)
		}
	}
	var req fossaChooseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if req.ProjectID == 0 {
		http.Error(w, "missing projectId", http.StatusBadRequest)
		return
	}
	project, err := s.store.GetProjectByID(req.ProjectID)
	if err != nil {
		if errors.Is(err, db.ErrProjectNotFound) {
			http.Error(w, "project not found", http.StatusNotFound)
			return
		}
		writeChooseError(http.StatusInternalServerError, fmt.Errorf("failed to load project"))
		return
	}
	serviceID, err := s.getFossaServiceID()
	if err != nil {
		writeChooseError(http.StatusInternalServerError, fmt.Errorf("failed to resolve FOSSA service"))
		return
	}
	var service model.Service
	if err := s.store.DB().First(&service, serviceID).Error; err != nil {
		writeChooseError(http.StatusInternalServerError, fmt.Errorf("failed to load FOSSA service"))
		return
	}
	if err := s.store.DB().Model(project).Association("Services").Append(&service); err != nil {
		writeChooseError(http.StatusInternalServerError, fmt.Errorf("failed to associate FOSSA service"))
		return
	}
	staffID := lookupStaffID(s.store, session.Login)
	logFossaTeamAudit := func(action string, team *model.RemoteTeam) {
		teamName := ""
		if team.RemoteTeamName != nil {
			teamName = *team.RemoteTeamName
		}
		metadata := map[string]map[string]string{
			"team": {
				"id":   fmt.Sprintf("%d", team.RemoteTeamID),
				"name": teamName,
			},
		}
		metadataJSON, err := json.Marshal(metadata)
		if err != nil {
			s.logger.Printf("web-bff: handleFossaChoose marshal metadata error: %v", err)
			return
		}
		event := model.AuditLog{
			StaffID:   staffID,
			Action:    action,
			Message:   action,
			Metadata:  string(metadataJSON),
			ProjectID: &project.ID,
		}
		if err := s.store.DB().Create(&event).Error; err != nil {
			s.logger.Printf("web-bff: handleFossaChoose audit log failed: %v", err)
		}
	}
	if (s.testMode && !s.allowLiveFossa) || strings.TrimSpace(s.fossaToken) == "" {
		s.logger.Printf(
			"web-bff: skipping live FOSSA choose project=%d testMode=%t allowLiveFossa=%t fossaTokenSet=%t",
			project.ID,
			s.testMode,
			s.allowLiveFossa,
			strings.TrimSpace(s.fossaToken) != "",
		)
		if team, err := s.store.GetRemoteTeamByProject(project.ID, serviceID); err == nil && team != nil {
			logFossaTeamAudit("FOSSA_TEAM_REUSED", team)
			w.Header().Set(headerContentType, contentTypeJSON)
			if err := json.NewEncoder(w).Encode(map[string]string{"status": "ok"}); err != nil {
				s.logger.Printf("web-bff: handleFossaChoose encode error: %v", err)
			}
			return
		}
		if s.testMode {
			if raw := strings.TrimSpace(os.Getenv("BFF_TEST_FOSSA_TEAM_ID")); raw != "" {
				if parsed, err := strconv.ParseUint(raw, 10, 64); err == nil && parsed > 0 {
					teamName := project.Name
					serviceTeam := model.RemoteTeam{
						ProjectID:      project.ID,
						ServiceID:      serviceID,
						RemoteTeamID:   uint(parsed),
						RemoteTeamName: &teamName,
						ProjectName:    &teamName,
					}
					if err := s.store.DB().Create(&serviceTeam).Error; err != nil {
						writeChooseError(http.StatusInternalServerError, fmt.Errorf("failed to create FOSSA team"))
						return
					}
					logFossaTeamAudit("FOSSA_TEAM_CREATED", &serviceTeam)
					w.Header().Set(headerContentType, contentTypeJSON)
					if err := json.NewEncoder(w).Encode(map[string]string{"status": "ok"}); err != nil {
						s.logger.Printf("web-bff: handleFossaChoose encode error: %v", err)
					}
					return
				}
			}
		}
		writeChooseError(http.StatusBadRequest, fmt.Errorf("FOSSA team not found; RemoteTeamID must come from FOSSA API"))
		return
	}
	client, err := s.fossaClient()
	if err != nil {
		writeChooseError(http.StatusInternalServerError, err)
		return
	}
	team, created, err := s.ensureFossaTeam(*project, client)
	if err != nil {
		s.logger.Printf("web-bff: fossa choose ensure team failed project=%d err=%v", project.ID, err)
		writeChooseError(http.StatusBadGateway, fmt.Errorf("failed to start FOSSA onboarding"))
		return
	}
	if team != nil {
		if created {
			logFossaTeamAudit("FOSSA_TEAM_CREATED", team)
		} else {
			logFossaTeamAudit("FOSSA_TEAM_REUSED", team)
		}
	}
	w.Header().Set(headerContentType, contentTypeJSON)
	if err := json.NewEncoder(w).Encode(map[string]string{"status": "ok"}); err != nil {
		s.logger.Printf("web-bff: handleFossaChoose encode error: %v", err)
	}
}

func (s *server) handleFossaInvite(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	session := sessionFromContext(r.Context())
	if session == nil || session.Role != roleStaff {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var req fossaInviteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if req.ProjectID == 0 {
		http.Error(w, "missing projectId", http.StatusBadRequest)
		return
	}
	project, err := s.store.GetProjectByID(req.ProjectID)
	if err != nil {
		if errors.Is(err, db.ErrProjectNotFound) {
			http.Error(w, "project not found", http.StatusNotFound)
			return
		}
		http.Error(w, "failed to load project", http.StatusInternalServerError)
		return
	}
	maintainers := project.Maintainers
	if len(req.MaintainerIDs) > 0 {
		allowed := make(map[uint]struct{}, len(req.MaintainerIDs))
		for _, id := range req.MaintainerIDs {
			allowed[id] = struct{}{}
		}
		filtered := make([]model.Maintainer, 0, len(req.MaintainerIDs))
		for _, maintainer := range maintainers {
			if _, ok := allowed[maintainer.ID]; ok {
				filtered = append(filtered, maintainer)
			}
		}
		maintainers = filtered
	}
	serviceID, err := s.getFossaServiceID()
	if err != nil {
		http.Error(w, "failed to resolve FOSSA service", http.StatusInternalServerError)
		return
	}
	if s.testMode && strings.TrimSpace(s.fossaToken) == "" {
		serviceTeam, err := s.store.GetRemoteTeamByProject(project.ID, serviceID)
		if err != nil || serviceTeam == nil {
			http.Error(w, "failed to resolve FOSSA team", http.StatusInternalServerError)
			return
		}
		now := time.Now().UTC()
		resp := fossaInviteResponse{
			Invited: []string{},
			Skipped: []string{},
			Errors:  map[string]string{},
		}
		for _, maintainer := range maintainers {
			if maintainer.MaintainerStatus != "" && maintainer.MaintainerStatus != model.ActiveMaintainer {
				resp.Skipped = append(resp.Skipped, maintainer.GitHubAccount)
				continue
			}
			email := strings.TrimSpace(maintainer.Email)
			if email == "" || strings.EqualFold(email, "EMAIL_MISSING") {
				resp.Skipped = append(resp.Skipped, maintainer.GitHubAccount)
				continue
			}
			inviteStatus := &model.ServiceInvitation{
				ProjectID:     project.ID,
				MaintainerID:  &maintainer.ID,
				ServiceID:     serviceID,
				ServiceEmail:  email,
				RemoteTeamID:  serviceTeam.RemoteTeamID,
				Status:        "pending",
				SentAt:        &now,
				LastCheckedAt: &now,
			}
			if _, upsertErr := s.store.UpsertServiceInvitation(inviteStatus); upsertErr != nil {
				resp.Errors[email] = "failed to store invite status"
				continue
			}
			resp.Invited = append(resp.Invited, email)
		}
		w.Header().Set(headerContentType, contentTypeJSON)
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			s.logger.Printf("web-bff: handleFossaInvite encode error: %v", err)
		}
		return
	}

	client, err := s.fossaClient()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	serviceTeam, _, err := s.ensureFossaTeam(*project, client)
	if err != nil {
		s.logger.Printf("web-bff: fossa invite ensure team failed project=%d err=%v", project.ID, err)
		http.Error(w, "failed to resolve FOSSA team", http.StatusInternalServerError)
		return
	}
	now := time.Now().UTC()
	resp := fossaInviteResponse{
		Invited: []string{},
		Skipped: []string{},
		Errors:  map[string]string{},
	}
	for _, maintainer := range maintainers {
		if maintainer.MaintainerStatus != "" && maintainer.MaintainerStatus != model.ActiveMaintainer {
			resp.Skipped = append(resp.Skipped, maintainer.GitHubAccount)
			continue
		}
		email := strings.TrimSpace(maintainer.Email)
		if email == "" || strings.EqualFold(email, "EMAIL_MISSING") {
			resp.Skipped = append(resp.Skipped, maintainer.GitHubAccount)
			continue
		}

		err := client.SendUserInvitation(email)
		if err == nil || errors.Is(err, fossa.ErrInviteAlreadyExists) {
			inviteStatus := &model.ServiceInvitation{
				ProjectID:     project.ID,
				MaintainerID:  &maintainer.ID,
				ServiceID:     serviceID,
				ServiceEmail:  email,
				RemoteTeamID:  serviceTeam.RemoteTeamID,
				Status:        "pending",
				SentAt:        &now,
				LastCheckedAt: &now,
			}
			if _, upsertErr := s.store.UpsertServiceInvitation(inviteStatus); upsertErr != nil {
				resp.Errors[email] = "failed to store invite status"
				continue
			}
			resp.Invited = append(resp.Invited, email)
			continue
		}

		if errors.Is(err, fossa.ErrUserAlreadyMember) {
			teamAdminRoleID, resolveErr := client.ResolveTeamAdminRoleID()
			if resolveErr != nil {
				msg := resolveErr.Error()
				inviteStatus := &model.ServiceInvitation{
					ProjectID:     project.ID,
					MaintainerID:  &maintainer.ID,
					ServiceID:     serviceID,
					ServiceEmail:  email,
					RemoteTeamID:  serviceTeam.RemoteTeamID,
					Status:        "error",
					LastError:     &msg,
					LastCheckedAt: &now,
				}
				if _, upsertErr := s.store.UpsertServiceInvitation(inviteStatus); upsertErr != nil {
					resp.Errors[email] = "failed to store invite status"
				} else {
					resp.Errors[email] = msg
				}
				continue
			}
			if addErr := client.AddUserToTeamByEmail(serviceTeam.RemoteTeamID, email, teamAdminRoleID); addErr == nil {
				inviteStatus := &model.ServiceInvitation{
					ProjectID:     project.ID,
					MaintainerID:  &maintainer.ID,
					ServiceID:     serviceID,
					ServiceEmail:  email,
					RemoteTeamID:  serviceTeam.RemoteTeamID,
					Status:        "accepted",
					LastCheckedAt: &now,
				}
				if _, upsertErr := s.store.UpsertServiceInvitation(inviteStatus); upsertErr != nil {
					resp.Errors[email] = "failed to store invite status"
				}
				continue
			} else {
				msg := addErr.Error()
				inviteStatus := &model.ServiceInvitation{
					ProjectID:     project.ID,
					MaintainerID:  &maintainer.ID,
					ServiceID:     serviceID,
					ServiceEmail:  email,
					RemoteTeamID:  serviceTeam.RemoteTeamID,
					Status:        "error",
					LastError:     &msg,
					LastCheckedAt: &now,
				}
				if _, upsertErr := s.store.UpsertServiceInvitation(inviteStatus); upsertErr != nil {
					resp.Errors[email] = "failed to store invite status"
				} else {
					resp.Errors[email] = msg
				}
				continue
			}
		}

		msg := err.Error()
		inviteStatus := &model.ServiceInvitation{
			ProjectID:     project.ID,
			MaintainerID:  &maintainer.ID,
			ServiceID:     serviceID,
			ServiceEmail:  email,
			RemoteTeamID:  serviceTeam.RemoteTeamID,
			Status:        "error",
			LastError:     &msg,
			LastCheckedAt: &now,
		}
		if _, upsertErr := s.store.UpsertServiceInvitation(inviteStatus); upsertErr != nil {
			resp.Errors[email] = "failed to store invite status"
		} else {
			resp.Errors[email] = msg
		}
	}

	w.Header().Set(headerContentType, contentTypeJSON)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		s.logger.Printf("web-bff: handleFossaInvite encode error: %v", err)
	}
}

func (s *server) handleFossaInvites(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	session := sessionFromContext(r.Context())
	if session == nil || session.Role != roleStaff {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	projectIDStr := strings.TrimSpace(r.URL.Query().Get("projectId"))
	if projectIDStr == "" {
		http.Error(w, "missing projectId", http.StatusBadRequest)
		return
	}
	projectID, err := strconv.ParseUint(projectIDStr, 10, 64)
	if err != nil || projectID == 0 {
		http.Error(w, "invalid projectId", http.StatusBadRequest)
		return
	}
	serviceID, err := s.getFossaServiceID()
	if err != nil {
		http.Error(w, "failed to resolve FOSSA service", http.StatusInternalServerError)
		return
	}
	invites, err := s.store.ListServiceInvitations(uint(projectID), serviceID)
	if err != nil {
		http.Error(w, "failed to load invites", http.StatusInternalServerError)
		return
	}
	var fossaTeamName string
	if serviceTeam, err := s.store.GetRemoteTeamByProject(uint(projectID), serviceID); err == nil && serviceTeam != nil {
		if serviceTeam.RemoteTeamName != nil {
			fossaTeamName = *serviceTeam.RemoteTeamName
		}
	}
	resp := make([]fossaInviteSummary, 0, len(invites))
	for _, invite := range invites {
		resp = append(resp, fossaInviteSummary{
			ID:                   invite.ID,
			ProjectID:            invite.ProjectID,
			MaintainerID:         invite.MaintainerID,
			Email:                invite.ServiceEmail,
			FossaTeamID:          invite.RemoteTeamID,
			FossaTeamName:        fossaTeamName,
			Status:               invite.Status,
			TeamAssignmentStatus: invite.TeamAssignmentStatus,
			TeamAddAttempts:      invite.TeamAddAttempts,
			NextTeamAddAt:        invite.NextTeamAddAt,
			LastError:            invite.LastError,
			SentAt:               invite.SentAt,
			LastCheckedAt:        invite.LastCheckedAt,
		})
	}
	w.Header().Set(headerContentType, contentTypeJSON)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		s.logger.Printf("web-bff: handleFossaInvites encode error: %v", err)
	}
}

func (s *server) handleFossaInviteRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	session := sessionFromContext(r.Context())
	if session == nil || session.Role != roleStaff {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	projectIDStr := strings.TrimSpace(r.URL.Query().Get("projectId"))
	if projectIDStr == "" {
		http.Error(w, "missing projectId", http.StatusBadRequest)
		return
	}
	projectID, err := strconv.ParseUint(projectIDStr, 10, 64)
	if err != nil || projectID == 0 {
		http.Error(w, "invalid projectId", http.StatusBadRequest)
		return
	}
	project, err := s.store.GetProjectByID(uint(projectID))
	if err != nil {
		if errors.Is(err, db.ErrProjectNotFound) {
			http.Error(w, "project not found", http.StatusNotFound)
			return
		}
		http.Error(w, "failed to load project", http.StatusInternalServerError)
		return
	}
	serviceID, err := s.getFossaServiceID()
	if err != nil {
		http.Error(w, "failed to resolve FOSSA service", http.StatusInternalServerError)
		return
	}
	serviceTeam, err := s.store.GetRemoteTeamByProject(project.ID, serviceID)
	if err != nil {
		http.Error(w, "failed to load FOSSA team", http.StatusInternalServerError)
		return
	}
	if serviceTeam == nil {
		http.Error(w, "FOSSA team not assigned", http.StatusBadRequest)
		return
	}
	client, err := s.fossaClient()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	pendingEmails, err := client.FetchUserInvitationEmails()
	if err != nil {
		s.logger.Printf("web-bff: refresh FOSSA invites failed project=%d err=%v", project.ID, err)
		http.Error(w, "failed to refresh FOSSA invites", http.StatusBadGateway)
		return
	}

	activeMaintainers := make(map[string]model.Maintainer)
	for _, maintainer := range project.Maintainers {
		if maintainer.MaintainerStatus != "" && maintainer.MaintainerStatus != model.ActiveMaintainer {
			continue
		}
		email := strings.TrimSpace(maintainer.Email)
		if email == "" || strings.EqualFold(email, "EMAIL_MISSING") {
			continue
		}
		activeMaintainers[strings.ToLower(email)] = maintainer
	}

	invites, err := s.store.ListServiceInvitations(project.ID, serviceID)
	if err != nil {
		http.Error(w, "failed to load invites", http.StatusInternalServerError)
		return
	}
	existing := make(map[string]model.ServiceInvitation, len(invites))
	for _, invite := range invites {
		email := strings.TrimSpace(invite.ServiceEmail)
		if email == "" || strings.EqualFold(email, "EMAIL_MISSING") {
			continue
		}
		existing[strings.ToLower(email)] = invite
	}

	now := time.Now().UTC()
	added := 0
	updated := 0
	removed := 0

	for email := range pendingEmails {
		maintainer, ok := activeMaintainers[email]
		if !ok {
			continue
		}
		if invite, ok := existing[email]; ok {
			invite.Status = "pending"
			invite.LastError = nil
			invite.LastCheckedAt = &now
			if _, upsertErr := s.store.UpsertServiceInvitation(&invite); upsertErr == nil {
				updated++
			}
			continue
		}
		invite := &model.ServiceInvitation{
			ProjectID:     project.ID,
			MaintainerID:  &maintainer.ID,
			ServiceID:     serviceID,
			ServiceEmail:  maintainer.Email,
			RemoteTeamID:  serviceTeam.RemoteTeamID,
			Status:        "pending",
			LastCheckedAt: &now,
		}
		if _, upsertErr := s.store.UpsertServiceInvitation(invite); upsertErr == nil {
			added++
		}
	}

	for _, invite := range invites {
		if invite.RemoteTeamID != serviceTeam.RemoteTeamID {
			continue
		}
		if invite.Status == "accepted" && invite.TeamAssignmentStatus != nil && *invite.TeamAssignmentStatus != "done" {
			continue
		}
		email := strings.ToLower(strings.TrimSpace(invite.ServiceEmail))
		if email == "" || strings.EqualFold(email, "email_missing") {
			continue
		}
		if _, ok := pendingEmails[email]; ok {
			continue
		}
		if err := s.store.DeleteServiceInvitation(invite.ID); err == nil {
			removed++
		}
	}

	w.Header().Set(headerContentType, contentTypeJSON)
	if err := json.NewEncoder(w).Encode(map[string]int{
		"added":   added,
		"updated": updated,
		"removed": removed,
	}); err != nil {
		s.logger.Printf("web-bff: handleFossaInviteRefresh encode error: %v", err)
	}
}

func (s *server) handleFossaTeamSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	session := sessionFromContext(r.Context())
	if session == nil || session.Role != roleStaff {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	projectIDStr := strings.TrimSpace(r.URL.Query().Get("projectId"))
	if projectIDStr == "" {
		http.Error(w, "missing projectId", http.StatusBadRequest)
		return
	}
	projectID, err := strconv.ParseUint(projectIDStr, 10, 64)
	if err != nil || projectID == 0 {
		http.Error(w, "invalid projectId", http.StatusBadRequest)
		return
	}
	project, err := s.store.GetProjectByID(uint(projectID))
	if err != nil {
		if errors.Is(err, db.ErrProjectNotFound) {
			http.Error(w, "project not found", http.StatusNotFound)
			return
		}
		http.Error(w, "failed to load project", http.StatusInternalServerError)
		return
	}
	serviceID, err := s.getFossaServiceID()
	if err != nil {
		http.Error(w, "failed to resolve FOSSA service", http.StatusInternalServerError)
		return
	}
	serviceTeam, err := s.store.GetRemoteTeamByProject(project.ID, serviceID)
	if err != nil {
		http.Error(w, "failed to load FOSSA team", http.StatusInternalServerError)
		return
	}
	if serviceTeam == nil {
		http.Error(w, "FOSSA team not assigned", http.StatusBadRequest)
		return
	}
	client, err := s.fossaClient()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	members, err := client.FetchTeamMembers(serviceTeam.RemoteTeamID)
	if err != nil {
		s.logger.Printf("web-bff: sync FOSSA team failed project=%d err=%v", project.ID, err)
		http.Error(w, "failed to sync FOSSA team", http.StatusBadGateway)
		return
	}

	maintainerByEmail := make(map[string]model.Maintainer, len(project.Maintainers))
	for _, maintainer := range project.Maintainers {
		email := strings.ToLower(strings.TrimSpace(maintainer.Email))
		if email == "" || strings.EqualFold(email, "EMAIL_MISSING") {
			continue
		}
		maintainerByEmail[email] = maintainer
	}

	emails := make([]string, 0, len(members))
	updatedUsers := 0
	linkedMaintainers := 0
	for _, member := range members {
		email := strings.TrimSpace(member.Email)
		if email == "" {
			continue
		}
		emails = append(emails, email)
		remoteUser, err := s.store.UpsertRemoteUser(&model.RemoteUser{
			ServiceID:    serviceID,
			RemoteUserID: member.UserID,
			ServiceEmail: email,
			RemoteRef:    member.Username,
		})
		if err == nil {
			updatedUsers++
		} else {
			s.logger.Printf("web-bff: sync FOSSA user upsert failed project=%d userID=%d err=%v",
				project.ID, member.UserID, err)
		}
		var maintainerID *uint
		if maintainer, ok := maintainerByEmail[strings.ToLower(email)]; ok {
			maintainerID = &maintainer.ID
		}
		if remoteUser != nil {
			if _, err := s.store.UpsertRemoteUserTeam(&model.RemoteTeamUser{
				ServiceID:    serviceID,
				TeamID:       serviceTeam.ID,
				UserID:       remoteUser.ID,
				MaintainerID: maintainerID,
			}); err == nil {
				if maintainerID != nil {
					linkedMaintainers++
				}
			} else {
				s.logger.Printf("web-bff: sync FOSSA team link failed project=%d teamID=%d userID=%d err=%v",
					project.ID, serviceTeam.ID, member.UserID, err)
			}
		} else {
			s.logger.Printf("web-bff: sync FOSSA team link skipped project=%d teamID=%d userID=%d err=missing remote user",
				project.ID, serviceTeam.ID, member.UserID)
		}
	}
	if len(emails) > 0 {
		s.setCachedFossaTeamEmails(serviceTeam.RemoteTeamID, emails)
	}

	w.Header().Set(headerContentType, contentTypeJSON)
	if err := json.NewEncoder(w).Encode(map[string]int{
		"usersUpserted": updatedUsers,
		"linksUpserted": linkedMaintainers,
	}); err != nil {
		s.logger.Printf("web-bff: handleFossaTeamSync encode error: %v", err)
	}
}

func (s *server) handleFossaInviteAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	session := sessionFromContext(r.Context())
	if session == nil || session.Role != roleStaff {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	trimmed := strings.TrimPrefix(r.URL.Path, "/api/services/fossa/invites/")
	parts := strings.Split(strings.Trim(trimmed, "/"), "/")
	if len(parts) != 2 || (parts[1] != "reissue" && parts[1] != "delete") {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	id, err := strconv.Atoi(parts[0])
	if err != nil || id <= 0 {
		http.Error(w, "invalid invite id", http.StatusBadRequest)
		return
	}
	invite, err := s.store.GetServiceInvitationByID(uint(id))
	if err != nil {
		http.Error(w, "invite not found", http.StatusNotFound)
		return
	}
	if invite.ServiceEmail == "" {
		http.Error(w, "missing email", http.StatusBadRequest)
		return
	}
	email := strings.TrimSpace(invite.ServiceEmail)
	if email == "" || strings.EqualFold(email, "EMAIL_MISSING") {
		http.Error(w, "missing email", http.StatusBadRequest)
		return
	}
	if parts[1] == "delete" {
		client, err := s.fossaClient()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		s.logger.Printf("web-bff: delete FOSSA invite attempt invite=%d remoteTeamID=%d email=%s", invite.ID, invite.RemoteTeamID, email)
		if err := client.DeleteUserInvitation(email); err != nil {
			s.logger.Printf("web-bff: delete FOSSA invite failed invite=%d email=%s err=%v", invite.ID, email, err)
			http.Error(w, "failed to delete invite on FOSSA", http.StatusBadGateway)
			return
		}
		if err := s.store.DeleteServiceInvitation(invite.ID); err != nil {
			http.Error(w, "failed to delete invite", http.StatusInternalServerError)
			return
		}
		w.Header().Set(headerContentType, contentTypeJSON)
		if err := json.NewEncoder(w).Encode(map[string]string{"status": "deleted"}); err != nil {
			s.logger.Printf("web-bff: handleFossaInviteAction encode error: %v", err)
		}
		return
	}
	client, err := s.fossaClient()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	now := time.Now().UTC()

	err = client.SendUserInvitation(email)
	if err == nil || errors.Is(err, fossa.ErrInviteAlreadyExists) {
		invite.Status = "pending"
		invite.LastError = nil
		invite.SentAt = &now
		invite.LastCheckedAt = &now
		if _, upsertErr := s.store.UpsertServiceInvitation(invite); upsertErr != nil {
			http.Error(w, "failed to update invite", http.StatusInternalServerError)
			return
		}
		w.Header().Set(headerContentType, contentTypeJSON)
		if err := json.NewEncoder(w).Encode(map[string]string{"status": "ok"}); err != nil {
			s.logger.Printf("web-bff: handleFossaInviteAction encode error: %v", err)
		}
		return
	}
	if errors.Is(err, fossa.ErrUserAlreadyMember) {
		teamAdminRoleID, resolveErr := client.ResolveTeamAdminRoleID()
		if resolveErr != nil {
			msg := resolveErr.Error()
			invite.Status = "error"
			invite.LastError = &msg
			invite.LastCheckedAt = &now
			if _, upsertErr := s.store.UpsertServiceInvitation(invite); upsertErr != nil {
				s.logger.Printf("web-bff: handleFossaInviteAction upsert error: %v", upsertErr)
			}
			http.Error(w, msg, http.StatusBadGateway)
			return
		}
		if addErr := client.AddUserToTeamByEmail(invite.RemoteTeamID, email, teamAdminRoleID); addErr == nil {
			invite.Status = "accepted"
			invite.LastError = nil
			invite.LastCheckedAt = &now
			if _, upsertErr := s.store.UpsertServiceInvitation(invite); upsertErr != nil {
				http.Error(w, "failed to update invite", http.StatusInternalServerError)
				return
			}
			w.Header().Set(headerContentType, contentTypeJSON)
			if err := json.NewEncoder(w).Encode(map[string]string{"status": "added"}); err != nil {
				s.logger.Printf("web-bff: handleFossaInviteAction encode error: %v", err)
			}
			return
		} else {
			msg := addErr.Error()
			invite.Status = "error"
			invite.LastError = &msg
			invite.LastCheckedAt = &now
			if _, upsertErr := s.store.UpsertServiceInvitation(invite); upsertErr != nil {
				s.logger.Printf("web-bff: handleFossaInviteAction upsert error: %v", upsertErr)
			}
			http.Error(w, msg, http.StatusBadRequest)
			return
		}
	}

	msg := err.Error()
	invite.Status = "error"
	invite.LastError = &msg
	invite.LastCheckedAt = &now
	if _, upsertErr := s.store.UpsertServiceInvitation(invite); upsertErr != nil {
		s.logger.Printf("web-bff: handleFossaInviteAction upsert error: %v", upsertErr)
	}
	http.Error(w, msg, http.StatusBadRequest)
}

func (s *server) fossaClient() (*fossa.Client, error) {
	if strings.TrimSpace(s.fossaToken) == "" {
		return nil, fmt.Errorf("FOSSA_API_TOKEN not set")
	}
	return fossa.NewClient(s.fossaToken), nil
}

func (s *server) getFossaServiceID() (uint, error) {
	var service model.Service
	if err := s.store.DB().Where("name = ?", "FOSSA").First(&service).Error; err != nil {
		return 0, err
	}
	return service.ID, nil
}

func (s *server) ensureFossaTeam(project model.Project, client *fossa.Client) (*model.RemoteTeam, bool, error) {
	serviceID, err := s.getFossaServiceID()
	if err != nil {
		return nil, false, err
	}
	serviceTeam, err := s.store.GetRemoteTeamByProject(project.ID, serviceID)
	if err != nil {
		return nil, false, err
	}
	if serviceTeam != nil {
		return serviceTeam, false, nil
	}
	team, err := client.FetchTeam(project.Name)
	if err != nil {
		team, err = client.CreateTeam(project.Name)
		if err != nil {
			return nil, false, err
		}
		createdTeam, err := s.store.CreateRemoteTeam(project.ID, project.Name, serviceID, team.ID, team.Name)
		return createdTeam, true, err
	}
	createdTeam, err := s.store.CreateRemoteTeam(project.ID, project.Name, serviceID, team.ID, team.Name)
	return createdTeam, false, err
}

func classifyIneligibleMaintainers(maintainers []model.Maintainer) []fossaInviteIneligibleSummary {
	results := make([]fossaInviteIneligibleSummary, 0)
	for _, m := range maintainers {
		var reasons []string
		if m.MaintainerStatus != "" && m.MaintainerStatus != model.ActiveMaintainer {
			reasons = append(reasons, "Not active")
		}
		email := strings.TrimSpace(m.Email)
		github := strings.TrimSpace(m.GitHubAccount)
		if email == "" || strings.EqualFold(email, "EMAIL_MISSING") {
			reasons = append(reasons, "Missing email")
		}
		if github == "" || strings.EqualFold(github, "GITHUB_MISSING") {
			reasons = append(reasons, "Missing GitHub handle")
		}
		if len(reasons) == 0 {
			continue
		}
		name := strings.TrimSpace(m.Name)
		if name == "" {
			name = github
		}
		results = append(results, fossaInviteIneligibleSummary{
			ID:     m.ID,
			Name:   name,
			GitHub: github,
			Email:  email,
			Reason: strings.Join(reasons, ", "),
		})
	}
	return results
}

func buildFossaInviteCandidates(maintainers []model.Maintainer, teamEmails []string, pendingInviteEmails map[string]struct{}) []fossaInviteCandidateSummary {
	teamEmailSet := make(map[string]struct{}, len(teamEmails))
	for _, email := range teamEmails {
		normalized := strings.TrimSpace(email)
		if normalized == "" || strings.EqualFold(normalized, "EMAIL_MISSING") {
			continue
		}
		teamEmailSet[strings.ToLower(normalized)] = struct{}{}
	}
	if len(teamEmailSet) == 0 {
		log.Printf("web-bff: FOSSA team email set empty")
	} else {
		log.Printf("web-bff: FOSSA team email set size=%d", len(teamEmailSet))
	}
	results := make([]fossaInviteCandidateSummary, 0)
	for _, m := range maintainers {
		if m.MaintainerStatus != "" && m.MaintainerStatus != model.ActiveMaintainer {
			continue
		}
		email := strings.TrimSpace(m.Email)
		github := strings.TrimSpace(m.GitHubAccount)
		if email == "" || strings.EqualFold(email, "EMAIL_MISSING") {
			continue
		}
		if github == "" || strings.EqualFold(github, "GITHUB_MISSING") {
			continue
		}
		normalized := strings.ToLower(email)
		if _, ok := teamEmailSet[normalized]; ok {
			continue
		}
		if _, ok := pendingInviteEmails[normalized]; ok {
			continue
		}
		name := strings.TrimSpace(m.Name)
		if name == "" {
			name = github
		}
		results = append(results, fossaInviteCandidateSummary{
			ID:     m.ID,
			Name:   name,
			GitHub: github,
			Email:  email,
		})
	}
	return results
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

func (s *server) getCachedFossaTeamEmails(teamID uint) ([]string, bool) {
	const cacheTTL = 30 * time.Second
	s.fossaTeamCacheMu.RLock()
	cached, ok := s.fossaTeamCache[teamID]
	s.fossaTeamCacheMu.RUnlock()
	if !ok {
		return nil, false
	}
	if time.Since(cached.fetchedAt) > cacheTTL {
		return nil, false
	}
	return cached.emails, true
}

func (s *server) setCachedFossaTeamEmails(teamID uint, emails []string) {
	s.fossaTeamCacheMu.Lock()
	s.fossaTeamCache[teamID] = cachedFossaTeam{
		emails:    emails,
		fetchedAt: time.Now(),
	}
	s.fossaTeamCacheMu.Unlock()
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
	query := `repo:cncf/sandbox is:issue state:open "[PROJECT ONBOARDING]" in:title sort:created-desc`
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

func (s *server) dotProjectMaintainerPullRequestStatus(ctx context.Context, projectID uint, owner, repo, filePath, githubToken string) *dotProjectMaintainerPRStatus {
	owner = strings.TrimSpace(owner)
	repo = strings.TrimSpace(repo)
	filePath = normalizeGitHubPath(filePath)
	if owner == "" || repo == "" || filePath == "" {
		return nil
	}
	client := s.githubClientForToken(ctx, firstNonEmpty(githubToken, s.githubToken))
	if status := s.auditedDotProjectMaintainerPullRequestStatus(ctx, client, projectID, owner, repo, filePath); status != nil {
		return status
	}
	status, err := openGitHubMaintainerPullRequestStatus(ctx, client, owner, repo, filePath)
	if err != nil {
		s.logger.Printf("web-bff: dot-project open maintainer PR lookup failed project=%d owner=%s repo=%s path=%s err=%v", projectID, owner, repo, filePath, err)
		return &dotProjectMaintainerPRStatus{
			Status:  "unknown",
			Warning: "Unable to check GitHub for open maintainer-file pull requests.",
		}
	}
	return status
}

func (s *server) auditedDotProjectMaintainerPullRequestStatus(ctx context.Context, client *github.Client, projectID uint, owner, repo, filePath string) *dotProjectMaintainerPRStatus {
	var audits []model.AuditLog
	if err := s.store.DB().
		Where("project_id = ? AND action = ?", projectID, "DOT_PROJECT_MAINTAINER_PR_CREATE").
		Order("created_at DESC").
		Limit(20).
		Find(&audits).Error; err != nil {
		s.logger.Printf("web-bff: dot-project PR audit lookup failed project=%d err=%v", projectID, err)
		return nil
	}
	for _, audit := range audits {
		var metadata dotProjectPRAuditMetadata
		if err := json.Unmarshal([]byte(audit.Metadata), &metadata); err != nil {
			continue
		}
		if !sameGitHubRepo(metadata.Repository.Owner, metadata.Repository.Repo, owner, repo) || !sameGitHubPath(metadata.Repository.Path, filePath) || metadata.PullRequest.Number <= 0 {
			continue
		}
		pr, _, err := client.PullRequests.Get(ctx, owner, repo, metadata.PullRequest.Number)
		if err != nil {
			s.logger.Printf("web-bff: dot-project audited PR lookup failed project=%d owner=%s repo=%s number=%d err=%v", projectID, owner, repo, metadata.PullRequest.Number, err)
			continue
		}
		if pr.GetState() == "open" {
			return dotProjectMaintainerStatusFromPullRequest(pr, "maintainer-d")
		}
	}
	return nil
}

func openGitHubMaintainerPullRequestStatus(ctx context.Context, client *github.Client, owner, repo, filePath string) (*dotProjectMaintainerPRStatus, error) {
	pulls, _, err := client.PullRequests.List(ctx, owner, repo, &github.PullRequestListOptions{
		State:     "open",
		Sort:      "updated",
		Direction: "desc",
		ListOptions: github.ListOptions{
			PerPage: 20,
		},
	})
	if err != nil {
		return nil, err
	}
	for _, pr := range pulls {
		files, _, err := client.PullRequests.ListFiles(ctx, owner, repo, pr.GetNumber(), &github.ListOptions{PerPage: 100})
		if err != nil {
			return nil, err
		}
		for _, file := range files {
			if sameGitHubPath(file.GetFilename(), filePath) {
				return dotProjectMaintainerStatusFromPullRequest(pr, "github"), nil
			}
		}
	}
	return nil, nil
}

func dotProjectMaintainerStatusFromPullRequest(pr *github.PullRequest, source string) *dotProjectMaintainerPRStatus {
	if pr == nil {
		return nil
	}
	createdAt := pr.GetCreatedAt().Time
	updatedAt := pr.GetUpdatedAt().Time
	return &dotProjectMaintainerPRStatus{
		Status:    pr.GetState(),
		URL:       pr.GetHTMLURL(),
		Number:    pr.GetNumber(),
		Title:     pr.GetTitle(),
		Author:    pr.GetUser().GetLogin(),
		Source:    source,
		CreatedAt: &createdAt,
		UpdatedAt: &updatedAt,
	}
}

func (s *server) githubClientForToken(ctx context.Context, token string) *github.Client {
	if s.githubClient != nil {
		return s.githubClient(ctx, token)
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return github.NewClient(nil)
	}
	return github.NewClient(oauth2.NewClient(ctx, oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})))
}

func sameGitHubRepo(leftOwner, leftRepo, rightOwner, rightRepo string) bool {
	return strings.EqualFold(strings.TrimSpace(leftOwner), strings.TrimSpace(rightOwner)) && strings.EqualFold(strings.TrimSpace(leftRepo), strings.TrimSpace(rightRepo))
}

func sameGitHubPath(left, right string) bool {
	return strings.EqualFold(normalizeGitHubPath(left), normalizeGitHubPath(right))
}

func normalizeGitHubPath(value string) string {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\\", "/"))
	if value == "" {
		return ""
	}
	cleaned := pathpkg.Clean(value)
	if cleaned == "." || cleaned == "/" {
		return ""
	}
	return strings.TrimPrefix(cleaned, "/")
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

func (s *server) createDotProjectMaintainerPullRequest(ctx context.Context, input dotProjectPullRequestInput, githubToken string) (*dotProjectPullRequestResponse, error) {
	githubToken = strings.TrimSpace(githubToken)
	if githubToken == "" {
		return nil, fmt.Errorf("github user credentials are not available")
	}
	client := github.NewClient(oauth2.NewClient(ctx, oauth2.StaticTokenSource(&oauth2.Token{
		AccessToken: githubToken,
	})))
	forkOwner := strings.TrimSpace(input.ForkOwner)
	if forkOwner == "" {
		forkOwner = input.SubmittedByLogin
	}
	forkRepo := strings.TrimSpace(input.ForkRepo)
	if forkRepo == "" {
		forkRepo = dotProjectForkRepoName(input.ProjectName, input.Repo)
	}

	baseRef, _, err := client.Git.GetRef(ctx, input.Owner, input.Repo, "heads/"+input.BaseBranch)
	if err != nil {
		return nil, fmt.Errorf("get base ref: %w", err)
	}
	if baseRef == nil || baseRef.Object == nil || baseRef.Object.SHA == nil || strings.TrimSpace(*baseRef.Object.SHA) == "" {
		return nil, fmt.Errorf("base ref %q did not include a commit sha", input.BaseBranch)
	}

	content, _, _, err := client.Repositories.GetContents(ctx, input.Owner, input.Repo, input.FilePath, &github.RepositoryContentGetOptions{
		Ref: input.BaseBranch,
	})
	if err != nil {
		return nil, fmt.Errorf("get maintainer file content: %w", err)
	}
	if content == nil || strings.TrimSpace(content.GetSHA()) == "" {
		return nil, fmt.Errorf("maintainer file %q did not include a content sha", input.FilePath)
	}
	currentContent, err := content.GetContent()
	if err != nil {
		return nil, fmt.Errorf("decode current maintainer file content: %w", err)
	}
	if currentContent != input.Original {
		return nil, fmt.Errorf("cached maintainer file is stale; run dot-project sync before submitting a pull request")
	}

	fork, err := ensureDotProjectFork(ctx, client, input.Owner, input.Repo, forkOwner, forkRepo)
	if err != nil {
		return nil, err
	}
	if fork.GetOwner().GetLogin() != "" {
		forkOwner = fork.GetOwner().GetLogin()
	}
	if fork.GetName() != "" {
		forkRepo = fork.GetName()
	}

	if _, _, err := client.Git.CreateRef(ctx, forkOwner, forkRepo, &github.Reference{
		Ref: github.String("refs/heads/" + input.HeadBranch),
		Object: &github.GitObject{
			SHA: baseRef.Object.SHA,
		},
	}); err != nil {
		return nil, fmt.Errorf("create branch in fork %s/%s: %w", forkOwner, forkRepo, err)
	}

	commitAuthor, err := dotProjectCommitAuthor(ctx, client, input)
	if err != nil {
		return nil, err
	}
	contentResponse, _, err := client.Repositories.UpdateFile(ctx, forkOwner, forkRepo, input.FilePath, &github.RepositoryContentFileOptions{
		Message:   github.String(dotProjectMaintainerCommitMessage(input.ProjectName, commitAuthor)),
		Content:   []byte(input.Proposed),
		SHA:       github.String(content.GetSHA()),
		Branch:    github.String(input.HeadBranch),
		Author:    commitAuthor,
		Committer: commitAuthor,
	})
	if err != nil {
		return nil, fmt.Errorf("update maintainer file in fork %s/%s: %w", forkOwner, forkRepo, err)
	}

	pr, _, err := client.PullRequests.Create(ctx, input.Owner, input.Repo, &github.NewPullRequest{
		Title:               github.String(fmt.Sprintf("Update %s maintainers.yaml", input.ProjectName)),
		Head:                github.String(forkOwner + ":" + input.HeadBranch),
		Base:                github.String(input.BaseBranch),
		Body:                github.String(buildDotProjectPullRequestBody(input)),
		MaintainerCanModify: github.Bool(true),
	})
	if err != nil {
		return nil, fmt.Errorf("create pull request: %w", err)
	}

	commitSHA := ""
	if contentResponse != nil {
		commitSHA = contentResponse.GetSHA()
	}
	return &dotProjectPullRequestResponse{
		URL:                 pr.GetHTMLURL(),
		Number:              pr.GetNumber(),
		Branch:              input.HeadBranch,
		BaseBranch:          input.BaseBranch,
		ForkOwner:           forkOwner,
		ForkRepo:            forkRepo,
		FilePath:            input.FilePath,
		CommitSHA:           commitSHA,
		AddedHandles:        input.AddedHandles,
		RemovedPlaceholders: input.RemovedPlaceholders,
		SubmittedBy:         input.SubmittedByName,
	}, nil
}

func ensureDotProjectFork(ctx context.Context, client *github.Client, owner, repo, forkOwner, forkRepo string) (*github.Repository, error) {
	if forkOwner == "" {
		return nil, fmt.Errorf("fork owner is required")
	}
	if forkRepo == "" {
		return nil, fmt.Errorf("fork repo name is required")
	}
	if existing, _, err := client.Repositories.Get(ctx, forkOwner, forkRepo); err == nil {
		if !repositoryIsForkOf(existing, owner, repo) {
			return nil, fmt.Errorf("github repository %s/%s already exists but is not a fork of %s/%s", forkOwner, forkRepo, owner, repo)
		}
		return existing, nil
	} else if !isGitHubNotFound(err) {
		return nil, fmt.Errorf("check fork %s/%s: %w", forkOwner, forkRepo, err)
	}

	fork, _, err := client.Repositories.CreateFork(ctx, owner, repo, &github.RepositoryCreateForkOptions{
		Name:              forkRepo,
		DefaultBranchOnly: true,
	})
	if err != nil {
		var accepted *github.AcceptedError
		if !errors.As(err, &accepted) {
			return nil, fmt.Errorf("create fork %s/%s from %s/%s: %w", forkOwner, forkRepo, owner, repo, err)
		}
	}
	if fork != nil && fork.GetName() != "" {
		forkRepo = fork.GetName()
	}

	readyFork, err := waitForDotProjectFork(ctx, client, forkOwner, forkRepo, owner, repo)
	if err != nil {
		return nil, err
	}
	return readyFork, nil
}

func waitForDotProjectFork(ctx context.Context, client *github.Client, forkOwner, forkRepo, upstreamOwner, upstreamRepo string) (*github.Repository, error) {
	var lastErr error
	for attempt := 0; attempt < 10; attempt++ {
		fork, _, err := client.Repositories.Get(ctx, forkOwner, forkRepo)
		if err == nil {
			if repositoryIsForkOf(fork, upstreamOwner, upstreamRepo) {
				return fork, nil
			}
			lastErr = fmt.Errorf("github repository %s/%s exists but is not a fork of %s/%s", forkOwner, forkRepo, upstreamOwner, upstreamRepo)
		} else if !isGitHubNotFound(err) {
			lastErr = err
		}

		timer := time.NewTimer(time.Duration(attempt+1) * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	if lastErr != nil {
		return nil, fmt.Errorf("wait for fork %s/%s: %w", forkOwner, forkRepo, lastErr)
	}
	return nil, fmt.Errorf("fork %s/%s was not ready before timeout", forkOwner, forkRepo)
}

func dotProjectCommitAuthor(ctx context.Context, client *github.Client, input dotProjectPullRequestInput) (*github.CommitAuthor, error) {
	user, _, err := client.Users.Get(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("load github user for commit sign-off: %w", err)
	}
	name := strings.TrimSpace(user.GetName())
	if name == "" {
		name = strings.TrimSpace(input.SubmittedByName)
	}
	if name == "" {
		name = strings.TrimSpace(input.SubmittedByLogin)
	}
	email := strings.TrimSpace(user.GetEmail())
	if email == "" && user.GetID() > 0 && strings.TrimSpace(user.GetLogin()) != "" {
		email = fmt.Sprintf("%d+%s@users.noreply.github.com", user.GetID(), strings.TrimSpace(user.GetLogin()))
	}
	if email == "" && strings.TrimSpace(input.SubmittedByLogin) != "" {
		email = strings.TrimSpace(input.SubmittedByLogin) + "@users.noreply.github.com"
	}
	if name == "" || email == "" {
		return nil, fmt.Errorf("github user name and email are required for commit sign-off")
	}
	return &github.CommitAuthor{
		Name:  github.String(name),
		Email: github.String(email),
	}, nil
}

func dotProjectMaintainerCommitMessage(projectName string, author *github.CommitAuthor) string {
	name := ""
	email := ""
	if author != nil {
		name = strings.TrimSpace(author.GetName())
		email = strings.TrimSpace(author.GetEmail())
	}
	return fmt.Sprintf("Update %s maintainers\n\nSigned-off-by: %s <%s>", strings.TrimSpace(projectName), name, email)
}

func repositoryIsForkOf(repo *github.Repository, upstreamOwner, upstreamRepo string) bool {
	if repo == nil || !repo.GetFork() || repo.Parent == nil {
		return false
	}
	return strings.EqualFold(repo.Parent.GetOwner().GetLogin(), upstreamOwner) && strings.EqualFold(repo.Parent.GetName(), upstreamRepo)
}

func isGitHubNotFound(err error) bool {
	var githubErr *github.ErrorResponse
	return errors.As(err, &githubErr) && githubErr.Response != nil && githubErr.Response.StatusCode == http.StatusNotFound
}

func dotProjectForkRepoName(projectName, sourceRepo string) string {
	prefix := slugForGitHubRepoName(projectName)
	if prefix == "" {
		prefix = "project"
	}
	sourceRepo = strings.TrimSpace(sourceRepo)
	if sourceRepo == "" {
		sourceRepo = ".project"
	}
	name := prefix + sourceRepo
	if len(name) <= 100 {
		return name
	}
	suffix := "-" + sourceRepo
	maxPrefix := 100 - len(suffix)
	if maxPrefix < 1 {
		return name[:100]
	}
	return strings.Trim(name[:maxPrefix], "-.") + suffix
}

func slugForGitHubRepoName(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var builder strings.Builder
	lastDash := false
	for _, char := range value {
		allowed := (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || char == '.' || char == '_' || char == '-'
		if allowed {
			builder.WriteRune(char)
			lastDash = false
			continue
		}
		if !lastDash {
			builder.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(builder.String(), "-.")
}

func buildDotProjectPullRequestBody(input dotProjectPullRequestInput) string {
	lines := []string{
		"Maintainer-D generated this pull request from the current project maintainer database.",
		"",
		"Changes:",
	}
	if len(input.AddedHandles) > 0 {
		lines = append(lines, fmt.Sprintf("- Add active maintainer-d handles missing from `%s`: %s", input.FilePath, strings.Join(dotProjectMentionedMaintainers(input.AddedHandles), ", ")))
	}
	if len(input.RemovedPlaceholders) > 0 {
		lines = append(lines, fmt.Sprintf("- Remove placeholder maintainer lines: %s", strings.Join(input.RemovedPlaceholders, ", ")))
	}
	lines = append(lines,
		"",
		fmt.Sprintf("Submitted by: %s (%s)", input.SubmittedByName, input.SubmittedByLogin),
	)
	if input.OriginalBodyHash != "" {
		lines = append(lines, fmt.Sprintf("Cached source body hash: `%s`", input.OriginalBodyHash))
	}
	return strings.Join(lines, "\n")
}

func dotProjectMentionedMaintainers(handles []string) []string {
	mentionHandles := dotProjectMaintainerMentionHandles(handles)
	mentions := make([]string, 0, len(mentionHandles))
	for _, handle := range mentionHandles {
		mentions = append(mentions, "@"+handle)
	}
	return mentions
}

func dotProjectMaintainerMentionHandles(handles []string) []string {
	mentionHandles := make([]string, 0, len(handles))
	seen := make(map[string]struct{}, len(handles))
	for _, raw := range handles {
		handle := dotproject.NormalizeGitHubHandle(raw)
		if handle == "" {
			continue
		}
		if _, ok := seen[handle]; ok {
			continue
		}
		seen[handle] = struct{}{}
		mentionHandles = append(mentionHandles, handle)
	}
	return mentionHandles
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

func parseMaintainerServiceActionPath(path string) (uint, string, string, bool) {
	trimmed := strings.Trim(strings.TrimPrefix(path, "/api/maintainers/"), "/")
	parts := strings.Split(trimmed, "/")
	if len(parts) != 4 || parts[1] != "services" {
		return 0, "", "", false
	}
	id, err := strconv.Atoi(parts[0])
	if err != nil || id <= 0 {
		return 0, "", "", false
	}
	return uint(id), parts[2], parts[3], true
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

func parseProjectSubresourceID(path, suffix string) (uint, error) {
	if !strings.HasSuffix(path, suffix) {
		return 0, fmt.Errorf("missing suffix")
	}
	base := strings.TrimSuffix(path, suffix)
	return parseIDParam(base, "/api/projects/")
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
		summary := maintainerSummary{
			ID:     maintainer.ID,
			Name:   name,
			GitHub: github,
		}
		if maintainer.Country != nil {
			summary.Country = *maintainer.Country
		}
		if maintainer.Location != nil {
			summary.Location = *maintainer.Location
		}
		if maintainer.Timezone != nil {
			summary.Timezone = *maintainer.Timezone
		}
		result = append(result, summary)
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
	// #nosec G704 -- URL is validated and allowlisted in rewriteMaintainerRefURL.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rewritten, nil)
	if err != nil {
		return "", err
	}
	client := &http.Client{
		Timeout: 5 * time.Second,
	}
	// #nosec G704 -- URL is validated and allowlisted in rewriteMaintainerRefURL.
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
	if !strings.EqualFold(parsed.Scheme, "https") {
		return "", fmt.Errorf("invalid maintainer ref url")
	}
	if !strings.EqualFold(parsed.Host, "raw.githubusercontent.com") {
		return "", fmt.Errorf("invalid maintainer ref url")
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
			// #nosec G706 -- safeLogf sanitizes control characters before logging.
			log.Print(safeLogf("maintainer ref parse error (maintainer=%d): %v", maintainer.ID, err))
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

func safeLogf(format string, args ...any) string {
	sanitized := make([]any, 0, len(args))
	for _, arg := range args {
		sanitized = append(sanitized, sanitizeLogValue(arg))
	}
	return fmt.Sprintf(format, sanitized...)
}

func sanitizeLogValue(v any) string {
	switch t := v.(type) {
	case string:
		return sanitizeLogString(t)
	case []byte:
		return sanitizeLogString(string(t))
	case error:
		return sanitizeLogString(t.Error())
	default:
		return sanitizeLogString(fmt.Sprint(t))
	}
}

func sanitizeLogString(s string) string {
	if s == "" {
		return s
	}
	replaced := strings.NewReplacer(
		"\n", " ",
		"\r", " ",
		"\t", " ",
	)
	return replaced.Replace(s)
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
