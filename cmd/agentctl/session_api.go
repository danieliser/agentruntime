package main

import (
	"bytes"
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
		return ctx.API.safeError(fmt.Errorf("request failed with status %s: %s", resp.Status, strings.TrimSpace(string(b))))
	}
	if out != nil {
		if e = json.NewDecoder(resp.Body).Decode(out); e != nil {
			return ctx.API.safeError(e)
		}
	}
	return nil
}
func approvals(ctx *Context, id string) (approvalList, error) {
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
