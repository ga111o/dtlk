package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

var version = "dev"

func usage() {
	server, _ := serverURL()
	fmt.Fprintf(os.Stderr, `dtlk - Transfer files and folders with a 6-digit code

Usage:
  dtlk up [--h hours] <file-or-dir>   Upload and print a 6-digit code (retention: default 24h, 1-168)
  dtlk down <6-digit-code>            Download into the current directory
  dtlk version                        Print version

Server: %s
  (config: %s, override with DTLK_SERVER)
`, server, configPath())
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch args[0] {
	case "up":
		err = cmdUp(ctx, args[1:])
	case "down":
		err = cmdDown(ctx, args[1:])
	case "version", "--version":
		fmt.Println(version)
		return 0
	case "help", "-h", "--help":
		usage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n\n", args[0])
		usage()
		return 2
	}

	if err != nil {
		if errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, "\nCanceled.")
			return 130
		}
		var ue *usageError
		if errors.As(err, &ue) {
			fmt.Fprintf(os.Stderr, "Error: %s\n", ue.msg)
			return 2
		}
		fmt.Fprintf(os.Stderr, "Error: %s\n", err)
		return 1
	}
	return 0
}

// usageError is a bad-arguments error (exit code 2).
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

func usageErrorf(format string, a ...any) error {
	return &usageError{msg: fmt.Sprintf(format, a...)}
}
