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
	ctx := &Context{Config: inv.Config, API: NewAPIClient(inv.Config), Out: out, Log: log, Command: inv.Command}
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
