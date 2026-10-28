package verify

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"autodoc/internal/tools"
)

// fullLinkRe matches a Markdown link with its text: [text](target "title").
var fullLinkRe = regexp.MustCompile(`\[([^\]]*)\]\(([^)\s]+)(?:\s+"[^"]*")?\)`)

// githubLineRe matches a GitHub-style line anchor: #L31 or #L31-L40.
var githubLineRe = regexp.MustCompile(`#L(\d+)(?:-L(\d+))?$`)

// RewriteCodeLinks turns Markdown links to repository code into the backtick
// citations the docs are told to use, in every docs page under dir, and
// returns how many it rewrote per page. Models often link code anyway, and
// such a link does not resolve from docs/; what it means is not in doubt,
// so it is rewritten here instead of costing a gate B attempt:
// [index](internal/api/h.go:31) becomes index (`internal/api/h.go:31`).
//
// Only a link that does not resolve as written, and whose target read from
// the repository root is an existing file outside docs/, is rewritten; gate B
// then checks the citation like any other. Links between pages, links that
// resolve, images, fenced code and inline code are left alone.
func RewriteCodeLinks(dir string) (map[string]int, error) {
	out := map[string]int{}
	for _, page := range DocPages {
		file := filepath.Join(dir, filepath.FromSlash(page))
		data, err := os.ReadFile(file)
		if os.IsNotExist(err) {
			continue // gate B reports the missing page
		}
		if err != nil {
			return out, err
		}
		src, n := rewriteCodeLinks(dir, page, string(data))
		if n == 0 {
			continue
		}
		if err := os.WriteFile(file, []byte(src), 0o644); err != nil {
			return out, err
		}
		out[page] = n
	}
	return out, nil
}

func rewriteCodeLinks(dir, page, src string) (string, int) {
	lines := strings.Split(src, "\n")
	fence, count := "", 0
	for i, line := range lines {
		t := strings.TrimSpace(line)
		if fence != "" {
			if strings.HasPrefix(t, fence) {
				fence = ""
			}
			continue
		}
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			fence = t[:3]
			continue
		}
		var b strings.Builder
		last := 0
		for _, m := range fullLinkRe.FindAllStringSubmatchIndex(line, -1) {
			start, end := m[0], m[1]
			text, target := line[m[2]:m[3]], line[m[4]:m[5]]
			// An image, or a link inside an inline code span (an odd number of
			// backticks before it), is not a link to rewrite.
			if start > 0 && line[start-1] == '!' || strings.Count(line[:start], "`")%2 == 1 {
				continue
			}
			ref, ok := codeRef(dir, page, target)
			if !ok {
				continue
			}
			cite := "`" + ref + "`"
			if t := strings.Trim(strings.TrimSpace(text), "`"); t != "" && t != ref {
				cite = strings.TrimSpace(text) + " (" + cite + ")"
			}
			b.WriteString(line[last:start])
			b.WriteString(cite)
			last = end
			count++
		}
		if last > 0 {
			b.WriteString(line[last:])
			lines[i] = b.String()
		}
	}
	return strings.Join(lines, "\n"), count
}

// codeRef returns the citation a broken link to repository code means:
// its target from the repository root, with a GitHub line anchor as :line.
func codeRef(dir, page, target string) (string, bool) {
	if strings.Contains(target, "://") || strings.HasPrefix(target, "#") || strings.HasPrefix(target, "mailto:") {
		return "", false
	}
	if exists(dir, path.Join(path.Dir(page), strings.SplitN(target, "#", 2)[0])) {
		return "", false // it resolves as written
	}
	if m := githubLineRe.FindStringSubmatch(target); m != nil {
		target = strings.TrimSuffix(target, m[0]) + ":" + m[1]
		if m[2] != "" {
			target += "-" + m[2]
		}
	} else if strings.Contains(target, "#") {
		return "", false
	}
	ref := strings.TrimPrefix(target, "/")
	if strings.HasPrefix(target, "../") {
		ref = path.Join(path.Dir(page), target) // relative to docs/, as a link should be
	}
	m := fileRe.FindStringSubmatch(ref)
	if m == nil || strings.HasPrefix(ref, "../") || strings.HasPrefix(m[1], tools.DocsDir+"/") || !exists(dir, m[1]) {
		return "", false
	}
	return ref, true
}

func exists(dir, rel string) bool {
	if strings.HasPrefix(path.Clean(rel), "../") {
		return false
	}
	st, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel)))
	return err == nil && !st.IsDir()
}

// RewriteSummary is one line for the trace: how many links were rewritten.
func RewriteSummary(n map[string]int) string {
	var parts []string
	total := 0
	for _, p := range DocPages {
		if n[p] > 0 {
			parts = append(parts, fmt.Sprintf("%s: %d", path.Base(p), n[p]))
			total += n[p]
		}
	}
	return fmt.Sprintf("rewrote %d link(s) to code as citations (%s)", total, strings.Join(parts, ", "))
}
