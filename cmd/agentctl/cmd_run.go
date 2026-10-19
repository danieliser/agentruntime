package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/danieliser/agentruntime/pkg/api"
)

func cmdRun(ctx *Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	provider, model, effort, cwd, runtime := "", "", "", "", ""
	fs.StringVar(&provider, "provider", "", "")
	fs.StringVar(&model, "model", "", "")
	fs.StringVar(&effort, "effort", "", "")
	fs.StringVar(&cwd, "cwd", "", "")
	fs.StringVar(&runtime, "runtime", "local", "")
	keepWarm := fs.Duration("keep-warm", 0, "maintain Docker transport for this idle duration (max 24h)")
	contextMode := fs.String("context", "", "clean (default inherits provider context)")
	portable := fs.Bool("portable-resume", false, "snapshot provider state before Docker cleanup")
	permissions := fs.String("permissions", "", "restricted or unrestricted provider permissions")
	approval := fs.String("approval-policy", "", "on-request or never (requires --permissions)")
	if fs.Parse(args) != nil || len(fs.Args()) != 1 || model == "" || cwd == "" || (provider != "claude" && provider != "codex") || (runtime != "local" && runtime != "docker") {
		return fmt.Errorf("%w: run --provider claude|codex --model M [--effort E] --cwd DIR [--runtime local|docker] \"task\"", ErrUsage)
	}
	if *contextMode != "" && *contextMode != "clean" {
		return fmt.Errorf("%w: --context supports clean", ErrUsage)
	}
	req := api.SessionRequest{Context: *contextMode, Agent: provider, Model: model, Effort: effort, WorkDir: cwd, Runtime: runtime, Prompt: fs.Arg(0)}
	if *keepWarm < 0 || *keepWarm > 24*time.Hour || ((*keepWarm > 0 || *portable) && runtime != "docker") {
		return fmt.Errorf("%w: --keep-warm requires Docker and a positive duration up to 24h; --portable-resume requires Docker", ErrUsage)
	}
	keepWarmSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "keep-warm" {
			keepWarmSet = true
		}
	})
	if keepWarmSet && *keepWarm == 0 {
		return fmt.Errorf("%w: --keep-warm must be positive", ErrUsage)
	}
	if *keepWarm > 0 || *portable {
		mode := "delete"
		ttl := ""
		if *keepWarm > 0 {
			mode, ttl = "maintain", keepWarm.String()
		}
		req.ContainerLease = &api.ContainerLease{Mode: mode, IdleTTL: ttl, PortableResume: *portable}
	}
	if *permissions != "" {
		policy := *approval
		if policy == "" {
			policy = "never"
			if *permissions == "restricted" {
				policy = "on-request"
			}
		}
		if (*permissions != "restricted" && *permissions != "unrestricted") || (policy != "never" && policy != "on-request") || (*permissions == "restricted" && runtime != "docker") || (*permissions == "unrestricted" && policy != "never") {
			return fmt.Errorf("%w: restricted permissions require Docker and on-request|never; unrestricted requires never", ErrUsage)
		}
		req.ProviderPermissions = &api.ProviderPermissions{Mode: *permissions, ApprovalPolicy: policy}
	} else if *approval != "" {
		return fmt.Errorf("%w: --approval-policy requires --permissions", ErrUsage)
	}
	s, err := ctx.API.client.DispatchDurable(context.Background(), req)
	if err != nil {
		return ctx.API.safeError(err)
	}
	markCreated(ctx, s.SessionID)
	return WriteOutput(ctx.Out, ctx.Config, map[string]string{"session_id": s.SessionID}, s.SessionID)
}
