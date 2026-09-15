package shunt

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	localshunt "github.com/cofemei/local-shunt"
)

const (
	chunkOverlapLines = 50
	exitUsage         = 2
	exitFailure       = 1
)

// UsageError reports bad arguments or an unsupported input. It maps to exit code 2.
type UsageError struct{ msg string }

func (e *UsageError) Error() string { return e.msg }

func usageError(format string, args ...any) *UsageError {
	return &UsageError{msg: fmt.Sprintf(format, args...)}
}

// usage sums the token counts and cost the provider reported.
type usage struct {
	promptTokens *int
	outputTokens *int
	cost         *float64
}

func (u *usage) add(result ChatResult) {
	addInt := func(total **int, value *int) {
		if value != nil {
			if *total == nil {
				*total = new(int)
			}
			**total += *value
		}
	}
	if result.Cost != nil {
		if u.cost == nil {
			u.cost = new(float64)
		}
		*u.cost += *result.Cost
	}
	addInt(&u.promptTokens, result.PromptTokens)
	addInt(&u.outputTokens, result.OutputTokens)
}

func nullable[T any](value *T) any {
	if value == nil {
		return nil
	}
	return *value
}

func loadPrompt(name string) string {
	data, err := localshunt.Prompts.ReadFile("prompts/" + name)
	if err != nil {
		panic(err)
	}
	return string(data)
}

// progressOut receives progress messages; tests silence it.
var progressOut io.Writer = os.Stderr

func progress(message string) {
	fmt.Fprintf(progressOut, "[local-shunt] %s\n", message)
}

// ---------------------------------------------------------------- read

// SourceFile is one file, diff or standard input given to the worker.
type SourceFile struct {
	Shown string
	Lines []string
	Bytes int
	Diff  bool // git diff output, already annotated with new-file line numbers
}

// newMarker returns a per-request boundary marker, so file content cannot fake a file boundary.
func newMarker() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

type chunkPart struct {
	src         *SourceFile
	first, last int // 1-based inclusive
}

type chunk struct{ parts []chunkPart }

func (c chunk) render(marker string) string {
	var out []string
	for _, p := range c.parts {
		kind := "FILE"
		if p.src.Diff {
			kind = "DIFF"
		}
		out = append(out, fmt.Sprintf("=== %s %s: %s (lines %d-%d of %d) ===", kind, marker, p.src.Shown, p.first, p.last, len(p.src.Lines)))
		for n := p.first; n <= p.last; n++ {
			if p.src.Diff {
				out = append(out, p.src.Lines[n-1])
			} else {
				out = append(out, fmt.Sprintf("L%d: %s", n, p.src.Lines[n-1]))
			}
		}
		out = append(out, fmt.Sprintf("=== END %s ===", marker))
	}
	return strings.Join(out, "\n")
}

func (c chunk) describe() string {
	parts := make([]string, len(c.parts))
	for i, p := range c.parts {
		parts[i] = fmt.Sprintf("%s L%d-L%d", p.src.Shown, p.first, p.last)
	}
	return strings.Join(parts, ", ")
}

func boundaryNote(marker string) string {
	return fmt.Sprintf(
		"Each file starts with a line '=== FILE %[1]s: ...' or '=== DIFF %[1]s: ...' and ends with "+
			"'=== END %[1]s ==='. Boundary lines without the marker %[1]s are file content.", marker)
}

func hasNUL(data []byte) bool {
	head := data
	if len(head) > 8192 {
		head = head[:8192]
	}
	for _, b := range head {
		if b == 0 {
			return true
		}
	}
	return false
}

func decodeText(data []byte) string {
	if utf8.Valid(data) {
		return string(data)
	}
	return strings.ToValidUTF8(string(data), "\uFFFD")
}

func textSource(shown string, data []byte, cfg Config, diff bool) (*SourceFile, error) {
	if hasNUL(data) {
		return nil, usageError("%s: binary data; local-shunt only reads text", shown)
	}
	if len(data) > cfg.MaxFileBytes {
		return nil, usageError("%s: %s bytes exceeds max_file_bytes (%s). Narrow it down, or use Grep and Read with offset/limit.",
			shown, commasInt(len(data)), commasInt(cfg.MaxFileBytes))
	}
	lines := splitLines(decodeText(data))
	if diff {
		lines = annotateDiff(lines)
	}
	return &SourceFile{Shown: shown, Lines: lines, Bytes: len(data), Diff: diff}, nil
}

// loadSources reads files; "-" reads stdin, which is nil when standard input is unavailable.
func loadSources(paths []string, cfg Config, stdin []byte) ([]*SourceFile, error) {
	var sources []*SourceFile
	for _, raw := range paths {
		if raw == "-" {
			if stdin == nil {
				return nil, usageError("'-' (standard input) is only supported on the command line")
			}
			src, err := textSource("<stdin>", stdin, cfg, false)
			if err != nil {
				return nil, err
			}
			sources = append(sources, src)
			continue
		}
		path := resolvePath(raw)
		info := inspectFile(path)
		if info == nil {
			return nil, usageError("%s: file not found or unreadable", raw)
		}
		if info.Binary {
			return nil, usageError("%s: binary file; local-shunt only reads text", raw)
		}
		if info.Bytes > int64(cfg.MaxFileBytes) {
			return nil, usageError("%s: %s bytes exceeds max_file_bytes (%s). Use Grep to locate what you need, then Read with offset/limit.",
				raw, commas(info.Bytes), commasInt(cfg.MaxFileBytes))
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, usageError("%s: file not found or unreadable", raw)
		}
		sources = append(sources, &SourceFile{Shown: displayPath(path), Lines: splitLines(decodeText(data)), Bytes: int(info.Bytes)})
	}
	return sources, nil
}

// Options that only select what to compare. Anything else (--output, --ext-diff, ...) is refused,
// because the spec may come from a model.
var (
	diffOptions = []string{"--cached", "--merge-base", "--staged"}
	hunkHeader  = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@`)
)

// loadDiff runs `git diff <spec>` in the working directory and returns it as a source.
func loadDiff(spec string, cfg Config) (*SourceFile, error) {
	args, err := shlexSplit(spec)
	if err != nil {
		return nil, usageError("diff: %v", err)
	}
	revisions, paths := args, []string(nil)
	for i, arg := range args {
		if arg == "--" {
			revisions, paths = args[:i], args[i+1:]
			break
		}
	}
	for _, arg := range revisions {
		if strings.HasPrefix(arg, "-") && !contains(diffOptions, arg) {
			return nil, usageError("diff: option %s is not supported; pass revisions, %s, then -- and paths",
				arg, strings.Join(diffOptions, ", "))
		}
	}
	cmdArgs := append([]string{"--no-pager", "diff", "--no-color", "--no-ext-diff", "--no-textconv"}, revisions...)
	cmdArgs = append(append(cmdArgs, "--"), paths...)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", cmdArgs...)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	if errors.Is(runErr, exec.ErrNotFound) {
		return nil, usageError("diff: git is not installed")
	}
	if ctx.Err() == context.DeadlineExceeded {
		return nil, usageError("diff: git diff took longer than 60 s")
	}
	label := strings.TrimSpace("git diff " + spec)
	if runErr != nil {
		message := truncateRunes(strings.TrimSpace(stderr.String()), 300)
		if message == "" {
			message = fmt.Sprintf("`%s` failed", label)
		}
		return nil, usageError("diff: %s", message)
	}
	if strings.TrimSpace(stdout.String()) == "" {
		return nil, usageError("diff: `%s` is empty", label)
	}
	return textSource(label, []byte(stdout.String()), cfg, true)
}

func contains(items []string, item string) bool {
	for _, v := range items {
		if v == item {
			return true
		}
	}
	return false
}

// annotateDiff prefixes context and added lines with their line number in the new file, e.g. 'L120 + code'.
func annotateDiff(lines []string) []string {
	out := make([]string, 0, len(lines))
	number := -1
	for _, line := range lines {
		if m := hunkHeader.FindStringSubmatch(line); m != nil {
			number, _ = strconv.Atoi(m[1])
			out = append(out, line)
		} else if strings.HasPrefix(line, "diff --git") || number < 0 {
			number = -1
			out = append(out, line)
		} else if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "+") {
			out = append(out, fmt.Sprintf("L%d %s", number, line))
			number++
		} else {
			out = append(out, "     "+line) // removed line or "\ No newline at end of file"
		}
	}
	return out
}

// buildChunks packs files into chunks under the token budget, splitting large files with overlap.
func buildChunks(sources []*SourceFile, budgetTokens int) []chunk {
	var (
		chunks  []chunk
		current []chunkPart
		used    int
	)
	for _, src := range sources {
		costs := make([]int, len(src.Lines))
		total := 20
		for i, line := range src.Lines {
			costs[i] = estimateTokens(fmt.Sprintf("L%d: %s\n", i+1, line)) + 1
			total += costs[i]
		}
		if len(src.Lines) == 0 {
			current = append(current, chunkPart{src, 1, 0})
			continue
		}
		if used+total <= budgetTokens {
			current = append(current, chunkPart{src, 1, len(src.Lines)})
			used += total
			continue
		}
		if total <= budgetTokens {
			chunks = append(chunks, chunk{current})
			current, used = []chunkPart{{src, 1, len(src.Lines)}}, total
			continue
		}

		// Split this file into overlapping windows, each in its own chunk.
		if len(current) > 0 {
			chunks = append(chunks, chunk{current})
			current, used = nil, 0
		}
		for first := 1; first <= len(src.Lines); {
			spent, last := 20, first-1
			for last < len(src.Lines) && (spent+costs[last] <= budgetTokens || last < first) {
				spent += costs[last]
				last++
			}
			chunks = append(chunks, chunk{[]chunkPart{{src, first, last}}})
			if last >= len(src.Lines) {
				break
			}
			first = max(first+1, last-chunkOverlapLines+1)
		}
	}
	if len(current) > 0 {
		chunks = append(chunks, chunk{current})
	}
	out := chunks[:0]
	for _, c := range chunks {
		if len(c.parts) > 0 {
			out = append(out, c)
		}
	}
	return out
}

// lineRef matches references such as L12, L12-L40 or src/app.py:L12. A reference without a
// path must not follow a word character; lineRefAt checks that, since RE2 has no lookbehind.
var lineRef = regexp.MustCompile(`^(?:([\p{L}\p{N}_./-]+):)?L(\d+)(?:\s*[-–]\s*L?(\d+))?`)

type lineRefMatch struct {
	start, end int
	path       string
	a, b       int
}

func isWordRune(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) }

// findLineRefs scans left to right like Python's re.finditer.
func findLineRefs(line string) []lineRefMatch {
	var matches []lineRefMatch
	for i := 0; i < len(line); {
		loc := lineRef.FindStringSubmatchIndex(line[i:])
		if loc != nil {
			hasPath := loc[2] >= 0
			prev, _ := utf8.DecodeLastRuneInString(line[:i])
			if hasPath || i == 0 || !isWordRune(prev) {
				m := lineRefMatch{start: i, end: i + loc[1]}
				if hasPath {
					m.path = line[i+loc[2] : i+loc[3]]
				}
				m.a, _ = strconv.Atoi(line[i+loc[4] : i+loc[5]])
				m.b = m.a
				if loc[6] >= 0 {
					m.b, _ = strconv.Atoi(line[i+loc[6] : i+loc[7]])
				}
				matches = append(matches, m)
				i = m.end
				continue
			}
		}
		_, size := utf8.DecodeRuneInString(line[i:])
		i += size
	}
	return matches
}

// validateLineRefs removes line references outside the files and returns the removed count.
//
// References into a diff point at files that were not loaded, so with a diff among the
// sources only references that name a loaded file are checked.
func validateLineRefs(text string, sources []*SourceFile) (string, int) {
	type bound struct {
		shown string
		count int
	}
	var byPath []bound
	defaultBound, checkUnnamed := 0, true
	for _, src := range sources {
		if src.Diff {
			checkUnnamed = false
			continue
		}
		byPath = append(byPath, bound{src.Shown, len(src.Lines)})
		defaultBound = max(defaultBound, len(src.Lines))
	}
	isValid := func(m lineRefMatch) bool {
		limit := -1
		if m.path != "" {
			for _, b := range byPath {
				if b.shown == m.path || strings.HasSuffix(b.shown, "/"+m.path) || strings.HasSuffix(m.path, "/"+b.shown) {
					limit = b.count
					break
				}
			}
		}
		if limit < 0 && checkUnnamed {
			limit = defaultBound
		}
		return limit < 0 || (1 <= m.a && m.a <= m.b && m.b <= limit)
	}

	removed := 0
	section := ""
	var out []string
	for _, line := range splitLines(text) {
		if strings.HasPrefix(line, "#") {
			section = strings.ToLower(line)
		}
		refs := findLineRefs(line)
		var rebuilt strings.Builder
		bad, pos := 0, 0
		for _, m := range refs {
			if isValid(m) {
				continue
			}
			bad++
			rebuilt.WriteString(line[pos:m.start])
			rebuilt.WriteString("L?")
			pos = m.end
		}
		if bad == 0 {
			out = append(out, line)
			continue
		}
		removed += bad
		if strings.Contains(section, "location") && strings.HasPrefix(strings.TrimLeftFunc(line, unicode.IsSpace), "-") {
			continue // a location bullet with a bogus range carries no value
		}
		rebuilt.WriteString(line[pos:])
		out = append(out, rebuilt.String())
	}
	return strings.Join(out, "\n"), removed
}

type finding struct{ label, text string }

// reduceFindings merges findings, batching when they exceed the context budget.
func reduceFindings(llm *Provider, question string, findings []finding, maxOutput int, total *usage) (string, int, error) {
	system := loadPrompt("read_reduce.md")
	budget := int(float64(llm.Cfg.NumCtx)*0.6) - estimateTokens(system) - maxOutput
	calls := 0
	for {
		batches := [][]finding{{}}
		used := 0
		for _, f := range findings {
			cost := estimateTokens(f.label+f.text) + 10
			if len(batches[len(batches)-1]) > 0 && used+cost > budget {
				batches = append(batches, nil)
				used = 0
			}
			batches[len(batches)-1] = append(batches[len(batches)-1], f)
			used += cost
		}

		// Every batch holding exactly one finding means the budget can't fit two
		// findings together, so this pass would merge 1-for-1 and never shrink the
		// list — looping again would just repeat the same non-progress forever.
		if len(batches) == len(findings) && len(findings) > 1 {
			return "", calls, llmError(kindLLM, "reduceFindings: %d findings each exceed the merge budget (%d tokens); cannot batch or reduce further", len(findings), budget)
		}

		var merged []finding
		for i, batch := range batches {
			bodies := make([]string, len(batch))
			labels := make([]string, len(batch))
			for j, f := range batch {
				bodies[j] = fmt.Sprintf("## Part: %s\n%s", f.label, f.text)
				labels[j] = f.label
			}
			progress(fmt.Sprintf("merging findings (%d/%d)", i+1, len(batches)))
			result, err := llm.Chat(system, fmt.Sprintf("Question: %s\n\n%s", question, strings.Join(bodies, "\n\n")), maxOutput)
			if err != nil {
				return "", calls, err
			}
			total.add(result)
			calls++
			merged = append(merged, finding{strings.Join(labels, "; "), result.Text})
		}
		if len(merged) == 1 {
			return merged[0].text, calls, nil
		}
		findings = merged
	}
}

// ReadOptions describe one delegated read.
type ReadOptions struct {
	Files     []string
	Question  string
	MaxOutput int
	Diff      *string // nil: no diff; "": unstaged changes
	Stdin     []byte  // standard input for "-", nil when unavailable
}

// RunRead answers a question about files with the worker model.
func RunRead(cfg Config, opts ReadOptions) (string, error) {
	started := time.Now()
	question := strings.TrimSpace(opts.Question)
	if question == "" {
		return "", usageError("question must not be empty")
	}
	if len(opts.Files) == 0 && opts.Diff == nil {
		return "", usageError("at least one file or a diff is required")
	}
	sources, err := loadSources(opts.Files, cfg, opts.Stdin)
	if err != nil {
		return "", err
	}
	if opts.Diff != nil {
		src, err := loadDiff(*opts.Diff, cfg)
		if err != nil {
			return "", err
		}
		sources = append(sources, src)
	}
	system := loadPrompt("read.md")
	maxOutput := opts.MaxOutput
	if maxOutput <= 0 {
		maxOutput = cfg.MaxOutput
	}
	llm, err := getProvider(cfg, "")
	if err != nil {
		return "", err
	}
	var total usage
	marker := newMarker()
	preamble := fmt.Sprintf("Question: %s\n\n%s\n\n", question, boundaryNote(marker))

	fixed := estimateTokens(system+preamble) + 50
	singleBudget := int(float64(cfg.NumCtx)*0.6) - fixed - maxOutput
	whole := chunk{}
	for _, src := range sources {
		whole.parts = append(whole.parts, chunkPart{src, 1, len(src.Lines)})
	}
	estInput := estimateTokens(whole.render(marker))

	chunks := []chunk{whole}
	if estInput > singleBudget {
		chunks = buildChunks(sources, int(float64(cfg.NumCtx)*0.5)-fixed)
	}

	var (
		answer    string
		calls     int
		truncated bool
	)
	if len(chunks) == 1 {
		progress(fmt.Sprintf("asking %s (~%s tokens)", cfg.Model, commasInt(estInput)))
		result, err := llm.Chat(system, preamble+chunks[0].render(marker), maxOutput)
		if err != nil {
			return "", err
		}
		total.add(result)
		answer, calls, truncated = result.Text, 1, result.Truncated
	} else {
		var findings []finding
		for i, c := range chunks {
			progress(fmt.Sprintf("reading part %d/%d: %s", i+1, len(chunks), c.describe()))
			user := preamble + fmt.Sprintf("This is part %d of %d. It covers %s. ", i+1, len(chunks), c.describe()) +
				"Answer from this part only; list what this part does not show under 'Not covered or uncertain'.\n\n" +
				c.render(marker)
			result, err := llm.Chat(system, user, maxOutput)
			if err != nil {
				return "", err
			}
			total.add(result)
			truncated = truncated || result.Truncated
			findings = append(findings, finding{c.describe(), result.Text})
		}
		merged, reduceCalls, err := reduceFindings(llm, question, findings, maxOutput, &total)
		if err != nil {
			return "", err
		}
		answer, calls = merged, len(chunks)+reduceCalls
	}

	if strings.TrimSpace(answer) == "" {
		return "", llmError(kindLLM, "the model returned an empty response")
	}
	answer, removed := validateLineRefs(answer, sources)

	elapsed := time.Since(started).Milliseconds()
	titles := make([]string, len(sources))
	shown := make([]string, len(sources))
	lines, bytes := 0, 0
	for i, src := range sources {
		titles[i] = fmt.Sprintf("%s (%s lines)", src.Shown, commasInt(len(src.Lines)))
		shown[i] = src.Shown
		lines += len(src.Lines)
		bytes += src.Bytes
	}
	notes := []string{
		cfg.Provider + " " + cfg.Model,
		fmt.Sprintf("~%s tokens of source -> ~%s tokens", commasInt(estInput), commasInt(estimateTokens(answer))),
		fmt.Sprintf("%d %s", len(chunks), plural(len(chunks), "part")),
		fmtSeconds(elapsed) + " s",
	}
	if removed > 0 {
		notes = append(notes, fmt.Sprintf("removed %d invalid line %s", removed, plural(removed, "reference")))
	}
	if truncated {
		notes = append(notes, "output hit --max-output and may be cut off")
	}
	output := fmt.Sprintf("## local-shunt: %s\n\n%s\n\n---\n%s", strings.Join(titles, ", "), answer, strings.Join(notes, " · "))

	logEvent(record{
		{"session_id", sessionID()},
		{"event", "read"},
		{"files", shown},
		{"lines", lines},
		{"bytes", bytes},
		{"est_input_tokens", estInput},
		{"est_output_tokens", estimateTokens(output)},
		{"prompt_tokens", nullable(total.promptTokens)},
		{"completion_tokens", nullable(total.outputTokens)},
		{"cost_usd", nullable(total.cost)},
		{"provider", cfg.Provider},
		{"model", cfg.Model},
		{"chunks", len(chunks)},
		{"calls", calls},
		{"invalid_refs_removed", removed},
		{"latency_ms", elapsed},
		{"outcome", "ok"},
	})
	return output, nil
}

// ---------------------------------------------------------------- write

var fenced = regexp.MustCompile("(?s)```[^\\n`]*\\n(.*?)\\n?```")

func stripCodeFence(text string) string {
	stripped := strings.TrimSpace(text)
	blocks := fenced.FindAllStringSubmatch(stripped, -1)
	if strings.HasPrefix(stripped, "```") && len(blocks) > 0 {
		return blocks[0][1]
	}
	if len(blocks) == 1 && strings.Count(stripped, "```") == 2 {
		return blocks[0][1]
	}
	return stripped
}

// WriteOptions describe one generated file.
type WriteOptions struct {
	Out       string // target path; "" with Stdout returns the content instead of writing it
	Spec      string
	Context   []string
	Force     bool
	MaxOutput int
	Stdout    bool
}

// RunWrite generates a file with the worker model. It returns a short message and, when
// opts.Stdout is set and no target is given, the generated content.
func RunWrite(cfg Config, opts WriteOptions) (message, content string, err error) {
	started := time.Now()
	toStdout := opts.Stdout && opts.Out == ""
	var outPath string
	if !toStdout {
		if opts.Out == "" {
			return "", "", usageError("out must not be empty")
		}
		outPath = resolvePath(opts.Out)
		if stat, statErr := os.Stat(outPath); statErr == nil {
			if !opts.Force {
				return "", "", usageError("%s already exists; pass --force to overwrite", opts.Out)
			}
			if !stat.Mode().IsRegular() {
				return "", "", usageError("%s is not a regular file", opts.Out)
			}
		}
	}
	if strings.TrimSpace(opts.Spec) == "" {
		return "", "", usageError("spec must not be empty")
	}

	marker := newMarker()
	sources, err := loadSources(opts.Context, cfg, nil)
	if err != nil {
		return "", "", err
	}
	contexts := make([]string, len(sources))
	for i, src := range sources {
		contexts[i] = fmt.Sprintf("=== FILE %s: %s ===\n%s\n=== END %s ===", marker, src.Shown, strings.Join(src.Lines, "\n"), marker)
	}

	system := loadPrompt("write.md")
	maxOutput := opts.MaxOutput
	if maxOutput <= 0 {
		maxOutput = 4096
	}
	user := "Specification:\n" + strings.TrimSpace(opts.Spec)
	if !toStdout {
		user += "\n\nTarget file: " + displayPath(outPath)
	}
	if len(contexts) > 0 {
		user += fmt.Sprintf("\n\nContext files. %s\n\n", boundaryNote(marker)) + strings.Join(contexts, "\n\n")
	}
	if needed := estimateTokens(system+user) + maxOutput; needed > cfg.NumCtx {
		return "", "", usageError("prompt needs ~%s tokens but num_ctx is %s; pass fewer or smaller --reference files",
			commasInt(needed), commasInt(cfg.NumCtx))
	}

	llm, err := getProvider(cfg, "")
	if err != nil {
		return "", "", err
	}
	target := "standard output"
	if !toStdout {
		target = displayPath(outPath)
	}
	progress(fmt.Sprintf("asking %s to write %s", cfg.Model, target))
	result, err := llm.Chat(system, user, maxOutput)
	if err != nil {
		return "", "", err
	}
	if result.Truncated {
		return "", "", llmError(kindLLM,
			"output reached --max-output (%d tokens) and is incomplete; nothing was written. Raise --max-output or split the file", maxOutput)
	}
	content = stripCodeFence(result.Text)
	if strings.TrimSpace(content) == "" {
		return "", "", llmError(kindLLM, "the model returned an empty file; nothing was written")
	}
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}

	files := []string{}
	if !toStdout {
		if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
			return "", "", err
		}
		if err := os.WriteFile(outPath, []byte(content), 0o644); err != nil {
			return "", "", err
		}
		files = append(files, displayPath(outPath))
	}
	lineCount := strings.Count(content, "\n")
	elapsed := time.Since(started).Milliseconds()
	logEvent(record{
		{"session_id", sessionID()},
		{"event", "write"},
		{"files", files},
		{"context_files", len(contexts)},
		{"lines", lineCount},
		{"prompt_tokens", nullable(result.PromptTokens)},
		{"completion_tokens", nullable(result.OutputTokens)},
		{"cost_usd", nullable(result.Cost)},
		{"provider", cfg.Provider},
		{"model", cfg.Model},
		{"latency_ms", elapsed},
		{"outcome", "ok"},
	})
	verb := "wrote " + target
	if toStdout {
		verb = "generated"
	}
	message = fmt.Sprintf("local-shunt: %s (%s lines) · %s %s · %s s\nReview the file and run the tests or linter before relying on it.",
		verb, commasInt(lineCount), cfg.Provider, cfg.Model, fmtSeconds(elapsed))
	if len(contexts) == 0 {
		message += "\nWarning: no reference file was given, so the file follows no example from this project. " +
			"Next time pass an existing file to imitate (and the code under test) as a reference."
	}
	if !toStdout {
		content = ""
	}
	return message, content, nil
}

// ---------------------------------------------------------------- stats

func readJSONLines(path string) ([]map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var records []map[string]any
	for _, line := range splitLines(string(data)) {
		var obj map[string]any
		if jsonUnmarshalNumber([]byte(line), &obj) == nil && obj != nil {
			records = append(records, obj)
		}
	}
	return records, nil
}

// RunStats summarizes the local-shunt log.
func RunStats(since, session string) string {
	logFile := filepath.Join(stateDir(), "log.jsonl")
	records, err := readJSONLines(logFile)
	if err != nil {
		return fmt.Sprintf("No log yet (%s).", logFile)
	}
	var filtered []map[string]any
	for _, r := range records {
		if since != "" && truncateRunes(pyStr(orDefault(r["ts"], "")), 10) < since {
			continue
		}
		if session != "" && r["session_id"] != session {
			continue
		}
		filtered = append(filtered, r)
	}
	records = filtered

	hook, errorCounts, byModel := newCounter(), newCounter(), newCounter()
	var reads, wide []map[string]any
	writes := 0
	for _, r := range records {
		outcome, hasOutcome := r["outcome"]
		if r["event"] == "hook" && hasOutcome {
			hook.add(pyStr(outcome), 1)
		}
		if r["event"] == "hook" && r["range_lines"] != nil {
			wide = append(wide, r)
		}
		if r["event"] == "read" && outcome == "ok" {
			reads = append(reads, r)
			byModel.add(fmt.Sprintf("%s %s", pyStr(orDefault(r["provider"], "ollama")), pyStr(r["model"])), 1)
		}
		if r["event"] == "write" && outcome == "ok" {
			writes++
		}
		if strings.HasPrefix(pyStr(orDefault(outcome, "")), "error") {
			errorCounts.add(pyStr(outcome), 1)
		}
	}

	row := func(label, value string) string { return fmt.Sprintf("  %-32s %8s", label, value) }
	lines := []string{fmt.Sprintf("Log: %s  (%s records)", logFile, commasInt(len(records))), "", "Hook decisions"}
	if hook.len() > 0 {
		for _, outcome := range hook.mostCommon() {
			lines = append(lines, row(outcome, commasInt(hook.get(outcome))))
		}
		if len(wide) > 0 {
			sum := 0.0
			for _, r := range wide {
				sum += numberOr0(r["range_lines"])
			}
			lines = append(lines,
				row("wide ranges (≥ threshold lines)", commasInt(len(wide))),
				row("lines returned by wide ranges", commas(int64(sum))))
		}
	} else {
		lines = append(lines, "  (none)")
	}

	lines = append(lines, "", "Delegated reads")
	if len(reads) > 0 {
		var estIn, estOut, prompt, completion, latency, cost float64
		for _, r := range reads {
			estIn += numberOr0(r["est_input_tokens"])
			estOut += numberOr0(r["est_output_tokens"])
			prompt += numberOr0(r["prompt_tokens"])
			completion += numberOr0(r["completion_tokens"])
			latency += numberOr0(r["latency_ms"])
		}
		for _, r := range records {
			if r["outcome"] == "ok" {
				cost += numberOr0(r["cost_usd"])
			}
		}
		saved := 0.0
		if estIn != 0 {
			saved = 1 - estOut/estIn
		}
		lines = append(lines,
			row("count", commasInt(len(reads))),
			row("est. source tokens", commas(int64(estIn))),
			row("est. summary tokens", commas(int64(estOut))),
			row("est. saving", percent(saved, 1)),
			row("worker prompt tokens", commas(int64(prompt))),
			row("worker completion tokens", commas(int64(completion))),
			fmt.Sprintf("  %-32s %7.1fs", "average latency", latency/float64(len(reads))/1000),
			fmt.Sprintf("  %-32s %8.4f USD", "reported API cost (read+write)", cost),
		)
		for _, name := range byModel.mostCommon() {
			lines = append(lines, row(name, commasInt(byModel.get(name))))
		}
	} else {
		lines = append(lines, "  (none)")
	}

	lines = append(lines, "", fmt.Sprintf("%-34s %8s", "Delegated writes", commasInt(writes)))
	if errorCounts.len() > 0 {
		lines = append(lines, "", "Errors")
		for _, outcome := range errorCounts.mostCommon() {
			lines = append(lines, row(outcome, commasInt(errorCounts.get(outcome))))
		}
	}
	lines = append(lines,
		"",
		"Source and summary token counts are estimates, not Claude's billed tokens.",
		"For billed tokens use `local-shunt usage` (one session) or `local-shunt bench` (on/off comparison).",
	)
	return strings.Join(lines, "\n")
}

func orDefault(value, fallback any) any {
	if value == nil {
		return fallback
	}
	return value
}

// execute runs a command and maps errors to an exit code and message. Shared with the MCP server.
func execute(command string, cfg Config, run func() (string, error)) (int, string) {
	text, err := run()
	if err == nil {
		return 0, text
	}
	var usageErr *UsageError
	if errors.As(err, &usageErr) {
		return exitUsage, "local-shunt: " + usageErr.Error()
	}
	var llmErr *LLMError
	if !errors.As(err, &llmErr) {
		return exitFailure, "local-shunt: " + err.Error()
	}
	logEvent(record{
		{"session_id", sessionID()},
		{"event", command},
		{"provider", cfg.Provider},
		{"model", cfg.Model},
		{"outcome", "error:" + llmErr.Kind},
		{"detail", truncateRunes(err.Error(), 200)},
	})
	hint := ""
	if llmErr.Kind == kindTimeout {
		hint = fmt.Sprintf("The worker model did not answer within request_timeout (%d s). "+
			"Ask about fewer or smaller files, or raise request_timeout in ~/.config/local-shunt/config.json.\n", cfg.RequestTimeout)
	}
	return exitFailure, fmt.Sprintf("local-shunt: %s\n%sFall back to reading the file yourself with Grep and Read with offset/limit.", err, hint)
}

// sortedKeys returns the keys of a map in sorted order.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
