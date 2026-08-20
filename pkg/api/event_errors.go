package api

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/danieliser/agentruntime/pkg/durable"
)

type apiErrorEnvelope struct {
	Code    durable.ErrorCode `json:"code"`
	Message string            `json:"message"`
}

func writeDurableError(c *gin.Context, err error) {
	var storeErr *durable.Error
	if !errors.As(err, &storeErr) {
		c.JSON(http.StatusInternalServerError, gin.H{"error": apiErrorEnvelope{Code: durable.CodeIndeterminate, Message: "durable operation failed"}})
		return
	}
	status := http.StatusConflict
	switch storeErr.Code {
	case durable.CodeInvalidArgument, durable.CodeInvalidCursor, durable.CodeExecutionPolicyUnsupported, durable.CodeResourceLimitExceeded, durable.CodeStructuredOutputUnsupported:
		status = http.StatusBadRequest
	case durable.CodeNotFound:
		status = http.StatusNotFound
	case durable.CodeBackpressure:
		status = http.StatusTooManyRequests
	case durable.CodeIndeterminate, durable.CodeStoreClosed, durable.CodeRuntimeUnavailable,
		durable.CodeEgressPreflightFailed, durable.CodeEgressDenied, durable.CodeProviderStartupFailed:
		status = http.StatusServiceUnavailable
	}
	c.JSON(status, gin.H{"error": apiErrorEnvelope{Code: storeErr.Code, Message: storeErr.Message}})
}
