package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// session 表示一次固定测试工作负载，独立于控制连接存活。
type session struct {
	// id 是 Host 分配的 session 标识。
	id string
	// command 是唯一创建并等待的实际子进程。
	command *exec.Cmd
	// pty 是父进程持有的 PTY master。
	pty *device
	// cancel 终止 PTY 设备循环。
	cancel context.CancelFunc
	// done 在进程退出结果固定后关闭。
	done chan struct{}
	// outputDone 在 PTY 输出读取结束后关闭。
	outputDone chan struct{}
	// mu 保护退出结果和有界终端缓冲。
	mu sync.Mutex
	// exit 是单次写入的实际进程退出结果。
	exit map[string]any
	// output 是最多 64 KiB 的待发送终端字节。
	output []byte
	// dropped 是累计丢弃的最旧字节数。
	dropped uint64
	// changed 通知终端发送循环重新检查缓冲和退出状态。
	changed chan struct{}
	// space 唤醒等待有界缓冲空位的 PTY 读取循环。
	space chan struct{}
	// attached 决定满缓冲时背压还是丢弃最旧内容。
	attached bool
}

// newID 生成原型使用的不可预测标识。
func newID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(value[:])
}

// ioctl 对受控 PTY 句柄执行指定终端操作。
func ioctl(fd uintptr, operation uintptr, pointer unsafe.Pointer) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, operation, uintptr(pointer))
	if errno != 0 {
		return errno
	}
	return nil
}

// resize 设置实际 PTY 的行列数。
func (s *session) resize(rows, cols uint16) error {
	window := [4]uint16{rows, cols, 0, 0}
	return ioctl(s.pty.file.Fd(), syscall.TIOCSWINSZ, unsafe.Pointer(&window[0]))
}

// startSession 创建 PTY，设置初始窗口，再启动固定非 root 测试程序。
func startSession(id string, rows, cols uint16, exited func(*session)) (*session, error) {
	ctx, cancel := context.WithCancel(context.Background())
	master, err := openDevice(ctx, "/dev/ptmx")
	if err != nil {
		cancel()
		return nil, err
	}
	var unlock, number uint32
	if err = ioctl(master.file.Fd(), syscall.TIOCSPTLCK, unsafe.Pointer(&unlock)); err != nil {
		master.file.Close()
		cancel()
		return nil, err
	}
	if err = ioctl(master.file.Fd(), syscall.TIOCGPTN, unsafe.Pointer(&number)); err != nil {
		master.file.Close()
		cancel()
		return nil, err
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR, 0)
	if err != nil {
		master.file.Close()
		cancel()
		return nil, err
	}
	s := &session{id: id, pty: master, cancel: cancel, done: make(chan struct{}), outputDone: make(chan struct{}), changed: make(chan struct{}, 1), space: make(chan struct{}, 1)}
	if err = s.resize(rows, cols); err != nil {
		slave.Close()
		master.file.Close()
		cancel()
		return nil, err
	}
	command := exec.Command("/bin/sh", "/bin/probe-workload")
	command.Env = []string{"PATH=/bin:/usr/bin", "HOME=/home/agent", "TERM=xterm-256color"}
	command.Dir = "/home/agent"
	command.Stdin, command.Stdout, command.Stderr = slave, slave, slave
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0, Credential: &syscall.Credential{Uid: 1000, Gid: 1000, NoSetGroups: true}}
	if err = command.Start(); err != nil {
		slave.Close()
		master.file.Close()
		cancel()
		return nil, err
	}
	s.command = command
	slave.Close()
	go s.collectOutput()
	// 只有这一条路径调用 Wait，结果先固定再通知控制连接。
	go func() {
		err := command.Wait()
		code, signal := command.ProcessState.ExitCode(), ""
		if status, ok := command.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			signal = status.Signal().String()
		}
		s.mu.Lock()
		s.exit = map[string]any{"sessionID": id, "code": code, "signal": signal}
		s.mu.Unlock()
		close(s.done)
		s.notify()
		if err != nil && code == 0 {
			fmt.Fprintln(os.Stderr, "WAIT_ERROR", err)
		}
		exited(s)
	}()
	return s, nil
}

// notify 合并缓冲变化通知，不阻塞进程回收或控制请求。
func (s *session) notify() {
	select {
	case s.changed <- struct{}{}:
	default:
	}
}

// wakeSpace 合并缓冲释放通知，使满缓冲读取可以继续或切换为脱离模式。
func (s *session) wakeSpace() {
	select {
	case s.space <- struct{}{}:
	default:
	}
}

// collectOutput 将 PTY 持续排空到有界缓冲，断开终端不影响工作负载。
func (s *session) collectOutput() {
	defer close(s.outputDone)
	defer s.notify()
	buffer := make([]byte, 4096)
	for {
		s.mu.Lock()
		attached, available := s.attached, 65536-len(s.output)
		s.mu.Unlock()
		if attached && available == 0 {
			select {
			case <-s.pty.ctx.Done():
				return
			case <-s.space:
			}
			continue
		}
		size := len(buffer)
		if attached {
			size = min(size, available)
		}
		n, err := s.pty.Read(buffer[:size])
		if n > 0 {
			s.mu.Lock()
			s.output = append(s.output, buffer[:n]...)
			if excess := len(s.output) - 65536; excess > 0 {
				s.output = append([]byte(nil), s.output[excess:]...)
				s.dropped += uint64(excess)
			}
			s.mu.Unlock()
			s.notify()
		}
		if err != nil {
			if !errors.Is(err, syscall.EIO) && !errors.Is(err, io.EOF) && !errors.Is(err, context.Canceled) {
				fmt.Fprintln(os.Stderr, "PTY_READ_ERROR", err)
			}
			return
		}
	}
}

// snapshot 返回固定退出结果或运行状态，不暴露内部可变数据。
func (s *session) snapshot() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return map[string]any{"sessionID": s.id, "pid": s.command.Process.Pid, "exit": s.exit, "bufferedBytes": len(s.output), "droppedBytes": s.dropped}
}

// groupRunning 检查本实验进程组是否仍有可执行成员，忽略已退出的僵尸。
func (s *session) groupRunning() (bool, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false, err
	}
	group := strconv.Itoa(s.command.Process.Pid)
	for _, entry := range entries {
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		data, err := os.ReadFile("/proc/" + entry.Name() + "/stat")
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, err
		}
		// comm 允许含空格或括号，字段从最后一个右括号后开始解析。
		fields := strings.Fields(string(data)[strings.LastIndexByte(string(data), ')')+1:])
		if fields[2] == group && fields[0] != "Z" && fields[0] != "X" {
			return true, nil
		}
	}
	return false, nil
}

// stop 终止固定测试进程组，等待退出或请求取消，不依赖终端读写。
func (s *session) stop(ctx context.Context, grace time.Duration, force bool) error {
	if err := syscall.Kill(-s.command.Process.Pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		running, err := s.groupRunning()
		if err != nil {
			return err
		}
		if !running {
			select {
			case <-s.done:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		case <-timer.C:
			if !force {
				return errors.New("grace period exceeded")
			}
			if err := syscall.Kill(-s.command.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
				return err
			}
		}
	}
}
