package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	fSetOwn = 8
	fSetSig = 10
	oAsync  = 0o20000
)

type reply struct {
	Mode  string `json:"mode"`
	Errno int    `json:"errno"`
	Note  string `json:"note,omitempty"`
}

func main() {
	if len(os.Args) > 1 {
		encoded, _ := json.Marshal(handle(os.Args[1], os.Args[2:]))
		_, _ = os.Stdout.Write(append(encoded, '\n'))
		return
	}
	reader := bufio.NewReaderSize(os.Stdin, 1<<20)
	writer := bufio.NewWriter(os.Stdout)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		out := handle(fields[0], fields[1:])
		encoded, _ := json.Marshal(out)
		_, _ = writer.Write(append(encoded, '\n'))
		_ = writer.Flush()
	}
}

func number(args []string, index int) int {
	if index >= len(args) {
		return 0
	}
	value, _ := strconv.Atoi(args[index])
	return value
}

func errnoOf(errno syscall.Errno) int {
	return int(errno)
}

func handle(mode string, args []string) reply {
	switch mode {
	case "echo":
		return reply{Mode: mode, Note: strings.Join(args, " ")}
	case "work":
		work()
		return reply{Mode: mode, Note: strconv.Itoa(runtime.NumGoroutine())}
	case "pid":
		return reply{Mode: mode, Errno: os.Getpid()}
	case "kill":
		_, _, errno := syscall.RawSyscall(syscall.SYS_KILL, uintptr(number(args, 0)), uintptr(syscall.SIGKILL), 0)
		return reply{Mode: mode, Errno: errnoOf(errno)}
	case "tgkill":
		victim := uintptr(number(args, 0))
		_, _, errno := syscall.RawSyscall(syscall.SYS_TGKILL, victim, victim, uintptr(syscall.SIGKILL))
		return reply{Mode: mode, Errno: errnoOf(errno)}
	case "tgkill-self":
		_, _, errno := syscall.RawSyscall(syscall.SYS_TGKILL, uintptr(os.Getpid()), uintptr(syscall.Gettid()), 0)
		return reply{Mode: mode, Errno: errnoOf(errno)}
	case "setown":
		return setOwn(args)
	case "prlimit":
		limit := [2]uint64{7, 7}
		_, _, errno := syscall.RawSyscall6(syscall.SYS_PRLIMIT64, uintptr(number(args, 0)), 7, uintptr(unsafe.Pointer(&limit)), 0, 0, 0)
		return reply{Mode: mode, Errno: errnoOf(errno)}
	case "socket":
		_, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
		return reply{Mode: mode, Note: fmt.Sprint(err)}
	case "ptrace":
		_, _, errno := syscall.RawSyscall6(syscall.SYS_PTRACE, uintptr(syscall.PTRACE_ATTACH), uintptr(number(args, 0)), 0, 0, 0, 0)
		return reply{Mode: mode, Errno: errnoOf(errno)}
	case "bpf":
		_, _, errno := syscall.RawSyscall(unix.SYS_BPF, 0, 0, 0)
		return reply{Mode: mode, Errno: errnoOf(errno)}
	case "fork":
		pid, _, errno := syscall.RawSyscall(syscall.SYS_FORK, 0, 0, 0)
		if pid == 0 && errno == 0 {
			syscall.RawSyscall(syscall.SYS_EXIT_GROUP, 0, 0, 0)
		}
		return reply{Mode: mode, Errno: errnoOf(errno)}
	case "clone-newuser":
		runtime.LockOSThread()
		pid, _, errno := syscall.RawSyscall6(syscall.SYS_CLONE, syscall.CLONE_NEWUSER|uintptr(syscall.SIGCHLD), 0, 0, 0, 0, 0)
		if pid == 0 && errno == 0 {
			syscall.RawSyscall(syscall.SYS_EXIT_GROUP, 0, 0, 0)
		}
		return reply{Mode: mode, Errno: errnoOf(errno)}
	case "clone3":
		_, _, errno := syscall.RawSyscall(435, 0, 0, 0)
		return reply{Mode: mode, Errno: errnoOf(errno)}
	case "prctl":
		_, _, errno := syscall.RawSyscall6(syscall.SYS_PRCTL, 15, uintptr(unsafe.Pointer(&[]byte("probe\x00")[0])), 0, 0, 0, 0)
		return reply{Mode: mode, Errno: errnoOf(errno)}
	case "write":
		err := os.WriteFile(filepath.Join(args[0], "probe-written"), []byte("x"), 0o600)
		return reply{Mode: mode, Note: fmt.Sprint(err), Errno: boolToInt(err == nil)}
	case "tmpdir":
		return reply{Mode: mode, Note: os.TempDir()}
	case "scratch-write":
		path := filepath.Join(os.TempDir(), "probe-scratch")
		err := os.WriteFile(path, []byte("x"), 0o600)
		return reply{Mode: mode, Note: fmt.Sprint(err), Errno: boolToInt(err == nil)}
	case "stderr":
		_, _ = os.Stderr.Write(make([]byte, number(args, 0)))
		return reply{Mode: mode}
	case "threads":
		return threads(number(args, 0))
	case "memory":
		hold := make([][]byte, 0, number(args, 0))
		for range number(args, 0) {
			block := make([]byte, 1<<20)
			for i := range block {
				block[i] = 1
			}
			hold = append(hold, block)
		}
		return reply{Mode: mode, Note: strconv.Itoa(len(hold))}
	case "exit":
		os.Exit(number(args, 0))
	}
	return reply{Mode: mode, Errno: -1, Note: "unknown mode"}
}

func boolToInt(ok bool) int {
	if ok {
		return 1
	}
	return 0
}

func setOwn(args []string) reply {
	var pipe [2]int
	if err := syscall.Pipe(pipe[:]); err != nil {
		return reply{Mode: "setown", Errno: -1, Note: err.Error()}
	}
	read := uintptr(pipe[0])
	_, _, first := syscall.RawSyscall(syscall.SYS_FCNTL, read, fSetOwn, uintptr(number(args, 0)))
	_, _, second := syscall.RawSyscall(syscall.SYS_FCNTL, read, fSetSig, uintptr(syscall.SIGKILL))
	_, _, third := syscall.RawSyscall(syscall.SYS_FCNTL, read, syscall.F_SETFL, oAsync)
	_, _ = syscall.Write(pipe[1], []byte("x"))
	time.Sleep(100 * time.Millisecond)
	return reply{Mode: "setown", Errno: errnoOf(first) + errnoOf(second) + errnoOf(third)}
}

func threads(count int) reply {
	var ready sync.WaitGroup
	release := make(chan struct{})
	for range count {
		ready.Add(1)
		go func() {
			runtime.LockOSThread()
			ready.Done()
			<-release
		}()
	}
	ready.Wait()
	close(release)
	return reply{Mode: "threads", Note: strconv.Itoa(count)}
}

func work() {
	var group sync.WaitGroup
	for range 8 {
		group.Add(1)
		go func() {
			defer group.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			buffer := make([]byte, 1<<20)
			_ = sha256.Sum256(buffer)
			counter := 0
			for {
				counter++
				if counter&0xffffff == 0 {
					select {
					case <-ctx.Done():
						return
					default:
					}
				}
			}
		}()
	}
	group.Wait()
	runtime.GC()
	time.Sleep(5 * time.Millisecond)
}
