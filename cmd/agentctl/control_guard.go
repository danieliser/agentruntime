package main

import (
	"context"
	"fmt"
)

// guardControl leaves orchestration policy to the session's named manager.
// Reads and untagged sessions always use AgentD directly.
func guardControl(ctx *Context, session string) error {
	if ctx.Force {
		return nil
	}
	view, err := ctx.API.GetSession(context.Background(), session)
	if err != nil {
		return err
	}
	if manager := view.Tags["managed_by"]; manager != "" {
		return fmt.Errorf("%w: session is managed by %s; use %sctl or --force to control it directly", ErrUsage, manager, manager)
	}
	return nil
}
