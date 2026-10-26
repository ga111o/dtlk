package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/klauspost/compress/zstd"
)

var codeRe = regexp.MustCompile(`^[0-9]{6}$`)

// parseLink parses a link that dtlk up prints (Web:, List:). Like a configured
// server, http(s):// is optional; a query or fragment is rejected.
func parseLink(arg string) (*url.URL, error) {
	raw := arg
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return nil, usageErrorf("invalid server address: %s", arg)
	}
	u.Path, u.RawPath = strings.TrimRight(u.Path, "/"), ""
	return u, nil
}

// cutLast removes the last segment of u's path and returns it.
func cutLast(u *url.URL) string {
	dir, last := path.Split(u.Path)
	u.Path = strings.TrimRight(dir, "/")
	return last
}

// parseDownArg accepts "<code>" or "<server>/<code>", such as the Web: link
// dtlk up prints. server is "" for a bare code (use the configured server);
// otherwise it is normalized like a configured server, keeping any path prefix.
func parseDownArg(arg string) (server, code string, err error) {
	arg = strings.TrimSpace(arg)
	if !strings.Contains(arg, "/") {
		if !codeRe.MatchString(arg) {
			return "", "", usageErrorf("code must be a 6-digit number: %s", arg)
		}
		return "", arg, nil
	}
	u, err := parseLink(arg)
	if err != nil {
		return "", "", err
	}
	if code = cutLast(u); !codeRe.MatchString(code) {
		return "", "", usageErrorf("expected <server>/<6-digit-code>: %s", arg)
	}
	return u.String(), code, nil
}

func cmdDown(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return usageErrorf("usage: dtlk down [server/]<6-digit-code>")
	}
	server, code, err := parseDownArg(args[0])
	if err != nil {
		return err
	}

	var c *apiClient
	if server != "" {
		c = newAPIClientAt(server)
	} else if c, err = newAPIClient(); err != nil {
		return err
	}
	info, err := c.fileInfo(ctx, code)
	if err != nil {
		return err
	}
	if validName(info.Name) != nil {
		return fmt.Errorf("server returned an invalid file name: %q", info.Name)
	}
	if info.Text && info.Kind != "file" {
		return fmt.Errorf("server returned a text upload of kind %q", info.Kind)
	}

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	var plan []save
	if !info.Text {
		var p *prompter
		if isTerminal(os.Stdin) {
			p = &prompter{in: bufio.NewReader(os.Stdin), out: os.Stderr}
		}
		if plan, err = planSaves(cwd, info, p); err != nil {
			return err
		}
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
	if info.Kind == "bundle" {
		if err := checkItems(filepath.Join(tmp, top), info.Items); err != nil {
			return err
		}
	}
	if info.Text {
		return showText(filepath.Join(tmp, top))
	}

	for _, s := range plan {
		if taken, err := exists(filepath.Join(cwd, s.to)); !s.overwrite && (err != nil || taken) {
			return fmt.Errorf("'%s' was created while downloading; please run again", s.to)
		}
	}
	for _, s := range plan {
		dst := filepath.Join(cwd, s.to)
		if s.overwrite {
			if err := os.RemoveAll(dst); err != nil {
				return fmt.Errorf("failed to remove existing '%s': %w", s.to, err)
			}
		}
		if err := os.Rename(filepath.Join(tmp, filepath.FromSlash(s.from)), dst); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "Saved: ./%s\n", s.to)
	}
	return nil
}

// showText copies a text upload (dtlk up -m), unpacked at path, to the
// clipboard and prints it as it is instead of saving it.
func showText(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return errors.New("downloaded text upload is not a regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	copyToClipboard(string(data))
	if _, err := os.Stdout.Write(data); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Copied to clipboard (%d chars)\n", utf8.RuneCount(data))
	return nil
}

// save is one top-level entry of a download: its path in the unpacked
// archive and the name it is saved as in the current directory.
type save struct {
	from, to  string
	overwrite bool
}

// planSaves picks where the download goes in dir before anything is fetched:
// the upload as a whole, or each item of a bundle (unpacked in place).
func planSaves(dir string, info *fileInfo, p *prompter) ([]save, error) {
	if info.Kind != "bundle" {
		to, overwrite, err := resolveTarget(dir, info.Name, info.Kind == "dir", nil, p)
		if err != nil {
			return nil, err
		}
		return []save{{info.Name, to, overwrite}}, nil
	}
	if len(info.Items) == 0 {
		return nil, errors.New("server returned a bundle without items")
	}
	used := map[string]bool{}
	for _, it := range info.Items {
		if validName(it.Name) != nil || used[it.Name] {
			return nil, fmt.Errorf("server returned an invalid item name: %q", it.Name)
		}
		used[it.Name] = true
	}
	plan := make([]save, 0, len(info.Items))
	for _, it := range info.Items {
		to, overwrite, err := resolveTarget(dir, it.Name, it.Kind == "dir", used, p)
		if err != nil {
			return nil, err
		}
		used[to] = true
		plan = append(plan, save{info.Name + "/" + it.Name, to, overwrite})
	}
	return plan, nil
}

// checkItems makes sure the unpacked bundle at dir holds exactly items.
func checkItems(dir string, items []bundleItem) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	want := map[string]bool{}
	for _, it := range items {
		want[it.Name] = true
	}
	if len(entries) != len(want) {
		return errors.New("downloaded archive does not match its item list")
	}
	for _, e := range entries {
		if !want[e.Name()] {
			return fmt.Errorf("downloaded archive does not match its item list: unexpected %q", e.Name())
		}
	}
	return nil
}
