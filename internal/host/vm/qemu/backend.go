// Package qemu 实现 Linux/amd64 microvm 的磁盘和进程操作。
package qemu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"safeagent/internal/host/vm"
)

// Driver 执行 QEMU 私有资源副作用，不保存第二份生命周期状态。
type Driver struct {
	// qemuPath 是受控 QEMU 可执行文件绝对路径。
	qemuPath string
	// qemuImgPath 是受控 qemu-img 可执行文件绝对路径。
	qemuImgPath string
	// dataRoot 是仅由本后端拥有的资源目录。
	dataRoot string
	// channelsMu 保护本驱动启动的私有端点记录。
	channelsMu sync.Mutex
	// channels 仅记录当前进程实际启动的实例，不从磁盘恢复授权。
	channels map[vm.VMInstance]*channelRun
}

// process 保存进程句柄和停止请求，进程只由 Manager 的观察者回收。
type process struct {
	// cmd 是独立于请求 context 的长驻进程。
	cmd *exec.Cmd
	// instance 绑定本次启动的持久化 epoch。
	instance vm.VMInstance
	// mu 协调停止请求和真实退出状态的发布。
	mu sync.Mutex
	// stopRequested 表示已请求 SIGKILL，不代表进程已经退出。
	stopRequested bool
	// driver 在真实退出后回收当前运行的端点映射。
	driver *Driver
	// channels 在实际 VMM 退出后失效全部 Guest 句柄。
	channels *channelRun
}

var _ vm.Driver = (*Driver)(nil)

// NewDriver 校验宿主平台及目录边界，供共享 Manager 注入后端使用。
func NewDriver(qemuPath, qemuImgPath, dataRoot string) (*Driver, error) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		return nil, fmt.Errorf("%w: only linux/amd64 supported", vm.ErrInvalidConfig)
	}
	if !filepath.IsAbs(qemuPath) || !filepath.IsAbs(qemuImgPath) || !filepath.IsAbs(dataRoot) {
		return nil, fmt.Errorf("%w: paths must be absolute", vm.ErrInvalidConfig)
	}
	if err := os.MkdirAll(dataRoot, 0700); err != nil {
		return nil, err
	}
	dataRoot, err := filepath.EvalSymlinks(dataRoot)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(dataRoot)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm() != 0700 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) {
		return nil, fmt.Errorf("%w: data root must be owned private directory", vm.ErrInvalidConfig)
	}
	return &Driver{qemuPath: qemuPath, qemuImgPath: qemuImgPath, dataRoot: dataRoot, channels: make(map[vm.VMInstance]*channelRun)}, nil
}

// Prepare 创建或核验本后端资源，不覆盖已有差分盘。
func (d *Driver) Prepare(ctx context.Context, c vm.VMConfig) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.Architecture != vm.AMD64 || c.Machine != vm.MicroVM {
		return fmt.Errorf("%w: only amd64 microvm supported", vm.ErrInvalidConfig)
	}
	for _, path := range []string{c.Image.KernelPath, c.Image.InitrdPath, c.Image.BaseDiskPath} {
		if path == "" {
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%w: image must be regular file", vm.ErrInvalidConfig)
		}
	}
	dir := filepath.Join(d.dataRoot, c.ID)
	data, _ := json.Marshal(c)
	err := os.Mkdir(dir, 0700)
	if err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	old, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err == nil {
		if string(old) != string(data) {
			return vm.ErrConflict
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	} else {
		// 以原子重命名发布完整配置，未发布的临时文件可在重试时重写。
		tmp := filepath.Join(dir, "config.json.creating")
		if err = os.WriteFile(tmp, data, 0600); err != nil {
			return err
		}
		if err = os.Rename(tmp, filepath.Join(dir, "config.json")); err != nil {
			return err
		}
	}
	overlay := filepath.Join(dir, "overlay.qcow2")
	if _, err = os.Stat(overlay); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// 临时差分盘创建成功后再发布，崩溃不会把部分输出当成已完成磁盘。
	tmp := overlay + ".creating"
	cmd := exec.CommandContext(ctx, d.qemuImgPath, "create", "-f", "qcow2", "-F", "raw", "-b", c.Image.BaseDiskPath, tmp)
	if output, runErr := cmd.CombinedOutput(); runErr != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("qemu-img: %w: %s", runErr, output)
	}
	return os.Rename(tmp, overlay)
}

// Launch 启动指定 epoch 的独立进程，错误路径不遗留子进程。
func (d *Driver) Launch(ctx context.Context, c vm.VMConfig, i vm.VMInstance) (vm.Process, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := accelerator(c.Accelerator); err != nil {
		return nil, err
	}
	dir := filepath.Join(d.dataRoot, c.ID)
	runDir := channelDir(dir, i.Epoch)
	if err := os.Mkdir(runDir, 0700); err != nil {
		return nil, err
	}
	runCtx, runCancel := context.WithCancel(context.Background())
	run := &channelRun{ctx: runCtx, cancel: runCancel, dir: runDir, connections: make(map[*trackedConn]bool)}
	cmd := exec.Command(d.qemuPath, args(c, dir, i.Epoch)...)
	cmd.Dir = dir
	if err := cmd.Start(); err != nil {
		run.close()
		return nil, err
	}
	d.channelsMu.Lock()
	d.channels[i] = run
	d.channelsMu.Unlock()
	return &process{cmd: cmd, instance: i, channels: run, driver: d}, nil
}

// Remove 删除私有目录，保留数据库与不可变基础盘。
func (d *Driver) Remove(ctx context.Context, ref vm.VMRef) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return os.RemoveAll(filepath.Join(d.dataRoot, ref.ID))
}

// Stop 请求强停 microvm，未授权强停时明确拒绝。
func (p *process) Stop(o vm.StopOptions) error {
	if !o.ForceAfterTimeout {
		return vm.ErrShutdownUnsupported
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	err := p.cmd.Process.Kill()
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	if err == nil {
		p.stopRequested = true
	}
	return err
}

// Wait 从实际进程状态提取退出结果，只由 Manager 观察者调用一次。
func (p *process) Wait() (vm.VMExit, error) {
	err := p.cmd.Wait()
	p.channels.close()
	p.driver.channelsMu.Lock()
	if p.driver.channels[p.instance] == p.channels {
		delete(p.driver.channels, p.instance)
	}
	p.driver.channelsMu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	exit := vm.VMExit{Instance: p.instance, Reason: vm.ExitProcess}
	if err != nil {
		var x *exec.ExitError
		if !errors.As(err, &x) {
			return vm.VMExit{}, err
		}
		exit.Code = x.ExitCode()
		status := x.Sys().(syscall.WaitStatus)
		if status.Signaled() {
			exit.Signal = status.Signal().String()
			if p.stopRequested && status.Signal() == syscall.SIGKILL {
				exit.Reason = vm.ExitForced
			}
		}
	}
	return exit, nil
}

// accelerator 检查显式加速器能力，禁止隐式回退。
func accelerator(a vm.Accelerator) error {
	if a == vm.TCG {
		return nil
	}
	if a != vm.KVM || runtime.GOOS != "linux" {
		return fmt.Errorf("%w: accelerator %s unsupported", vm.ErrInvalidConfig, a)
	}
	if f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0); err != nil {
		return fmt.Errorf("qemu: kvm unavailable: %w", err)
	} else {
		return f.Close()
	}
}

// args 构造固定的 QEMU 参数。
func args(c vm.VMConfig, dir, epoch string) []string {
	a := []string{"-machine", string(c.Machine), "-accel", string(c.Accelerator), "-cpu", "max", "-m", strconv.Itoa(int(c.MemoryMiB)), "-smp", strconv.Itoa(int(c.VCPUs)), "-nodefaults", "-no-user-config", "-display", "none", "-no-reboot", "-nic", "none", "-serial", "none", "-drive", "file=" + strings.ReplaceAll(filepath.Join(dir, "overlay.qcow2"), ",", ",,") + ",if=none,format=qcow2,id=workspace", "-device", "virtio-blk-device,drive=workspace", "-name", epoch, "-kernel", c.Image.KernelPath}
	runDir := channelDir(dir, epoch)
	a = append(a, "-device", "virtio-serial-device,id=guestserial", "-chardev", "socket,id=guestcontrol,path="+strings.ReplaceAll(filepath.Join(runDir, "control.sock"), ",", ",,")+",server=on,wait=off", "-device", "virtserialport,bus=guestserial.0,chardev=guestcontrol,name=safeagent.control", "-chardev", "socket,id=guestterminal,path="+strings.ReplaceAll(filepath.Join(runDir, "terminal.sock"), ",", ",,")+",server=on,wait=off", "-device", "virtserialport,bus=guestserial.0,chardev=guestterminal,name=safeagent.terminal", "-append", "console=ttyS0 panic=-1")
	if c.Image.InitrdPath != "" {
		a = append(a, "-initrd", c.Image.InitrdPath)
	}
	return a
}

// Identity 返回解析符号链接后的受控资源根目录。
func (d *Driver) Identity() string { return d.dataRoot }

// Acquire 防止不同数据库的 Manager 同时控制同一资源根目录。
func (d *Driver) Acquire() (io.Closer, error) {
	f, err := os.OpenFile(filepath.Join(d.dataRoot, ".manager.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return f, nil
}
