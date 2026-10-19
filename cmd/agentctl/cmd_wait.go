package main

import (
	"context"
	"flag"
	"fmt"
	"strings"
)

func cmdWait(ctx *Context, args []string) error {
	fs := flag.NewFlagSet("wait", flag.ContinueOnError)
	fs.SetOutput(ctx.Log)
	dir := fs.String("cursor-dir", "", "cursor directory")
	fs.BoolVar(&ctx.Config.Raw, "raw", ctx.Config.Raw, "raw events")
	until := fs.String("until", "turn_end", "reasons: turn_end,approval,error,done,stalled")
	stall := fs.Duration("stall", 0, "provider silence threshold (required for stalled)")
	timeout := fs.Duration("timeout", 0, "maximum wait duration")
	if err := fs.Parse(normalizeStreamArgs(args, map[string]bool{"cursor-dir": true, "until": true, "timeout": true, "stall": true})); err != nil {
		return fmt.Errorf("%w: %v", ErrUsage, err)
	}
	if fs.NArg() != 1 || *timeout < 0 || *stall < 0 || contains(strings.Split(*until, ","), "stalled") && *stall <= 0 {
		return fmt.Errorf("%w: wait ID --until reason[,reason] --timeout DUR", ErrUsage)
	}
	reasons := strings.Split(*until, ",")
	valid := map[string]bool{"turn_end": true, "approval": true, "error": true, "done": true, "stalled": true}
	for _, r := range reasons {
		if !valid[r] {
			return fmt.Errorf("%w: unknown wait reason %q", ErrUsage, r)
		}
	}
	store, err := commandCursorStore(ctx, *dir)
	if err != nil {
		return err
	}
	var ctxWait context.Context
	var cancel context.CancelFunc
	if *timeout > 0 {
		ctxWait, cancel = context.WithTimeout(context.Background(), *timeout)
	} else {
		ctxWait, cancel = context.WithCancel(context.Background())
	}
	defer cancel()
	id := fs.Arg(0)
	reason, err := waitStart(ctxWait, ctx, id, store, reasons)
	if err != nil {
		return err
	}
	if reason != "" {
		return WaitReasonError(reason)
	}
	return waitStream(ctxWait, ctx.Config, id, store, reasons, *timeout, ctx.Out, *stall)
}
