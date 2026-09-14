package shunt

// Build a short outline of a text file: definitions and headings with their line ranges.
//
// The hook puts the outline in its denial message, so Claude can Read just the relevant
// range, or answer from the outline, without another round trip. No model is involved:
// the outline comes from line patterns and indentation, so it is fast but approximate.

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

var markdownExtensions = map[string]bool{".md": true, ".markdown": true, ".mdx": true, ".rst": true}

const maxOutlineTextChars = 110

// Definitions in common languages, matched at the start of a line.
var definitionRE = regexp.MustCompile(`^(?:` +
	`(?:async\s+)?def\s+\w+` + // Python
	`|class\s+\w+` + // Python, JS/TS, Ruby
	`|module\s+\w+` + // Ruby
	`|(?:export\s+)?(?:default\s+)?(?:async\s+)?function\*?\s+\w+` + // JS/TS, shell
	`|(?:export\s+)?(?:abstract\s+)?(?:class|interface|enum)\s+\w+` + // TS
	`|(?:export\s+)?type\s+\w+\s*(?:<[^>]*>)?\s*=` + // TS
	`|(?:export\s+)?(?:const|let|var)\s+\w+\s*=\s*(?:async\s*)?(?:\([^)]*\)|\w+)\s*=>` + // JS/TS arrow functions
	`|func\s+(?:\([^)]*\)\s*)?\w+` + // Go
	`|type\s+\w+\s+(?:struct|interface)` + // Go
	`|(?:pub(?:\([\w:]+\))?\s+)?(?:async\s+)?(?:unsafe\s+)?(?:fn|struct|enum|trait|mod)\s+\w+` + // Rust
	`|impl\b` + // Rust
	`|(?:public|private|protected|internal)\s+(?:static\s+|final\s+|abstract\s+|sealed\s+)*` +
	`(?:class|interface|enum|record)\s+\w+` + // Java, C#, Kotlin
	`|\w+\s*\(\)\s*\{` + // shell
	`)`)

// Top-level constants such as `chunkOverlapLines = 50`: their value often answers a question.
// The match must not be followed by "=" (a comparison); isConstant checks that.
var (
	constantRE = regexp.MustCompile(`^(?:export\s+)?(?:const\s+|final\s+)?[A-Z][A-Z0-9_]{2,}\s*(?::[^=]+)?=`)
	headingRE  = regexp.MustCompile(`^(#{1,6})\s+\S`)
	fenceRE    = regexp.MustCompile("^\\s*(```|~~~)")
)

func isConstant(line string) bool {
	loc := constantRE.FindStringIndex(line)
	return loc != nil && !strings.HasPrefix(line[loc[1]:], "=")
}

type outlineEntry struct {
	first, last int // 1-based
	depth       int
	text        string
}

// indentWidth counts leading whitespace with tabs expanded to multiples of 4.
func indentWidth(line string) int {
	col := 0
	for _, r := range line {
		switch {
		case r == '\t':
			col += 4 - col%4
		case unicode.IsSpace(r):
			col++
		default:
			return col
		}
	}
	return col
}

// blockEnd returns the 1-based last line of the block whose header is lines[start], judged by indentation.
func blockEnd(lines []string, start, indent int) int {
	last := start
	for j := start + 1; j < len(lines); j++ {
		stripped := strings.TrimSpace(lines[j])
		if stripped == "" {
			continue
		}
		if indentWidth(lines[j]) > indent || stripped[0] == ')' || stripped[0] == ']' {
			last = j // body, or the end of a multi-line signature
			continue
		}
		if stripped[0] == '}' || stripped == "end" || strings.HasPrefix(stripped, "end ") {
			return j + 1 // closing brace or Ruby `end` belongs to the block
		}
		break
	}
	return last + 1
}

func outlineEntries(lines []string, markdown bool) []outlineEntry {
	var entries []outlineEntry
	if markdown {
		type heading struct {
			index, level int
			text         string
		}
		var headings []heading
		inFence := false
		for i, line := range lines {
			if fenceRE.MatchString(line) {
				inFence = !inFence
			} else if m := headingRE.FindStringSubmatch(line); !inFence && m != nil {
				headings = append(headings, heading{i, len(m[1]), strings.TrimSpace(line)})
			}
		}
		for n, h := range headings {
			// A section ends before the next heading at the same or a higher level.
			end := len(lines)
			for _, next := range headings[n+1:] {
				if next.level <= h.level {
					end = next.index
					break
				}
			}
			for end > h.index+1 && strings.TrimSpace(lines[end-1]) == "" {
				end--
			}
			entries = append(entries, outlineEntry{h.index + 1, end, h.level - 1, h.text})
		}
	} else {
		type found struct {
			index, indent int
			text          string
			definition    bool
		}
		var matches []found
		indents := map[int]bool{}
		for i, line := range lines {
			stripped := strings.TrimSpace(line)
			if stripped == "" {
				continue
			}
			indent := indentWidth(line)
			if definitionRE.MatchString(stripped) {
				matches = append(matches, found{i, indent, stripped, true})
				indents[indent] = true
			} else if indent == 0 && isConstant(stripped) {
				matches = append(matches, found{i, 0, stripped, false})
				indents[0] = true
			}
		}
		// Turn indentation into nesting depth: 0 for top level, 1 for methods, ...
		levels := make([]int, 0, len(indents))
		for indent := range indents {
			levels = append(levels, indent)
		}
		sort.Ints(levels)
		rank := map[int]int{}
		for n, indent := range levels {
			rank[indent] = n
		}
		for _, m := range matches {
			end := m.index + 1
			if m.definition {
				end = blockEnd(lines, m.index, m.indent)
			}
			entries = append(entries, outlineEntry{m.index + 1, end, rank[m.indent], m.text})
		}
	}

	for i := range entries {
		if utf8.RuneCountInString(entries[i].text) > maxOutlineTextChars {
			entries[i].text = string([]rune(entries[i].text)[:maxOutlineTextChars-1]) + "…"
		}
	}
	return entries
}

// buildOutline returns outline lines such as `L126-L167  def build_chunks(...)`, or "" when
// nothing matched. When the outline exceeds maxChars, the deepest entries are dropped first.
func buildOutline(lines []string, suffix string, maxChars int) string {
	entries := outlineEntries(lines, markdownExtensions[strings.ToLower(suffix)])
	if len(entries) == 0 {
		return ""
	}

	render := func(items []outlineEntry) []string {
		rows := make([]string, len(items))
		for i, e := range items {
			where := fmt.Sprintf("L%d", e.first)
			if e.first != e.last {
				where = fmt.Sprintf("L%d-L%d", e.first, e.last)
			}
			rows[i] = fmt.Sprintf("%-12s %s%s", where, strings.Repeat("  ", e.depth), e.text)
		}
		return rows
	}
	size := func(rows []string) int {
		total := 0
		for _, r := range rows {
			total += utf8.RuneCountInString(r) + 1
		}
		return total
	}

	depthSet := map[int]bool{}
	for _, e := range entries {
		if e.depth > 0 {
			depthSet[e.depth] = true
		}
	}
	depths := make([]int, 0, len(depthSet))
	for d := range depthSet {
		depths = append(depths, d)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(depths)))

	kept := entries
	for _, maxDepth := range depths {
		if size(render(kept)) <= maxChars {
			break
		}
		var shallower []outlineEntry
		for _, e := range kept {
			if e.depth < maxDepth {
				shallower = append(shallower, e)
			}
		}
		kept = shallower
	}
	rows := render(kept)
	omitted := len(entries) - len(kept)
	for len(rows) > 0 && size(rows) > maxChars {
		rows = rows[:len(rows)-1]
		omitted++
	}
	if omitted > 0 {
		rows = append(rows, fmt.Sprintf("… %d more entries not shown", omitted))
	}
	return strings.Join(rows, "\n")
}
