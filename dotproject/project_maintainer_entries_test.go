package dotproject

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const maintainerEntriesFixture = `maintainers:
  - teams:
      - name: project-maintainers
        members:
          - alice-example
          - bob-example
      - name: other-team
        members:
          - carol-example
`

func TestParseProjectMaintainerEntriesReturnsLineNumbers(t *testing.T) {
	entries, status, parseErr := ParseProjectMaintainerEntries(maintainerEntriesFixture)
	require.Equal(t, ParseStatusParsed, status, parseErr)
	require.Len(t, entries, 3)

	byHandle := make(map[string]int, len(entries))
	for _, entry := range entries {
		byHandle[entry.Handle] = entry.Line
	}
	assert.Equal(t, 5, byHandle["alice-example"])
	assert.Equal(t, 6, byHandle["bob-example"])
	assert.Equal(t, 9, byHandle["carol-example"])
}

func TestParseProjectMaintainerHandlesMatchesEntriesWrapper(t *testing.T) {
	entries, entryStatus, entryErr := ParseProjectMaintainerEntries(maintainerEntriesFixture)
	require.Equal(t, ParseStatusParsed, entryStatus, entryErr)

	handles, status, parseErr := ParseProjectMaintainerHandles(maintainerEntriesFixture)
	require.Equal(t, ParseStatusParsed, status, parseErr)

	expected := make([]string, 0, len(entries))
	for _, entry := range entries {
		expected = append(expected, entry.Handle)
	}
	sort.Strings(expected)
	sort.Strings(handles)
	assert.Equal(t, expected, handles)
}

func TestParseProjectMaintainerEntriesNoTeams(t *testing.T) {
	_, status, parseErr := ParseProjectMaintainerEntries(`maintainers:
  - teams: []
`)
	assert.Equal(t, ParseStatusInvalidShape, status)
	assert.Equal(t, "no maintainer teams found", parseErr)
}

func TestParseProjectMaintainerEntriesTeamPresentButEmpty(t *testing.T) {
	_, status, parseErr := ParseProjectMaintainerEntries(`maintainers:
  - teams:
      - name: project-maintainers
        members: []
`)
	assert.Equal(t, ParseStatusInvalidShape, status)
	assert.Equal(t, "maintainer teams have no members", parseErr)
}

func TestParseProjectMaintainerEntriesAnyTeamName(t *testing.T) {
	// Upstream (cncf/automation#602) dropped the requirement that a
	// maintainer team be named exactly "project-maintainers" - any team
	// name, including one that used to be treated as a near-miss, is now
	// accepted.
	entries, status, parseErr := ParseProjectMaintainerEntries(`maintainers:
  - teams:
      - name: Project Maintainers
        members:
          - carol-example
`)
	require.Equal(t, ParseStatusParsed, status, parseErr)
	require.Len(t, entries, 1)
	assert.Equal(t, "carol-example", entries[0].Handle)
}

func TestParseProjectMaintainerEntriesRejectsNonScalarMember(t *testing.T) {
	// A mapping member has no scalar value; silently skipping it would
	// report a parsed roster missing that maintainer, so the whole file is
	// rejected as malformed even when other members are valid strings.
	_, status, parseErr := ParseProjectMaintainerEntries(`maintainers:
  - teams:
      - name: project-maintainers
        members:
          - alice-example
          - {github: bob-example}
`)
	assert.Equal(t, ParseStatusInvalidShape, status)
	assert.Equal(t, "maintainer team member at line 6 is not a plain string", parseErr)
}

func TestParseProjectMaintainerEntriesRejectsNonStringScalarMember(t *testing.T) {
	// A YAML null, bool, or number is still a ScalarNode, so the Kind check
	// alone lets `- null` or `- 123` through as a bogus GitHub handle. The
	// tag must be checked too.
	_, status, parseErr := ParseProjectMaintainerEntries(`maintainers:
  - teams:
      - name: project-maintainers
        members:
          - alice-example
          - null
`)
	assert.Equal(t, ParseStatusInvalidShape, status)
	assert.Equal(t, "maintainer team member at line 6 is not a plain string", parseErr)
}

func TestParseProjectMaintainerEntriesResolvesAliasedMembers(t *testing.T) {
	// A typed yaml.Unmarshal resolves aliases transparently, but node-based
	// decoding surfaces `members: *maintainers` as an AliasNode, not a
	// SequenceNode - a Kind check that doesn't see through the alias would
	// wrongly report this roster as having no members.
	entries, status, parseErr := ParseProjectMaintainerEntries(`shared: &maintainer-list
  - alice-example
  - bob-example
maintainers:
  - teams:
      - name: project-maintainers
        members: *maintainer-list
`)
	require.Equal(t, ParseStatusParsed, status, parseErr)

	handles := make([]string, 0, len(entries))
	for _, entry := range entries {
		handles = append(handles, entry.Handle)
	}
	sort.Strings(handles)
	assert.Equal(t, []string{"alice-example", "bob-example"}, handles)
}
