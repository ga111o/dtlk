package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

var version = "dev"

func usage() {
	server, _ := serverURL()
	defaults, err := effectiveConfig(configKeys)
	if err != nil {
		defaults = []string{"(cannot read config: " + err.Error() + ")"}
	}
	fmt.Fprintf(os.Stderr, `dtlk - Transfer files and folders with a 6-digit code

Usage:
  dtlk up [-h hours] [-t tag] <file-or-dir>...
                                      Upload, print a 6-digit code and copy its web URL to the clipboard
                                      (retention: 1-%d hours)
                                      Several paths go up as one code (names must differ) and download unpacked
                                      -t groups uploads under a tag, listed by dtlk files <tag> or at %s/files/<tag>
                                      -t "" uploads without the configured tag
  dtlk up [-h hours] [-t tag] -m text Upload text as dtlk-yymmdd-hhmmss.txt (no paths with -m)
  dtlk down [server/]<6-digit-code>   Download into the current directory
                                      server/ (e.g. the Web: link) is used for this download only
                                      Text (up -m) is copied to the clipboard and printed instead of saved
  dtlk files [[server/files/]<tag>]   List a tag's files: code, expiry, size, name (default: the configured tag)
                                      server/files/ (e.g. the List: link) is used for this listing only
  dtlk config [key [value]]           Show or set defaults (keys: %s; "" unsets)
  dtlk -v | version                   Print version
  dtlk -help | help                   Show this help

Defaults (config: %s, DTLK_SERVER overrides server):
  %s
`, maxHours, server, strings.Join(configKeys, ", "), configPath(), strings.Join(defaults, "\n  "))
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
	case "files":
		err = cmdFiles(ctx, args[1:])
	case "config":
		err = cmdConfig(args[1:])
	case "version", "-v":
		fmt.Println(version)
		return 0
	case "help", "-help":
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
