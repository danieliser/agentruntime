package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
)

func cmdSince(ctx *Context, args []string) error {
	fs, dir, err := streamFlags(ctx, "since", args)
	if err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("%w: since requires ID", ErrUsage)
	}
	ref, refErr := ParseSessionReference(fs.Arg(0))
	if refErr != nil {
		return refErr
	}
	store, err := cursorStoreFrom(processEnv(), *dir)
	if err != nil {
		return err
	}
	if ref.Kind == "plan" || ref.Kind == "plan_node" {
		return sincePlan(ctx, ref, store)
	}
	cursor, err := store.Load(ctx.Config.Caller, fs.Arg(0))
	if err != nil {
		return err
	}
	var events []streamEvent
	for {
		page, err := ctx.API.Events(context.Background(), fs.Arg(0), cursor, 1000)
		if err != nil {
			return err
		}
		for _, e := range page.Events {
			events = append(events, streamEvent{e.Sequence, e.Type, e.Stream, e.Payload, base64.StdEncoding.EncodeToString(e.Raw)})
		}
		if !page.HasMore {
			break
		}
		cursor = page.LastSequence
	}
	if len(events) == 0 {
		return nil
	}
	if ctx.Config.JSON {
		err = WriteOutput(ctx.Out, ctx.Config, events, "")
	} else {
		output := summarizeEvents(events, ctx.Config.Raw)
		if output != "" {
			_, err = fmt.Fprint(ctx.Out, output)
		}
	}
	if err != nil {
		return err
	}
	return store.Save(ctx.Config.Caller, fs.Arg(0), events[len(events)-1].Sequence)
}

func sincePlan(ctx *Context, ref SessionReference, store CursorStore) error {
	g, e := ctx.Persist.planGraph(ref.ID)
	if e != nil {
		return e
	}
	var b strings.Builder
	type tagged struct {
		Node  string      `json:"node"`
		Event streamEvent `json:"event"`
	}
	var taggedEvents []tagged
	advanced := map[string]int64{}
	for _, n := range g.Nodes {
		if ref.Kind == "plan_node" && n.ID != ref.Node && slug(n.Title) != ref.Node {
			continue
		}
		for _, session := range n.sessionIDs() {
			cursor, e := store.Load(ctx.Config.Caller, session)
			if e != nil {
				return e
			}
			for {
				page, e := ctx.API.Events(context.Background(), session, cursor, 1000)
				if e != nil {
					return e
				}
				var out []streamEvent
				for _, ev := range page.Events {
					out = append(out, streamEvent{ev.Sequence, ev.Type, ev.Stream, ev.Payload, base64.StdEncoding.EncodeToString(ev.Raw)})
				}
				if len(out) > 0 {
					for _, event := range out {
						taggedEvents = append(taggedEvents, tagged{Node: n.ID, Event: event})
					}
					fmt.Fprintf(&b, "[%s] %s", n.ID, summarizeEvents(out, ctx.Config.Raw))
					cursor = out[len(out)-1].Sequence
				}
				if !page.HasMore || len(page.Events) == 0 {
					break
				}
			}
			advanced[session] = cursor
		}
	}
	if ctx.Config.JSON {
		e = WriteOutput(ctx.Out, ctx.Config, taggedEvents, "")
	} else {
		_, e = fmt.Fprint(ctx.Out, b.String())
	}
	if e != nil {
		return e
	}
	for session, cursor := range advanced {
		if e = store.Save(ctx.Config.Caller, session, cursor); e != nil {
			return e
		}
	}
	return nil
}
