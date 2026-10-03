// Package cli holds what the three programs' command lines share, so that
// they look and behave alike: a subcommand first ("run" when there is none),
// then its flags.
package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

// Command is one subcommand: Run gets the arguments after its name.
type Command struct {
	Name string
	Run  func(args []string) error
}

// Main runs the subcommand named by the first argument, or the first of
// cmds when the arguments start with a flag or there are none, and exits.
// "version" and "help" are always available.
func Main(version, usage string, cmds ...Command) {
	name, args := cmds[0].Name, os.Args[1:]
	if len(args) > 0 && args[0] != "" && args[0][0] != '-' {
		name, args = args[0], args[1:]
	}
	switch name {
	case "version":
		fmt.Println(version)
		return
	case "help", "-h", "--help":
		fmt.Fprint(os.Stderr, usage)
		return
	}
	for _, c := range cmds {
		if c.Name == name {
			if err := c.Run(args); err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				os.Exit(1)
			}
			return
		}
	}
	fmt.Fprint(os.Stderr, usage)
	os.Exit(2)
}

// Logger writes to stderr, where systemd picks it up for the journal.
func Logger(debug bool) *slog.Logger {
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}

// Context is done when the process is asked to stop.
func Context() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
}
