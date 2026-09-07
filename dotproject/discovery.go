package dotproject

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"maintainerd/model"

	"github.com/google/go-github/v55/github"
	"gopkg.in/yaml.v3"
)

const (
	DefaultRepoName        = ".project"
	ImporterVersion        = "dot-project-discovery/v1"
	SupportedSchemaVersion = "1.0.0"
	maintainersFileName    = "maintainers.yaml"
)

var (
	ErrDotProjectRepoNotFound = errors.New("dot-project repo not found")
	ErrDotProjectFileNotFound = errors.New("dot-project file not found")
)

type ParseStatus string

const (
	ParseStatusMissing           ParseStatus = "missing"
	ParseStatusParsed            ParseStatus = "parsed"
	ParseStatusUnsupportedSchema ParseStatus = "unsupported_schema"
	ParseStatusInvalidYAML       ParseStatus = "invalid_yaml"
	ParseStatusInvalidShape      ParseStatus = "invalid_shape"
)

type RepositoryClient interface {
	GetDefaultBranch(ctx context.Context, owner, repo string) (string, error)
	GetFile(ctx context.Context, owner, repo, ref, path string) (*FetchedFile, error)
	ListFiles(ctx context.Context, owner, repo, ref, path string) ([]ListedFile, error)
	// GetCommitSHA resolves ref (a branch name) to the commit SHA it points
	// at, so a permalink built from a file fetched at ref can be pinned to
	// a specific commit rather than drifting with the branch.
	GetCommitSHA(ctx context.Context, owner, repo, ref string) (string, error)
}

type FetchedFile struct {
	Path    string
	BlobURL string
	RawURL  string
	ETag    string
	Body    string
}

type ListedFile struct {
	Path string
}

type FileDiscovery struct {
	Path     string
	Exists   bool
	BlobURL  string
	RawURL   string
	ETag     string
	BodyHash string
	Body     string
	// CommitSHA is the commit the default branch pointed at when this file
	// was fetched - the file's blob SHA is not usable here, since a
	// permalink needs the commit, not the blob hash.
	CommitSHA string
}

type DiscoveryResult struct {
	Org      string
	RepoName string

	RepoExists    bool
	RepoRef       string
	DefaultBranch string

	ProjectFile      FileDiscovery
	MaintainersFile  FileDiscovery
	SecurityFile     FileDiscovery
	ContributingFile FileDiscovery
	GovernanceFile   FileDiscovery

	MaintainersFilename    string
	SchemaVersion          string
	SchemaSupported        bool
	MaintainerCount        *uint
	ProjectParseStatus     ParseStatus
	MaintainersParseStatus ParseStatus
	ProjectParseError      string
	MaintainersParseError  string
}

type Discoverer struct {
	Client RepositoryClient
}

func (d *Discoverer) Discover(ctx context.Context, project model.Project) (*DiscoveryResult, error) {
	if d == nil || d.Client == nil {
		return nil, errors.New("dot-project discoverer client is required")
	}

	org := strings.TrimSpace(project.GitHubOrg)
	if org == "" {
		return nil, errors.New("project github org is required")
	}

	result := &DiscoveryResult{
		Org:                    org,
		RepoName:               DefaultRepoName,
		RepoRef:                repoRef(org),
		ProjectParseStatus:     ParseStatusMissing,
		MaintainersParseStatus: ParseStatusMissing,
	}

	defaultBranch, err := d.Client.GetDefaultBranch(ctx, org, DefaultRepoName)
	if err != nil {
		if errors.Is(err, ErrDotProjectRepoNotFound) {
			return result, nil
		}
		return nil, fmt.Errorf("get default branch for %s/%s: %w", org, DefaultRepoName, err)
	}

	result.RepoExists = true
	result.DefaultBranch = defaultBranch

	commitSHA, err := d.Client.GetCommitSHA(ctx, org, DefaultRepoName, defaultBranch)
	if err != nil && !errors.Is(err, ErrDotProjectRepoNotFound) {
		return nil, fmt.Errorf("get commit sha for %s/%s@%s: %w", org, DefaultRepoName, defaultBranch, err)
	}

	// Every read below must come from the one commit the whole discovery is
	// attributed to: fetching at the moving branch name instead would let a
	// push between calls hand back bodies and line numbers from a different
	// commit than the recorded CommitSHA and provenance.
	fetchRef := defaultBranch
	if strings.TrimSpace(commitSHA) != "" {
		fetchRef = commitSHA
	}

	projectFile, err := d.fetchOptionalFile(ctx, org, fetchRef, "project.yaml", commitSHA)
	if err != nil {
		return nil, err
	}
	result.ProjectFile = projectFile
	if result.ProjectFile.Exists {
		result.SchemaVersion, result.SchemaSupported, result.ProjectParseStatus, result.ProjectParseError =
			parseProjectYAML(result.ProjectFile)
	}

	maintainersFile, err := d.fetchMaintainersFile(ctx, org, fetchRef, commitSHA)
	if err != nil {
		return nil, err
	}
	result.MaintainersFile = maintainersFile
	if result.MaintainersFile.Exists {
		result.MaintainersFilename = result.MaintainersFile.Path
		count, status, parseErr := parseMaintainersYAML(result.MaintainersFile)
		result.MaintainerCount = count
		result.MaintainersParseStatus = status
		result.MaintainersParseError = parseErr
	}

	securityFile, err := d.fetchOptionalFile(ctx, org, fetchRef, "SECURITY.md", commitSHA)
	if err != nil {
		return nil, err
	}
	result.SecurityFile = securityFile

	contributingFile, err := d.fetchOptionalFile(ctx, org, fetchRef, "CONTRIBUTING.md", commitSHA)
	if err != nil {
		return nil, err
	}
	result.ContributingFile = contributingFile

	governanceFile, err := d.fetchOptionalFile(ctx, org, fetchRef, "GOVERNANCE.md", commitSHA)
	if err != nil {
		return nil, err
	}
	result.GovernanceFile = governanceFile

	return result, nil
}

func (d *Discoverer) fetchMaintainersFile(ctx context.Context, org, ref, commitSHA string) (FileDiscovery, error) {
	path, ok, err := d.findMaintainersPath(ctx, org, ref)
	if err != nil {
		return FileDiscovery{}, err
	}
	if !ok {
		return FileDiscovery{}, nil
	}
	return d.fetchOptionalFile(ctx, org, ref, path, commitSHA)
}

func (d *Discoverer) findMaintainersPath(ctx context.Context, org, ref string) (string, bool, error) {
	files, err := d.Client.ListFiles(ctx, org, DefaultRepoName, ref, "")
	if err != nil {
		return "", false, fmt.Errorf("list %s/%s root: %w", org, DefaultRepoName, err)
	}
	for _, file := range files {
		path := strings.TrimSpace(file.Path)
		if strings.EqualFold(path, maintainersFileName) {
			return path, true, nil
		}
	}
	return "", false, nil
}

func (d *Discoverer) fetchOptionalFile(ctx context.Context, org, ref, path, commitSHA string) (FileDiscovery, error) {
	fetched, err := d.Client.GetFile(ctx, org, DefaultRepoName, ref, path)
	if err != nil {
		if errors.Is(err, ErrDotProjectFileNotFound) {
			return FileDiscovery{}, nil
		}
		return FileDiscovery{}, fmt.Errorf("fetch %s/%s %s: %w", org, DefaultRepoName, path, err)
	}

	return FileDiscovery{
		Path:      fetched.Path,
		Exists:    true,
		BlobURL:   fetched.BlobURL,
		RawURL:    fetched.RawURL,
		ETag:      fetched.ETag,
		BodyHash:  hashBody(fetched.Body),
		Body:      fetched.Body,
		CommitSHA: commitSHA,
	}, nil
}

type projectYAML struct {
	SchemaVersion string `yaml:"schema_version"`
}

func parseProjectYAML(file FileDiscovery) (string, bool, ParseStatus, string) {
	raw := projectYAML{}
	if err := yaml.Unmarshal([]byte(file.Body), &raw); err != nil {
		return "", false, ParseStatusInvalidYAML, err.Error()
	}
	version := strings.TrimSpace(raw.SchemaVersion)
	if version == "" {
		return "", false, ParseStatusInvalidShape, "schema_version is required"
	}
	if version != SupportedSchemaVersion {
		return version, false, ParseStatusUnsupportedSchema, fmt.Sprintf("unsupported schema version %q", version)
	}
	return version, true, ParseStatusParsed, ""
}

func parseMaintainersYAML(file FileDiscovery) (*uint, ParseStatus, string) {
	handles, status, parseErr := ParseMaintainerHandles(file.Body)
	if status != ParseStatusParsed {
		return nil, status, parseErr
	}
	count := uint(len(handles))
	return &count, ParseStatusParsed, ""
}

func ParseMaintainerHandles(body string) ([]string, ParseStatus, string) {
	raw := struct {
		Maintainers []struct {
			Teams []struct {
				Name    string   `yaml:"name"`
				Members []string `yaml:"members"`
			} `yaml:"teams"`
		} `yaml:"maintainers"`
	}{}

	if err := yaml.Unmarshal([]byte(body), &raw); err != nil {
		return nil, ParseStatusInvalidYAML, err.Error()
	}
	if len(raw.Maintainers) == 0 {
		return nil, ParseStatusInvalidShape, "maintainers must contain at least one entry"
	}

	handles := make(map[string]struct{})
	for _, maintainerGroup := range raw.Maintainers {
		for _, team := range maintainerGroup.Teams {
			for _, member := range team.Members {
				normalized := normalizeHandle(member)
				if normalized == "" {
					continue
				}
				handles[normalized] = struct{}{}
			}
		}
	}

	result := make([]string, 0, len(handles))
	for handle := range handles {
		result = append(result, handle)
	}
	return result, ParseStatusParsed, ""
}

// MaintainerEntry is a single project-maintainers team member together with
// the 1-based line it appears on in the source YAML, so a caller can build
// a commit-pinned permalink to the exact line that granted maintainer
// status.
type MaintainerEntry struct {
	Handle string
	Line   int
}

// ParseProjectMaintainerEntries parses the project-maintainers team members
// out of a maintainers.yaml body using yaml.Node decoding, which preserves
// line numbers that a typed yaml.Unmarshal would discard.
func ParseProjectMaintainerEntries(body string) ([]MaintainerEntry, ParseStatus, string) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(body), &doc); err != nil {
		return nil, ParseStatusInvalidYAML, err.Error()
	}
	root := &doc
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		root = root.Content[0]
	}

	maintainersNode := mappingValue(root, "maintainers")
	if maintainersNode == nil || maintainersNode.Kind != yaml.SequenceNode || len(maintainersNode.Content) == 0 {
		return nil, ParseStatusInvalidShape, "maintainers must contain at least one entry"
	}

	seen := make(map[string]MaintainerEntry)
	teamFound := false
	var otherTeamNames []string
	for _, maintainerGroup := range maintainersNode.Content {
		teamsNode := mappingValue(maintainerGroup, "teams")
		if teamsNode == nil || teamsNode.Kind != yaml.SequenceNode {
			continue
		}
		for _, team := range teamsNode.Content {
			nameNode := mappingValue(team, "name")
			if nameNode == nil {
				continue
			}
			teamName := strings.TrimSpace(nameNode.Value)
			if !strings.EqualFold(teamName, "project-maintainers") {
				// Recorded only to make the exact-hyphenated-slug filter's
				// false-negative rate measurable (see the PR-reviewed
				// provenance plan section 6) - the match itself is
				// deliberately left exact, not widened here.
				if strings.Contains(strings.ToLower(teamName), "maintainer") {
					otherTeamNames = append(otherTeamNames, teamName)
				}
				continue
			}
			teamFound = true
			membersNode := mappingValue(team, "members")
			if membersNode == nil || membersNode.Kind != yaml.SequenceNode {
				continue
			}
			for _, member := range membersNode.Content {
				// A non-scalar member (e.g. `- {github: alice}`) has an
				// empty Value; skipping it silently would report a parsed
				// roster missing that maintainer, so the whole file must be
				// rejected as malformed instead. ScalarNode also covers YAML
				// nulls, booleans, and numbers (`- null`, `- 123`), so the
				// string tag must be checked too, or those become bogus
				// handles instead of being rejected.
				if member.Kind != yaml.ScalarNode || member.ShortTag() != "!!str" {
					return nil, ParseStatusInvalidShape, fmt.Sprintf(
						"project-maintainers member at line %d is not a plain string", member.Line)
				}
				normalized := normalizeHandle(member.Value)
				if normalized == "" {
					continue
				}
				if _, exists := seen[normalized]; exists {
					continue
				}
				seen[normalized] = MaintainerEntry{Handle: normalized, Line: member.Line}
			}
		}
	}
	if len(seen) == 0 {
		if !teamFound {
			if len(otherTeamNames) > 0 {
				return nil, ParseStatusInvalidShape, fmt.Sprintf(
					"no project-maintainers team found; saw maintainer-like team name(s) that did not match exactly: %s",
					strings.Join(otherTeamNames, ", "))
			}
			return nil, ParseStatusInvalidShape, "no project-maintainers team found"
		}
		return nil, ParseStatusInvalidShape, "project-maintainers team has no members"
	}

	result := make([]MaintainerEntry, 0, len(seen))
	for _, entry := range seen {
		result = append(result, entry)
	}
	return result, ParseStatusParsed, ""
}

// mappingValue returns the value node for key in a mapping node, or nil if
// node is not a mapping or the key is absent.
func mappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

func ParseProjectMaintainerHandles(body string) ([]string, ParseStatus, string) {
	entries, status, parseErr := ParseProjectMaintainerEntries(body)
	if status != ParseStatusParsed {
		return nil, status, parseErr
	}
	result := make([]string, 0, len(entries))
	for _, entry := range entries {
		result = append(result, entry.Handle)
	}
	return result, ParseStatusParsed, ""
}

func normalizeHandle(raw string) string {
	handle := strings.TrimSpace(raw)
	handle = strings.TrimPrefix(handle, "@")
	handle = strings.ToLower(handle)
	return handle
}

func hashBody(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

func repoRef(org string) string {
	return fmt.Sprintf("https://github.com/%s/%s", org, DefaultRepoName)
}

func blobRef(org, branch, path string) string {
	return fmt.Sprintf("https://github.com/%s/%s/blob/%s/%s", org, DefaultRepoName, branch, path)
}

func rawRef(org, branch, path string) string {
	return fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s/%s", org, DefaultRepoName, branch, path)
}

type GitHubRepositoryClient struct {
	Client *github.Client
}

func (c *GitHubRepositoryClient) GetDefaultBranch(ctx context.Context, owner, repo string) (string, error) {
	if c == nil || c.Client == nil {
		return "", errors.New("github client is required")
	}
	repository, resp, err := c.Client.Repositories.Get(ctx, owner, repo)
	if err != nil {
		if isGitHubNotFound(resp, err) {
			return "", ErrDotProjectRepoNotFound
		}
		return "", err
	}
	return strings.TrimSpace(repository.GetDefaultBranch()), nil
}

func (c *GitHubRepositoryClient) GetFile(ctx context.Context, owner, repo, ref, path string) (*FetchedFile, error) {
	if c == nil || c.Client == nil {
		return nil, errors.New("github client is required")
	}
	file, _, resp, err := c.Client.Repositories.GetContents(ctx, owner, repo, path, &github.RepositoryContentGetOptions{Ref: ref})
	if err != nil {
		if isGitHubNotFound(resp, err) {
			return nil, ErrDotProjectFileNotFound
		}
		return nil, err
	}
	if file == nil {
		return nil, ErrDotProjectFileNotFound
	}

	body, err := file.GetContent()
	if err != nil {
		return nil, err
	}

	etag := ""
	if resp != nil {
		etag = resp.Header.Get("ETag")
	}

	fetched := &FetchedFile{
		Path:    path,
		BlobURL: strings.TrimSpace(file.GetHTMLURL()),
		RawURL:  strings.TrimSpace(file.GetDownloadURL()),
		ETag:    etag,
		Body:    body,
	}
	if fetched.BlobURL == "" {
		fetched.BlobURL = blobRef(owner, ref, path)
	}
	if fetched.RawURL == "" {
		fetched.RawURL = rawRef(owner, ref, path)
	}
	return fetched, nil
}

func (c *GitHubRepositoryClient) GetCommitSHA(ctx context.Context, owner, repo, ref string) (string, error) {
	if c == nil || c.Client == nil {
		return "", errors.New("github client is required")
	}
	branch, resp, err := c.Client.Repositories.GetBranch(ctx, owner, repo, ref, false)
	if err != nil {
		if isGitHubNotFound(resp, err) {
			return "", ErrDotProjectRepoNotFound
		}
		return "", err
	}
	return strings.TrimSpace(branch.GetCommit().GetSHA()), nil
}

func (c *GitHubRepositoryClient) ListFiles(ctx context.Context, owner, repo, ref, path string) ([]ListedFile, error) {
	if c == nil || c.Client == nil {
		return nil, errors.New("github client is required")
	}
	_, dir, resp, err := c.Client.Repositories.GetContents(ctx, owner, repo, path, &github.RepositoryContentGetOptions{Ref: ref})
	if err != nil {
		if isGitHubNotFound(resp, err) {
			return nil, ErrDotProjectFileNotFound
		}
		return nil, err
	}
	files := make([]ListedFile, 0, len(dir))
	for _, entry := range dir {
		if entry == nil {
			continue
		}
		files = append(files, ListedFile{Path: strings.TrimSpace(entry.GetPath())})
	}
	return files, nil
}

func isGitHubNotFound(resp *github.Response, err error) bool {
	if resp != nil && resp.StatusCode == 404 {
		return true
	}
	var ghErr *github.ErrorResponse
	if errors.As(err, &ghErr) && ghErr.Response != nil && ghErr.Response.StatusCode == 404 {
		return true
	}
	return false
}
