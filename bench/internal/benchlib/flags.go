package benchlib

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// DefaultServerArgs are the server arguments per target when --server-args is not given.
var DefaultServerArgs = map[string]string{
	"v2": "serve --db {db} --addr {addr}",
	"v3": "serve --db {db} --listen {listen}",
}

// ServerFlags are the flags every server-driving tool shares.
type ServerFlags struct {
	Target   string
	Label    string
	Base     string
	Bin      string
	Args     string
	DB       string
	Work     string
	Token    string
	Log      string
	Out      string
	Scenario string
}

// Register adds the flags to fs.
func (f *ServerFlags) Register(fs *flag.FlagSet) {
	fs.StringVar(&f.Target, "target", "v3", "API under test: v2 (TimeShadeDB master) or v3")
	fs.StringVar(&f.Label, "label", "", "name of the system under test in the result (default: the target)")
	fs.StringVar(&f.Base, "base", "", "URL of an already running server (skips starting one; no memory sampling)")
	fs.StringVar(&f.Bin, "server-bin", "", "timeshadedb binary to start")
	fs.StringVar(&f.Args, "server-args", "", "server arguments; placeholders {db}, {addr} (harness-chosen port), {listen} (127.0.0.1:0 + 'listening on' line). Default per target: v2 '"+DefaultServerArgs["v2"]+"', v3 '"+DefaultServerArgs["v3"]+"'")
	fs.StringVar(&f.DB, "db", "", "database directory for the started server (default: a fresh directory under --work)")
	fs.StringVar(&f.Work, "work", "", "scratch directory for fresh databases (default: OS temp)")
	fs.StringVar(&f.Token, "token", "", "bearer token for v3 ingest (added to v3 server args as --ingest-token)")
	fs.StringVar(&f.Log, "server-log", "", "file for the server's output (default: discarded)")
	fs.StringVar(&f.Out, "out", "", "result JSON file")
	fs.StringVar(&f.Scenario, "scenario", "", "workload name recorded in the result (matched by bench/targets.json)")
}

// NewResult starts a result with the tool name, label, target and scenario.
func (f *ServerFlags) NewResult(tool string) *Result {
	r := NewResult(tool, f.LabelOrTarget(), f.Target)
	r.Scenario = f.Scenario
	return r
}

// LabelOrTarget returns the label, defaulting to the target.
func (f *ServerFlags) LabelOrTarget() string {
	if f.Label != "" {
		return f.Label
	}
	return f.Target
}

// EnsureDB sets DB to a fresh, not yet existing path when it is empty (v2 creates a
// database only when the directory does not exist). An explicit --db is used as given.
func (f *ServerFlags) EnsureDB(prefix string) error {
	if f.DB != "" {
		return nil
	}
	d, err := TempDir(f.Work, prefix)
	if err != nil {
		return err
	}
	f.DB = filepath.Join(d, "db")
	return nil
}

// defaultArgs is DefaultServerArgs for the target, with the ingest token for v3.
func (f *ServerFlags) defaultArgs() string {
	args := DefaultServerArgs[f.Target]
	if f.Target == "v3" && f.Token != "" {
		args += " --ingest-token " + f.Token
	}
	return args
}

// AddServerArgs appends extra to the server arguments (the target's defaults when
// --server-args was not given). Tools use it for flags only they need, such as
// --clock data for a v3 server that replays recorded data.
func (f *ServerFlags) AddServerArgs(extra string) {
	if f.Args == "" {
		f.Args = f.defaultArgs()
	}
	f.Args += " " + extra
}

// Spec builds the ServerSpec. The binary must be set.
func (f *ServerFlags) Spec() (ServerSpec, error) {
	if f.Bin == "" {
		return ServerSpec{}, errors.New("--server-bin or --base is required")
	}
	args := f.Args
	if args == "" {
		args = f.defaultArgs()
	}
	if args == "" {
		return ServerSpec{}, fmt.Errorf("no default server args for target %q", f.Target)
	}
	bin, err := filepath.Abs(f.Bin)
	if err != nil {
		return ServerSpec{}, err
	}
	spec := ServerSpec{Binary: bin, Args: strings.Fields(args), DB: f.DB}
	if f.Log != "" {
		lf, err := os.OpenFile(f.Log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return ServerSpec{}, err
		}
		spec.Log = lf
	} else {
		spec.Log = io.Discard
	}
	return spec, nil
}

// Start starts the server unless --base is set. It returns the server (nil with --base)
// and the base URL.
func (f *ServerFlags) Start() (*Server, string, error) {
	if f.Base != "" {
		return nil, strings.TrimRight(f.Base, "/"), nil
	}
	spec, err := f.Spec()
	if err != nil {
		return nil, "", err
	}
	s, err := StartServer(spec)
	if err != nil {
		return nil, "", err
	}
	return s, s.BaseURL, nil
}

// Fatal prints an error, stops every server this process started (os.Exit skips
// deferred Stop calls) and exits with status 1.
func Fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	StopAll()
	os.Exit(1)
}
