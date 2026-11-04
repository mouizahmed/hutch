package config

import (
	"flag"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite docs/reference/settings.md from the schema")

const referencePath = "../../docs/reference/settings.md"

// The settings reference is generated from the schema, so the docs can't drift
// from it: go test ./internal/config -run TestSettingsReference -update
func TestSettingsReference(t *testing.T) {
	want := settingsReference()
	if *update {
		if err := os.WriteFile(referencePath, []byte(want), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	got, err := os.ReadFile(referencePath)
	if err != nil || string(got) != want {
		t.Fatalf("%s is out of date with the schema: go test ./internal/config -run TestSettingsReference -update", referencePath)
	}
}

var kindNames = map[Kind]string{String: "text", Bool: "true or false", Int: "whole number", Duration: "duration", Size: "size",
	Percent: "percentage", Enum: "one of", List: "list", Float: "number"}

// referenceGroups orders the keys under headings by their first path segment.
var referenceGroups = []struct {
	title, intro string
	prefixes     []string
}{
	{"Boxes", "Each box's size, and when it sleeps.", []string{"default_agent", "size", "disk", "idle_timeout", "idle", "base", "limits"}},
	{"Access", "What boxes may use. A project can only request these: see [grants](../configuration.md#grants).", []string{"autonomy", "credentials", "network", "mcp_hosts", "keys"}},
	{"Templates", "", []string{"template", "image", "retention.template_versions"}},
	{"The machine", "Memory, disk, and placement on each machine.", []string{"machine", "placement"}},
	{"Several machines", "", []string{"machines"}},
	{"Backups", "", []string{"checkpoint", "archive_after", "backup", "retention", "gc"}},
	{"Activity and ports", "How Hutch tells a working box or terminal from an idle one, and waits for wakes.", []string{"threads", "ports"}},
	{"Doctor and MCP checks", "", []string{"doctor", "mcp"}},
	{"Notifications and the TUI", "", []string{"notify", "tui"}},
	{"Grants", "Written by Hutch when you grant a project more than your own settings; see [grants](../configuration.md#grants).", []string{"grants"}},
}

// cliFlag is a flag like --agent in a setting's text, which Markdown would turn into a dash.
var cliFlag = regexp.MustCompile(`--[a-z][a-z-]*`)

func referenceGroup(k *Key) int {
	best, bestLen := -1, 0
	for i, g := range referenceGroups {
		for _, p := range g.prefixes {
			if (k.Path == p || strings.HasPrefix(k.Path, p+".")) && len(p) > bestLen {
				best, bestLen = i, len(p)
			}
		}
	}
	return best
}

func settingsReference() string {
	var b strings.Builder
	b.WriteString("# Settings reference\n\n")
	b.WriteString("Every setting Hutch reads, generated from its schema. See [configuration](../configuration.md) for the layers, and how to set them.\n\n")
	b.WriteString("Durations are like `30s`, `10m`, `8h`, or `30d`. Sizes are like `512M`, `8G`, or `1T`. **Layers** are where a setting can be set: `machine` (`hutch config set --machine`, or `--on <machine>`), `user` (your config), and `project` (`.hutch/hutch.toml`). `hutch config explain` shows each setting's value for the current project, and where it comes from.\n")
	groups := make([][]*Key, len(referenceGroups))
	for _, k := range Keys {
		g := referenceGroup(k)
		if g < 0 {
			panic("settings reference: no group for " + k.Path)
		}
		groups[g] = append(groups[g], k)
	}
	for i, g := range referenceGroups {
		if len(groups[i]) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n## %s\n\n", g.title)
		if g.intro != "" {
			fmt.Fprintf(&b, "%s\n\n", g.intro)
		}
		b.WriteString("| Setting | Default | Layers | |\n| --- | --- | --- | --- |\n")
		for _, k := range groups[i] {
			def := "none"
			if k.Default != nil {
				def = "`" + Display(k.Default) + "`"
			}
			layers := make([]string, len(k.Layers))
			for j, l := range k.Layers {
				layers[j] = l.String()
			}
			ls := strings.Join(layers, ", ")
			if k.Request {
				ls += " (a request)"
			}
			typ := kindNames[k.Kind]
			switch {
			case k.Kind == Enum:
				typ += " `" + strings.Join(k.Enum, "`, `") + "`"
			case k.Min != "" && k.Max != "":
				typ += ", " + k.Min + " to " + k.Max
			case k.Min != "":
				typ += ", at least " + k.Min
			}
			doc := cliFlag.ReplaceAllString(strings.ReplaceAll(k.Doc, "|", `\|`), "`$0`")
			fmt.Fprintf(&b, "| `%s` | %s | %s | %s. *%s.* |\n", k.Path, def, ls, upperFirst(doc), typ)
		}
	}
	return b.String()
}

// upperFirst starts a sentence, leaving words like vCPUs as they are.
func upperFirst(s string) string {
	if len(s) < 2 || strings.ToLower(s[1:2]) != s[1:2] {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
