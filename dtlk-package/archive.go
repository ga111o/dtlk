package main

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Archives are plain tar streams whose entries all live under one top-level
// name: the basename of what was uploaded ("test1/hello/world/text1.txt").

type scanResult struct {
	Files, Dirs, Links, Skipped int
	Bytes                       int64 // regular file contents
	TarSize                     int64 // upper bound of the uncompressed tar stream
}

// tarEntrySize over-estimates one entry, allowing for a PAX header when needed.
func tarEntrySize(nameLen, linkLen int, size int64) int64 {
	s := int64(512) + (size+511)/512*512
	if nameLen > 100 || linkLen > 100 || size >= 8<<30 {
		s += 1024 + int64(nameLen+linkLen+511)/512*512
	}
	return s
}

// walkSource visits root and everything below it, calling fn with the
// archive name of each entry. A symlink given as root itself is followed;
// symlinks below it are reported as links.
func walkSource(root, top string, fn func(path, name string, info fs.FileInfo) error) error {
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	info, err := os.Lstat(real)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fn(real, top, info)
	}
	return filepath.WalkDir(real, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(real, p)
		if err != nil {
			return err
		}
		name := top
		if rel != "." {
			name = top + "/" + filepath.ToSlash(rel)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		return fn(p, name, info)
	})
}

func scanSource(root, top string) (scanResult, error) {
	var r scanResult
	err := walkSource(root, top, func(p, name string, info fs.FileInfo) error {
		switch mode := info.Mode(); {
		case mode.IsDir():
			r.Dirs++
			r.TarSize += tarEntrySize(len(name)+1, 0, 0)
		case mode.IsRegular():
			r.Files++
			r.Bytes += info.Size()
			r.TarSize += tarEntrySize(len(name), 0, info.Size())
		case mode&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			r.Links++
			r.TarSize += tarEntrySize(len(name), len(target), 0)
		default:
			r.Skipped++
		}
		return nil
	})
	r.TarSize += 1024
	return r, err
}

type countingReader struct {
	r  io.Reader
	on func(int)
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 && c.on != nil {
		c.on(n)
	}
	return n, err
}

// writeTar streams root as a tar archive into w. onRead reports file bytes consumed.
func writeTar(ctx context.Context, w io.Writer, root, top string, warn func(string), onRead func(int)) error {
	tw := tar.NewWriter(w)
	err := walkSource(root, top, func(p, name string, info fs.FileInfo) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		mode := info.Mode()
		var link string
		switch {
		case mode.IsDir():
			name += "/"
		case mode.IsRegular():
		case mode&fs.ModeSymlink != 0:
			var err error
			if link, err = os.Readlink(p); err != nil {
				return err
			}
		default:
			warn(fmt.Sprintf("Skipping unsupported file type: %s", p))
			return nil
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		hdr.Name = name
		hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname = 0, 0, "", ""
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !mode.IsRegular() {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		if _, err := io.CopyN(tw, &countingReader{r: f, on: onRead}, hdr.Size); err != nil {
			if errors.Is(err, io.EOF) {
				return fmt.Errorf("file size changed during upload: %s", p)
			}
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

// cleanEntryName validates an archive path and returns it without a trailing slash.
func cleanEntryName(name string) (string, error) {
	n := strings.TrimSuffix(name, "/")
	if n == "" || strings.HasPrefix(n, "/") {
		return "", fmt.Errorf("invalid archive entry path: %q", name)
	}
	for _, part := range strings.Split(n, "/") {
		if part == "" || part == "." || part == ".." || strings.ContainsRune(part, 0) {
			return "", fmt.Errorf("invalid archive entry path: %q", name)
		}
	}
	return n, nil
}

// checkNoSymlinkParents rejects rel if any existing directory on its way
// (below dest) is a symlink, so nothing is ever written through a link.
func checkNoSymlinkParents(dest, rel string) error {
	parts := strings.Split(rel, "/")
	p := dest
	for _, part := range parts[:len(parts)-1] {
		p = filepath.Join(p, part)
		fi, err := os.Lstat(p)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("refusing to extract through symlink: %q", rel)
		}
	}
	return nil
}

// extractTar unpacks a dtlk tar stream into dest (an empty directory) and
// returns the archive's top-level name. Symlinks are created last and
// directory modes/mtimes are applied after their contents are written.
func extractTar(r io.Reader, dest string) (string, error) {
	type dirMeta struct {
		path  string
		mode  fs.FileMode
		mtime time.Time
	}
	type linkMeta struct {
		rel, path, target string
	}
	var (
		top   string
		dirs  []dirMeta
		links []linkMeta
	)
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("downloaded archive is corrupted: %w", err)
		}
		rel, err := cleanEntryName(hdr.Name)
		if err != nil {
			return "", err
		}
		first, _, _ := strings.Cut(rel, "/")
		if top == "" {
			top = first
		} else if first != top {
			return "", fmt.Errorf("invalid archive entry path: %q", hdr.Name)
		}
		if err := checkNoSymlinkParents(dest, rel); err != nil {
			return "", err
		}
		target := filepath.Join(dest, filepath.FromSlash(rel))

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o700); err != nil {
				return "", err
			}
			dirs = append(dirs, dirMeta{target, hdr.FileInfo().Mode().Perm(), hdr.ModTime})
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return "", err
			}
			if err := writeFile(target, tr, hdr); err != nil {
				return "", err
			}
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return "", err
			}
			links = append(links, linkMeta{rel, target, hdr.Linkname})
		default:
			return "", fmt.Errorf("unsupported archive entry type: %q", hdr.Name)
		}
	}
	if top == "" {
		return "", errors.New("downloaded archive is empty")
	}
	for _, l := range links {
		if err := checkNoSymlinkParents(dest, l.rel); err != nil {
			return "", err
		}
		if err := os.Symlink(l.target, l.path); err != nil {
			return "", err
		}
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		d := dirs[i]
		if err := os.Chmod(d.path, d.mode); err != nil {
			return "", err
		}
		_ = os.Chtimes(d.path, d.mtime, d.mtime)
	}
	return top, nil
}

func writeFile(path string, r io.Reader, hdr *tar.Header) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return fmt.Errorf("downloaded archive is corrupted: %w", err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(path, hdr.FileInfo().Mode().Perm()); err != nil {
		return err
	}
	return os.Chtimes(path, hdr.ModTime, hdr.ModTime)
}
