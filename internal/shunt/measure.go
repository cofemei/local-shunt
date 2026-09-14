package shunt

// Measure Claude's own token usage.
//
// Two sources:
//
//   - Session transcripts. Claude Code writes every API response, with its `usage`, to
//     ~/.claude/projects/<project>/<session>.jsonl, so these counts are Claude's billed
//     tokens, not estimates.
//   - A/B benchmark runs. The same prompt goes through `claude -p` with local-shunt
//     enabled and disabled. Only a paired run shows what a task costs without the plugin.

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	mcpPrefix      = "mcp__plugin_local-shunt_local-shunt__"
	defaultTimeout = 900
	verifyTimeout  = 300
	verifyChars    = 1000

	// Prompt-caching price multipliers relative to uncached input tokens.
	cacheWrite5m = 1.25
	cacheWrite1h = 2.0
	cacheRead    = 0.1

	targetSaving = 0.70
)

var (
	workerTools         = []string{mcpPrefix + "shunt_read", mcpPrefix + "shunt_write"}
	defaultAllowedTools = []string{"Read", "Grep", "Glob", "Bash(bulk-read:*)", mcpPrefix + "shunt_read"}

	shuntBashRead  = regexp.MustCompile(`(?:\bbulk-read\b|local-shunt['"]?\s+(?:bulk-)?read\b|shunt\.py['"]?\s+read\b)`)
	shuntBashWrite = regexp.MustCompile(`(?:\bcode-write\b|local-shunt['"]?\s+(?:code-)?write\b|shunt\.py['"]?\s+write\b)`)
	denialRE       = regexp.MustCompile(`local-shunt: .+ is large \(`)
	taskIDRE       = regexp.MustCompile(`^[\w.-]+$`)
	sessionRefRE   = regexp.MustCompile(`^[\w-]+$`)

	readCategories       = []string{"Read", "Read (range)", "shunt_read (MCP)", "shunt read (Bash)"}
	delegationCategories = []string{"shunt_read (MCP)", "shunt read (Bash)"}
	// Names usable in a task's expect_tools and forbid_tools besides the tool categories themselves.
	toolGroups = map[string][]string{
		"delegate-read":  delegationCategories,
		"delegate-write": {"shunt_write (MCP)", "shunt write (Bash)"},
	}
)

// MeasureError reports a problem with a task file, results file or benchmark run.
type MeasureError struct{ msg string }

func (e *MeasureError) Error() string { return e.msg }

func measureError(format string, args ...any) *MeasureError {
	return &MeasureError{msg: fmt.Sprintf(format, args...)}
}

// ---------------------------------------------------------------- transcripts

// ClaudeUsage sums Claude's API usage and tool calls over a session.
type ClaudeUsage struct {
	SessionID             string
	Transcripts           int // main transcript plus subagent transcripts
	Requests              int
	InputTokens           int // uncached input
	CacheCreationTokens   int
	CacheCreation1hTokens int
	CacheReadTokens       int
	OutputTokens          int
	Models                *counter // API requests per model
	ToolCalls             *counter
	ToolResultTokens      *counter // estimated from the result text
	Denials               int
}

func (u *ClaudeUsage) totalInputTokens() int {
	return u.InputTokens + u.CacheCreationTokens + u.CacheReadTokens
}

// weightedInputTokens weights input tokens by their price relative to uncached input.
func (u *ClaudeUsage) weightedInputTokens() int {
	fiveMin := u.CacheCreationTokens - u.CacheCreation1hTokens
	return int(math.RoundToEven(float64(u.InputTokens) + cacheWrite5m*float64(fiveMin) +
		cacheWrite1h*float64(u.CacheCreation1hTokens) + cacheRead*float64(u.CacheReadTokens)))
}

func (u *ClaudeUsage) readResultTokens() int {
	total := 0
	for _, c := range readCategories {
		total += u.ToolResultTokens.get(c)
	}
	return total
}

func (u *ClaudeUsage) delegations() int {
	total := 0
	for _, c := range delegationCategories {
		total += u.ToolCalls.get(c)
	}
	return total
}

func (u *ClaudeUsage) toRecord() record {
	return record{
		{"session_id", u.SessionID},
		{"transcripts", u.Transcripts},
		{"requests", u.Requests},
		{"input_tokens", u.InputTokens},
		{"cache_creation_tokens", u.CacheCreationTokens},
		{"cache_creation_1h_tokens", u.CacheCreation1hTokens},
		{"cache_read_tokens", u.CacheReadTokens},
		{"output_tokens", u.OutputTokens},
		{"models", u.Models},
		{"tool_calls", u.ToolCalls},
		{"tool_result_tokens", u.ToolResultTokens},
		{"denials", u.Denials},
		{"total_input_tokens", u.totalInputTokens()},
		{"weighted_input_tokens", u.weightedInputTokens()},
		{"read_result_tokens", u.readResultTokens()},
		{"delegations", u.delegations()},
	}
}

func claudeHome() string {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return dir
	}
	return filepath.Join(homeDir(), ".claude")
}

func isFile(path string) bool {
	stat, err := os.Stat(path)
	return err == nil && stat.Mode().IsRegular()
}

// findTranscript resolves a session ID or a transcript path to the transcript file.
func findTranscript(ref string) string {
	path := expandUser(ref)
	if filepath.Ext(path) == ".jsonl" {
		if isFile(path) {
			return path
		}
		return ""
	}
	if !sessionRefRE.MatchString(ref) {
		return ""
	}
	matches, _ := filepath.Glob(filepath.Join(claudeHome(), "projects", "*", ref+".jsonl"))
	best, bestTime := "", time.Time{}
	for _, m := range matches {
		stat, err := os.Stat(m)
		if err != nil || !stat.Mode().IsRegular() {
			continue
		}
		if best == "" || stat.ModTime().After(bestTime) {
			best, bestTime = m, stat.ModTime()
		}
	}
	return best
}

func textOf(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var parts []string
		for _, item := range v {
			if obj, ok := item.(map[string]any); ok && obj["type"] == "text" {
				parts = append(parts, pyStr(orDefault(obj["text"], "")))
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func categorize(name string, input map[string]any) string {
	command := pyStr(orDefault(input["command"], ""))
	switch {
	case name == "Read":
		if input["offset"] != nil || input["limit"] != nil {
			return "Read (range)"
		}
		return "Read"
	case name == "Bash" && shuntBashRead.MatchString(command):
		return "shunt read (Bash)"
	case name == "Bash" && shuntBashWrite.MatchString(command):
		return "shunt write (Bash)"
	case strings.HasSuffix(name, "__shunt_read"):
		return "shunt_read (MCP)"
	case strings.HasSuffix(name, "__shunt_write"):
		return "shunt_write (MCP)"
	}
	return name
}

func intOf(value any) int {
	f, _ := number(value)
	return int(f)
}

// parseTranscript sums API usage and tool calls over a transcript and its subagent transcripts.
func parseTranscript(path string) *ClaudeUsage {
	u := &ClaudeUsage{
		SessionID:        strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)),
		Models:           newCounter(),
		ToolCalls:        newCounter(),
		ToolResultTokens: newCounter(),
	}
	files := []string{path}
	subagents := filepath.Join(strings.TrimSuffix(path, filepath.Ext(path)), "subagents")
	if stat, err := os.Stat(subagents); err == nil && stat.IsDir() {
		found, _ := filepath.Glob(filepath.Join(subagents, "*.jsonl"))
		sort.Strings(found)
		files = append(files, found...)
	}
	for _, file := range files {
		u.Transcripts++
		data, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		// Claude Code writes one entry per content block, each repeating the response's usage.
		type response struct {
			model string
			usage map[string]any
		}
		responses := map[string]response{}
		var responseOrder []string
		toolCategories := map[string]string{}
		seenResults := map[string]bool{}

		for n, line := range splitLines(string(data)) {
			var entry map[string]any
			if json.Unmarshal([]byte(line), &entry) != nil {
				continue
			}
			message, ok := entry["message"].(map[string]any)
			if !ok {
				continue
			}
			var blocks []map[string]any
			if content, isList := message["content"].([]any); isList {
				for _, b := range content {
					if obj, isObj := b.(map[string]any); isObj {
						blocks = append(blocks, obj)
					}
				}
			}

			switch entry["type"] {
			case "assistant":
				model := "unknown"
				if truthy(message["model"]) {
					model = pyStr(message["model"])
				}
				if usageObj, isObj := message["usage"].(map[string]any); isObj && model != "<synthetic>" {
					key := message["id"]
					if !truthy(key) {
						key = entry["requestId"]
					}
					keyStr := fmt.Sprintf("line-%d", n)
					if truthy(key) {
						keyStr = pyStr(key)
					}
					if _, seen := responses[keyStr]; !seen {
						responseOrder = append(responseOrder, keyStr)
					}
					responses[keyStr] = response{model, usageObj}
				}
				for _, block := range blocks {
					toolID := pyStr(block["id"])
					if block["type"] != "tool_use" {
						continue
					}
					if _, seen := toolCategories[toolID]; seen {
						continue
					}
					input := asMap(block["input"])
					if input == nil {
						input = map[string]any{}
					}
					category := categorize(pyStr(orDefault(block["name"], "")), input)
					toolCategories[toolID] = category
					u.ToolCalls.add(category, 1)
				}
			case "user":
				for _, block := range blocks {
					toolID := pyStr(block["tool_use_id"])
					if block["type"] != "tool_result" || seenResults[toolID] {
						continue
					}
					seenResults[toolID] = true
					text := textOf(block["content"])
					if truthy(block["is_error"]) && denialRE.MatchString(text) {
						u.Denials++
					}
					category, ok := toolCategories[toolID]
					if !ok {
						category = "unknown"
					}
					u.ToolResultTokens.add(category, estimateTokens(text))
				}
			}
		}

		for _, key := range responseOrder {
			r := responses[key]
			u.Requests++
			u.Models.add(r.model, 1)
			u.InputTokens += intOf(r.usage["input_tokens"])
			u.CacheCreationTokens += intOf(r.usage["cache_creation_input_tokens"])
			u.CacheReadTokens += intOf(r.usage["cache_read_input_tokens"])
			u.OutputTokens += intOf(r.usage["output_tokens"])
			if details, isObj := r.usage["cache_creation"].(map[string]any); isObj {
				u.CacheCreation1hTokens += intOf(details["ephemeral_1h_input_tokens"])
			}
		}
	}
	return u
}

// ---------------------------------------------------------------- worker log

// WorkerUsage sums the worker's log records for one session.
type WorkerUsage struct {
	Reads, Writes, Errors             int
	EstSourceTokens, EstSummaryTokens int
	PromptTokens, CompletionTokens    int
	CostUSD                           float64
}

func (w WorkerUsage) toRecord() record {
	return record{
		{"reads", w.Reads},
		{"writes", w.Writes},
		{"errors", w.Errors},
		{"est_source_tokens", w.EstSourceTokens},
		{"est_summary_tokens", w.EstSummaryTokens},
		{"prompt_tokens", w.PromptTokens},
		{"completion_tokens", w.CompletionTokens},
		{"cost_usd", w.CostUSD},
	}
}

func readLog() []map[string]any {
	var records []map[string]any
	for _, name := range []string{"log.jsonl.1", "log.jsonl"} {
		found, err := readJSONLines(filepath.Join(stateDir(), name))
		if err == nil {
			records = append(records, found...)
		}
	}
	return records
}

func workerUsage(session string, records []map[string]any) WorkerUsage {
	var u WorkerUsage
	for _, r := range records {
		if r["session_id"] != session || (r["event"] != "read" && r["event"] != "write") {
			continue
		}
		if r["outcome"] != "ok" {
			u.Errors++
			continue
		}
		if r["event"] == "read" {
			u.Reads++
			u.EstSourceTokens += int(numberOr0(r["est_input_tokens"]))
			u.EstSummaryTokens += int(numberOr0(r["est_output_tokens"]))
		} else {
			u.Writes++
		}
		u.PromptTokens += int(numberOr0(r["prompt_tokens"]))
		u.CompletionTokens += int(numberOr0(r["completion_tokens"]))
		u.CostUSD += numberOr0(r["cost_usd"])
	}
	return u
}

func formatUsage(claude *ClaudeUsage, worker WorkerUsage) string {
	row := func(label string, value any, indent int) string {
		text := fmt.Sprint(value)
		if n, ok := value.(int); ok {
			text = commasInt(n)
		}
		return fmt.Sprintf("%s%-*s %14s", strings.Repeat(" ", indent), 36-indent, label, text)
	}
	byCount := func(c *counter) []string { return c.mostCommon() }

	title := "Session " + claude.SessionID
	if subagents := claude.Transcripts - 1; subagents > 0 {
		title += fmt.Sprintf(" (+%d subagent transcripts)", subagents)
	}
	lines := []string{title, "", row("Claude API requests", claude.Requests, 0)}
	for _, model := range byCount(claude.Models) {
		lines = append(lines, row(model, claude.Models.get(model), 2))
	}
	lines = append(lines,
		row("Input tokens", claude.totalInputTokens(), 0),
		row("uncached", claude.InputTokens, 2),
		row("cache write", claude.CacheCreationTokens, 2),
		row("cache read", claude.CacheReadTokens, 2),
		row("Input tokens, price-weighted", claude.weightedInputTokens(), 0),
		row("Output tokens", claude.OutputTokens, 0),
		"",
		fmt.Sprintf("%-28s %7s %20s", "Tool calls", "calls", "est. result tokens"),
	)
	for _, name := range byCount(claude.ToolCalls) {
		lines = append(lines, fmt.Sprintf("  %-26s %7s %20s", name, commasInt(claude.ToolCalls.get(name)), commasInt(claude.ToolResultTokens.get(name))))
	}
	lines = append(lines,
		row("File-read result tokens (est.)", claude.readResultTokens(), 0),
		row("Hook denials", claude.Denials, 0),
		"",
		"local-shunt worker in this session",
		row("delegated reads", worker.Reads, 2),
		row("delegated writes", worker.Writes, 2),
		row("errors", worker.Errors, 2),
		row("est. source tokens", worker.EstSourceTokens, 2),
		row("est. summary tokens", worker.EstSummaryTokens, 2),
		row("worker prompt tokens", worker.PromptTokens, 2),
		row("worker completion tokens", worker.CompletionTokens, 2),
		row("reported API cost (USD)", fmt.Sprintf("%.4f", worker.CostUSD), 2),
		"",
		"Claude's token counts come from the transcript; tool result and source tokens are estimates.",
		"A single session cannot show the saving. Run the same task with local-shunt disabled: local-shunt bench.",
	)
	return strings.Join(lines, "\n")
}

// RunUsage reports Claude's token usage for sessions, from their transcripts.
func RunUsage(refs []string, asJSON bool) (string, error) {
	if len(refs) == 0 {
		if id, ok := sessionID().(string); ok {
			refs = []string{id}
		}
	}
	if len(refs) == 0 {
		return "", usageError("pass a session ID or transcript path (no Claude Code session in the environment)")
	}
	records := readLog()
	var reports []string
	for _, ref := range refs {
		transcript := findTranscript(ref)
		if transcript == "" {
			return "", usageError("%s: transcript not found under %s", ref, filepath.Join(claudeHome(), "projects"))
		}
		claude := parseTranscript(transcript)
		worker := workerUsage(claude.SessionID, records)
		if asJSON {
			data, _ := marshalJSON(record{{"claude", claude.toRecord()}, {"worker", worker.toRecord()}})
			reports = append(reports, string(data))
		} else {
			reports = append(reports, formatUsage(claude, worker))
		}
	}
	if asJSON {
		return strings.Join(reports, "\n"), nil
	}
	return strings.Join(reports, "\n\n"), nil
}

// ---------------------------------------------------------------- benchmark

type benchTask struct {
	ID           string
	Prompt       string
	Cwd          string
	Expect       []string
	Model        string
	AllowedTools []string
	Timeout      int
	ExpectTools  []string // checked in 'on' runs only
	ForbidTools  []string // checked in 'on' runs only
	Isolate      bool     // run in a fresh copy of cwd, so edits do not touch the original
	Verify       []string // command run in the working directory after the run
}

func (t *benchTask) checksAnswer() bool { return len(t.Expect) > 0 || len(t.Verify) > 0 }

func taskStringList(value any, name, where string) ([]string, error) {
	switch v := value.(type) {
	case nil:
		return nil, nil
	case string:
		return []string{v}, nil
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			s, ok := item.(string)
			if !ok {
				return nil, measureError("%s: %s must be a string or a list of strings", where, name)
			}
			out = append(out, s)
		}
		return out, nil
	}
	return nil, measureError("%s: %s must be a string or a list of strings", where, name)
}

// toolCheckFailures returns the expect_tools and forbid_tools rules a run broke.
func toolCheckFailures(task *benchTask, toolCalls map[string]int) []string {
	used := func(name string) bool {
		group, ok := toolGroups[name]
		if !ok {
			group = []string{name}
		}
		for _, c := range group {
			if toolCalls[c] > 0 {
				return true
			}
		}
		return false
	}
	failures := []string{}
	for _, name := range task.ExpectTools {
		if !used(name) {
			failures = append(failures, "did not use "+name)
		}
	}
	for _, name := range task.ForbidTools {
		if used(name) {
			failures = append(failures, "used "+name)
		}
	}
	return failures
}

func errnoText(err error) string {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return pathErr.Err.Error()
	}
	return err.Error()
}

func loadTasks(path string) ([]*benchTask, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, measureError("%s: %s", path, errnoText(err))
	}
	var decoded any
	if err := json.Unmarshal(data, &decoded); err != nil {
		return nil, measureError("%s: invalid JSON (%v)", path, err)
	}
	defaults := map[string]any{}
	rawTasks := decoded
	if obj, ok := decoded.(map[string]any); ok {
		rawTasks = obj["tasks"]
		if obj["defaults"] != nil {
			d, isObj := obj["defaults"].(map[string]any)
			if !isObj {
				return nil, measureError(`%s: expected a non-empty "tasks" list`, path)
			}
			defaults = d
		}
	}
	list, ok := rawTasks.([]any)
	if !ok || len(list) == 0 {
		return nil, measureError(`%s: expected a non-empty "tasks" list`, path)
	}

	tasksDir, _ := filepath.Abs(filepath.Dir(path))
	tasksDir = resolveSymlinks(tasksDir)
	var tasks []*benchTask
	for i, raw := range list {
		obj, isObj := raw.(map[string]any)
		if !isObj {
			return nil, measureError("%s: task %d must be an object", path, i+1)
		}
		spec := map[string]any{}
		for k, v := range defaults {
			spec[k] = v
		}
		for k, v := range obj {
			spec[k] = v
		}
		taskID, idOK := spec["id"].(string)
		if !idOK || !taskIDRE.MatchString(taskID) {
			return nil, measureError("%s: task %d needs an id of letters, digits, '.', '_' or '-'", path, i+1)
		}
		for _, t := range tasks {
			if t.ID == taskID {
				return nil, measureError("%s: duplicate task id %s", path, pyQuote(taskID))
			}
		}
		prompt, promptOK := spec["prompt"].(string)
		if !promptOK || strings.TrimSpace(prompt) == "" {
			return nil, measureError("%s: task %s needs a prompt", path, taskID)
		}
		cwdValue := "."
		if truthy(spec["cwd"]) {
			cwdValue = pyStr(spec["cwd"])
		}
		cwd := expandUser(cwdValue)
		if !filepath.IsAbs(cwd) {
			cwd = filepath.Join(tasksDir, cwd)
		}
		cwd = resolveSymlinks(filepath.Clean(cwd))
		if stat, err := os.Stat(cwd); err != nil || !stat.IsDir() {
			return nil, measureError("%s: task %s: cwd %s is not a directory", path, taskID, cwd)
		}
		where := fmt.Sprintf("%s: task %s", path, taskID)
		task := &benchTask{ID: taskID, Prompt: prompt, Cwd: cwd, Timeout: defaultTimeout}
		if task.Expect, err = taskStringList(spec["expect"], "expect", where); err != nil {
			return nil, err
		}
		if task.AllowedTools, err = taskStringList(spec["allowed_tools"], "allowed_tools", where); err != nil {
			return nil, err
		}
		if len(task.AllowedTools) == 0 {
			task.AllowedTools = append([]string(nil), defaultAllowedTools...)
		}
		if task.ExpectTools, err = taskStringList(spec["expect_tools"], "expect_tools", where); err != nil {
			return nil, err
		}
		if task.ForbidTools, err = taskStringList(spec["forbid_tools"], "forbid_tools", where); err != nil {
			return nil, err
		}
		if task.Verify, err = taskStringList(spec["verify"], "verify", where); err != nil {
			return nil, err
		}
		for j, arg := range task.Verify {
			task.Verify[j] = strings.ReplaceAll(arg, "{tasks_dir}", tasksDir)
		}
		if value, set := spec["isolate"]; set {
			isolate, isBool := value.(bool)
			if !isBool {
				return nil, measureError("%s: isolate must be true or false", where)
			}
			task.Isolate = isolate
		}
		for _, pattern := range task.Expect {
			if _, err := regexp.Compile(pattern); err != nil {
				return nil, measureError("%s: task %s: invalid expect pattern %s: %v", path, taskID, pyQuote(pattern), err)
			}
		}
		if truthy(spec["timeout"]) {
			timeout, err := coerceInt(spec["timeout"])
			if err != nil {
				return nil, measureError("%s: task %s: timeout must be a number of seconds", path, taskID)
			}
			task.Timeout = timeout
		}
		if truthy(spec["model"]) {
			task.Model = pyStr(spec["model"])
		}
		tasks = append(tasks, task)
	}
	return tasks, nil
}

// prepareWorkspace returns the directory a run works in: the task's cwd, or a fresh copy of it.
func prepareWorkspace(task *benchTask) (string, error) {
	if !task.Isolate {
		return task.Cwd, nil
	}
	// A stable path per task keeps the transcripts of all its runs in one Claude Code project
	// and leaves the last run's files for inspection.
	workspace := filepath.Join(stateDir(), "bench", "workspaces", task.ID)
	_ = os.RemoveAll(workspace)
	return workspace, copyTree(task.Cwd, workspace)
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path != src && (d.Name() == "__pycache__" || d.Name() == ".git") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)
		stat, err := os.Stat(path) // follow symlinks, like shutil.copytree
		if err != nil {
			return err
		}
		if stat.IsDir() {
			return os.MkdirAll(target, stat.Mode().Perm()|0o700)
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, stat.Mode().Perm())
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	})
}

type processResult struct {
	exitCode       int
	stdout, stderr string
	timedOut       bool
}

func runProcess(args []string, stdin string, dir string, env []string, timeout time.Duration) (processResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Dir, cmd.Env = dir, env
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	result := processResult{stdout: stdout.String(), stderr: stderr.String()}
	if ctx.Err() == context.DeadlineExceeded {
		result.timedOut = true
		return result, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		result.exitCode = exitErr.ExitCode()
		return result, nil
	}
	return result, err
}

func runVerify(task *benchTask, workspace string) record {
	env := append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	proc, err := runProcess(task.Verify, "", workspace, env, verifyTimeout*time.Second)
	if err != nil {
		return record{{"exit_code", nil}, {"output", fmt.Sprintf("cannot run %s: %s", task.Verify[0], errnoText(err))}}
	}
	if proc.timedOut {
		return record{{"exit_code", nil}, {"output", fmt.Sprintf("timed out after %d s", verifyTimeout)}}
	}
	output := []rune(proc.stdout + proc.stderr)
	if len(output) > verifyChars {
		output = output[len(output)-verifyChars:]
	}
	return record{{"exit_code", proc.exitCode}, {"output", string(output)}}
}

// claudeCommand builds the `claude -p` command and environment for one run. The prompt goes to stdin.
func claudeCommand(task *benchTask, mode, session, claude, pluginDir string) ([]string, []string) {
	cmd := []string{claude, "-p", "--output-format", "json", "--session-id", session, "--plugin-dir", pluginDir}
	if task.Model != "" {
		cmd = append(cmd, "--model", task.Model)
	}
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if name == "CLAUDE_CODE_SESSION_ID" || name == "CLAUDE_SESSION_ID" || name == "LOCAL_SHUNT_DISABLE" {
			continue
		}
		env = append(env, kv)
	}
	allowed := task.AllowedTools
	if mode == "off" {
		// The plugin stays loaded so both modes see the same tool list; only its behavior is off.
		env = append(env, "LOCAL_SHUNT_DISABLE=1")
		allowed = nil
		for _, t := range task.AllowedTools {
			if !contains(workerTools, t) {
				allowed = append(allowed, t)
			}
		}
		cmd = append(append(cmd, "--disallowedTools"), workerTools...)
	}
	cmd = append(append(cmd, "--allowedTools"), allowed...)
	return cmd, env
}

func newUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func runOnce(task *benchTask, mode string, run int, claude, pluginDir string) (record, error) {
	session := newUUID()
	cmd, env := claudeCommand(task, mode, session, claude, pluginDir)
	var model any
	if task.Model != "" {
		model = task.Model
	}
	rec := record{
		{"ts", timestamp()},
		{"task", task.ID},
		{"mode", mode},
		{"run", run},
		{"session_id", session},
		{"model", model},
	}
	if len(task.ExpectTools) > 0 {
		rec.set("expect_tools", task.ExpectTools)
	}
	if len(task.ForbidTools) > 0 {
		rec.set("forbid_tools", task.ForbidTools)
	}
	workspace, err := prepareWorkspace(task)
	if err != nil {
		return nil, measureError("cannot copy %s for task %s: %s", task.Cwd, task.ID, errnoText(err))
	}
	started := time.Now()
	proc, err := runProcess(cmd, task.Prompt, workspace, env, time.Duration(task.Timeout)*time.Second)
	switch {
	case err != nil:
		return nil, measureError("cannot run %s: %s", claude, errnoText(err))
	case proc.timedOut:
		rec.set("error", fmt.Sprintf("timed out after %d s", task.Timeout))
	default:
		rec.set("exit_code", proc.exitCode)
		var result map[string]any
		if json.Unmarshal([]byte(proc.stdout), &result) == nil && result != nil {
			answer := ""
			if truthy(result["result"]) {
				answer = pyStr(result["result"])
			}
			rec.set("num_turns", result["num_turns"])
			rec.set("cost_usd", result["total_cost_usd"])
			rec.set("result_usage", result["usage"])
			rec.set("answer", answer)
			if truthy(result["is_error"]) || proc.exitCode != 0 {
				message := truncateRunes(answer, 300)
				if message == "" && truthy(result["subtype"]) {
					message = pyStr(result["subtype"])
				}
				if message == "" {
					message = fmt.Sprintf("exit code %d", proc.exitCode)
				}
				rec.set("error", message)
			}
		} else {
			output := proc.stderr
			if output == "" {
				output = proc.stdout
			}
			message := truncateRunes(strings.TrimSpace(output), 300)
			if message == "" {
				message = fmt.Sprintf("exit code %d, no JSON output", proc.exitCode)
			}
			rec.set("error", message)
		}
	}
	rec.set("duration_ms", time.Since(started).Milliseconds())
	_, failed := rec.get("error")
	if task.checksAnswer() {
		answerValue, _ := rec.get("answer")
		answer, _ := answerValue.(string)
		passed := !failed
		for _, pattern := range task.Expect {
			if !regexp.MustCompile("(?i)" + pattern).MatchString(answer) {
				passed = false
			}
		}
		if len(task.Verify) > 0 && !failed {
			verify := runVerify(task, workspace)
			rec.set("verify", verify)
			code, _ := verify.get("exit_code")
			passed = passed && code == 0
		}
		rec.set("passed", passed)
	}

	if transcript := findTranscript(session); transcript != "" {
		claudeUsage := parseTranscript(transcript)
		rec.set("claude", claudeUsage.toRecord())
		if mode == "on" && (len(task.ExpectTools) > 0 || len(task.ForbidTools) > 0) {
			rec.set("tool_check_failures", toolCheckFailures(task, claudeUsage.ToolCalls.counts))
		}
	} else if !failed {
		rec.set("error", "transcript not found")
	}
	rec.set("worker", workerUsage(session, readLog()).toRecord())
	return rec, nil
}

// runBench runs every task with local-shunt on and off, `runs` times each. It returns the results file.
func runBench(taskFile string, runs int, out string, only []string, claude, pluginDir string, report func(string)) (string, error) {
	if runs < 1 {
		return "", measureError("runs must be at least 1")
	}
	tasks, err := loadTasks(taskFile)
	if err != nil {
		return "", err
	}
	if len(only) > 0 {
		known := map[string]bool{}
		for _, t := range tasks {
			known[t.ID] = true
		}
		var unknown []string
		for _, id := range only {
			if !known[id] && !contains(unknown, id) {
				unknown = append(unknown, id)
			}
		}
		if len(unknown) > 0 {
			sort.Strings(unknown)
			return "", measureError("unknown task id: %s", strings.Join(unknown, ", "))
		}
		var selected []*benchTask
		for _, t := range tasks {
			if contains(only, t.ID) {
				selected = append(selected, t)
			}
		}
		tasks = selected
	}
	if out == "" {
		out = filepath.Join(stateDir(), "bench", time.Now().Format("20060102-150405")+".jsonl")
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return "", measureError("%s: %s", out, errnoText(err))
	}

	total, done := len(tasks)*runs*2, 0
	for run := 1; run <= runs; run++ {
		for index, task := range tasks {
			// Alternate the order so prompt-cache warmth and drift do not favor one mode.
			modes := []string{"off", "on"}
			if (run+index)%2 == 1 {
				modes = []string{"on", "off"}
			}
			for _, mode := range modes {
				done++
				report(fmt.Sprintf("[%d/%d] %s · local-shunt %s · run %d", done, total, task.ID, mode, run))
				rec, err := runOnce(task, mode, run, claude, pluginDir)
				if err != nil {
					return "", err
				}
				line, _ := marshalJSON(rec)
				f, err := os.OpenFile(out, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
				if err != nil {
					return "", measureError("%s: %s", out, errnoText(err))
				}
				_, _ = f.Write(append(line, '\n'))
				f.Close()
				if message, failed := rec.get("error"); failed {
					report("  failed: " + truncateRunes(pyStr(message), 200))
				}
			}
		}
	}
	return out, nil
}

// ---------------------------------------------------------------- report

func loadResults(paths []string) ([]map[string]any, error) {
	var records []map[string]any
	for _, path := range paths {
		found, err := readJSONLines(path)
		if err != nil {
			return nil, measureError("%s: %s", path, errnoText(err))
		}
		for _, r := range found {
			if (r["mode"] == "on" || r["mode"] == "off") && truthy(r["task"]) {
				records = append(records, r)
			}
		}
	}
	return records, nil
}

type metric struct {
	key, label string
	get        func(r map[string]any) (float64, bool)
}

func claudeField(name string) func(map[string]any) (float64, bool) {
	return func(r map[string]any) (float64, bool) { return number(asMap(r["claude"])[name]) }
}

var metrics = []metric{
	{"input", "input tokens", claudeField("total_input_tokens")},
	{"weighted", "input tokens, price-weighted", claudeField("weighted_input_tokens")},
	{"output", "output tokens", claudeField("output_tokens")},
	{"read", "file-read result tokens (est.)", claudeField("read_result_tokens")},
	{"cost", "cost USD (reported by claude)", func(r map[string]any) (float64, bool) { return number(r["cost_usd"]) }},
	{"turns", "turns", func(r map[string]any) (float64, bool) { return number(r["num_turns"]) }},
	{"seconds", "duration s", func(r map[string]any) (float64, bool) { return numberOr0(r["duration_ms"]) / 1000, true }},
}

// optional is a float64 that may be missing (Python's None).
type optional struct {
	value float64
	ok    bool
}

func saving(on, off optional) optional {
	if !on.ok || !off.ok || off.value == 0 {
		return optional{}
	}
	return optional{1 - on.value/off.value, true}
}

func median(values []float64) float64 {
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	n := len(sorted)
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}

type modeStats struct {
	runs, ok, passed, checked         int
	delegations, workerCalls, denials int
	values                            map[string]optional
}

type taskSummary struct {
	id      string
	on, off *modeStats
	saving  map[string]optional
}

// summarize takes the median of each metric per task and mode, over runs that completed.
func summarize(records []map[string]any) []*taskSummary {
	var order []string
	byTask := map[string]map[string][]map[string]any{}
	for _, r := range records {
		id := pyStr(r["task"])
		if _, ok := byTask[id]; !ok {
			order = append(order, id)
			byTask[id] = map[string][]map[string]any{"on": nil, "off": nil}
		}
		mode := r["mode"].(string)
		byTask[id][mode] = append(byTask[id][mode], r)
	}

	var summary []*taskSummary
	for _, id := range order {
		entry := &taskSummary{id: id, saving: map[string]optional{}}
		for _, mode := range []string{"on", "off"} {
			runs := byTask[id][mode]
			stats := &modeStats{runs: len(runs), values: map[string]optional{}}
			var good []map[string]any
			for _, r := range runs {
				_, failed := r["error"]
				if _, isObj := r["claude"].(map[string]any); !failed && isObj {
					good = append(good, r)
				}
				if passed, checked := r["passed"]; checked {
					stats.checked++
					if truthy(passed) {
						stats.passed++
					}
				}
			}
			stats.ok = len(good)
			for _, r := range good {
				claude := asMap(r["claude"])
				stats.delegations += int(numberOr0(claude["delegations"]))
				stats.workerCalls += int(numberOr0(asMap(r["worker"])["reads"]))
				stats.denials += int(numberOr0(claude["denials"]))
			}
			for _, m := range metrics {
				var values []float64
				for _, r := range good {
					if v, ok := m.get(r); ok {
						values = append(values, v)
					}
				}
				if len(values) > 0 {
					stats.values[m.key] = optional{median(values), true}
				} else {
					stats.values[m.key] = optional{}
				}
			}
			if mode == "on" {
				entry.on = stats
			} else {
				entry.off = stats
			}
		}
		for _, m := range metrics {
			entry.saving[m.key] = saving(entry.on.values[m.key], entry.off.values[m.key])
		}
		summary = append(summary, entry)
	}
	return summary
}

func fmtMetric(value optional, key string) string {
	if !value.ok {
		return "-"
	}
	switch key {
	case "cost":
		return fmt.Sprintf("%.4f", value.value)
	case "seconds":
		return fmt.Sprintf("%.1f", value.value)
	}
	return commas(int64(math.RoundToEven(value.value)))
}

func fmtPct(value optional) string {
	if !value.ok {
		return "-"
	}
	return percent(value.value, 1)
}

func formatReport(records []map[string]any) string {
	if len(records) == 0 {
		return "No benchmark records found."
	}
	summary := summarize(records)
	failed := 0
	for _, r := range records {
		if _, ok := r["error"]; ok {
			failed++
		}
	}
	lines := []string{fmt.Sprintf("Benchmark: %d tasks, %d runs (%d failed). Values are medians.", len(summary), len(records), failed), ""}
	var warnings []string
	header := fmt.Sprintf("  %-32s %12s %12s %8s", "", "on", "off", "saving")
	pair := func(label, on, off string) string { return fmt.Sprintf("  %-32s %12s %12s", label, on, off) }

	for _, entry := range summary {
		on, off := entry.on, entry.off
		lines = append(lines, "Task "+entry.id, header, pair("runs ok", fmt.Sprintf("%d/%d", on.ok, on.runs), fmt.Sprintf("%d/%d", off.ok, off.runs)))
		for _, m := range metrics {
			lines = append(lines, fmt.Sprintf("  %-32s %12s %12s %8s", m.label, fmtMetric(on.values[m.key], m.key), fmtMetric(off.values[m.key], m.key), fmtPct(entry.saving[m.key])))
		}
		lines = append(lines, pair("delegations / hook denials", fmt.Sprintf("%d / %d", on.delegations, on.denials), fmt.Sprintf("%d / %d", off.delegations, off.denials)))
		if on.checked > 0 || off.checked > 0 {
			lines = append(lines, pair("answers passed", fmt.Sprintf("%d/%d", on.passed, on.checked), fmt.Sprintf("%d/%d", off.passed, off.checked)))
		}
		lines = append(lines, "")

		if on.ok == 0 || off.ok == 0 {
			warnings = append(warnings, entry.id+": no completed run in one mode; it is left out of the totals")
		}
		if off.delegations > 0 || off.workerCalls > 0 || off.denials > 0 {
			warnings = append(warnings, entry.id+": local-shunt was used in 'off' runs, so the baseline is contaminated")
		}
		if on.ok > 0 && on.delegations == 0 && on.denials == 0 {
			warnings = append(warnings, entry.id+": 'on' runs never delegated a read; the task does not exercise local-shunt")
		}
		if on.passed < off.passed {
			warnings = append(warnings, fmt.Sprintf("%s: fewer answers passed with local-shunt on (%d vs %d)", entry.id, on.passed, off.passed))
		}
	}

	var complete []*taskSummary
	for _, entry := range summary {
		if entry.on.ok > 0 && entry.off.ok > 0 {
			complete = append(complete, entry)
		}
	}
	lines = append(lines, fmt.Sprintf("Overall (%d tasks with runs in both modes; sums of per-task medians)", len(complete)), header)
	for _, m := range metrics[:5] {
		onSum, offSum, missing := 0.0, 0.0, false
		for _, entry := range complete {
			a, b := entry.on.values[m.key], entry.off.values[m.key]
			if !a.ok || !b.ok {
				missing = true
				break
			}
			onSum += a.value
			offSum += b.value
		}
		if missing {
			continue
		}
		on, off := optional{onSum, true}, optional{offSum, true}
		lines = append(lines, fmt.Sprintf("  %-32s %12s %12s %8s", m.label, fmtMetric(on, m.key), fmtMetric(off, m.key), fmtPct(saving(on, off))))
	}
	for _, target := range [][2]string{{"read", "file-read tokens"}, {"input", "input tokens"}} {
		met := 0
		for _, entry := range complete {
			if s := entry.saving[target[0]]; s.ok && s.value >= targetSaving {
				met++
			}
		}
		lines = append(lines, fmt.Sprintf("  tasks saving ≥%s of %-17s %3d / %d", percent(targetSaving, 0), target[1], met, len(complete)))
	}

	if len(warnings) > 0 {
		lines = append(lines, "", "Warnings")
		for _, w := range warnings {
			lines = append(lines, "  - "+w)
		}
	}
	lines = append(lines,
		"",
		"Input and output tokens are Claude's billed counts from the transcripts. File-read result tokens",
		"are estimated from the text of Read and local-shunt tool results. Worker model tokens are not included.",
	)
	return strings.Join(lines, "\n")
}

// RunBench runs a benchmark and reports it.
func RunBench(taskFile string, runs int, only []string, out, claude string) (string, error) {
	outPath := ""
	if out != "" {
		outPath = resolvePath(out)
	}
	results, err := runBench(resolvePath(taskFile), runs, outPath, only, claude, pluginRoot(), progress)
	if err != nil {
		return "", asUsageError(err)
	}
	records, err := loadResults([]string{results})
	if err != nil {
		return "", asUsageError(err)
	}
	return fmt.Sprintf("%s\n\nResults: %s", formatReport(records), results), nil
}

// RunBenchReport summarizes benchmark result files.
func RunBenchReport(paths []string) (string, error) {
	resolved := make([]string, len(paths))
	for i, p := range paths {
		resolved[i] = resolvePath(p)
	}
	records, err := loadResults(resolved)
	if err != nil {
		return "", asUsageError(err)
	}
	return formatReport(records), nil
}

func asUsageError(err error) error {
	var m *MeasureError
	if errors.As(err, &m) {
		return usageError("%s", m.msg)
	}
	return err
}

// pluginRoot finds the plugin directory: $LOCAL_SHUNT_ROOT (set by bin/local-shunt), else the
// nearest parent of the executable or working directory that holds .claude-plugin/plugin.json.
func pluginRoot() string {
	if root := os.Getenv("LOCAL_SHUNT_ROOT"); root != "" {
		return root
	}
	var starts []string
	if exe, err := os.Executable(); err == nil {
		starts = append(starts, filepath.Dir(resolveSymlinks(exe)))
	}
	starts = append(starts, workingDir())
	for _, dir := range starts {
		for {
			if isFile(filepath.Join(dir, ".claude-plugin", "plugin.json")) {
				return dir
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	return workingDir()
}
