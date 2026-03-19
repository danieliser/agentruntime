package main

import (
	"flag"
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"
)

func main() {
	port := flag.Int("port", 8090, "Daemon port")
	noReplay := flag.Bool("no-replay", false, "Skip replay history")
	create := flag.Bool("create", false, "Create chat if it doesn't exist (default agent: claude)")
	agent := flag.String("agent", "claude", "Agent for --create (claude, codex)")
	idleTimeout := flag.String("idle-timeout", "5m", "Idle timeout for --create")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: agentd-tui <chat-name|session-id> [options]\n\n")
		fmt.Fprintf(os.Stderr, "Attach to a running agentd chat or session with a rich terminal UI.\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	if flag.NArg() < 1 {
		flag.Usage()
		os.Exit(2)
	}
	target := flag.Arg(0)

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

	// Pump WS events into the Bubble Tea program.
	go pumpEvents(conn, p)

	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
