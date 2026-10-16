package main

import (
	"context"
	"fmt"
)

func cmdLS(ctx *Context, args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("%w: ls", ErrUsage)
	}
	v, e := ctx.API.ListSessions(context.Background())
	if e != nil {
		return e
	}
	return WriteOutput(ctx.Out, ctx.Config, v, fmt.Sprintf("%d sessions", len(v)))
}
