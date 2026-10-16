package main

import (
	"errors"
	"testing"
)

func TestExitCodeMapping(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int
	}{
		{nil, ExitSuccess}, {ErrUsage, ExitUsage}, {ErrAuth, ExitAuth}, {ErrNotFound, ExitNotFound}, {ErrAPI, ExitAPI}, {ErrTimeout, ExitTimeout},
		{WaitReasonError("turn_end"), ExitWaitTurnEnd}, {WaitReasonError("approval"), ExitWaitApproval}, {WaitReasonError("error"), ExitWaitError}, {WaitReasonError("done"), ExitWaitDone},
	} {
		if got := ExitCode(tc.err); got != tc.want {
			t.Errorf("ExitCode(%v)=%d, want %d", tc.err, got, tc.want)
		}
	}
	if ExitCode(errors.New("unexpected")) != ExitAPI {
		t.Fatal("unknown errors must map to API error")
	}
}
