package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

func main() {
	port := flag.Int("port", 8090, "Daemon port")
	noReplay := flag.Bool("no-replay", false, "Skip replay history")
	create := flag.Bool("create", false, "Create chat if it doesn't exist (default agent: claude)")
	agent := flag.String("agent", "claude", "Agent for --create (claude, codex)")
	idleTimeout := flag.String("idle-timeout", "5m", "Idle timeout for --create")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: agentd-tui [options] <chat-name|session-id>\n\n")
		fmt.Fprintf(os.Stderr, "Attach to a running agentd chat or session with a rich terminal UI.\n\n")
		flag.PrintDefaults()
	}

	// Go's flag package stops at the first non-flag arg.
	// Manually extract the target from os.Args so flags work in any position.
	var target string
	var filtered []string
	filtered = append(filtered, os.Args[0])
	for _, arg := range os.Args[1:] {
		if !strings.HasPrefix(arg, "-") && target == "" {
			target = arg
		} else {
			filtered = append(filtered, arg)
		}
	}
	os.Args = filtered
	flag.Parse()

	// Also check flag.Args() in case target came after flags.
	if target == "" && flag.NArg() > 0 {
		target = flag.Arg(0)
	}
	if target == "" {
		flag.Usage()
		os.Exit(2)
	}

	// Resolve target to a session ID + chat metadata.
	conn, meta, err := connect(target, *port, *noReplay, connectOpts{
		create:      *create,
		agent:       *agent,
		idleTimeout: *idleTimeout,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	m := newModel(conn, meta)
	p := tea.NewProgram(m,
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
	)
	// Store program reference for reconnect pump (double pointer survives model copy).
	*m.program = p

	// Pump WS events into the Bubble Tea program.
	go pumpEvents(conn, p)

	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
