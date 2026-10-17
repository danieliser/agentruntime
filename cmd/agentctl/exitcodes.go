package main

import "errors"

// Exit codes: 0 success; 2 usage; 3 auth; 4 not found; 5 API failure;
// 6 timeout; 10 wait turn_end; 11 wait approval; 12 wait error; 13 wait done.
const (
	ExitSuccess         = 0
	ExitUsage           = 2
	ExitAuth            = 3
	ExitNotFound        = 4
	ExitAPI             = 5
	ExitTimeout         = 6
	ExitWaitTurnEnd     = 10
	ExitWaitApproval    = 11
	ExitWaitError       = 12
	ExitWaitDone        = 13
	ExitWaitNodeStarted = 14
	ExitWaitNodeDone    = 15
	ExitWaitFailed      = 16
	ExitWaitStalled     = 17
)

var (
	ErrUsage    = errors.New("usage error")
	ErrAuth     = errors.New("authentication error")
	ErrNotFound = errors.New("not found")
	ErrAPI      = errors.New("API error")
	ErrTimeout  = errors.New("timeout")
)

type WaitReasonError string

func (e WaitReasonError) Error() string { return "wait completed: " + string(e) }

func ExitCode(err error) int {
	if err == nil {
		return ExitSuccess
	}
	switch {
	case errors.Is(err, ErrUsage):
		return ExitUsage
	case errors.Is(err, ErrAuth):
		return ExitAuth
	case errors.Is(err, ErrNotFound):
		return ExitNotFound
	case errors.Is(err, ErrTimeout):
		return ExitTimeout
	case errors.Is(err, ErrAPI):
		return ExitAPI
	}
	var reason WaitReasonError
	if errors.As(err, &reason) {
		switch reason {
		case "turn_end":
			return ExitWaitTurnEnd
		case "approval":
			return ExitWaitApproval
		case "error":
			return ExitWaitError
		case "done":
			return ExitWaitDone
		case "node_started":
			return ExitWaitNodeStarted
		case "node_done":
			return ExitWaitNodeDone
		case "failed":
			return ExitWaitFailed
		case "stalled":
			return ExitWaitStalled
		}
	}
	return ExitAPI
}
