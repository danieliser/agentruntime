package main

import (
	"context"
	"fmt"
	"github.com/danieliser/agentruntime/pkg/client"
	"regexp"
	"strings"
)

func cmdGrep(ctx *Context, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("%w: grep ID PATTERN [--type TYPE]", ErrUsage)
	}
	id, pattern := args[0], args[1]
	ref, refErr := ParseSessionReference(id)
	if refErr != nil {
		return refErr
	}
	filter := ""
	for i := 2; i < len(args); i++ {
		if args[i] == "--type" && i+1 < len(args) {
			filter = args[i+1]
			i++
		} else {
			return fmt.Errorf("%w: unknown grep option", ErrUsage)
		}
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return fmt.Errorf("%w: invalid pattern: %v", ErrUsage, err)
	}
	if ref.Kind == "plan" || ref.Kind == "plan_node" {
		g, e := ctx.Persist.planGraph(ref.ID)
		if e != nil {
			return e
		}
		var tagged []map[string]any
		var lines []string
		for _, n := range g.Nodes {
			if ref.Kind == "plan_node" && n.ID != ref.Node && slug(n.Title) != ref.Node {
				continue
			}
			for _, session := range n.sessionIDs() {
				var cursor int64
				for {
					p, e := ctx.API.Events(context.Background(), session, cursor, 500)
					if e != nil {
						return e
					}
					for _, ev := range p.Events {
						cursor = ev.Sequence
						if (filter == "" || ev.Type == filter) && re.Match(append(append([]byte(ev.Type+" "), ev.Payload...), ev.Raw...)) {
							tagged = append(tagged, map[string]any{"node": n.ID, "session_id": session, "event": ev})
							lines = append(lines, fmt.Sprintf("[%s/%s] %s %s", n.ID, session, ev.Type, string(ev.Payload)))
						}
					}
					if !p.HasMore || len(p.Events) == 0 {
						break
					}
				}
			}
		}
		return WriteOutput(ctx.Out, ctx.Config, tagged, strings.Join(lines, "\n"))
	}
	var matches []client.Event
	var cursor int64
	for {
		p, e := ctx.API.Events(context.Background(), id, cursor, 500)
		if e != nil {
			return e
		}
		for _, ev := range p.Events {
			cursor = ev.Sequence
			if (filter == "" || ev.Type == filter) && re.Match(append(append([]byte(ev.Type+" "), ev.Payload...), ev.Raw...)) {
				matches = append(matches, ev)
			}
		}
		if !p.HasMore || len(p.Events) == 0 {
			break
		}
	}
	return WriteOutput(ctx.Out, ctx.Config, matches, fmt.Sprintf("%d matching events", len(matches)))
}
