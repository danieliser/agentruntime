package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gorilla/websocket"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func waitStream(ctx context.Context, cfg Config, session string, store CursorStore, reasons []string, timeout time.Duration, out interface{ Write([]byte) (int, error) }) error {
	cursor, err := store.Load(cfg.Caller, session)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	for {
		if err := ctx.Err(); err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return ErrTimeout
			}
			return err
		}
		u, err := url.Parse(cfg.URL)
		if err != nil {
			return errors.Join(ErrAPI, err)
		}
		if u.Scheme == "https" {
			u.Scheme = "wss"
		} else {
			u.Scheme = "ws"
		}
		u.Path = strings.TrimRight(u.Path, "/") + "/api/v1/ws/sessions/" + url.PathEscape(session) + "/events"
		q := u.Query()
		q.Set("after_sequence", strconv.FormatInt(cursor, 10))
		u.RawQuery = q.Encode()
		h := http.Header{}
		if cfg.Token != "" {
			h.Set("Authorization", "Bearer "+cfg.Token)
		}
		conn, response, dialErr := websocket.DefaultDialer.DialContext(ctx, u.String(), h)
		if dialErr == nil {
			if timeout > 0 {
				_ = conn.SetReadDeadline(deadline)
			}
			for {
				var frame map[string]json.RawMessage
				if err = conn.ReadJSON(&frame); err != nil {
					break
				}
				var kind string
				_ = json.Unmarshal(frame["frame_type"], &kind)
				if kind == "stream.ready" {
					continue
				}
				var event streamEvent
				if e := json.Unmarshal(mustJSON(frame), &event); e != nil {
					err = e
					break
				}
				if event.Sequence <= cursor {
					continue
				}
				if event.Sequence != cursor+1 {
					err = fmt.Errorf("event sequence gap after %d", cursor)
					break
				}
				reason := matchingReason(event)
				matched := contains(reasons, reason)
				if matched {
					notice := struct {
						Sequence int64  `json:"sequence"`
						Type     string `json:"type"`
						Reason   string `json:"reason"`
						Summary  string `json:"summary,omitempty"`
					}{event.Sequence, event.Type, reason, strings.TrimPrefix(summarySuffix(event), ": ")}
					if e := WriteOutput(out, cfg, notice, event.Type+summarySuffix(event)); e != nil {
						_ = conn.Close()
						return e
					}
				}
				if e := store.Save(cfg.Caller, session, event.Sequence); e != nil {
					_ = conn.Close()
					return e
				}
				cursor = event.Sequence
				if matched {
					_ = conn.Close()
					return WaitReasonError(reason)
				}
			}
			_ = conn.Close()
		} else {
			if response != nil {
				response.Body.Close()
				switch response.StatusCode {
				case http.StatusUnauthorized, http.StatusForbidden:
					return ErrAuth
				case http.StatusNotFound:
					return ErrNotFound
				default:
					if response.StatusCode >= 400 && response.StatusCode < 500 {
						return ErrAPI
					}
				}
			}
			err = dialErr
		}
		if timeout > 0 && time.Until(deadline) <= 0 {
			return ErrTimeout
		}
		select {
		case <-ctx.Done():
			continue
		case <-time.After(100 * time.Millisecond):
		}
		_ = err
	}
}
