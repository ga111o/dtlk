package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"unicode"
	"unicode/utf8"
)

// parseFilesArg accepts "<tag>" or "<server>/files/<tag>", such as the List:
// link dtlk up prints. server is "" for a bare tag (use the configured server);
// otherwise it is normalized like a configured server, keeping any path prefix.
func parseFilesArg(arg string) (server, tag string, err error) {
	tag = strings.TrimSpace(arg)
	if strings.Contains(tag, "/") {
		u, err := parseLink(tag)
		if err != nil {
			return "", "", err
		}
		if tag = cutLast(u); cutLast(u) != "files" {
			return "", "", usageErrorf("expected <server>/files/<tag>: %s", arg)
		}
		server = u.String()
	}
	if tag == "" {
		return "", "", usageErrorf("missing tag: %q", arg)
	}
	if tag, err = parseTag(tag); err != nil {
		return "", "", err
	}
	return server, tag, nil
}

func cmdFiles(ctx context.Context, args []string) error {
	var server, tag string
	var err error
	switch len(args) {
	case 0:
		cfg, err := readConfig()
		if err != nil {
			return fmt.Errorf("failed to read config file: %w", err)
		}
		if tag, err = parseTag(cfg["tag"]); err != nil {
			return usageErrorf("%v (in %s)", err, configPath())
		}
		if tag == "" {
			return usageErrorf("usage: dtlk files [[server/files/]<tag>] (no tag given and none set with dtlk config tag)")
		}
	case 1:
		if server, tag, err = parseFilesArg(args[0]); err != nil {
			return err
		}
	default:
		return usageErrorf("usage: dtlk files [[server/files/]<tag>]")
	}

	var c *apiClient
	if server != "" {
		c = newAPIClientAt(server)
	} else if c, err = newAPIClient(); err != nil {
		return err
	}
	files, err := c.tagFiles(ctx, tag)
	if err != nil {
		return err
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "CODE\tEXPIRES\tSIZE\tNAME")
	for _, f := range files {
		name := f.Name
		if f.Kind != "file" {
			name += "/"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", f.Code, f.ExpiresAt.Local().Format("2006-01-02 15:04"), humanBytes(f.Size), printable(name))
	}
	return w.Flush()
}

// printable quotes s when it holds non-printing characters or invalid UTF-8:
// anyone who knows a tag can add to it, so a name must not drive the terminal.
func printable(s string) string {
	if !utf8.ValidString(s) || strings.IndexFunc(s, func(r rune) bool { return !unicode.IsPrint(r) }) >= 0 {
		return strconv.Quote(s)
	}
	return s
}
