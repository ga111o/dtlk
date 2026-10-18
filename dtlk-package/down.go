package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/klauspost/compress/zstd"
)

var codeRe = regexp.MustCompile(`^[0-9]{6}$`)

func cmdDown(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return usageErrorf("usage: dtlk down <6-digit-code>")
	}
	code := strings.TrimSpace(args[0])
	if !codeRe.MatchString(code) {
		return usageErrorf("code must be a 6-digit number: %s", code)
	}

	c, err := newAPIClient()
	if err != nil {
		return err
	}
	info, err := c.fileInfo(ctx, code)
	if err != nil {
		return err
	}
	if validName(info.Name) != nil {
		return fmt.Errorf("server returned an invalid file name: %q", info.Name)
	}

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	var p *prompter
	if isTerminal(os.Stdin) {
		p = &prompter{in: bufio.NewReader(os.Stdin), out: os.Stderr}
	}
	target, overwrite, err := resolveTarget(cwd, info.Name, info.Kind == "dir", p)
	if err != nil {
		return err
	}

	tmp, err := os.MkdirTemp(cwd, ".dtlk-tmp-")
	if err != nil {
		return fmt.Errorf("cannot write to current directory: %w", err)
	}
	defer os.RemoveAll(tmp)

	fmt.Fprintf(os.Stderr, "Downloading: %s (%s, expires %s)\n", info.Name, humanBytes(info.Size), info.ExpiresAt.Local().Format("2006-01-02 15:04"))
	prog := startProgress("Downloading", info.Size)
	body := c.openDownload(ctx, code, info.Size, prog.add, prog.note)
	defer body.Close()
	zr, err := zstd.NewReader(body)
	if err != nil {
		return err
	}
	defer zr.Close()
	top, err := extractTar(zr, tmp)
	prog.finish()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	if top != info.Name {
		return fmt.Errorf("downloaded archive root (%q) does not match expected name (%q)", top, info.Name)
	}

	dst := filepath.Join(cwd, target)
	if overwrite {
		if err := os.RemoveAll(dst); err != nil {
			return fmt.Errorf("failed to remove existing '%s': %w", target, err)
		}
	} else if taken, err := exists(dst); err != nil || taken {
		return fmt.Errorf("'%s' was created while downloading; please run again", target)
	}
	if err := os.Rename(filepath.Join(tmp, top), dst); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Saved: ./%s\n", target)
	return nil
}
