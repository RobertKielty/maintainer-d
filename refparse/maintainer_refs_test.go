package refparse

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExtractGitHubHandleLocationsMarkdownTable(t *testing.T) {
	body := `| Name | GitHub |
| --- | --- |
| Alice Example | alice-example |
| Bob Example | @bob-example |
`
	locations := ExtractGitHubHandleLocations(body)
	require.Contains(t, locations, "alice-example")
	require.Contains(t, locations, "bob-example")
	assert.Equal(t, []int{3}, locations["alice-example"])
	assert.Equal(t, []int{4}, locations["bob-example"])
}

func TestExtractGitHubHandleLocationsAtHandle(t *testing.T) {
	body := "Maintainers:\nSee @carol-example for details.\n"
	locations := ExtractGitHubHandleLocations(body)
	assert.Equal(t, []int{2}, locations["carol-example"])
}

func TestExtractGitHubHandleLocationsGitHubURL(t *testing.T) {
	body := "Profile: https://github.com/dave-example\nOrg: https://github.com/example-org/orgs\n"
	locations := ExtractGitHubHandleLocations(body)
	assert.Equal(t, []int{1}, locations["dave-example"])
	assert.NotContains(t, locations, "example-org")
}

func TestExtractGitHubHandleLocationsYAMLList(t *testing.T) {
	body := "maintainers:\n  - erin-example\n  github: frank-example\n"
	locations := ExtractGitHubHandleLocations(body)
	assert.Equal(t, []int{2}, locations["erin-example"])
	assert.Equal(t, []int{3}, locations["frank-example"])
}

func TestExtractGitHubHandlesMatchesLocationsWrapper(t *testing.T) {
	body := `| Name | GitHub |
| --- | --- |
| Alice Example | alice-example |

See @bob-example and https://github.com/carol-example for more.
`
	locations := ExtractGitHubHandleLocations(body)
	expected := make([]string, 0, len(locations))
	for handle := range locations {
		expected = append(expected, handle)
	}
	sort.Strings(expected)

	handles := ExtractGitHubHandles(body)
	actual := make([]string, 0, len(handles))
	for handle := range handles {
		actual = append(actual, handle)
	}
	sort.Strings(actual)

	assert.Equal(t, expected, actual)
}

func TestMaintainerRefContainsStillWorks(t *testing.T) {
	found, err := MaintainerRefContains("maintainer: @grace-example", "grace-example")
	require.NoError(t, err)
	assert.True(t, found)

	found, err = MaintainerRefContains("maintainer: @grace-example", "henry-example")
	require.NoError(t, err)
	assert.False(t, found)
}
