package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

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
		return 0, usageErrorf("--h는 1~%d 사이의 정수(시간)여야 합니다: %s", maxHours, s)
	}
	return h, nil
}

// parseUpArgs accepts "--h N" / "--h=N" anywhere around a single path.
func parseUpArgs(args []string) (hours int, path string, err error) {
	hours = defaultHours
	var paths []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--h":
			if i+1 >= len(args) {
				return 0, "", usageErrorf("--h 뒤에 시간을 입력하세요 (1~%d)", maxHours)
			}
			i++
			if hours, err = parseHours(args[i]); err != nil {
				return 0, "", err
			}
		case strings.HasPrefix(a, "--h="):
			if hours, err = parseHours(strings.TrimPrefix(a, "--h=")); err != nil {
				return 0, "", err
			}
		case a == "--":
			paths = append(paths, args[i+1:]...)
			i = len(args)
		case strings.HasPrefix(a, "-") && a != "-":
			return 0, "", usageErrorf("알 수 없는 옵션: %s", a)
		default:
			paths = append(paths, a)
		}
	}
	if len(paths) != 1 {
		return 0, "", usageErrorf("사용법: dtlk up [--h 시간] <파일 또는 폴더>")
	}
	return hours, paths[0], nil
}

func cmdUp(ctx context.Context, args []string) error {
	hours, path, err := parseUpArgs(args)
	if err != nil {
		return err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	info, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("경로를 찾을 수 없습니다: %s", path)
		}
		return err
	}
	name := filepath.Base(abs)
	if abs == "/" || validName(name) != nil {
		return fmt.Errorf("이 경로는 올릴 수 없습니다: %s", path)
	}
	kind := "file"
	switch {
	case info.IsDir():
		kind = "dir"
	case !info.Mode().IsRegular():
		return fmt.Errorf("일반 파일이나 폴더만 올릴 수 있습니다: %s", path)
	}

	scan, err := scanSource(abs, name)
	if err != nil {
		return fmt.Errorf("파일 목록을 읽을 수 없습니다: %w", err)
	}
	if kind == "dir" {
		fmt.Fprintf(os.Stderr, "%s/: 파일 %d개, 폴더 %d개, 링크 %d개, %s\n", name, scan.Files, scan.Dirs, scan.Links, humanBytes(scan.Bytes))
	} else {
		fmt.Fprintf(os.Stderr, "%s: %s\n", name, humanBytes(scan.Bytes))
	}

	c, err := newAPIClient()
	if err != nil {
		return err
	}
	up, err := c.createUpload(ctx, createUploadReq{
		Name:          name,
		Kind:          kind,
		EstimatedSize: scan.TarSize + scan.TarSize/100 + 1<<20,
		Hours:         hours,
	})
	if err != nil {
		return err
	}
	if scan.TarSize > up.MaxSize {
		fmt.Fprintf(os.Stderr, "원본이 %s보다 큽니다. 압축 후에도 넘으면 업로드가 중단됩니다.\n", humanBytes(up.MaxSize))
	}

	done := false
	defer func() {
		if !done {
			c.abort(up.UploadID)
		}
	}()

	prog := startProgress("업로드", scan.Bytes)
	sent, err := uploadStream(ctx, c, up, abs, name, prog)
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
	fmt.Fprintf(os.Stderr, "업로드 완료 (압축 후 %s)\n", humanBytes(sent))
	fmt.Printf("코드: %s\n", res.Code)
	fmt.Printf("만료: %s (%d시간 후)\n", exp.Format("2006-01-02 15:04"), hours)
	fmt.Printf("받기: dtlk down %s\n", res.Code)
	fmt.Printf("웹:   %s/%s\n", c.base, res.Code)
	return nil
}

// uploadStream tars and zstd-compresses root on the fly and uploads it in
// chunk-sized parts, partWorkers at a time. It returns the compressed size.
func uploadStream(ctx context.Context, c *apiClient, up *createUploadResp, root, top string, prog *progress) (int64, error) {
	parent := ctx
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	pr, pw := io.Pipe()
	go func() {
		zw, err := zstd.NewWriter(pw)
		if err == nil {
			err = writeTar(ctx, zw, root, top, prog.note, prog.add)
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
				fail(fmt.Errorf("압축 후 크기가 %s를 넘어 업로드를 중단했습니다", humanBytes(up.MaxSize)))
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
