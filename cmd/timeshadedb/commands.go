package main

// Command registry. Every subcommand registers itself from an init function in its own
// file, cmd_<name>.go:
//
//	func init() {
//		register(Command{Name: "publish", Summary: "export the database as static files", Run: cmdPublish})
//	}
//
// Command names are lower-case words joined by '-'. Run receives the arguments after
// the command name and should parse them with its own flag.FlagSet.

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
)

// Command is one timeshadedb subcommand.
type Command struct {
	// Name is the word after "timeshadedb" on the command line.
	Name string
	// Summary is one line for the usage text.
	Summary string
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
	fmt.Fprintln(w, "\ncommands:")
	for _, name := range commandNames() {
		fmt.Fprintf(w, "  %-18s %s\n", name, commands[name].Summary)
	}
}
