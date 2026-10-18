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
	fmt.Fprintf(os.Stderr, `dtlk - 파일/폴더를 6자리 코드로 주고받기

사용법:
  dtlk up [--h 시간] <파일 또는 폴더>   업로드하고 6자리 코드를 출력 (보관 기본 24시간, 1~168)
  dtlk down <6자리 코드>              현재 폴더에 다운로드
  dtlk version                       버전 출력

서버: %s
  (설정 파일 %s, 환경변수 DTLK_SERVER로 변경)
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
		fmt.Fprintf(os.Stderr, "알 수 없는 명령: %s\n\n", args[0])
		usage()
		return 2
	}

	if err != nil {
		if errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, "\n취소되었습니다.")
			return 130
		}
		var ue *usageError
		if errors.As(err, &ue) {
			fmt.Fprintf(os.Stderr, "오류: %s\n", ue.msg)
			return 2
		}
		fmt.Fprintf(os.Stderr, "오류: %s\n", err)
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
