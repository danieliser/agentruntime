package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"sync"
	"time"

	"github.com/danieliser/agentruntime/pkg/eventstream"
	"github.com/danieliser/agentruntime/pkg/nativeprotocol"
	"github.com/danieliser/agentruntime/pkg/runtime"
	"github.com/danieliser/agentruntime/pkg/session"
)

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
	stopOnTurnCompletion bool,
	reconnect bool,
	broker *eventstream.Broker,
	terminalReason func() string,
	onAttach func(nativeprotocol.Transport),
	onExit func(runtime.ExitResult, error),
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
	bootstrapCtx, cancelBootstrap := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelBootstrap()
	if err := transport.Bootstrap(bootstrapCtx, nativeprotocol.BootstrapRequest{
		ProviderID: providerID, ClientName: "agentruntime", ClientVersion: "v1", Reconnect: reconnect,
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

	logWriter, err := session.NewLogWriter(logDir, sess.ID)
	if err != nil {
		log.Printf("[session %s] warning: native log file creation failed: %v", sess.ID, err)
	}
	drainTarget := session.DrainWriter(sess.Replay, logWriter)
	var ingestMu sync.Mutex
	var ingestErr error
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
					line := append(append([]byte(nil), record.Raw...), '\n')
					if _, err := drainTarget.Write(line); err != nil {
						log.Printf("[session %s] diagnostic output write failed: %v", sess.ID, err)
					}
					if record.Stream == nativeprotocol.StreamProviderStdout {
						parseAndTrackEvent(sess, record.Raw)
						if stopOnTurnCompletion && event.Type == "turn.completed" {
							turnCompleted = true
							go func() { _ = transport.Close() }()
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
		if turnCompleted && ingestErr == nil && nativeExit.Err == nil {
			nativeExit.Code = 0
		}
		endedAt := nativeExit.EndedAt
		if endedAt.IsZero() {
			endedAt = time.Now().UTC()
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
			Err: nativeExit.Err,
		}
		log.Printf("[session %s] native transport exited: code=%d err=%v stream_err=%v replay_bytes=%d", sess.ID, result.Code, result.Err, streamErr, sess.Replay.TotalBytes())
		sess.SetCompleted(result.Code)
		if onExit != nil {
			onExit(result, streamErr)
		}
	}()
	return nil
}
