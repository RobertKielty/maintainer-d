package dotproject

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

type MaintainerPatch struct {
	Proposed             string
	AddedHandles         []string
	RemovedPlaceholders  []string
	ProjectMaintainersAt int
}

var (
	// teamNameLineRE matches a `- name: <anything>` list item under a
	// `teams:` sequence. It used to require the literal name
	// "project-maintainers" - upstream (cncf/automation#602) dropped that
	// requirement, so every team is now a candidate. This write path still
	// has to pick one team to patch into, so it targets whichever team
	// comes first in the file, regardless of name.
	teamNameLineRE = regexp.MustCompile(`(?i)^\s*(?:-\s*)?name:\s*\S.*$`)
	membersLineRE  = regexp.MustCompile(`(?i)^\s*members:\s*(?:#.*)?$`)
	sequenceItemRE = regexp.MustCompile(`^\s*-\s*(\S+)\s*(?:#.*)?$`)
	todoCommentRE  = regexp.MustCompile(`(?i)^\s*#\s*TODO\b.*$`)
)

func BuildMaintainerRosterPatch(source string, activeHandles []string) (*MaintainerPatch, error) {
	lines := splitPatchLines(source)
	teamIndex := findMaintainerTeam(lines)
	if teamIndex < 0 {
		return nil, fmt.Errorf("no maintainer team found in maintainers.yaml")
	}

	teamIndent := leadingWhitespaceLength(lines[teamIndex])
	membersIndex := -1
	for index := teamIndex + 1; index < len(lines); index++ {
		line := lines[index]
		trimmed := strings.TrimSpace(line)
		if trimmed != "" && leadingWhitespaceLength(line) <= teamIndent && isSequenceItem(line) {
			break
		}
		if membersLineRE.MatchString(line) {
			membersIndex = index
			break
		}
	}

	proposed := append([]string(nil), lines...)
	if membersIndex < 0 {
		insertLines := append([]string{strings.Repeat(" ", teamIndent+2) + "members:"}, missingHandleLines(source, activeHandles, strings.Repeat(" ", teamIndent+4))...)
		if len(insertLines) == 1 {
			return &MaintainerPatch{Proposed: source, ProjectMaintainersAt: teamIndex + 1}, nil
		}
		proposed = insertAt(proposed, teamIndex+1, insertLines)
		return &MaintainerPatch{
			Proposed:             strings.Join(proposed, "\n"),
			AddedHandles:         linesToHandles(insertLines[1:]),
			ProjectMaintainersAt: teamIndex + 1,
		}, nil
	}

	membersIndent := leadingWhitespaceLength(lines[membersIndex])
	itemIndent := strings.Repeat(" ", membersIndent+2)
	insertAtIndex := membersIndex + 1
	existing := make(map[string]struct{})
	placeholderIndexes := map[int]string{}

	for index := membersIndex + 1; index < len(lines); index++ {
		line := lines[index]
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		lineIndent := leadingWhitespaceLength(line)
		if lineIndent <= membersIndent {
			break
		}
		if todoCommentRE.MatchString(line) {
			placeholderIndexes[index] = strings.TrimSpace(line)
			continue
		}
		if isSequenceItem(line) {
			itemIndent = line[:leadingWhitespaceLength(line)]
			insertAtIndex = index + 1
			handle := extractSequenceHandle(line)
			normalized := NormalizeGitHubHandle(handle)
			if normalized != "" {
				existing[normalized] = struct{}{}
			}
			if normalized == "github-handle" {
				placeholderIndexes[index] = strings.TrimSpace(line)
			}
			continue
		}
		insertAtIndex = index + 1
	}

	for index := len(proposed) - 1; index >= 0; index-- {
		if _, ok := placeholderIndexes[index]; !ok {
			continue
		}
		if index < insertAtIndex {
			insertAtIndex--
		}
		proposed = append(proposed[:index], proposed[index+1:]...)
	}

	added := missingActiveHandles(activeHandles, existing)
	insertLines := make([]string, 0, len(added))
	for _, handle := range added {
		insertLines = append(insertLines, fmt.Sprintf("%s- %s", itemIndent, handle))
	}
	proposed = insertAt(proposed, insertAtIndex, insertLines)

	removed := make([]string, 0, len(placeholderIndexes))
	for _, value := range placeholderIndexes {
		removed = append(removed, value)
	}
	sort.Strings(removed)

	return &MaintainerPatch{
		Proposed:             strings.Join(proposed, "\n"),
		AddedHandles:         added,
		RemovedPlaceholders:  removed,
		ProjectMaintainersAt: teamIndex + 1,
	}, nil
}

// GenerateMaintainersRosterYAML builds a scaffold MAINTAINERS.yaml for a
// project that does not yet have a .project repo, populating a maintainer
// team from the given active GitHub handles. The team name matches
// upstream's current scaffold (cncf/automation#602), which emits "maintainers"
// rather than the retired "project-maintainers" name.
func GenerateMaintainersRosterYAML(activeHandles []string) string {
	handles := missingActiveHandles(activeHandles, map[string]struct{}{})

	var builder strings.Builder
	builder.WriteString("maintainers:\n")
	builder.WriteString("  - teams:\n")
	builder.WriteString("      - name: \"maintainers\"\n")
	builder.WriteString("        members:\n")
	if len(handles) == 0 {
		builder.WriteString("          # TODO: Add maintainer GitHub handles\n")
		builder.WriteString("          - github-handle\n")
		return builder.String()
	}
	for _, handle := range handles {
		builder.WriteString("          - " + handle + "\n")
	}
	return builder.String()
}

func NormalizeGitHubHandle(handle string) string {
	return strings.ToLower(strings.Trim(strings.TrimSpace(handle), `@"'`))
}

func splitPatchLines(source string) []string {
	source = strings.ReplaceAll(strings.ReplaceAll(source, "\r\n", "\n"), "\r", "\n")
	return strings.Split(source, "\n")
}

func findMaintainerTeam(lines []string) int {
	for index, line := range lines {
		if teamNameLineRE.MatchString(line) {
			return index
		}
	}
	return -1
}

func leadingWhitespaceLength(line string) int {
	return len(line) - len(strings.TrimLeft(line, " \t"))
}

func isSequenceItem(line string) bool {
	return sequenceItemRE.MatchString(line)
}

func extractSequenceHandle(line string) string {
	match := sequenceItemRE.FindStringSubmatch(line)
	if len(match) != 2 {
		return ""
	}
	return strings.Trim(match[1], `"'`)
}

func missingActiveHandles(activeHandles []string, existing map[string]struct{}) []string {
	seen := make(map[string]struct{}, len(activeHandles))
	missing := []string{}
	for _, raw := range activeHandles {
		normalized := NormalizeGitHubHandle(raw)
		if normalized == "" {
			continue
		}
		if _, ok := seen[normalized]; ok {
			continue
		}
		seen[normalized] = struct{}{}
		if _, ok := existing[normalized]; ok {
			continue
		}
		missing = append(missing, normalized)
	}
	return missing
}

func missingHandleLines(source string, activeHandles []string, indent string) []string {
	existing := map[string]struct{}{}
	for _, line := range splitPatchLines(source) {
		if handle := NormalizeGitHubHandle(extractSequenceHandle(line)); handle != "" {
			existing[handle] = struct{}{}
		}
	}
	missing := missingActiveHandles(activeHandles, existing)
	lines := make([]string, 0, len(missing))
	for _, handle := range missing {
		lines = append(lines, fmt.Sprintf("%s- %s", indent, handle))
	}
	return lines
}

func linesToHandles(lines []string) []string {
	handles := make([]string, 0, len(lines))
	for _, line := range lines {
		if handle := NormalizeGitHubHandle(extractSequenceHandle(line)); handle != "" {
			handles = append(handles, handle)
		}
	}
	return handles
}

func insertAt(lines []string, index int, insertLines []string) []string {
	if len(insertLines) == 0 {
		return lines
	}
	out := make([]string, 0, len(lines)+len(insertLines))
	out = append(out, lines[:index]...)
	out = append(out, insertLines...)
	out = append(out, lines[index:]...)
	return out
}
