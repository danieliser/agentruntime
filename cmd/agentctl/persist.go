package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type SessionReference struct{ Kind, ID, Node string }

func ParseSessionReference(value string) (SessionReference, error) {
	if !strings.HasPrefix(value, "task:") && !strings.HasPrefix(value, "plan:") {
		return SessionReference{Kind: "session", ID: value}, nil
	}
	kind, rest, _ := strings.Cut(value, ":")
	if kind == "task" && rest != "" {
		return SessionReference{Kind: kind, ID: rest}, nil
	}
	if kind == "plan" && rest != "" {
		id, node, hasNode := strings.Cut(rest, "/")
		if id != "" {
			ref := SessionReference{Kind: "plan", ID: id}
			if hasNode {
				if node == "" {
					return SessionReference{}, fmt.Errorf("%w: empty plan node", ErrUsage)
				}
				ref.Kind = "plan_node"
				ref.Node = node
			}
			return ref, nil
		}
	}
	return SessionReference{}, fmt.Errorf("%w: invalid session reference %q", ErrUsage, value)
}

type PersistClient struct {
	cfg     Config
	environ map[string]string
	client  *http.Client
}

func NewPersistClient(cfg Config, environ ...map[string]string) *PersistClient {
	env := processEnv()
	if len(environ) > 0 {
		env = environ[0]
	}
	return &PersistClient{cfg: cfg, environ: env, client: &http.Client{Timeout: 30 * time.Second}}
}
func (p *PersistClient) token() (string, error) {
	if p.cfg.PersistToken != "" {
		return p.cfg.PersistToken, nil
	}
	v, e := LoadPersistToken(p.environ)
	if e != nil {
		return "", errors.Join(ErrAuth, e)
	}
	p.cfg.PersistToken = v
	return v, nil
}
func (p *PersistClient) get(ctx context.Context, path string, out any) error {
	return p.request(ctx, http.MethodGet, path, nil, out)
}
func (p *PersistClient) request(ctx context.Context, method, path string, body any, out any) error {
	token, e := p.token()
	if e != nil {
		return e
	}
	var reader *strings.Reader
	if body == nil {
		reader = strings.NewReader("")
	} else {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = strings.NewReader(string(b))
	}
	req, e := http.NewRequestWithContext(ctx, method, strings.TrimRight(p.cfg.PersistURL, "/")+path, reader)
	if e != nil {
		return e
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, e := p.client.Do(req)
	if e != nil {
		return e
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode == 404 {
			return ErrNotFound
		}
		return fmt.Errorf("PERSIST request failed with status %d", resp.StatusCode)
	}
	if out != nil {
		if e = json.NewDecoder(resp.Body).Decode(out); e != nil {
			return e
		}
	}
	return nil
}
func (p *PersistClient) resolve(ref SessionReference) (string, error) {
	if ref.Kind == "session" {
		return ref.ID, nil
	}
	if ref.Kind == "task" {
		var v map[string]any
		e := p.get(context.Background(), "/api/v1/tasks/"+url.PathEscape(ref.ID)+"?detail=full", &v)
		if e != nil {
			return "", e
		}
		id, _ := v["current_session_id"].(string)
		if id == "" {
			id, _ = v["session_id"].(string)
		}
		if id == "" {
			return "", fmt.Errorf("%w: task has no current session", ErrNotFound)
		}
		return id, nil
	}
	g, e := p.planGraph(ref.ID)
	if e != nil {
		return "", e
	}
	if ref.Kind == "plan" {
		for i := range g.Nodes {
			if g.Nodes[i].SessionID != "" {
				return g.Nodes[i].SessionID, nil
			}
		}
		return "", fmt.Errorf("%w: plan has no current session", ErrNotFound)
	}
	for _, n := range g.Nodes {
		if n.ID == ref.Node || slug(n.Title) == ref.Node {
			if n.SessionID == "" {
				return "", fmt.Errorf("%w: plan node has no current session", ErrNotFound)
			}
			return n.SessionID, nil
		}
	}
	return "", fmt.Errorf("%w: plan node %q not found", ErrNotFound, ref.Node)
}
func (p *PersistClient) planGraph(id string) (planGraph, error) {
	var result struct {
		Graph planGraph `json:"graph"`
	}
	e := p.get(context.Background(), "/api/v1/plans/"+url.PathEscape(id), &result)
	return result.Graph, e
}

type planEvent struct {
	Cursor    string `json:"cursor"`
	Type      string `json:"type"`
	TaskID    string `json:"task_id"`
	SessionID string `json:"session_id"`
	RequestID string `json:"request_id"`
	NewStatus string `json:"new_status"`
}

func (p *PersistClient) planEvents(id, after string) ([]planEvent, string, error) {
	var v struct {
		Events []planEvent `json:"events"`
		Next   string      `json:"next_cursor"`
	}
	e := p.get(context.Background(), "/api/v1/plans/"+url.PathEscape(id)+"/events?after="+url.QueryEscape(after), &v)
	return v.Events, v.Next, e
}

func (p *PersistClient) controlSession(session, command, payload string) (bool, error) {
	base := "/api/v1/sessions/" + url.PathEscape(session)
	if command == "approve" || command == "deny" {
		parts := strings.SplitN(payload, " ", 2)
		if len(parts) != 2 {
			return false, fmt.Errorf("%w: %s SESSION REQUEST_ID", ErrUsage, command)
		}
		requestID := parts[1]
		if strings.HasPrefix(requestID, "\"") {
			if err := json.Unmarshal([]byte(requestID), &requestID); err != nil {
				return true, fmt.Errorf("%w: invalid approval ID", ErrUsage)
			}
		}
		var out any
		e := p.request(context.Background(), http.MethodPost, base+"/approvals/"+url.PathEscape(requestID)+"/"+command, nil, &out)
		return true, e
	}
	path := base + "/" + command
	var body any
	switch command {
	case "steer":
		body = map[string]string{"message": payload}
	case "prompt":
		body = map[string]string{"prompt": payload}
	case "interrupt":
	default:
		return false, nil
	}
	var out any
	e := p.request(context.Background(), http.MethodPost, path, body, &out)
	return true, e
}
func (p *PersistClient) ownsSession(session string) (bool, error) {
	var out any
	e := p.get(context.Background(), "/api/v1/sessions/"+url.PathEscape(session)+"/approvals", &out)
	if errors.Is(e, ErrNotFound) {
		return false, nil
	}
	if e != nil {
		return false, e
	}
	return true, nil
}

func routePersistControl(ctx *Context, session, command, payload string) (bool, error) {
	if ctx.Direct || ctx.Persist == nil {
		return false, nil
	}
	if ctx.Reference != nil && ctx.Reference.Kind != "session" {
		resolved := session
		if ref, err := ParseSessionReference(session); err == nil && ref.Kind != "session" {
			var resolveErr error
			resolved, resolveErr = ctx.Persist.resolve(*ctx.Reference)
			if resolveErr != nil {
				return true, resolveErr
			}
		}
		return ctx.Persist.controlSession(resolved, command, payload)
	}
	owned, e := ctx.Persist.ownsSession(session)
	if e != nil {
		return true, e
	}
	if !owned {
		// A hidden/unauthorized PERSIST resource must never turn into a direct
		// AgentD write. Task ownership comes from AgentD's stored manifest.
		if ctx.API != nil {
			view, err := ctx.API.GetSession(context.Background(), session)
			if err != nil {
				return true, err
			}
			if view.TaskID != "" {
				return true, fmt.Errorf("task-owned session cannot be controlled outside PERSIST; use --direct explicitly")
			}
		}
		return false, nil
	}
	return ctx.Persist.controlSession(session, command, payload)
}

type planNode struct {
	ID               string        `json:"id"`
	Title            string        `json:"title"`
	Status           string        `json:"status"`
	BatchID          string        `json:"batchId"`
	SessionID        string        `json:"session_id"`
	PendingApprovals int           `json:"pending_approvals"`
	BlockedOn        []string      `json:"blocked_on"`
	Attempts         []taskAttempt `json:"attempts"`
}

// Normalize the supervision field at the decoding boundary so every plan
// consumer (including status/show/since) uses the same current session.
func (n *planNode) UnmarshalJSON(data []byte) error {
	type node planNode
	var decoded struct {
		node
		CurrentSessionID string `json:"current_session_id"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*n = planNode(decoded.node)
	if decoded.CurrentSessionID != "" {
		n.SessionID = decoded.CurrentSessionID
	}
	return nil
}

type taskAttempt struct {
	SessionID string `json:"session_id"`
	Status    string `json:"status"`
}

func (n planNode) sessionIDs() []string {
	out := make([]string, 0, len(n.Attempts)+1)
	seen := map[string]bool{}
	if n.SessionID != "" {
		seen[n.SessionID] = true
		out = append(out, n.SessionID)
	}
	for _, a := range n.Attempts {
		if a.SessionID != "" && !seen[a.SessionID] {
			seen[a.SessionID] = true
			out = append(out, a.SessionID)
		}
	}
	return out
}

type planGraph struct {
	PlanID string     `json:"planId"`
	Nodes  []planNode `json:"nodes"`
}

func slug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	dash := false
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.Trim(b.String(), "-")
}
func helpText() string {
	return "agentctl supervises sessions dispatched by PERSIST. Dispatch with `persist plan auto` and `persist plan approve`; see README for plan supervision."
}

func cmdPlan(ctx *Context, args []string) error {
	if len(args) != 2 || args[0] != "show" {
		return fmt.Errorf("%w: plan show PLAN", ErrUsage)
	}
	id := strings.TrimPrefix(args[1], "plan:")
	g, e := ctx.Persist.planGraph(id)
	if e != nil {
		return e
	}
	rows := make([]map[string]any, 0, len(g.Nodes))
	var lines []string
	for _, n := range g.Nodes {
		batch := n.BatchID
		if batch == "" {
			batch = "-"
		}
		var sessionID any = n.SessionID
		if n.SessionID == "" {
			sessionID = nil
		}
		rows = append(rows, map[string]any{"node": n.ID, "title": n.Title, "status": n.Status, "batch": batch, "session_id": sessionID, "pending_approvals": n.PendingApprovals, "blocked_on": n.BlockedOn})
		lines = append(lines, fmt.Sprintf("%s status=%s batch=%s session=%s approvals=%d blocked_on=%v", n.ID, n.Status, batch, emptyDash(n.SessionID), n.PendingApprovals, n.BlockedOn))
	}
	return WriteOutput(ctx.Out, ctx.Config, rows, strings.Join(lines, "\n"))
}
func emptyDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// Controls without a PERSIST route must never silently bypass its ownership.
func requireDirectForTaskLifecycle(ctx *Context, session string) error {
	if ctx.Direct || ctx.Persist == nil {
		return nil
	}
	if ctx.Reference != nil && ctx.Reference.Kind != "session" {
		return fmt.Errorf("%w: task-owned lifecycle control requires --direct", ErrUsage)
	}
	view, err := ctx.API.GetSession(context.Background(), session)
	if err != nil {
		return err
	}
	owned, err := ctx.Persist.ownsSession(session)
	if err != nil {
		return err
	}
	if view.TaskID != "" || owned {
		return fmt.Errorf("%w: task-owned lifecycle control requires --direct", ErrUsage)
	}
	return nil
}
