package main

import (
	"context"
	"fmt"
	"github.com/google/uuid"
	"strings"
)

func cmdInput(ctx *Context, args []string, kind string) error {
	if len(args) < 2 {
		return fmt.Errorf("%w: %s ID text", ErrUsage, kind)
	}
	input := args[1:]
	if input[0] == "--" {
		input = input[1:]
	}
	if len(input) == 0 {
		return fmt.Errorf("%w: missing input", ErrUsage)
	}
	text := strings.Join(input, " ")
	undo := markControl(ctx, args[0])
	if routed, err := routePersistControl(ctx, args[0], kind, text); routed {
		if err != nil {
			undo()
			return err
		}
		return WriteOutput(ctx.Out, ctx.Config, map[string]any{"session_id": args[0], "accepted": true}, "accepted")
	}
	key := uuid.NewString()
	e := ctx.API.client.SendInput(context.Background(), args[0], key, kind, text)
	if e != nil {
		undo()
		return ctx.API.safeError(e)
	}
	return WriteOutput(ctx.Out, ctx.Config, map[string]any{"session_id": args[0], "accepted": true}, "accepted")
}
