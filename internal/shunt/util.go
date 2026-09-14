package shunt

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// splitLines splits text into lines without their line endings ("\n" or "\r\n").
func splitLines(text string) []string {
	if text == "" {
		return nil
	}
	text = strings.TrimSuffix(text, "\n")
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimSuffix(line, "\r")
	}
	return lines
}

// commas formats an integer with thousands separators, like Python's "{:,}".
func commas(n int64) string {
	s := strconv.FormatInt(n, 10)
	sign := ""
	if strings.HasPrefix(s, "-") {
		sign, s = "-", s[1:]
	}
	var out strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			out.WriteByte(',')
		}
		out.WriteRune(c)
	}
	return sign + out.String()
}

func commasInt(n int) string { return commas(int64(n)) }

// percent formats a ratio like Python's "{:.1%}".
func percent(value float64, decimals int) string {
	return strconv.FormatFloat(value*100, 'f', decimals, 64) + "%"
}

func pyQuote(s string) string {
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		return `"` + s + `"`
	}
	return "'" + strings.ReplaceAll(s, "'", `\'`) + "'"
}

func pyList(items []string) string {
	quoted := make([]string, len(items))
	for i, item := range items {
		quoted[i] = pyQuote(item)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// field is one key of an ordered JSON object.
type field struct {
	Key   string
	Value any
}

// record is a JSON object that keeps its key order.
type record []field

func (r record) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, f := range r {
		if i > 0 {
			buf.WriteByte(',')
		}
		key, _ := json.Marshal(f.Key)
		buf.Write(key)
		buf.WriteByte(':')
		value, err := marshalJSON(f.Value)
		if err != nil {
			return nil, err
		}
		buf.Write(value)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

func (r record) get(key string) (any, bool) {
	for _, f := range r {
		if f.Key == key {
			return f.Value, true
		}
	}
	return nil, false
}

func (r *record) set(key string, value any) {
	for i, f := range *r {
		if f.Key == key {
			(*r)[i].Value = value
			return
		}
	}
	*r = append(*r, field{key, value})
}

// marshalJSON encodes without HTML escaping and without a trailing newline.
func marshalJSON(value any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

func timestamp() string {
	return time.Now().Format("2006-01-02T15:04:05-07:00")
}

// logEvent appends one JSON line to the log. It never fails.
func logEvent(rec record) {
	defer func() { _ = recover() }()
	directory := stateDir()
	if os.MkdirAll(directory, 0o755) != nil {
		return
	}
	line, err := marshalJSON(append(record{{"ts", timestamp()}}, rec...))
	if err != nil {
		return
	}
	logFile := filepath.Join(directory, "log.jsonl")
	if stat, err := os.Stat(logFile); err == nil && stat.Size() > logRotateBytes {
		_ = os.Rename(logFile, filepath.Join(directory, "log.jsonl.1"))
	}
	f, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(line, '\n'))
}

// counter counts keys and remembers the order in which they first appeared.
type counter struct {
	keys   []string
	counts map[string]int
}

func newCounter() *counter { return &counter{counts: map[string]int{}} }

func (c *counter) add(key string, amount int) {
	if _, ok := c.counts[key]; !ok {
		c.keys = append(c.keys, key)
	}
	c.counts[key] += amount
}

func (c *counter) get(key string) int { return c.counts[key] }

func (c *counter) len() int { return len(c.keys) }

// mostCommon returns keys by count, highest first; ties keep first-seen order.
func (c *counter) mostCommon() []string {
	keys := append([]string(nil), c.keys...)
	sort.SliceStable(keys, func(i, j int) bool { return c.counts[keys[i]] > c.counts[keys[j]] })
	return keys
}

func (c *counter) MarshalJSON() ([]byte, error) {
	rec := record{}
	for _, key := range c.keys {
		rec = append(rec, field{key, c.counts[key]})
	}
	return rec.MarshalJSON()
}

// pyStr renders a decoded JSON value the way Python's str() renders it.
func pyStr(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case json.Number:
		return v.String()
	case float64:
		if v == math.Trunc(v) && math.Abs(v) < 1e15 {
			return strconv.FormatInt(int64(v), 10)
		}
		return strconv.FormatFloat(v, 'g', -1, 64)
	}
	return coerceString(value)
}

// number converts a decoded JSON number to float64. ok is false for anything else.
func number(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	}
	return 0, false
}

// numberOr0 mirrors Python's `value or 0` for numeric log fields.
func numberOr0(value any) float64 {
	f, _ := number(value)
	return f
}

func intPtr(value any) *int {
	if f, ok := number(value); ok {
		n := int(f)
		return &n
	}
	return nil
}

func asMap(value any) map[string]any {
	m, _ := value.(map[string]any)
	return m
}

func asString(value any) string {
	s, _ := value.(string)
	return s
}

// resolvePath makes a path absolute against the working directory and resolves symlinks.
func resolvePath(raw string) string {
	path := expandUser(raw)
	if !filepath.IsAbs(path) {
		path = filepath.Join(workingDir(), path)
	}
	return resolveSymlinks(filepath.Clean(path))
}

// resolveSymlinks resolves symlinks in the existing part of a path, like Python's
// Path.resolve(strict=False).
func resolveSymlinks(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	dir, base := filepath.Split(path)
	dir = filepath.Clean(dir)
	if dir == path || base == "" {
		return path
	}
	return filepath.Join(resolveSymlinks(dir), base)
}

func workingDir() string {
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return resolveSymlinks(wd)
}

// displayPath shows a path relative to the working directory when it lies inside it.
func displayPath(path string) string {
	if rel, ok := relativeTo(path, workingDir()); ok {
		return filepath.ToSlash(rel)
	}
	return filepath.ToSlash(path)
}

// shlexSplit splits a command line like Python's shlex.split (POSIX mode).
func shlexSplit(s string) ([]string, error) {
	var (
		tokens  []string
		current strings.Builder
		inToken bool
	)
	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		switch {
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			if inToken {
				tokens = append(tokens, current.String())
				current.Reset()
				inToken = false
			}
		case c == '\\':
			i++
			if i >= len(runes) {
				return nil, errors.New("No escaped character")
			}
			current.WriteRune(runes[i])
			inToken = true
		case c == '\'':
			inToken = true
			end := i + 1
			for end < len(runes) && runes[end] != '\'' {
				end++
			}
			if end >= len(runes) {
				return nil, errors.New("No closing quotation")
			}
			current.WriteString(string(runes[i+1 : end]))
			i = end
		case c == '"':
			inToken = true
			i++
			for ; i < len(runes) && runes[i] != '"'; i++ {
				if runes[i] == '\\' && i+1 < len(runes) && (runes[i+1] == '"' || runes[i+1] == '\\') {
					i++
				}
				current.WriteRune(runes[i])
			}
			if i >= len(runes) {
				return nil, errors.New("No closing quotation")
			}
		default:
			current.WriteRune(c)
			inToken = true
		}
	}
	if inToken {
		tokens = append(tokens, current.String())
	}
	return tokens, nil
}

// shellQuote quotes a string for a POSIX shell, like Python's shlex.quote.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("@%+=:,./-_", c)) {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

func fmtSeconds(ms int64) string {
	return fmt.Sprintf("%.1f", float64(ms)/1000)
}
