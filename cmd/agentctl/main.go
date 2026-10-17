package main

import (
	"fmt"
	"io"
	"os"
	"strings"
)

type Context struct {
	Config    Config
	API       *APIClient
	Persist   *PersistClient
	Out, Log  io.Writer
	Command   string
	Direct    bool
	Reference *SessionReference
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
	args, direct := extractDirectFlag(args)
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
	ctx := &Context{Config: inv.Config, API: NewAPIClient(inv.Config), Persist: NewPersistClient(inv.Config, environ), Out: out, Log: log, Command: inv.Command, Direct: direct}
	if len(inv.Args) > 0 && inv.Command != "plan" {
		if id, ok := sessionArgument(inv.Command); ok && id < len(inv.Args) {
			ref, parseErr := ParseSessionReference(inv.Args[id])
			if parseErr != nil {
				fmt.Fprintln(log, parseErr)
				return ExitCode(parseErr)
			}
			ctx.Reference = &ref
			planSurface := ref.Kind == "plan" || ref.Kind == "plan_node"
			planCommand := planSurface && (inv.Command == "status" || inv.Command == "since" || inv.Command == "grep" || inv.Command == "wait")
			shouldResolve := ref.Kind != "session" && !planCommand
			if shouldResolve {
				resolved, resolveErr := ctx.Persist.resolve(ref)
				if resolveErr != nil {
					fmt.Fprintln(log, redactedMessage(resolveErr, token))
					return ExitCode(resolveErr)
				}
				inv.Args[id] = resolved
			}
		}
	}
	ctx.Command = inv.Command
	if err = commandRegistry[inv.Command](ctx, inv.Args); err != nil {
		fmt.Fprintln(log, redactedMessage(err, token))
		return ExitCode(err)
	}
	return ExitSuccess
}

func sessionArgument(command string) (int, bool) {
	switch command {
	case "wait", "status", "since", "grep", "steer", "prompt", "interrupt", "cancel", "terminate", "resume", "approvals", "approve", "deny", "result":
		return 0, true
	}
	return 0, false
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
func extractDirectFlag(args []string) ([]string, bool) {
	result := make([]string, 0, len(args))
	direct, command, literal := false, false, false
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
		if arg == "--direct" {
			direct = true
			continue
		}
		result = append(result, arg)
		// Consume global flag values without treating them as positional text.
		if arg == "--url" || arg == "--persist-url" || arg == "--as" {
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
	return result, direct
}
