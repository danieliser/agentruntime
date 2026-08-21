package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"sync"
	"time"

	"github.com/danieliser/agentruntime/pkg/durable"
	"github.com/danieliser/agentruntime/pkg/eventstream"
	"github.com/danieliser/agentruntime/pkg/nativeprotocol"
	"github.com/danieliser/agentruntime/pkg/runtime"
	"github.com/danieliser/agentruntime/pkg/session"
)

const nativeBootstrapTimeout = 60 * time.Second

// AttachNativeSessionIO makes provider-native JSON the sole durable output
// authority for a v1 session. The NDJSON file is a diagnostic mirror and is
// never used as the durable replay cursor.
func AttachNativeSessionIO(
	sess *session.Session,
	logDir string,
	provider nativeprotocol.Provider,
	generation int64,
	providerID string,
	initialPrompt string,
	diagnosticValues []string,
	stopOnTurnCompletion bool,
	reconnect bool,
	policy nativeprotocol.InputPolicy,
	structuredOutput *StructuredOutput,
	broker *eventstream.Broker,
	terminalReason func() string,
	onAttach func(nativeprotocol.Transport),
	onExit func(runtime.ExitResult, error),
	onTurnCompleted func(),
	failureClassifiers ...func(runtime.ExitResult) error,
) error {
	const op = "attach_native_session_io"
	if sess == nil || sess.Handle == nil || generation < 1 || broker == nil {
		return fmt.Errorf("%s: session handle, generation, and event broker are required", op)
	}
	adapter, err := nativeprotocol.NewAdapter(provider)
	if err != nil {
		return err
	}
	handle := sess.Handle
	collector, err := newStructuredResultCollector(string(provider), structuredOutput)
	if err != nil {
		return durable.NewError(durable.CodeStructuredOutputUnsupported, op, "initialize structured-output validator", err)
	}
	nativeWait := make(chan nativeprotocol.Exit, 1)
	terminalReasons := make(chan string, 1)
	go func() {
		result, ok := <-handle.Wait()
		if !ok {
			result.Err = io.EOF
		}
		reason := ""
		if terminalReason != nil {
			reason = terminalReason()
		}
		if reason == "" {
			reason = result.FailureReason
		}
		terminalReasons <- reason
		close(terminalReasons)
		nativeWait <- nativeprotocol.Exit{
			Code: result.Code, Signal: result.Signal, OOMKilled: result.OOMKilled,
			ErrorDetail: result.ErrorDetail, StartedAt: result.StartedAt, EndedAt: result.EndedAt, Err: result.Err,
		}
		close(nativeWait)
	}()
	transport := nativeprotocol.NewStreamTransport(adapter, nativeprotocol.ProcessIO{
		Stdin: handle.Stdin(), Stdout: handle.Stdout(), Stderr: handle.Stderr(),
		Wait: nativeWait, Kill: handle.Kill,
	}, nativeprotocol.RecoveryMetadata{
		SessionID: sess.ID, Generation: generation, RuntimeID: runtimeGenerationIdentity(handle, sess.RuntimeName, sess.ID, generation),
	})
	if err := transport.Start(context.Background()); err != nil {
		return err
	}
	bootstrapCtx, cancelBootstrap := context.WithTimeout(context.Background(), nativeBootstrapTimeout)
	defer cancelBootstrap()
	if err := transport.Bootstrap(bootstrapCtx, nativeprotocol.BootstrapRequest{
		ProviderID: providerID, ClientName: "agentruntime", ClientVersion: "v1", Reconnect: reconnect, Policy: policy,
		OutputSchema: structuredOutputSchema(structuredOutput),
	}); err != nil {
		_ = transport.Close()
		return err
	}
	if initialPrompt != "" {
		if err := transport.Send(bootstrapCtx, nativeprotocol.Input{Kind: nativeprotocol.InputPrompt, Text: initialPrompt}); err != nil {
			_ = transport.Close()
			return err
		}
	}
	if onAttach != nil {
		onAttach(transport)
	}

	logWriter, err := session.NewLogWriter(logDir, sess.ID, session.WithDiagnosticRedactions(diagnosticValues...))
	if err != nil {
		log.Printf("[session %s] warning: native log file creation failed: %v", sess.ID, err)
	}
	drainTarget := session.DrainWriter(sess.Replay, logWriter)
	var ingestMu sync.Mutex
	var ingestErr error
	var structuredErr error
	turnCompleted := false
	var drains sync.WaitGroup
	drainRecords := func(records <-chan nativeprotocol.Record) {
		defer drains.Done()
		for record := range records {
			ingestMu.Lock()
			if ingestErr == nil {
				event, err := broker.Ingest(context.Background(), eventstream.IngestParams{
					SessionID: sess.ID, Generation: generation, Record: record,
				})
				if err != nil {
					ingestErr = err
				} else {
					if collector != nil && structuredErr == nil {
						structuredErr = collector.Observe(event.Type, event.Payload)
					}
					line := append(append([]byte(nil), record.Raw...), '\n')
					if _, err := drainTarget.Write(line); err != nil {
						log.Printf("[session %s] diagnostic output write failed: %v", sess.ID, err)
					}
					if record.Stream == nativeprotocol.StreamProviderStdout {
						parseAndTrackEvent(sess, record.Raw)
						if event.Type == "turn.completed" {
							turnCompleted = true
							if onTurnCompleted != nil {
								onTurnCompleted()
							}
							if stopOnTurnCompletion {
								go func() { _ = transport.Close() }()
							}
						}
					}
				}
			}
			ingestMu.Unlock()
		}
	}
	drains.Add(2)
	go drainRecords(transport.Records())
	go drainRecords(transport.Stderr())

	go func() {
		nativeExit, ok := <-transport.Wait()
		if !ok {
			nativeExit.Err = io.EOF
		}
		// The handle-wait goroutine claims the boundary before transport drain,
		// so already-produced records cannot let a later deadline reclassify it.
		reason := <-terminalReasons
		drains.Wait()
		ingestMu.Lock()
		streamErr := errors.Join(ingestErr, nativeExit.Err)
		if turnCompleted && ingestErr == nil && nativeExit.Err == nil && reason == "" && nativeExit.Signal == "" && !nativeExit.OOMKilled {
			nativeExit.Code = 0
		}
		endedAt := nativeExit.EndedAt
		if endedAt.IsZero() {
			endedAt = time.Now().UTC()
		}
		artifactHash := ""
		failureReason := reason
		if collector != nil && streamErr == nil && nativeExit.Code == 0 && reason == "" {
			result, err := collector.Finalize()
			if err != nil {
				structuredErr = errors.Join(structuredErr, err)
				failureReason = structuredOutputErrorCode(structuredErr)
				nativeExit.Code = 1
				nativeExit.ErrorDetail = structuredOutputErrorMessage(structuredErr)
				reason = failureReason
			} else {
				outputEvent, err := broker.IngestOutput(context.Background(), eventstream.OutputParams{
					SessionID: sess.ID, Generation: generation, Timestamp: endedAt, Raw: result.Bytes,
				})
				if err != nil {
					streamErr = errors.Join(streamErr, err)
				} else {
					artifactHash = outputEvent.RawSHA256
				}
			}
		}
		if len(failureClassifiers) > 0 && failureClassifiers[0] != nil {
			classified := failureClassifiers[0](runtime.ExitResult{
				Code: nativeExit.Code, Signal: nativeExit.Signal, OOMKilled: nativeExit.OOMKilled,
				ErrorDetail: nativeExit.ErrorDetail, StartedAt: nativeExit.StartedAt, EndedAt: nativeExit.EndedAt, Err: nativeExit.Err,
			})
			if classified != nil {
				nativeExit.Code = 1
				nativeExit.ErrorDetail = classified.Error()
				reason = "failed"
				failureReason = "failed"
			}
		}
		if streamErr == nil {
			if reason == "" {
				reason = string(runtimeTerminalState(runtime.ExitResult{
					Code: nativeExit.Code, Signal: nativeExit.Signal, OOMKilled: nativeExit.OOMKilled,
					ErrorDetail: nativeExit.ErrorDetail,
				}))
			}
			_, err := broker.IngestTerminal(context.Background(), eventstream.TerminalParams{
				SessionID: sess.ID, Generation: generation, Timestamp: endedAt,
				Reason: reason, ExitCode: nativeExit.Code, Signal: nativeExit.Signal, Error: nativeExit.ErrorDetail,
			})
			streamErr = errors.Join(streamErr, err)
		}
		ingestMu.Unlock()

		sess.Replay.Close()
		if logWriter != nil {
			if err := logWriter.Close(); err != nil {
				log.Printf("[session %s] warning: close native log failed: %v", sess.ID, err)
			} else {
				log.Printf("[session %s] native log saved: %s", sess.ID, logWriter.Path())
			}
		}
		result := runtime.ExitResult{
			Code: nativeExit.Code, Signal: nativeExit.Signal, OOMKilled: nativeExit.OOMKilled,
			ErrorDetail: nativeExit.ErrorDetail, StartedAt: nativeExit.StartedAt, EndedAt: nativeExit.EndedAt,
			FailureReason: failureReason, ArtifactHash: artifactHash, Err: nativeExit.Err,
		}
		log.Printf("[session %s] native transport exited: code=%d err=%v stream_err=%v replay_bytes=%d", sess.ID, result.Code, result.Err, streamErr, sess.Replay.TotalBytes())
		sess.SetCompleted(result.Code)
		if onExit != nil {
			onExit(result, streamErr)
		}
	}()
	return nil
}

func structuredOutputSchema(contract *StructuredOutput) []byte {
	if contract == nil {
		return nil
	}
	return append([]byte(nil), contract.JSONSchema...)
}

func structuredOutputErrorCode(err error) string {
	var structuredErr *durable.Error
	if errors.As(err, &structuredErr) && (structuredErr.Code == durable.CodeStructuredOutputInvalid || structuredErr.Code == durable.CodeStructuredOutputTooLarge) {
		return string(structuredErr.Code)
	}
	return string(durable.CodeStructuredOutputInvalid)
}

func structuredOutputErrorMessage(err error) string {
	if durable.IsCode(err, durable.CodeStructuredOutputTooLarge) {
		return "final structured output exceeded max_bytes"
	}
	return "final structured output did not satisfy the admitted JSON Schema"
}
