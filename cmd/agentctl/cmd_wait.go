package main

import (
	"context"
	"flag"
	"fmt"
	"strconv"
	"strings"
	"time"
)

func cmdWait(ctx *Context, args []string) error {
	fs := flag.NewFlagSet("wait", flag.ContinueOnError)
	fs.SetOutput(ctx.Log)
	dir := fs.String("cursor-dir", "", "cursor directory")
	fs.BoolVar(&ctx.Config.Raw, "raw", ctx.Config.Raw, "raw events")
	until := fs.String("until", "turn_end", "reasons: turn_end,approval,error,done")
	timeout := fs.Duration("timeout", 0, "maximum wait duration")
	if err := fs.Parse(normalizeStreamArgs(args, map[string]bool{"cursor-dir": true, "until": true, "timeout": true})); err != nil {
		return fmt.Errorf("%w: %v", ErrUsage, err)
	}
	if fs.NArg() != 1 || *timeout < 0 {
		return fmt.Errorf("%w: wait ID --until reason[,reason] --timeout DUR", ErrUsage)
	}
	ref, refErr := ParseSessionReference(fs.Arg(0))
	if refErr != nil {
		return refErr
	}
	if ref.Kind == "plan" {
		return waitPlan(ctx, ref.ID, strings.Split(*until, ","), *timeout, *dir)
	}
	reasons := strings.Split(*until, ",")
	valid := map[string]bool{"turn_end": true, "approval": true, "error": true, "done": true}
	for _, r := range reasons {
		if !valid[r] {
			return fmt.Errorf("%w: unknown wait reason %q", ErrUsage, r)
		}
	}
	store, err := cursorStoreFrom(processEnv(), *dir)
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
	return waitStream(ctxWait, ctx.Config, fs.Arg(0), store, reasons, *timeout, ctx.Out)
}

func waitPlan(ctx *Context, id string, reasons []string, timeout time.Duration, cursorDir string) error {
	valid := map[string]bool{"node_started": true, "approval": true, "node_done": true, "failed": true, "done": true}
	for _, r := range reasons {
		if !valid[r] {
			return fmt.Errorf("%w: unknown plan wait reason %q", ErrUsage, r)
		}
	}
	store, err := cursorStoreFrom(processEnv(), cursorDir)
	if err != nil {
		return err
	}
	key := "plan:" + id
	cur, err := store.Load(ctx.Config.Caller, key)
	if err != nil {
		return err
	}
	deadline := time.Time{}
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}
	for {
		events, next, e := ctx.Persist.planEvents(id, strconv.FormatInt(cur, 10))
		if e != nil {
			return e
		}
		for _, ev := range events {
			c, _ := strconv.ParseInt(ev.Cursor, 10, 64)
			if c > cur {
				cur = c
			}
			reason := planWaitReason(ev)
			if contains(reasons, reason) {
				node := ev.TaskID
				if node == "" {
					node = "plan"
				}
				if _, e = fmt.Fprintf(ctx.Out, "%s node=%s session=%s request_id=%s\n", reason, node, emptyDash(ev.SessionID), emptyDash(ev.RequestID)); e != nil {
					return e
				}
				if e = store.Save(ctx.Config.Caller, key, cur); e != nil {
					return e
				}
				return WaitReasonError(reason)
			}
		}
		if e = store.Save(ctx.Config.Caller, key, cur); e != nil {
			return e
		}
		if timeout > 0 && time.Now().After(deadline) {
			return ErrTimeout
		}
		_ = next
		time.Sleep(400 * time.Millisecond)
	}
}
func planWaitReason(e planEvent) string {
	switch e.Type {
	case "node_started":
		return "node_started"
	case "approval_requested":
		return "approval"
	case "node_completed":
		return "node_done"
	case "node_failed":
		return "failed"
	case "plan_status_changed":
		if e.NewStatus == "completed" || e.NewStatus == "done" {
			return "done"
		}
		if e.NewStatus == "failed" {
			return "failed"
		}
	}
	return ""
}
