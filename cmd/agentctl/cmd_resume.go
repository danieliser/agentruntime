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
	if err := requireDirectForTaskLifecycle(ctx, args[0]); err != nil {
		return err
	}
	undo := markControl(ctx, args[0])
	s, e := ctx.API.client.Resume(context.Background(), args[0], strings.Join(args[1:], " "), nil)
	if e != nil {
		undo()
		return ctx.API.safeError(e)
	}
	if s.SessionID != args[0] {
		markCreated(ctx, s.SessionID)
	}
	return WriteOutput(ctx.Out, ctx.Config, s, s.SessionID)
}
