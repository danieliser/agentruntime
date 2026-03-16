package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"syscall"

	"github.com/danieliser/agentruntime/pkg/api"
	"github.com/danieliser/agentruntime/pkg/client"
	"gopkg.in/yaml.v3"
)

const defaultDispatchServerURL = "http://localhost:8090"

type exitCoder interface {
	ExitCode() int
}

type dispatchExitError struct {
	code int
}

func (e dispatchExitError) Error() string {
	return fmt.Sprintf("exit with code %d", e.code)
}

func (e dispatchExitError) ExitCode() int {
	return e.code
}

func runDispatchCommand(args []string) int {
	fs := flag.NewFlagSet("dispatch", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	configPath := fs.String("config", "", "Path to session YAML config")
	serverURL := fs.String("server", defaultDispatchServerURL, "agentd server base URL")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: %s dispatch --config <path> [--server <url>]\n", filepath.Base(os.Args[0]))
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	if *configPath == "" {
		fs.Usage()
		fmt.Fprintln(os.Stderr, "dispatch: --config is required")
		return 2
	}

	if err := runDispatch(*configPath, *serverURL); err != nil {
		var coded exitCoder
		if errors.As(err, &coded) {
			return coded.ExitCode()
		}
		fmt.Fprintf(os.Stderr, "dispatch: %v\n", err)
		return 1
	}

	return 0
}

func runDispatch(configPath, serverURL string) error {
	req, err := loadDispatchRequest(configPath)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cl := client.New(serverURL)
	resp, err := cl.Dispatch(ctx, req)
	if err != nil {
		return fmt.Errorf("dispatch session: %w", err)
	}

	fmt.Fprintf(os.Stderr, "session_id: %s\n", resp.SessionID)
	fmt.Fprintf(os.Stderr, "ws_url: %s\n", resp.WSURL)
	fmt.Fprintf(os.Stderr, "log_url: %s\n", resp.LogURL)

	logs, err := cl.StreamLogs(ctx, resp.SessionID)
	if err != nil {
		return fmt.Errorf("stream logs: %w", err)
	}
	defer logs.Close()

	if _, err := io.Copy(os.Stdout, logs); err != nil {
		return fmt.Errorf("copy logs: %w", err)
	}

	sess, err := cl.GetSession(ctx, resp.SessionID)
	if err != nil {
		return fmt.Errorf("get final session status: %w", err)
	}

	switch sess.Status {
	case "completed":
		return nil
	case "failed":
		return dispatchExitError{code: 1}
	default:
		return fmt.Errorf("session %s ended with unexpected status %q", resp.SessionID, sess.Status)
	}
}

func loadDispatchRequest(configPath string) (api.SessionRequest, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return api.SessionRequest{}, fmt.Errorf("read config %q: %w", configPath, err)
	}

	var req api.SessionRequest
	if err := yaml.Unmarshal(data, &req); err != nil {
		return api.SessionRequest{}, fmt.Errorf("unmarshal config %q: %w", configPath, err)
	}

	expandEnvStrings(&req)
	return req, nil
}

func expandEnvStrings(target any) {
	value := reflect.ValueOf(target)
	if !value.IsValid() {
		return
	}

	expanded := expandEnvValue(value)
	if value.Kind() == reflect.Pointer && !value.IsNil() && value.Elem().CanSet() {
		value.Elem().Set(expanded.Elem())
	}
}

func expandEnvValue(value reflect.Value) reflect.Value {
	if !value.IsValid() {
		return value
	}

	switch value.Kind() {
	case reflect.Pointer:
		if value.IsNil() {
			return value
		}
		expanded := reflect.New(value.Type().Elem())
		expanded.Elem().Set(expandEnvValue(value.Elem()))
		return expanded
	case reflect.Interface:
		if value.IsNil() {
			return value
		}
		return expandEnvValue(value.Elem())
	case reflect.Struct:
		expanded := reflect.New(value.Type()).Elem()
		expanded.Set(value)
		for i := 0; i < value.NumField(); i++ {
			field := expanded.Field(i)
			if !field.CanSet() {
				continue
			}
			field.Set(expandEnvValue(value.Field(i)))
		}
		return expanded
	case reflect.String:
		return reflect.ValueOf(os.ExpandEnv(value.String())).Convert(value.Type())
	case reflect.Slice:
		if value.IsNil() {
			return value
		}
		expanded := reflect.MakeSlice(value.Type(), value.Len(), value.Len())
		for i := 0; i < value.Len(); i++ {
			expanded.Index(i).Set(expandEnvValue(value.Index(i)))
		}
		return expanded
	case reflect.Array:
		expanded := reflect.New(value.Type()).Elem()
		for i := 0; i < value.Len(); i++ {
			expanded.Index(i).Set(expandEnvValue(value.Index(i)))
		}
		return expanded
	case reflect.Map:
		if value.IsNil() {
			return value
		}
		expanded := reflect.MakeMapWithSize(value.Type(), value.Len())
		iter := value.MapRange()
		for iter.Next() {
			expanded.SetMapIndex(iter.Key(), expandEnvValue(iter.Value()))
		}
		return expanded
	default:
		return value
	}
}
