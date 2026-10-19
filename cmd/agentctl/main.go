package main

import (
	"fmt"
	"io"
	"os"
	"strings"
)

type Context struct {
	Config   Config
	API      *APIClient
	Out, Log io.Writer
	Command  string
	Force    bool
	Cursors  *CursorStore // control marks; nil disables them
}
type Handler func(*Context, []string) error

var commandRegistry = makeRegistry()

func makeRegistry() map[string]Handler {
	r := make(map[string]Handler, len(commandNames))
	for _, name := range commandNames {
		r[name] = skeletonHandler
	}
	return r
}

func skeletonHandler(ctx *Context, _ []string) error {
	return WriteOutput(ctx.Out, ctx.Config, map[string]string{"command": ctx.Command, "status": "command handler not implemented"}, "")
}

func run(args []string, out, log io.Writer, environ map[string]string) int {
	args, force := extractForceFlag(args)
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h" || args[0] == "help") {
		fmt.Fprintln(out, helpText())
		return ExitSuccess
	}
	inv, err := ParseArgs(args, environ)
	if err != nil {
		fmt.Fprintln(log, err)
		return ExitCode(err)
	}
	token, err := LoadToken(environ)
	if err != nil {
		err = fmt.Errorf("%w: unavailable credentials", ErrAuth)
		fmt.Fprintln(log, err)
		return ExitCode(err)
	}
	inv.Config.Token = token
	ctx := &Context{Config: inv.Config, API: NewAPIClient(inv.Config), Out: out, Log: log, Command: inv.Command, Force: force, Cursors: controlStoreFrom(environ)}
	if err = commandRegistry[inv.Command](ctx, inv.Args); err != nil {
		fmt.Fprintln(log, redactedMessage(err, token))
		return ExitCode(err)
	}
	return ExitSuccess
}

func main() {
	env := make(map[string]string)
	for _, pair := range os.Environ() {
		k, v, ok := strings.Cut(pair, "=")
		if ok {
			env[k] = v
		}
	}
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, env))
}

// Flags may precede the command or its first positional argument. After a
// positional argument or --, every token belongs to the command's payload.
func extractForceFlag(args []string) ([]string, bool) {
	result := make([]string, 0, len(args))
	force, command, literal := false, false, false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if literal {
			result = append(result, arg)
			continue
		}
		if arg == "--" {
			literal = true
			result = append(result, arg)
			continue
		}
		if arg == "--force" {
			force = true
			continue
		}
		result = append(result, arg)
		// Consume global flag values without treating them as positional text.
		if arg == "--url" || arg == "--as" {
			if i+1 < len(args) {
				i++
				result = append(result, args[i])
			}
			continue
		}
		if !strings.HasPrefix(arg, "-") {
			if command {
				literal = true
			} else {
				command = true
			}
		}
	}
	return result, force
}

func helpText() string {
	return `agentctl controls and reads AgentD sessions by session ID.
Commands: run, status, ls, since, grep, wait, result, approvals,
          prompt, steer, interrupt, cancel, terminate, resume, approve, deny, mark.
Global flags: --url URL, --as CALLER, --json, --raw.
Controls for managed sessions require their orchestrator's wrapper or --force
before the session ID. mark ID --as CALLER [--restore SEQUENCE|none]
[--cursor-dir DIR] [--json] records or restores a control mark. mark requires
an explicit --as; wrappers must use the same --cursor-dir for mark and wait.`
}
