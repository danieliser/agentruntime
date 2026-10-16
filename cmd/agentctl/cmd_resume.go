package main

import (
	"context"
	"fmt"
	"strings"
)

func cmdResume(ctx *Context, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("%w: resume ID prompt", ErrUsage)
	}
	s, e := ctx.API.client.Resume(context.Background(), args[0], strings.Join(args[1:], " "), nil)
	if e != nil {
		return ctx.API.safeError(e)
	}
	return WriteOutput(ctx.Out, ctx.Config, s, s.SessionID)
}
