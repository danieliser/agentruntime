package main

import (
	"context"
	"fmt"
	"github.com/google/uuid"
)

func control(ctx *Context, args []string, which string) error {
	if len(args) != 1 {
		return fmt.Errorf("%w: %s ID", ErrUsage, which)
	}
	key := uuid.NewString()
	var e error
	switch which {
	case "interrupt":
		e = ctx.API.client.Interrupt(context.Background(), args[0], key)
	case "cancel":
		e = ctx.API.client.Cancel(context.Background(), args[0], key)
	case "terminate":
		e = ctx.API.client.Terminate(context.Background(), args[0], key)
	}
	if e != nil {
		return ctx.API.safeError(e)
	}
	return WriteOutput(ctx.Out, ctx.Config, map[string]any{"session_id": args[0], "accepted": true}, "accepted")
}
