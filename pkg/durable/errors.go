package durable

import (
	"errors"
	"fmt"
)

// ErrorCode is DUR-101's stable machine-readable failure classification.
type ErrorCode string

const (
	CodeInvalidArgument     ErrorCode = "invalid_argument"
	CodeNotFound            ErrorCode = "not_found"
	CodeIdempotencyConflict ErrorCode = "idempotency_conflict"
	CodeImmutableConflict   ErrorCode = "immutable_conflict"
	CodeInvalidState        ErrorCode = "invalid_state"
	CodeInvalidCursor       ErrorCode = "invalid_cursor"
	CodeEventGap            ErrorCode = "event_gap"
	CodeBackpressure        ErrorCode = "backpressure"
	CodeIndeterminate       ErrorCode = "indeterminate"
	CodeStoreClosed         ErrorCode = "store_closed"
)

// Error is the common structured error returned by durable stores.
type Error struct {
	Code    ErrorCode
	Op      string
	Message string
	Err     error
}

func (err *Error) Error() string {
	if err == nil {
		return "<nil>"
	}
	message := err.Message
	if message == "" && err.Err != nil {
		message = err.Err.Error()
	}
	if err.Op == "" {
		return fmt.Sprintf("%s: %s", err.Code, message)
	}
	return fmt.Sprintf("%s: %s: %s", err.Op, err.Code, message)
}

func (err *Error) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.Err
}

// NewError creates a structured store error without exposing implementation
// details through string matching.
func NewError(code ErrorCode, op, message string, cause error) error {
	return &Error{Code: code, Op: op, Message: message, Err: cause}
}

// IsCode reports whether any wrapped durable Error has the requested code.
func IsCode(err error, code ErrorCode) bool {
	var durableErr *Error
	return errors.As(err, &durableErr) && durableErr.Code == code
}
