package main

import (
	"context"
	"flag"
	"fmt"
	"strconv"
	"strings"
)

type controlMark struct {
	SessionID string `json:"session_id"`
	Caller    string `json:"caller"`
	Sequence  *int64 `json:"sequence"`
	Previous  *int64 `json:"previous"`
}

func cmdMark(ctx *Context, args []string) error {
	fs := flag.NewFlagSet("mark", flag.ContinueOnError)
	fs.SetOutput(ctx.Log)
	dir := fs.String("cursor-dir", "", "cursor directory")
	restore := fs.String("restore", "", "previous sequence or none")
	if err := fs.Parse(normalizeStreamArgs(args, map[string]bool{"restore": true, "cursor-dir": true})); err != nil {
		return fmt.Errorf("%w: %v", ErrUsage, err)
	}
	if fs.NArg() != 1 || !ctx.Config.CallerExplicit || strings.TrimSpace(ctx.Config.Caller) == "" {
		return fmt.Errorf("%w: mark ID --as CALLER [--restore SEQUENCE|none] [--cursor-dir DIR]", ErrUsage)
	}
	restoring := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "restore" {
			restoring = true
		}
	})
	var sequence *int64
	if restoring && *restore != "none" {
		value, err := strconv.ParseInt(*restore, 10, 64)
		if err != nil || value < 0 {
			return fmt.Errorf("%w: --restore requires a nonnegative sequence or none", ErrUsage)
		}
		sequence = &value
	}
	store, err := commandCursorStore(ctx, *dir)
	if err != nil {
		return err
	}
	id, caller := fs.Arg(0), ctx.Config.Caller
	key := controlKey(id)
	previous, found, err := store.Lookup(caller, key)
	if err != nil {
		return err
	}
	result := controlMark{SessionID: id, Caller: caller, Sequence: sequence}
	if found {
		result.Previous = &previous
	}
	if !restoring || sequence != nil {
		view, err := ctx.API.GetSession(context.Background(), id)
		if err != nil {
			return err
		}
		if !restoring {
			result.Sequence = &view.LastSequence
		} else if *sequence > view.LastSequence {
			return fmt.Errorf("%w: --restore sequence %d exceeds session head %d", ErrUsage, *sequence, view.LastSequence)
		}
	}
	if result.Sequence == nil {
		err = store.Delete(caller, key)
	} else {
		err = store.Save(caller, key, *result.Sequence)
	}
	if err != nil {
		return err
	}
	text := "none"
	if result.Sequence != nil {
		text = strconv.FormatInt(*result.Sequence, 10)
	}
	return WriteOutput(ctx.Out, ctx.Config, result, fmt.Sprintf("%s caller=%s sequence=%s", id, caller, text))
}
