package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
)

const (
	defaultHours = 24
	maxHours     = 168
	partWorkers  = 3
)

func parseHours(s string) (int, error) {
	h, err := strconv.Atoi(s)
	if err != nil || h < 1 || h > maxHours {
		return 0, usageErrorf("hours must be an integer between 1 and %d: %s", maxHours, s)
	}
	return h, nil
}

var tagRe = regexp.MustCompile(`^[a-z0-9_-]{1,64}$`)

// parseTag lowercases a list tag (-t); the server stores it the same way. "" means no tag.
func parseTag(s string) (string, error) {
	t := strings.ToLower(s)
	if t != "" && !tagRe.MatchString(t) {
		return "", usageErrorf("tag must be 1-64 letters, digits, '-' or '_': %q", s)
	}
	return t, nil
}

type upOpts struct {
	hours   int
	tag     string
	paths   []string
	text    string // -m: uploaded as a text file instead of paths
	hasText bool   // -m was given (text may be "")
}

// parseUpArgs accepts "-h N" / "-h=N", "-t TAG" / "-t=TAG" and "-m TEXT" / "-m=TEXT"
// anywhere around one or more paths, or around nothing when -m is given.
// An option that is not given falls back to cfg (the config file), then to the built-in default.
// An empty tag (-t "" or -t=) uploads without one, even when cfg has a tag.
func parseUpArgs(args []string, cfg map[string]string) (o upOpts, err error) {
	var hoursSet, tagSet bool
	for i := 0; i < len(args); i++ {
		a := args[i]
		flag, val, hasVal := strings.Cut(a, "=")
		switch {
		case a == "--":
			o.paths = append(o.paths, args[i+1:]...)
			i = len(args)
		case flag == "-h" || flag == "-t" || flag == "-m":
			if !hasVal {
				if i+1 >= len(args) {
					switch flag {
					case "-h":
						return upOpts{}, usageErrorf("missing hours after -h (1-%d)", maxHours)
					case "-t":
						return upOpts{}, usageErrorf("missing tag after -t")
					}
					return upOpts{}, usageErrorf("missing text after -m")
				}
				i++
				val = args[i]
			}
			switch flag {
			case "-h":
				o.hours, err = parseHours(val)
				hoursSet = true
			case "-t":
				o.tag, err = parseTag(val)
				tagSet = true
			default:
				if o.hasText {
					return upOpts{}, usageErrorf("-m can be given only once")
				}
				o.text, o.hasText = val, true
			}
			if err != nil {
				return upOpts{}, err
			}
		case strings.HasPrefix(a, "-") && a != "-":
			return upOpts{}, usageErrorf("unknown option: %s", a)
		default:
			o.paths = append(o.paths, a)
		}
	}
	if o.hasText && len(o.paths) > 0 {
		return upOpts{}, usageErrorf("-m cannot be combined with paths")
	}
	if len(o.paths) == 0 && !o.hasText {
		return upOpts{}, usageErrorf("usage: dtlk up [-h hours] [-t tag] (<file-or-dir>... | -m text)")
	}

	if !hoursSet {
		o.hours = defaultHours
		if v := cfg["hours"]; v != "" {
			if o.hours, err = parseHours(v); err != nil {
				return upOpts{}, usageErrorf("%v (in %s)", err, configPath())
			}
		}
	}
	if !tagSet {
		if o.tag, err = parseTag(cfg["tag"]); err != nil {
			return upOpts{}, usageErrorf("%v (in %s)", err, configPath())
		}
	}
	return o, nil
}

// newBundle checks the paths given to dtlk up. One path is archived as itself;
// several go into a dtlk-yymmdd-hhmmss bundle (local time now), like the web
// upload page and the app do. Their names must differ.
func newBundle(paths []string, now time.Time) (bundle, error) {
	b := bundle{mtime: now}
	seen := map[string]string{}
	for _, path := range paths {
		abs, err := filepath.Abs(path)
		if err != nil {
			return bundle{}, err
		}
		info, err := os.Stat(abs)
		if err != nil {
			if os.IsNotExist(err) {
				return bundle{}, fmt.Errorf("path not found: %s", path)
			}
			return bundle{}, err
		}
		name := filepath.Base(abs)
		if abs == "/" || validName(name) != nil {
			return bundle{}, fmt.Errorf("cannot upload this path: %s", path)
		}
		kind := "file"
		switch {
		case info.IsDir():
			kind = "dir"
		case !info.Mode().IsRegular():
			return bundle{}, fmt.Errorf("only regular files or directories can be uploaded: %s", path)
		}
		if prev, ok := seen[name]; ok {
			return bundle{}, fmt.Errorf("duplicate name: %s (%s, %s)", name, prev, path)
		}
		seen[name] = path
		b.srcs = append(b.srcs, source{path: abs, name: name, kind: kind})
	}
	b.name, b.kind = b.srcs[0].name, b.srcs[0].kind
	if len(b.srcs) > 1 {
		b.name, b.kind = now.Format("dtlk-060102-150405"), "bundle"
	}
	return b, nil
}

// newTextBundle is the dtlk-yymmdd-hhmmss.txt file (local time now) that dtlk up -m
// uploads, holding text as it is, like the app does for shared text.
func newTextBundle(text string, now time.Time) bundle {
	name := now.Format("dtlk-060102-150405") + ".txt"
	return bundle{name: name, kind: "file", srcs: []source{{name: name, kind: "file", data: []byte(text)}}, mtime: now}
}

func cmdUp(ctx context.Context, args []string) error {
	cfg, err := readConfig()
	if err != nil {
		return fmt.Errorf("failed to read config file: %w", err)
	}
	opts, err := parseUpArgs(args, cfg)
	if err != nil {
		return err
	}
	hours := opts.hours
	var b bundle
	if opts.hasText {
		b = newTextBundle(opts.text, time.Now())
	} else if b, err = newBundle(opts.paths, time.Now()); err != nil {
		return err
	}

	scan, err := scanBundle(b)
	if err != nil {
		return fmt.Errorf("failed to scan files: %w", err)
	}
	if b.kind != "file" {
		fmt.Fprintf(os.Stderr, "%s/: %d files, %d dirs, %d symlinks, %s\n", b.name, scan.Files, scan.Dirs, scan.Links, humanBytes(scan.Bytes))
	} else {
		fmt.Fprintf(os.Stderr, "%s: %s\n", b.name, humanBytes(scan.Bytes))
	}

	c, err := newAPIClient()
	if err != nil {
		return err
	}
	up, err := c.createUpload(ctx, createUploadReq{
		Name:          b.name,
		Kind:          b.kind,
		EstimatedSize: scan.TarSize + scan.TarSize/100 + 1<<20,
		Hours:         hours,
		Tag:           opts.tag,
		Items:         b.items(),
		Text:          opts.hasText,
	})
	if err != nil {
		return err
	}
	if scan.TarSize > up.MaxSize {
		fmt.Fprintf(os.Stderr, "Uncompressed size exceeds %s; upload will abort if compressed size also exceeds the limit.\n", humanBytes(up.MaxSize))
	}

	done := false
	defer func() {
		if !done {
			c.abort(up.UploadID)
		}
	}()

	prog := startProgress("Uploading", scan.Bytes)
	sent, err := uploadStream(ctx, c, up, b, prog)
	prog.finish()
	if err != nil {
		return err
	}
	res, err := c.complete(ctx, up.UploadID)
	if err != nil {
		return err
	}
	done = true

	exp := res.ExpiresAt.Local()
	web := c.base + "/" + res.Code
	fmt.Fprintf(os.Stderr, "Upload complete (%s compressed)\n", humanBytes(sent))
	fmt.Printf("  Code:     %s\n", res.Code)
	fmt.Printf("  Expires:  %s (in %dh)\n", exp.Format("2006-01-02 15:04"), hours)
	if opts.tag != "" {
		fmt.Printf("  List:     %s/files/%s\n", c.base, opts.tag)
	}
	copyToClipboard(web)
	return nil
}

// uploadStream tars and zstd-compresses b on the fly and uploads it in
// chunk-sized parts, partWorkers at a time. It returns the compressed size.
func uploadStream(ctx context.Context, c *apiClient, up *createUploadResp, b bundle, prog *progress) (int64, error) {
	parent := ctx
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	pr, pw := io.Pipe()
	go func() {
		zw, err := zstd.NewWriter(pw)
		if err == nil {
			err = writeTar(ctx, zw, b, prog.note, prog.add)
			if cerr := zw.Close(); err == nil {
				err = cerr
			}
		}
		pw.CloseWithError(err)
	}()

	type part struct {
		n    int
		data []byte
	}
	parts := make(chan part)
	var (
		wg   sync.WaitGroup
		once sync.Once
		werr error
	)
	fail := func(err error) {
		once.Do(func() { werr = err; cancel(err) })
	}
	for range partWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range parts {
				if err := c.putPart(ctx, up.UploadID, p.n, p.data); err != nil {
					fail(err)
				}
			}
		}()
	}

	var total int64
read:
	for n := 1; ; n++ {
		buf := make([]byte, up.ChunkSize)
		k, err := io.ReadFull(pr, buf)
		if k > 0 {
			total += int64(k)
			if total > up.MaxSize {
				fail(fmt.Errorf("compressed size exceeds %s limit; upload aborted", humanBytes(up.MaxSize)))
				break
			}
			select {
			case parts <- part{n, buf[:k]}:
			case <-ctx.Done():
				break read
			}
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			break
		}
		if err != nil {
			fail(err)
			break
		}
	}
	close(parts)
	pr.CloseWithError(context.Canceled) // unblocks the tar writer if we stopped early
	wg.Wait()

	if err := parent.Err(); err != nil {
		return 0, err
	}
	if werr != nil {
		return 0, werr
	}
	return total, nil
}
