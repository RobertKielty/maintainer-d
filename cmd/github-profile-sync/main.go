package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"sort"
	"strings"
	"time"

	"maintainerd/db"
	"maintainerd/dotproject"
	"maintainerd/geo"
	"maintainerd/model"

	"github.com/google/go-github/v55/github"
	"go.uber.org/zap"
	"golang.org/x/oauth2"
	"gorm.io/gorm"
)

const missingPlaceholder = "GITHUB_MISSING"

type syncConfig struct {
	Pause        time.Duration
	MaxErrorRate float64
}

type syncSummary struct {
	Attempted           int
	Updated             int
	Cleared             int
	Unchanged           int
	Errored             int
	GitHubErrorCount    int
	RateLimitErrorCount int
	NotFoundCount       int
}

func main() {
	ctx := context.Background()
	cfg := parseFlags()

	dbDriver := envOr("MD_DB_DRIVER", "postgres")
	if dbDriver != "postgres" {
		log.Fatalf("github-profile-sync requires MD_DB_DRIVER=postgres, got %q", dbDriver)
	}
	dbDSN := envOr("MD_DB_DSN", "")
	if dbDSN == "" {
		log.Fatal("MD_DB_DSN is required when MD_DB_DRIVER=postgres")
	}

	githubToken := strings.TrimSpace(os.Getenv("GITHUB_API_TOKEN"))
	if githubToken == "" {
		log.Fatal("GITHUB_API_TOKEN is required")
	}

	dbConn, err := db.OpenGorm(dbDriver, dbDSN, &gorm.Config{})
	if err != nil {
		log.Fatalf("failed to open DB: %v", err)
	}
	store := db.NewSQLStore(dbConn)
	logger := zap.NewNop().Sugar()

	if err := store.LogAuditEvent(logger, buildStartAuditEvent(cfg)); err != nil {
		log.Printf("github-profile-sync start audit log failed: %v", err)
	}

	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: githubToken})
	tc := oauth2.NewClient(ctx, ts)
	client := github.NewClient(tc)

	var maintainers []model.Maintainer
	if err := dbConn.
		Where("git_hub_account != ? AND git_hub_account != '' AND deleted_at IS NULL", missingPlaceholder).
		Find(&maintainers).Error; err != nil {
		log.Fatalf("failed to load maintainers: %v", err)
	}

	log.Printf("github-profile-sync: syncing %d maintainers", len(maintainers))
	summary := syncSummary{}

	for _, m := range maintainers {
		summary.Attempted++
		ghUser, _, err := client.Users.Get(ctx, m.GitHubAccount)
		if err != nil {
			recordFetchError(&summary, err)
			log.Printf("github-profile-sync: WARN failed to fetch %q: %v", m.GitHubAccount, err)
			pause := cfg.Pause
			if dotproject.IsGitHubRateLimitError(err) {
				pause = dotproject.GitHubRateLimitWait(err, cfg.Pause)
			}
			time.Sleep(pause)
			continue
		}

		rawLocation := ghUser.GetLocation()
		previous := geo.ResolvedLocation{Location: m.Location, Country: m.Country, Timezone: m.Timezone}
		resolved := geo.ResolveLocation(rawLocation, previous)
		resolvedName := resolveMaintainerName(m.Name, ghUser.GetName())

		changes := diffMaintainerProfile(m.Name, resolvedName, previous, resolved)
		if len(changes) == 0 {
			summary.Unchanged++
			time.Sleep(cfg.Pause)
			continue
		}
		updates := map[string]any{
			"location": stringPtrToUpdateValue(resolved.Location),
			"country":  stringPtrToUpdateValue(resolved.Country),
			"timezone": stringPtrToUpdateValue(resolved.Timezone),
		}
		if _, nameChanged := changes["name"]; nameChanged {
			updates["name"] = resolvedName
		}
		if err := dbConn.Model(&model.Maintainer{}).
			Where("id = ?", m.ID).
			Updates(updates).Error; err != nil {
			log.Printf("github-profile-sync: WARN failed to update maintainer id=%d: %v", m.ID, err)
			summary.Errored++
			time.Sleep(cfg.Pause)
			continue
		}

		// Only count the outcome once the write has actually succeeded -- counting it
		// beforehand double-counts a failed write as both updated/cleared and errored,
		// which overstates the run log and finish-audit totals.
		_, locationChanged := changes["location"]
		if locationChanged && resolved.Location == nil {
			summary.Cleared++
		} else {
			summary.Updated++
		}

		if err := store.LogAuditEvent(logger, buildUpdateAuditEvent(m, changes)); err != nil {
			log.Printf("github-profile-sync: WARN failed to write audit log for maintainer id=%d: %v", m.ID, err)
		}

		time.Sleep(cfg.Pause)
	}

	errRate := errorRate(summary)
	log.Printf(
		"github-profile-sync: done attempted=%d updated=%d cleared=%d unchanged=%d errored=%d github_errors=%d rate_limit_errors=%d not_found=%d error_rate=%.3f",
		summary.Attempted, summary.Updated, summary.Cleared, summary.Unchanged, summary.Errored,
		summary.GitHubErrorCount, summary.RateLimitErrorCount, summary.NotFoundCount, errRate,
	)

	exceeded := shouldFailRun(summary, cfg)
	if err := store.LogAuditEvent(logger, buildFinishAuditEvent(summary, cfg, errRate, exceeded)); err != nil {
		log.Printf("github-profile-sync finish audit log failed: %v", err)
	}

	if exceeded {
		log.Fatalf("github-profile-sync: error rate %.3f exceeds -max-error-rate %.3f, failing run", errRate, cfg.MaxErrorRate)
	}
}

// resolveMaintainerName treats a non-empty GitHub profile name as the current
// display name. GitHub does not require users to set a name, so an empty
// profile field preserves the existing maintainer name rather than erasing it.
func resolveMaintainerName(existing, githubName string) string {
	if name := strings.TrimSpace(githubName); name != "" {
		return name
	}
	return existing
}

// recordFetchError classifies a GitHub API error into the appropriate summary counter.
// 404s (deleted/renamed accounts) are counted separately from GitHubErrorCount/
// RateLimitErrorCount so they don't inflate the error-rate threshold — they're expected
// attrition across thousands of maintainers, not an operational fault.
func recordFetchError(summary *syncSummary, err error) {
	summary.Errored++
	switch {
	case dotproject.IsGitHubNotFoundError(err):
		summary.NotFoundCount++
	case dotproject.IsGitHubRateLimitError(err):
		summary.RateLimitErrorCount++
		summary.GitHubErrorCount++
	case dotproject.IsGitHubAPIError(err):
		summary.GitHubErrorCount++
	}
}

// errorRate is the fraction of fetches -- excluding not-found accounts from both the numerator
// and the denominator -- that failed. A credible run's failures should be auth/rate-limit/5xx
// errors, not a mix diluted by expected 404 attrition: with 404s only removed from the
// numerator, e.g. 6 not-found accounts plus 4 auth failures out of 10 attempts would report
// 0.4 and pass the default threshold even though every existing account failed.
func errorRate(summary syncSummary) float64 {
	denominator := summary.Attempted - summary.NotFoundCount
	if denominator <= 0 {
		return 0
	}
	credibleErrors := max(summary.Errored-summary.NotFoundCount, 0)
	return float64(credibleErrors) / float64(denominator)
}

// shouldFailRun reports whether the run's credible error rate exceeds the configured threshold,
// making a genuine operational failure (auth, rate-limit, 5xx) visible as a non-zero exit
// instead of a silently "successful" Kubernetes Job.
func shouldFailRun(summary syncSummary, cfg syncConfig) bool {
	return summary.Attempted > 0 && errorRate(summary) > cfg.MaxErrorRate
}

func parseFlags() syncConfig {
	cfg := syncConfig{}
	flag.DurationVar(&cfg.Pause, "pause", 750*time.Millisecond, "minimum pause between GitHub API requests")
	flag.Float64Var(&cfg.MaxErrorRate, "max-error-rate", 0.5, "fail the run if the fraction of non-404 GitHub errors exceeds this")
	flag.Parse()

	if err := validateSyncConfig(cfg); err != nil {
		log.Fatalf("invalid flags: %v", err)
	}

	return cfg
}

// validateSyncConfig rejects nonsensical -pause/-max-error-rate combinations before the run
// starts: a negative pause silently disables throttling (time.Sleep returns immediately), and a
// max-error-rate outside [0, 1] (or NaN) can make errorRate's threshold check never trip,
// letting a run that should fail exit 0 instead.
func validateSyncConfig(cfg syncConfig) error {
	if cfg.Pause < 0 {
		return fmt.Errorf("-pause must not be negative, got %s", cfg.Pause)
	}
	if math.IsNaN(cfg.MaxErrorRate) || cfg.MaxErrorRate < 0 || cfg.MaxErrorRate > 1 {
		return fmt.Errorf("-max-error-rate must be between 0 and 1, got %v", cfg.MaxErrorRate)
	}
	return nil
}

func buildStartAuditEvent(cfg syncConfig) model.AuditLog {
	metadata := map[string]any{
		"pause_ms":       cfg.Pause.Milliseconds(),
		"max_error_rate": cfg.MaxErrorRate,
	}
	body, err := json.Marshal(metadata)
	if err != nil {
		body = []byte("{}")
	}
	return model.AuditLog{
		Action:   "GITHUB_PROFILE_SYNC_RUN_STARTED",
		Message:  fmt.Sprintf("GITHUB_PROFILE_SYNC_RUN_STARTED: pause=%s max_error_rate=%.3f", cfg.Pause, cfg.MaxErrorRate),
		Metadata: string(body),
	}
}

func buildFinishAuditEvent(summary syncSummary, cfg syncConfig, errRate float64, exceeded bool) model.AuditLog {
	metadata := map[string]any{
		"attempted":              summary.Attempted,
		"updated":                summary.Updated,
		"cleared":                summary.Cleared,
		"unchanged":              summary.Unchanged,
		"errored":                summary.Errored,
		"github_error_count":     summary.GitHubErrorCount,
		"rate_limit_error_count": summary.RateLimitErrorCount,
		"not_found_count":        summary.NotFoundCount,
		"error_rate":             errRate,
		"max_error_rate":         cfg.MaxErrorRate,
		"error_rate_exceeded":    exceeded,
	}
	body, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		body = []byte("{}")
	}
	message := fmt.Sprintf(
		"GITHUB_PROFILE_SYNC_RUN: attempted=%d updated=%d cleared=%d unchanged=%d errored=%d github_errors=%d rate_limit_errors=%d not_found=%d error_rate=%.3f",
		summary.Attempted, summary.Updated, summary.Cleared, summary.Unchanged, summary.Errored,
		summary.GitHubErrorCount, summary.RateLimitErrorCount, summary.NotFoundCount, errRate,
	)
	return model.AuditLog{
		Action:   "GITHUB_PROFILE_SYNC_RUN_FINISHED",
		Message:  message,
		Metadata: string(body),
	}
}

// diffResolvedLocation returns a {field: {from, to}} changes map for every field that differs
// between previous and resolved, using the display-value placeholder for nil fields. An empty
// map means nothing changed and no DB write or audit event should happen.
func diffResolvedLocation(previous, resolved geo.ResolvedLocation) map[string]map[string]string {
	changes := map[string]map[string]string{}
	addIfChanged := func(field string, from, to *string) {
		if stringPtrEqual(from, to) {
			return
		}
		changes[field] = map[string]string{
			"from": displayValue(field, from),
			"to":   displayValue(field, to),
		}
	}
	addIfChanged("location", previous.Location, resolved.Location)
	addIfChanged("country", previous.Country, resolved.Country)
	addIfChanged("timezone", previous.Timezone, resolved.Timezone)
	return changes
}

// diffMaintainerProfile includes the GitHub display name alongside the
// location fields managed by this sync.
func diffMaintainerProfile(previousName, resolvedName string, previous, resolved geo.ResolvedLocation) map[string]map[string]string {
	changes := diffResolvedLocation(previous, resolved)
	if previousName != resolvedName {
		from := strings.TrimSpace(previousName)
		if from == "" {
			from = "NAME_MISSING"
		}
		changes["name"] = map[string]string{"from": from, "to": resolvedName}
	}
	return changes
}

func stringPtrEqual(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func displayValue(field string, v *string) string {
	if v != nil {
		return *v
	}
	return strings.ToUpper(field) + "_MISSING"
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

// buildUpdateAuditEvent records a per-maintainer name/location/country/timezone change so staff
// unfamiliar with a maintainer can see, at a glance, what this sync run changed on their record.
func buildUpdateAuditEvent(m model.Maintainer, changes map[string]map[string]string) model.AuditLog {
	fields := make([]string, 0, len(changes))
	for field := range changes {
		fields = append(fields, field)
	}
	sort.Strings(fields)
	metadata := map[string]any{
		"actor":   map[string]string{"login": "github-profile-sync"},
		"changes": changes,
	}
	body, err := json.Marshal(metadata)
	if err != nil {
		body = []byte("{}")
	}
	message := fmt.Sprintf("GITHUB_PROFILE_SYNC_UPDATE: updated %s for %s", strings.Join(fields, ", "), m.GitHubAccount)
	return model.AuditLog{
		MaintainerID: &m.ID,
		Action:       "GITHUB_PROFILE_SYNC_UPDATE",
		Message:      message,
		Metadata:     string(body),
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
