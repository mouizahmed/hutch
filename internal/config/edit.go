package config

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
)

// Edits change a file's text line by line so comments and layout survive. The result
// is always parsed again; anything the simple model can't place safely is an error.

var (
	headerLine = regexp.MustCompile(`^\s*\[([^\[\]]+)\]\s*(#.*)?$`)
	arrayTable = regexp.MustCompile(`^\s*\[\[`)
)

type line struct {
	text   string
	table  []string // table the line is in
	key    []string // full path of a key line
	header []string // table a header line opens
}

func scan(text []byte) ([]line, error) {
	raw := strings.Split(string(text), "\n")
	if len(raw) > 0 && raw[len(raw)-1] == "" {
		raw = raw[:len(raw)-1]
	}
	var lines []line
	var table []string
	for i := 0; i < len(raw); i++ {
		t := raw[i]
		l := line{text: t, table: table}
		trimmed := strings.TrimSpace(t)
		switch {
		case trimmed == "" || strings.HasPrefix(trimmed, "#"):
		case arrayTable.MatchString(t):
			return nil, fmt.Errorf("arrays of tables are not supported by config edits")
		case headerLine.MatchString(t):
			p, err := SplitPath(headerLine.FindStringSubmatch(t)[1])
			if err != nil {
				return nil, err
			}
			table, l.header = p, p
		default:
			eq := keyEnd(t)
			if eq < 0 {
				return nil, fmt.Errorf("can't read line %d: %q", i+1, t)
			}
			p, err := SplitPath(t[:eq])
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", i+1, err)
			}
			l.key = append(slices.Clone(table), p...)
			// Multi-line arrays: continue until brackets balance.
			depth := brackets(t[eq+1:])
			for depth > 0 && i+1 < len(raw) {
				i++
				l.text += "\n" + raw[i]
				depth += brackets(raw[i])
			}
		}
		lines = append(lines, l)
	}
	return lines, nil
}

// keyEnd finds the "=" after a key, skipping quoted segments.
func keyEnd(s string) int {
	inQuote := false
	for i, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
		case r == '=' && !inQuote:
			return i
		}
	}
	return -1
}

// brackets counts open minus closed brackets outside strings and comments.
func brackets(s string) int {
	n, inQuote := 0, false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\\' && inQuote:
			i++
		case c == '"':
			inQuote = !inQuote
		case inQuote:
		case c == '#':
			return n
		case c == '[' || c == '{':
			n++
		case c == ']' || c == '}':
			n--
		}
	}
	return n
}

func hasPrefix(p, prefix []string) bool {
	return len(p) >= len(prefix) && slices.Equal(p[:len(prefix)], prefix)
}

func join(lines []line) []byte {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l.text)
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

// Set sets path to raw in a file's text. A key inside an inline table can't be edited.
func Set(text []byte, path []string, raw any) ([]byte, error) {
	lines, err := scan(text)
	if err != nil {
		return nil, err
	}
	for i, l := range lines {
		if l.key == nil {
			continue
		}
		if slices.Equal(l.key, path) {
			rel := FormatPath(l.key[len(l.table):])
			indent := l.text[:len(l.text)-len(strings.TrimLeft(l.text, " \t"))]
			lines[i] = line{text: indent + rel + " = " + literal(raw), table: l.table, key: l.key}
			return check(join(lines))
		}
		if hasPrefix(path, l.key) {
			text, err := setInline(l, path[len(l.key):], raw)
			if err != nil {
				return nil, fmt.Errorf("%s on line %d: %w", FormatPath(l.key), i+1, err)
			}
			lines[i].text = text
			return check(join(lines))
		}
	}

	// Insert into the deepest existing table that contains path, after its last key.
	best, at := -1, -1
	var bestTable []string
	for i, l := range lines {
		if p := l.header; p != nil && hasPrefix(path, p) && len(p) < len(path) && len(p) > best {
			best, bestTable, at = len(p), p, i
		}
	}
	if best >= 0 {
		end := at
		for j := at + 1; j < len(lines) && lines[j].header == nil; j++ {
			if strings.TrimSpace(lines[j].text) != "" {
				end = j
			}
		}
		ins := line{text: FormatPath(path[len(bestTable):]) + " = " + literal(raw)}
		lines = slices.Insert(lines, end+1, ins)
		return check(join(lines))
	}

	// Keys under a named entry (grants.<project>.…) get a table of their own at the end.
	if k := Lookup(path); k != nil {
		if star := slices.Index(strings.Split(k.Path, "."), "*"); star >= 0 {
			if len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1].text) != "" {
				lines = append(lines, line{})
			}
			lines = append(lines,
				line{text: "[" + FormatPath(path[:star+1]) + "]"},
				line{text: FormatPath(path[star+1:]) + " = " + literal(raw)})
			return check(join(lines))
		}
	}

	// Otherwise a top-level dotted key, before the first table.
	end := -1
	for j, l := range lines {
		if l.header != nil {
			break
		}
		if strings.TrimSpace(l.text) != "" {
			end = j
		}
	}
	lines = slices.Insert(lines, end+1, line{text: FormatPath(path) + " = " + literal(raw)})
	return check(join(lines))
}

// setInline sets rest inside a one-line inline table, keeping the line's comment.
func setInline(l line, rest []string, raw any) (string, error) {
	eq := keyEnd(l.text)
	value, comment := splitComment(l.text[eq+1:])
	value = strings.TrimSpace(value)
	if strings.Contains(l.text, "\n") || !strings.HasPrefix(value, "{") {
		return "", errors.New("not a one-line inline table; edit the file instead")
	}
	var m map[string]any
	if _, err := toml.Decode("v = "+value, &m); err != nil {
		return "", err
	}
	t, _ := m["v"].(map[string]any)
	cur := t
	for _, k := range rest[:len(rest)-1] {
		next, ok := cur[k].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[k] = next
		}
		cur = next
	}
	cur[rest[len(rest)-1]] = raw
	out := l.text[:eq+1] + " " + inline(t)
	if comment != "" {
		out += "   " + comment
	}
	return out, nil
}

func inline(m map[string]any) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		if sub, ok := m[k].(map[string]any); ok {
			parts[i] = FormatPath([]string{k}) + " = " + inline(sub)
		} else {
			parts[i] = FormatPath([]string{k}) + " = " + literal(m[k])
		}
	}
	return "{ " + strings.Join(parts, ", ") + " }"
}

// splitComment splits a value from a trailing "# comment" outside strings.
func splitComment(s string) (value, comment string) {
	inQuote := false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\\' && inQuote:
			i++
		case c == '"':
			inQuote = !inQuote
		case c == '#' && !inQuote:
			return s[:i], strings.TrimSpace(s[i:])
		}
	}
	return s, ""
}

// Comment returns the trailing comment on the line that sets path (or the inline
// table holding it), without the "#": where an agent writes its reason.
func Comment(text []byte, path []string) string {
	lines, err := scan(text)
	if err != nil {
		return ""
	}
	for _, l := range lines {
		if l.key != nil && hasPrefix(path, l.key) {
			_, c := splitComment(l.text[keyEnd(l.text)+1:])
			return strings.TrimSpace(strings.TrimPrefix(c, "#"))
		}
	}
	return ""
}

// Remove deletes every key under prefix, and tables under it.
func Remove(text []byte, prefix []string) ([]byte, error) {
	lines, err := scan(text)
	if err != nil {
		return nil, err
	}
	var out []line
	skipping := false
	for _, l := range lines {
		if l.header != nil {
			if skipping = hasPrefix(l.header, prefix); skipping {
				continue
			}
		}
		if skipping || (l.key != nil && hasPrefix(l.key, prefix)) {
			continue
		}
		out = append(out, l)
	}
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1].text) == "" {
		out = out[:len(out)-1]
	}
	return check(join(out))
}

func check(text []byte) ([]byte, error) {
	var m map[string]any
	if _, err := toml.Decode(string(text), &m); err != nil {
		return nil, fmt.Errorf("edit would make the file invalid (%v); edit it by hand instead", err)
	}
	return text, nil
}
