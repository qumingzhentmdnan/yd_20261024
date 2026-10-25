// Package runner 实现 Linux 固定工作负载的非 root PTY 与进程组管理。
package runner

import (
	"context"
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

	"golang.org/x/sys/unix"
	"safeagent/internal/protocol"
)

// Config 由镜像登记层提供，不能直接来自 RPC。
type Config struct {
	// Path 是固定可执行文件绝对路径。
	Path string
	// Args 是固定参数，不接受业务追加。
	Args []string
	// Dir 是固定已校验工作区。
	Dir string
	// UID 是非 root 用户身份。
	UID uint32
	// GID 是非 root 主组。
	GID uint32
	// Environment 是经过清单校验的显式环境。
	Environment []string
}

// Process 拥有 PTY、唯一子进程等待和停止协调。
type Process struct {
	// cmd 是已创建的固定子进程。
	cmd *exec.Cmd
	// master 是唯一 PTY master，由 Close 回收。
	master *os.File
	// mu 保护信号动作与固定退出结果。
	mu sync.Mutex
	// exit 是唯一 Wait 产生的固定结果。
	exit protocol.SessionExit
	// reason 标识已经发出的停止动作。
	reason protocol.ExitReason
	// done 在唯一 Wait 完成后关闭。
	done chan struct{}
	// groupEmpty 固定已观察组空，防止历史 Stop 操作复用后的 PGID。
	groupEmpty bool
	// waitErr 保存内核等待或状态读取失败。
	waitErr error
	// reaped 在唯一 cmd.Wait 回收 leader 后关闭。
	reaped chan struct{}
	// closed 避免重复释放 master。
	closed sync.Once
}

// Start 设置初始 PTY、会话领头进程、控制终端与明确非 root 身份。
func Start(c Config, id protocol.OpaqueID, rows, cols protocol.Dimension) (*Process, error) {
	if c.UID == 0 || c.GID == 0 {
		return nil, fmt.Errorf("runner requires non-root uid/gid")
	}
	fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	master := os.NewFile(uintptr(fd), "pty-master")
	started := false
	defer func() {
		if !started {
			master.Close()
		}
	}()
	if err = unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		return nil, err
	}
	number, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		return nil, err
	}
	slave, err := os.OpenFile("/dev/pts/"+strconv.Itoa(number), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, err
	}
	defer slave.Close()
	if os.Geteuid() == 0 {
		if err = slave.Chown(int(c.UID), int(c.GID)); err != nil {
			return nil, err
		}
	}
	if err = unix.IoctlSetWinsize(fd, unix.TIOCSWINSZ, &unix.Winsize{Row: uint16(rows), Col: uint16(cols)}); err != nil {
		return nil, err
	}
	cmd := exec.Command(c.Path, c.Args...)
	cmd.Dir = c.Dir
	cmd.Env = append([]string{"PATH=/usr/bin:/bin", "TERM=xterm", "LANG=C"}, c.Environment...)
	cmd.Stdin = slave
	cmd.Stdout = slave
	cmd.Stderr = slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if os.Geteuid() == 0 {
		cmd.SysProcAttr.Credential = &syscall.Credential{Uid: c.UID, Gid: c.GID, Groups: []uint32{}}
	} else if uint32(os.Geteuid()) != c.UID || uint32(os.Getegid()) != c.GID {
		return nil, fmt.Errorf("runner identity does not match configured uid/gid")
	}
	if err = cmd.Start(); err != nil {
		return nil, err
	}
	started = true
	p := &Process{cmd: cmd, master: master, done: make(chan struct{}), reason: protocol.ExitReasonProcessExited, reaped: make(chan struct{})}
	go p.wait(id)
	return p, nil
}

// wait 唯一回收直接子进程，并与信号发送串行固定退出原因。
func (p *Process) wait(id protocol.OpaqueID) {
	defer close(p.reaped)
	exit, err := p.observeExit(id)

	p.mu.Lock()
	if exit.Signal == "SIGTERM" && p.reason == protocol.ExitReasonStopped {
		exit.Reason = protocol.ExitReasonStopped
	}
	if exit.Signal == "SIGKILL" && p.reason == protocol.ExitReasonForced {
		exit.Reason = protocol.ExitReasonForced
	}
	p.exit = exit
	p.waitErr = err
	close(p.done)
	p.mu.Unlock()
	if err != nil {
		return
	}
	// 等组内可执行成员全部退出后，唯一回收 leader；主进程退出结果此前已经发布。
	for {
		state, groupErr := p.Group()
		if groupErr != nil {
			return
		}
		if state == protocol.GroupStateEmpty {
			p.cmd.Wait()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// observeExit 保留 leader 僵尸并读取内核退出状态，直到组空前都不允许复用 PGID。
func (p *Process) observeExit(id protocol.OpaqueID) (protocol.SessionExit, error) {
	exit := protocol.SessionExit{SessionID: id, Reason: protocol.ExitReasonProcessExited}
	var info unix.Siginfo
	if err := unix.Waitid(unix.P_PID, p.cmd.Process.Pid, &info, unix.WEXITED|unix.WNOWAIT, nil); err != nil {
		return exit, err
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", p.cmd.Process.Pid))
	if err != nil {
		return exit, err
	}
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return exit, fmt.Errorf("invalid exited process stat")
	}
	fields := strings.Fields(string(data)[end+1:])
	if len(fields) < 50 {
		return exit, fmt.Errorf("short exited process stat")
	}
	raw, err := strconv.ParseUint(fields[49], 10, 32)
	if err != nil {
		return exit, err
	}
	status := syscall.WaitStatus(raw)
	exit.Code = status.ExitStatus()
	if status.Signaled() {
		exit.Code = -1
		exit.Signal = unix.SignalName(status.Signal())
		if exit.Signal == "" {
			exit.Signal = "SIGRTMIN+" + strconv.Itoa(int(status.Signal())-34)
		}
	}
	return exit, nil
}

// Wait 观察唯一固定退出结果，取消只结束本次等待。
func (p *Process) Wait(ctx context.Context) (protocol.SessionExit, error) {
	select {
	case <-p.done:
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.exit, p.waitErr
	case <-ctx.Done():
		return protocol.SessionExit{}, ctx.Err()
	}
}

// Read 从 PTY 读取，Linux slave 全部关闭导致的 EIO 视为输出结束。
func (p *Process) Read(b []byte) (int, error) {
	n, err := p.master.Read(b)
	if errors.Is(err, syscall.EIO) {
		err = io.EOF
	}
	return n, err
}

// Write 将原始输入写入 PTY，deadline 支持 Detach 中断阻塞写。
func (p *Process) Write(ctx context.Context, b []byte) (int, error) {
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { defer close(done); p.master.SetWriteDeadline(time.Now()) })
	defer func() {
		if !stop() {
			<-done
		}
		p.master.SetWriteDeadline(time.Time{})
	}()
	return p.master.Write(b)
}

// Resize 对仍存在的 PTY 应用尺寸。
func (p *Process) Resize(rows, cols protocol.Dimension) error {
	return unix.IoctlSetWinsize(int(p.master.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: uint16(rows), Col: uint16(cols)})
}

// Group 查询仍可执行的组成员，忽略僵尸且处理进程消失竞态。
func (p *Process) Group() (protocol.GroupState, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.groupLocked()
}

// groupLocked 在保留 leader PID 的期间检查组，调用方持有 mu。
func (p *Process) groupLocked() (protocol.GroupState, error) {
	if p.groupEmpty {
		return protocol.GroupStateEmpty, nil
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return protocol.GroupStateUnknown, err
	}
	group := strconv.Itoa(p.cmd.Process.Pid)
	for _, entry := range entries {
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		running, err := processInGroup("/proc/"+entry.Name()+"/stat", group)
		if err != nil {
			return protocol.GroupStateUnknown, err
		}
		if running {
			return protocol.GroupStateRunning, nil
		}
	}
	p.groupEmpty = true
	return protocol.GroupStateEmpty, nil
}

// processInGroup 读取单个 proc stat，判断是否为指定组内仍可执行的成员。
func processInGroup(path, group string) (bool, error) {
	data, err := os.ReadFile(path)
	// proc 在打开前消失返回 ENOENT，打开后回收则可能在读取时返回 ESRCH。
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return false, fmt.Errorf("invalid proc stat")
	}
	fields := strings.Fields(string(data)[end+1:])
	if len(fields) < 3 {
		return false, fmt.Errorf("short proc stat")
	}
	return fields[2] == group && fields[0] != "Z" && fields[0] != "X", nil
}

// Signal 向仍存活的当前工作负载组发送显式停止信号。
func (p *Process) Signal(force bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	state, err := p.groupLocked()
	if err != nil {
		return err
	}
	if state == protocol.GroupStateEmpty {
		return nil
	}
	sig := syscall.SIGTERM
	reason := protocol.ExitReasonStopped
	if force {
		sig = syscall.SIGKILL
		reason = protocol.ExitReasonForced
	}
	if err = syscall.Kill(-p.cmd.Process.Pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	if force || p.reason != protocol.ExitReasonForced {
		p.reason = reason
	}
	return nil
}

// Stop 按本调用的宽限和授权停止组，取消不撤销已经发出的信号。
func (p *Process) Stop(ctx context.Context, grace time.Duration, force bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := p.Signal(false); err != nil {
		return err
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		state, err := p.Group()
		if err != nil {
			return err
		}
		if state == protocol.GroupStateEmpty {
			_, err = p.Wait(ctx)
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		case <-timer.C:
			if !force {
				return context.DeadlineExceeded
			}
			if err = ctx.Err(); err != nil {
				return err
			}
			if err = p.Signal(true); err != nil {
				return err
			}
		}
	}
}

// Close 仅释放 PTY master，不伪造进程退出。
func (p *Process) Close() error {
	var err error
	p.closed.Do(func() { err = p.master.Close() })
	return err
}

// Reap 等待 leader 被唯一等待路径实际回收，不把退出通知当作资源已经释放。
func (p *Process) Reap(ctx context.Context) error {
	select {
	case <-p.reaped:
		return p.waitErr
	case <-ctx.Done():
		return ctx.Err()
	}
}
