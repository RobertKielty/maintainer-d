package refparse

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

var (
	atHandleRe       = regexp.MustCompile(`(?i)(^|[^a-z0-9_-])@([a-z0-9-]{1,39})`)
	githubURLRe      = regexp.MustCompile(`(?i)github\.com/([a-z0-9-]{1,39})`)
	listItemHandleRe = regexp.MustCompile(`(?i)^\s*[-*]\s*([a-z0-9][a-z0-9-]{0,38})\b`)
	githubKeyRe      = regexp.MustCompile(`(?i)^\s*github\s*:\s*([a-z0-9][a-z0-9-]{0,38})\b`)
)

// ExtractGitHubHandles scans maintainer ref content and returns a set of detected GitHub handles.
func ExtractGitHubHandles(refBody string) map[string]struct{} {
	locations := ExtractGitHubHandleLocations(refBody)
	result := make(map[string]struct{}, len(locations))
	for handle := range locations {
		result[handle] = struct{}{}
	}
	return result
}

// ExtractGitHubHandleLocations scans maintainer ref content the same way
// ExtractGitHubHandles does, but also records the 1-based line each handle
// was found on, so a caller can resolve the line to its commit, PR, and
// review state. A handle found on more than one line keeps every line.
func ExtractGitHubHandleLocations(refBody string) map[string][]int {
	result := make(map[string][]int)
	if refBody == "" {
		return result
	}
	add := func(handle string, line int) {
		if handle == "" || isReservedHandle(handle) {
			return
		}
		handle = strings.ToLower(handle)
		result[handle] = appendLineOnce(result[handle], line)
	}

	lines := strings.Split(refBody, "\n")
	parseMarkdownTableHandleLocations(lines, add)

	for lineIdx, line := range lines {
		lineNumber := lineIdx + 1
		for _, match := range atHandleRe.FindAllStringSubmatch(line, -1) {
			if len(match) < 3 {
				continue
			}
			add(match[2], lineNumber)
		}
		for _, match := range githubURLRe.FindAllStringSubmatchIndex(line, -1) {
			if len(match) < 4 {
				continue
			}
			handle := line[match[2]:match[3]]
			// If the matched handle is followed by a path segment, skip it.
			if match[1] < len(line) && line[match[1]] == '/' {
				continue
			}
			add(handle, lineNumber)
		}
		if match := listItemHandleRe.FindStringSubmatch(line); len(match) > 1 {
			add(match[1], lineNumber)
		}
		if match := githubKeyRe.FindStringSubmatch(line); len(match) > 1 {
			add(match[1], lineNumber)
		}
	}
	return result
}

func appendLineOnce(lines []int, line int) []int {
	if slices.Contains(lines, line) {
		return lines
	}
	return append(lines, line)
}

func isReservedHandle(handle string) bool {
	switch strings.ToLower(handle) {
	case "organizations", "orgs", "repos":
		return true
	}
	return false
}

func parseMarkdownTableHandleLocations(lines []string, add func(handle string, line int)) {
	if len(lines) < 2 {
		return
	}
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
		if handle == "" || isReservedHandle(handle) {
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
			rowCells := parseRow(lines[row])
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
			add(cell, row+1)
		}
		i++
	}
}

// MaintainerRefContains checks if the maintainer ref contains a handle (case-insensitive) with word boundaries.
func MaintainerRefContains(refBody, handle string) (bool, error) {
	if handle == "" {
		return false, errors.New("handle is empty")
	}
	escaped := regexp.QuoteMeta(handle)
	pattern := fmt.Sprintf(`(?i)(^|[^a-z0-9_-])@?%s([^a-z0-9_-]|$)`, escaped)
	re, err := regexp.Compile(pattern)
	if err != nil {
		return false, err
	}
	return re.MatchString(refBody), nil
}
