package main

import (
	"context"
	"encoding/base64"
	"fmt"
)

func cmdSince(ctx *Context, args []string) error {
	fs, dir, err := streamFlags(ctx, "since", args)
	if err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("%w: since requires ID", ErrUsage)
	}
	store, err := commandCursorStore(ctx, *dir)
	if err != nil {
		return err
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
		if !page.HasMore || len(page.Events) == 0 {
			break
		}
		cursor = page.Events[len(page.Events)-1].Sequence
	}
	if len(events) == 0 {
		return nil
	}
	if ctx.Config.JSON {
		visible := events
		if !ctx.Config.Raw {
			visible = nil
			for _, event := range events {
				if !hiddenSubagentDelta(event.Type) {
					visible = append(visible, event)
				}
			}
		}
		if len(visible) > 0 {
			err = WriteOutput(ctx.Out, ctx.Config, visible, "")
		}
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
