package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

type approvalList struct {
	SessionID string            `json:"session_id"`
	Approvals []json.RawMessage `json:"approvals"`
}

type apiStatusError struct {
	Status int
	Err    error
}

func (err *apiStatusError) Error() string { return err.Err.Error() }
func (err *apiStatusError) Unwrap() error { return err.Err }

func apiRaw(ctx *Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		b, e := json.Marshal(body)
		if e != nil {
			return e
		}
		reader = bytes.NewReader(b)
	}
	req, e := http.NewRequest(method, ctx.API.client.BaseURL+path, reader)
	if e != nil {
		return e
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+ctx.Config.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, e := ctx.API.client.HTTPClient.Do(req)
	if e != nil {
		return ctx.API.safeError(e)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &apiStatusError{Status: resp.StatusCode, Err: ctx.API.safeError(fmt.Errorf("request failed with status %s: %s", resp.Status, strings.TrimSpace(string(b))))}
	}
	if out != nil {
		if e = json.NewDecoder(resp.Body).Decode(out); e != nil {
			return ctx.API.safeError(e)
		}
	}
	return nil
}
func approvals(ctx *Context, id string) (approvalList, error) {
	if ctx.Direct || ctx.Persist == nil {
		return directApprovals(ctx, id)
	}
	if ctx.API != nil {
		view, err := ctx.API.GetSession(context.Background(), id)
		if err != nil {
			return approvalList{}, err
		}
		// Reads of unstamped, task-free sessions stay direct (as before tags).
		if view.Tags[persistManagedTag] == "" && view.TaskID == "" {
			return directApprovals(ctx, id)
		}
	}
	owned, err := persistOwnsSession(ctx, id)
	if err != nil {
		return approvalList{}, err
	}
	if !owned {
		return directApprovals(ctx, id)
	}
	var result approvalList
	err = ctx.Persist.get(context.Background(), "/api/v1/sessions/"+url.PathEscape(id)+"/approvals", &result)
	if err != nil && persistUnavailable(err) {
		err = persistDownError(err)
	}
	return result, err
}
func directApprovals(ctx *Context, id string) (approvalList, error) {
	var v struct {
		Data approvalList `json:"data"`
	}
	e := apiRaw(ctx, http.MethodGet, "/api/v1/sessions/"+url.PathEscape(id)+"/approvals", nil, &v)
	return v.Data, e
}
func idemKey(kind, id, text string) string {
	h := sha256.Sum256([]byte(kind + "\x00" + id + "\x00" + text))
	return kind + ":" + hex.EncodeToString(h[:])
}
