package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/danieliser/agentruntime/pkg/client"
)

type receiptResult struct {
	Receipt            *client.TerminalReceipt `json:"receipt"`
	FinalAssistantText string                  `json:"final_assistant_text"`
}

func cmdResult(ctx *Context, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("%w: result ID", ErrUsage)
	}
	r, err := ctx.API.client.GetStructuredResult(context.Background(), args[0])
	if err == nil {
		if ctx.Config.JSON {
			return WriteOutput(ctx.Out, ctx.Config, json.RawMessage(r.Bytes), "")
		}
		_, err = fmt.Fprintln(ctx.Out, string(r.Bytes))
		return err
	}
	if !errors.Is(ctx.API.safeError(err), ErrNotFound) {
		return ctx.API.safeError(err)
	}
	receipt, err := ctx.API.client.GetTerminalReceipt(context.Background(), args[0])
	if err != nil {
		return ctx.API.safeError(err)
	}
	text, err := finalAssistantText(ctx, args[0])
	if err != nil {
		return err
	}
	return WriteOutput(ctx.Out, ctx.Config, receiptResult{receipt, text}, fmt.Sprintf("%s %s reason=%s\n%s", receipt.SessionID, receipt.State, receipt.Reason, text))
}

// Prefer completed assistant messages to deltas, which contain the same text.
// Reset at each provider turn so follow-ups don't return earlier replies.
func finalAssistantText(ctx *Context, id string) (string, error) {
	var cursor int64
	var completed, delta strings.Builder
	for {
		page, err := ctx.API.Events(context.Background(), id, cursor, 500)
		if err != nil {
			return "", err
		}
		for _, ev := range page.Events {
			cursor = ev.Sequence
			var p struct {
				Text string `json:"text"`
				Item struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"item"`
			}
			if err := json.Unmarshal(ev.Payload, &p); err != nil {
				return "", fmt.Errorf("decode event %d: %w", ev.Sequence, err)
			}
			switch ev.Type {
			case "turn.started", "lifecycle.turn.started", "control.prompt.dispatched":
				completed.Reset()
				delta.Reset()
			case "content.delta":
				delta.WriteString(p.Text)
			case "content.completed":
				completed.Reset()
				completed.WriteString(p.Text)
			case "tool.result":
				if p.Item.Type == "agentMessage" {
					completed.Reset()
					completed.WriteString(p.Item.Text)
				}
			}
		}
		if !page.HasMore || len(page.Events) == 0 {
			break
		}
	}
	if completed.Len() > 0 {
		return completed.String(), nil
	}
	return delta.String(), nil
}
