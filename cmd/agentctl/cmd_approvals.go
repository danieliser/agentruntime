package main

import (
	"fmt"
)

func cmdApprovals(ctx *Context, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("%w: approvals ID", ErrUsage)
	}
	a, e := approvals(ctx, args[0])
	if e != nil {
		return e
	}
	return WriteOutput(ctx.Out, ctx.Config, a, fmt.Sprintf("%d pending approvals", len(a.Approvals)))
}
