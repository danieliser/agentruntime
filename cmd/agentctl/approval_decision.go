package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

func decide(ctx *Context, args []string, decision string) error {
	if len(args) != 2 {
		return fmt.Errorf("%w: %s ID REQUEST_ID", ErrUsage, decision)
	}
	if err := guardControl(ctx, args[0]); err != nil {
		return err
	}
	undo := markControl(ctx, args[0])
	var requestID json.RawMessage
	if json.Valid([]byte(args[1])) {
		requestID = json.RawMessage(args[1])
	} else {
		b, _ := json.Marshal(args[1])
		requestID = b
	}
	body := map[string]any{"request_id": requestID, "decision": map[bool]string{true: "accept", false: "decline"}[decision == "approve"], "idempotency_key": idemKey("approval", args[0], string(requestID)+decision)}
	var result any
	if e := apiRaw(ctx, http.MethodPost, "/api/v1/sessions/"+url.PathEscape(args[0])+"/approvals", body, &result); e != nil {
		undo()
		return e
	}
	return WriteOutput(ctx.Out, ctx.Config, result, "accepted")
}
