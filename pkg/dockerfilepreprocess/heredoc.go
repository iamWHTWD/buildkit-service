package dockerfilepreprocess

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

var (
	runCatRedirectHeredocPattern = regexp.MustCompile(`^([ \t]*)RUN[ \t]+cat[ \t]+>[ \t]+([^ \t]+)[ \t]+<<(?:[ \t]*)(-?)(['\"]?)([A-Za-z_][A-Za-z0-9_]*)(['\"]?)[ \t]*$`)
	runCatHeredocRedirectPattern = regexp.MustCompile(`^([ \t]*)RUN[ \t]+cat[ \t]+<<(?:[ \t]*)(-?)(['\"]?)([A-Za-z_][A-Za-z0-9_]*)(['\"]?)[ \t]+>[ \t]+([^ \t]+)[ \t]*$`)
	runCatAppendHeredocPattern   = regexp.MustCompile(`^[ \t]*RUN[ \t]+cat[ \t]+>>[ \t]+[^ \t]+[ \t]+<<`)
)

// PreprocessDockerfile rewrites legacy shell heredocs that Dockerfile parsing
// cannot handle into BuildKit Dockerfile heredocs.
func PreprocessDockerfile(path string) (bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	processed, changed, err := TransformLegacyHeredocs(raw)
	if err != nil || !changed {
		return changed, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	if err := os.WriteFile(path, processed, info.Mode()); err != nil {
		return false, err
	}
	return true, nil
}

func TransformLegacyHeredocs(raw []byte) ([]byte, bool, error) {
	lines := splitDockerfileLines(string(raw))
	out := make([]string, 0, len(lines))
	changed := false

	for idx := 0; idx < len(lines); idx++ {
		line := lines[idx]
		plain := strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")

		if runCatAppendHeredocPattern.MatchString(plain) {
			return nil, false, fmt.Errorf("line %d: append heredoc with cat >> is not supported; use Dockerfile COPY heredoc or split the append into an explicit RUN", idx+1)
		}

		spec, ok, err := parseLegacyCatHeredoc(plain, idx+1)
		if err != nil {
			return nil, false, err
		}
		if !ok {
			out = append(out, line)
			continue
		}

		end := findHeredocEnd(lines, idx+1, spec.delimiter, spec.stripTabs)
		if end < 0 {
			return nil, false, fmt.Errorf("line %d: heredoc delimiter %q was not found", idx+1, spec.delimiter)
		}

		out = append(out, fmt.Sprintf("%sCOPY <<%s %s\n", spec.indent, spec.delimiterToken, spec.target))
		out = append(out, lines[idx+1:end+1]...)
		idx = end
		changed = true
	}

	if !changed {
		return raw, false, nil
	}
	return []byte(strings.Join(out, "")), true, nil
}

type legacyHeredocSpec struct {
	indent         string
	target         string
	delimiter      string
	delimiterToken string
	stripTabs      bool
}

func parseLegacyCatHeredoc(line string, lineNumber int) (legacyHeredocSpec, bool, error) {
	if match := runCatRedirectHeredocPattern.FindStringSubmatch(line); match != nil {
		quoteLeft, quoteRight := match[4], match[6]
		if quoteLeft != quoteRight {
			return legacyHeredocSpec{}, false, fmt.Errorf("line %d: mismatched heredoc delimiter quotes", lineNumber)
		}
		delimiterToken := match[3] + quoteLeft + match[5] + quoteRight
		return legacyHeredocSpec{indent: match[1], target: match[2], delimiter: match[5], delimiterToken: delimiterToken, stripTabs: match[3] == "-"}, true, nil
	}
	if match := runCatHeredocRedirectPattern.FindStringSubmatch(line); match != nil {
		quoteLeft, quoteRight := match[3], match[5]
		if quoteLeft != quoteRight {
			return legacyHeredocSpec{}, false, fmt.Errorf("line %d: mismatched heredoc delimiter quotes", lineNumber)
		}
		delimiterToken := match[2] + quoteLeft + match[4] + quoteRight
		return legacyHeredocSpec{indent: match[1], target: match[6], delimiter: match[4], delimiterToken: delimiterToken, stripTabs: match[2] == "-"}, true, nil
	}
	return legacyHeredocSpec{}, false, nil
}

func findHeredocEnd(lines []string, start int, delimiter string, stripTabs bool) int {
	for idx := start; idx < len(lines); idx++ {
		line := strings.TrimSuffix(strings.TrimSuffix(lines[idx], "\n"), "\r")
		candidate := line
		if stripTabs {
			candidate = strings.TrimLeft(candidate, "\t")
		}
		if candidate == delimiter {
			return idx
		}
	}
	return -1
}

func splitDockerfileLines(content string) []string {
	if content == "" {
		return nil
	}
	parts := strings.SplitAfter(content, "\n")
	if len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}
