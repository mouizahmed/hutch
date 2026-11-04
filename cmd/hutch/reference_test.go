package main

import (
	"flag"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

var updateReference = flag.Bool("update", false, "rewrite docs/reference/cli.md from the commands")

const cliReferencePath = "../../docs/reference/cli.md"

// The CLI reference is generated from the commands themselves, so the docs can't
// drift from them: go test ./cmd/hutch -run TestCLIReference -update
func TestCLIReference(t *testing.T) {
	want := cliReference(newRoot())
	if *updateReference {
		if err := os.WriteFile(cliReferencePath, []byte(want), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	got, err := os.ReadFile(cliReferencePath)
	if err != nil || string(got) != want {
		t.Fatalf("%s is out of date with the commands: go test ./cmd/hutch -run TestCLIReference -update", cliReferencePath)
	}
}

func cliReference(root *cobra.Command) string {
	var b strings.Builder
	b.WriteString("# CLI reference\n\n")
	b.WriteString("Every `hutch` command and flag, generated from the commands themselves. `hutch <command> --help` shows the same for the version you have installed. With no command, `hutch` opens [the TUI](../tui.md).\n")
	groups := []struct{ id, title string }{{"boxes", "Boxes"}, {"projects", "Projects and templates"}, {"setup", "Setup"}, {"", "Other"}}
	for _, g := range groups {
		var cmds []*cobra.Command
		for _, c := range root.Commands() {
			if c.GroupID == g.id && c.IsAvailableCommand() {
				cmds = append(cmds, c)
			}
		}
		if len(cmds) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n## %s\n", g.title)
		for _, c := range cmds {
			writeCommand(&b, c)
		}
	}
	return b.String()
}

// placeholder is a <name> in help text, which Markdown would take for HTML, or a
// --flag, which it would turn into a dash.
var placeholder = regexp.MustCompile(`[\w.-]*<[a-z][a-z -]*>[\w.-]*|--[a-z][a-z-]*`)

func writeCommand(b *strings.Builder, c *cobra.Command) {
	fmt.Fprintf(b, "\n### %s\n\n", c.CommandPath())
	text := c.Long
	if text == "" {
		text = c.Short
	}
	// Help text is wrapped for a terminal; a paragraph here is one line.
	for i, para := range strings.Split(strings.TrimSpace(text), "\n\n") {
		if i > 0 {
			b.WriteString("\n")
		}
		line := strings.TrimSuffix(strings.Join(strings.Fields(para), " "), ".")
		fmt.Fprintf(b, "%s.\n", placeholder.ReplaceAllString(line, "`$0`"))
	}
	if c.Runnable() {
		fmt.Fprintf(b, "\n```text\n%s\n```\n", c.UseLine())
	}
	if len(c.Aliases) > 0 {
		fmt.Fprintf(b, "\nAlso: `%s`.\n", strings.Join(c.Aliases, "`, `"))
	}
	var flags []*pflag.Flag
	c.LocalFlags().VisitAll(func(f *pflag.Flag) {
		if f.Name != "help" && !f.Hidden {
			flags = append(flags, f)
		}
	})
	if len(flags) > 0 {
		b.WriteString("\n| Flag | |\n| --- | --- |\n")
		for _, f := range flags {
			name := "`--" + f.Name
			if f.Shorthand != "" {
				name = "`-" + f.Shorthand + "`, " + name
			}
			if f.Value.Type() != "bool" {
				name += " <" + f.Value.Type() + ">"
			}
			name += "`"
			usage := placeholder.ReplaceAllString(strings.ReplaceAll(f.Usage, "|", `\|`), "`$0`")
			if def := f.DefValue; def != "" && def != "false" && def != "0" && def != "[]" {
				usage += fmt.Sprintf(" (default `%s`)", def)
			}
			fmt.Fprintf(b, "| %s | %s |\n", name, usage)
		}
	}
	for _, sub := range c.Commands() {
		if sub.IsAvailableCommand() {
			writeCommand(b, sub)
		}
	}
}
