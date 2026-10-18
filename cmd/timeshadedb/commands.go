package main

// Command registry. Every subcommand registers itself from an init function in its own
// file, so work packages add commands without editing a shared switch:
//
//	// cmd_publish.go (owned by WP12)
//	func init() {
//		register(Command{Name: "publish", Summary: "export the v3 database as static files", Group: GroupV3, Run: cmdPublish})
//	}
//
// File naming: cmd_<name>.go, one file per command or per closely related group.
// Command names are lower-case words joined by '-'. Run receives the arguments after
// the command name and should parse them with its own flag.FlagSet. The planned v3
// commands and their owners are listed in docs/v3/README.md.

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
)

// Command groups, used to order the usage text.
const (
	GroupV3     = "v3"
	GroupLegacy = "legacy"
	GroupTools  = "tools"
)

// Command is one timeshadedb subcommand.
type Command struct {
	// Name is the word after "timeshadedb" on the command line.
	Name string
	// Summary is one line for the usage text.
	Summary string
	// Group is GroupV3, GroupLegacy or GroupTools.
	Group string
	// Run executes the command with the arguments after the name.
	Run func(ctx context.Context, args []string) error
}

var (
	commands    = map[string]Command{}
	commandName = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
)

// register adds a command. It panics on a duplicate or malformed registration, which
// surfaces in every test run of this package.
func register(c Command) {
	if !commandName.MatchString(c.Name) {
		panic(fmt.Sprintf("timeshadedb: bad command name %q", c.Name))
	}
	if c.Run == nil {
		panic(fmt.Sprintf("timeshadedb: command %q has no Run", c.Name))
	}
	switch c.Group {
	case GroupV3, GroupLegacy, GroupTools:
	default:
		panic(fmt.Sprintf("timeshadedb: command %q has unknown group %q", c.Name, c.Group))
	}
	if _, dup := commands[c.Name]; dup {
		panic(fmt.Sprintf("timeshadedb: command %q registered twice", c.Name))
	}
	commands[c.Name] = c
}

// lookupCommand returns the command registered under name.
func lookupCommand(name string) (Command, bool) {
	c, ok := commands[name]
	return c, ok
}

// commandNames returns every registered name, sorted.
func commandNames() []string {
	names := make([]string, 0, len(commands))
	for name := range commands {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// runCommand dispatches args[0] to its registered command. "help" and "-h" print the usage.
func runCommand(ctx context.Context, stdout io.Writer, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: timeshadedb <%s>", strings.Join(commandNames(), "|"))
	}
	switch args[0] {
	case "help", "-h", "--help":
		writeUsage(stdout)
		return nil
	}
	c, ok := lookupCommand(args[0])
	if !ok {
		return fmt.Errorf("unknown command %q (run \"timeshadedb help\" for the list)", args[0])
	}
	return c.Run(ctx, args[1:])
}

func writeUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: timeshadedb <command> [flags]")
	for _, group := range []string{GroupV3, GroupTools, GroupLegacy} {
		var names []string
		for _, name := range commandNames() {
			if commands[name].Group == group {
				names = append(names, name)
			}
		}
		if len(names) == 0 {
			continue
		}
		fmt.Fprintf(w, "\n%s commands:\n", group)
		for _, name := range names {
			fmt.Fprintf(w, "  %-18s %s\n", name, commands[name].Summary)
		}
	}
}
