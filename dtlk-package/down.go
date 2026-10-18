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
		return usageErrorf("사용법: dtlk down <6자리 코드>")
	}
	code := strings.TrimSpace(args[0])
	if !codeRe.MatchString(code) {
		return usageErrorf("코드는 6자리 숫자여야 합니다: %s", code)
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
		return fmt.Errorf("서버가 잘못된 이름을 보냈습니다: %q", info.Name)
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
		return fmt.Errorf("현재 폴더에 쓸 수 없습니다: %w", err)
	}
	defer os.RemoveAll(tmp)

	fmt.Fprintf(os.Stderr, "받는 중: %s (%s, 만료 %s)\n", info.Name, humanBytes(info.Size), info.ExpiresAt.Local().Format("2006-01-02 15:04"))
	prog := startProgress("다운로드", info.Size)
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
		return fmt.Errorf("받은 데이터의 이름(%q)이 예상(%q)과 다릅니다", top, info.Name)
	}

	dst := filepath.Join(cwd, target)
	if overwrite {
		if err := os.RemoveAll(dst); err != nil {
			return fmt.Errorf("기존 '%s'을(를) 지울 수 없습니다: %w", target, err)
		}
	} else if taken, err := exists(dst); err != nil || taken {
		return fmt.Errorf("다운로드하는 사이에 '%s'이(가) 생겼습니다. 다시 실행하세요", target)
	}
	if err := os.Rename(filepath.Join(tmp, top), dst); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "완료: ./%s\n", target)
	return nil
}
