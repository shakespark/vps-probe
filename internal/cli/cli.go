// Package cli holds what the three programs' command lines share, so that
// they look and behave alike: a subcommand first, then its flags.
package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
)

// Command is one subcommand: Run gets the arguments after its name.
type Command struct {
	Name string
	Run  func(args []string) error
}

// Main runs the subcommand named by the first argument and exits. "version"
// and "help" are always available. Without a subcommand it prints the usage
// and fails: nothing is started by a command line that did not ask for it,
// such as a bare program name typed to see what it does.
func Main(version, usage string, cmds ...Command) {
	name, args := "", os.Args[1:]
	if len(args) > 0 {
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
	if name == "" || name[0] == '-' {
		fmt.Fprintf(os.Stderr, "error: no command given; to start the service: %s %s\n\n", filepath.Base(os.Args[0]), cmds[0].Name)
	} else {
		fmt.Fprintf(os.Stderr, "error: unknown command %q\n\n", name)
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
