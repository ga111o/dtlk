package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

// isTerminal reports whether f is a terminal (not just any character
// device: /dev/null is one too).
func isTerminal(f *os.File) bool {
	var t syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TCGETS, uintptr(unsafe.Pointer(&t)))
	return errno == 0
}

type prompter struct {
	in  *bufio.Reader
	out io.Writer
}

func (p *prompter) ask(msg string) (string, error) {
	fmt.Fprint(p.out, msg)
	line, err := p.in.ReadString('\n')
	if err != nil && (err != io.EOF || line == "") {
		return "", errors.New("입력이 끝나 중단합니다")
	}
	return strings.TrimSpace(line), nil
}

// validName accepts a single path component usable as a file or folder name.
func validName(s string) error {
	if s == "" || s == "." || s == ".." || strings.ContainsAny(s, "/\x00") {
		return fmt.Errorf("사용할 수 없는 이름입니다: %q", s)
	}
	return nil
}

func exists(path string) (bool, error) {
	_, err := os.Lstat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

// splitExt separates a file name's extension, treating .tar.* as one extension.
func splitExt(name string) (stem, ext string) {
	lower := strings.ToLower(name)
	for _, e := range []string{".tar.gz", ".tar.zst", ".tar.xz", ".tar.bz2"} {
		if strings.HasSuffix(lower, e) && len(name) > len(e) {
			return name[:len(name)-len(e)], name[len(name)-len(e):]
		}
	}
	ext = filepath.Ext(name)
	if ext == name { // dotfile such as ".bashrc"
		ext = ""
	}
	return name[:len(name)-len(ext)], ext
}

// freeName suggests "test1 (1)" / "text1 (1).txt", the first one not taken in dir.
func freeName(dir, name string, isDir bool) string {
	stem, ext := name, ""
	if !isDir {
		stem, ext = splitExt(name)
	}
	for i := 1; ; i++ {
		cand := fmt.Sprintf("%s (%d)%s", stem, i, ext)
		if ok, err := exists(filepath.Join(dir, cand)); err == nil && !ok {
			return cand
		}
	}
}

// resolveTarget picks the name to save a download as inside dir. If name is
// taken it asks y (overwrite) / n (enter a new name); without a prompter
// (not a terminal) it fails without touching anything.
func resolveTarget(dir, name string, isDir bool, p *prompter) (target string, overwrite bool, err error) {
	target = name
	for {
		taken, err := exists(filepath.Join(dir, target))
		if err != nil {
			return "", false, err
		}
		if !taken {
			return target, false, nil
		}
		if p == nil {
			return "", false, fmt.Errorf("'%s'이(가) 이미 있습니다. 터미널에서 실행하면 덮어쓰기 또는 이름 변경을 선택할 수 있습니다", target)
		}
		ans, err := p.ask(fmt.Sprintf("'%s'이(가) 이미 있습니다. 덮어쓸까요? [y/n]: ", target))
		if err != nil {
			return "", false, err
		}
		switch strings.ToLower(ans) {
		case "y", "yes":
			return target, true, nil
		case "n", "no":
			def := freeName(dir, target, isDir)
			for {
				in, err := p.ask(fmt.Sprintf("새 이름 [%s]: ", def))
				if err != nil {
					return "", false, err
				}
				if in == "" {
					in = def
				}
				if err := validName(in); err != nil {
					fmt.Fprintln(p.out, err)
					continue
				}
				target = in
				break
			}
		default:
			fmt.Fprintln(p.out, "y 또는 n을 입력하세요.")
		}
	}
}
