// Command guest-agent 运行镜像登记的单一固定工作负载，不提供任意命令入口。
package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"safeagent/internal/guest/bridge"
	"safeagent/internal/proxywire"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	server "safeagent/internal/guest"
	"safeagent/internal/guest/device"
	"safeagent/internal/guest/runner"
	"safeagent/internal/protocol"
)

// registration 是镜像构建时写入的固定内容清单，不接收 RPC 配置。
type registration struct {
	// Manifest 是 Manifest 的受控文件。
	Manifest string `json:"manifest"`
	// Configuration 是固定配置文件。
	Configuration string `json:"configuration"`
	// Workspace 是固定工作区内容文件。
	Workspace string `json:"workspace"`
	// Executable 是固定可执行文件，必须与 Manifest 的摘要绑定。
	Executable string `json:"executable"`
	// Directory 是固定工作负载工作目录。
	Directory string `json:"directory"`
	// Content 是镜像登记时生成的内容摘要及引用。
	Content protocol.Preparation `json:"content"`
}

// hashFile 计算实际登记文件摘要，读取失败原处返回。
func hashFile(path string) (protocol.Digest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return protocol.Digest(fmt.Sprintf("sha256:%x", sha256.Sum256(data))), nil
}

// load 校验固定登记的实际内容，不根据 Host 摘要生成配置或工作区。
func load() (server.Config, error) {
	var entry registration
	data, err := os.ReadFile("/etc/safeagent/registration.json")
	if err != nil {
		return server.Config{}, err
	}
	if err = json.Unmarshal(data, &entry); err != nil {
		return server.Config{}, err
	}
	checks := []struct {
		// path 是固定登记文件路径。
		path string
		// expected 是构建时写入的内容摘要。
		expected protocol.Digest
	}{{entry.Manifest, entry.Content.ManifestDigest}, {entry.Configuration, entry.Content.ConfigDigest}, {entry.Workspace, entry.Content.WorkspaceDigest}}
	for _, check := range checks {
		actual, err := hashFile(check.path)
		if err != nil {
			return server.Config{}, err
		}
		if actual != check.expected {
			return server.Config{}, fmt.Errorf("registered content digest mismatch")
		}
	}
	// Manifest 是固定可执行文件的完整字节副本，避免未绑定任意执行路径。
	executable, err := hashFile(entry.Executable)
	if err != nil {
		return server.Config{}, err
	}
	if executable != entry.Content.ManifestDigest {
		return server.Config{}, fmt.Errorf("registered executable digest mismatch")
	}
	return server.Config{Content: entry.Content, Build: "guest-agent-v01", ImageDigest: entry.Content.ManifestDigest, Runner: runner.Config{Path: entry.Executable, Dir: entry.Directory, UID: 1000, GID: 1000}}, nil
}

// acquireBoot 用 boot ID 和不可重入标记阻止同 boot 丢状态后静默服务。
func acquireBoot() (*os.File, error) { return acquireBootAt("/run/safeagent") }

// acquireBootAt 在可信启动目录建立不可重入boot标记，测试使用独立私有目录。
func acquireBootAt(root string) (*os.File, error) {
	bootID, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(root, strings.TrimSpace(string(bootID)))
	if err = os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		lock.Close()
		return nil, err
	}
	mark, err := os.OpenFile(filepath.Join(dir, "started"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		lock.Close()
		return nil, fmt.Errorf("guest state cannot be recreated in same boot: %w", err)
	}
	if err = mark.Close(); err != nil {
		lock.Close()
		return nil, err
	}
	return lock, nil
}

// run 接受本 boot 唯一控制连接，断线后退出且保留不可重入标记。
func run() error {
	lock, err := acquireBoot()
	if err != nil {
		return err
	}
	defer lock.Close()
	config, err := load()
	if err != nil {
		return err
	}
	// 镜像显式启用导入，根句柄由入口持有到所有导入任务退出。
	if _, statErr := os.Stat("/etc/safeagent/workspace-enabled"); statErr == nil {
		fd, openErr := unix.Open("/workspace", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if openErr != nil {
			return openErr
		}
		config.WorkspaceRoot = os.NewFile(uintptr(fd), "/workspace")
		defer config.WorkspaceRoot.Close()
	} else if !os.IsNotExist(statErr) {
		return statErr
	}
	boot, err := server.New(config)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	controlListener := device.Listen("/dev/safeagent.control")
	defer controlListener.Close()
	stop := context.AfterFunc(ctx, func() { controlListener.Close() })
	defer stop()
	control, err := controlListener.Accept()
	if err != nil {
		return err
	}
	// 镜像显式启用Bridge；旧PTY镜像不读取不存在的proxy设备。
	var tasks sync.WaitGroup
	proxyContext, stopProxy := context.WithCancel(ctx)
	defer func() { stopProxy(); tasks.Wait() }()
	var workspaceReady chan struct{}
	if config.WorkspaceRoot != nil {
		workspaceReady = make(chan struct{})
		tasks.Add(1)
		go func() {
			defer tasks.Done()
			listener := device.Listen("/dev/safeagent.workspace")
			defer listener.Close()
			ended := make(chan struct{})
			stop := context.AfterFunc(proxyContext, func() { defer close(ended); listener.Close() })
			defer func() {
				if !stop() {
					<-ended
				}
			}()
			port, openErr := listener.Accept()
			if openErr != nil {
				control.Close()
				return
			}
			if importErr := boot.ServeWorkspace(proxyContext, port); importErr != nil {
				log.Print("Workspace import ended: ", importErr)
				control.Close()
				return
			}
			close(workspaceReady)
		}()
	}
	if _, statErr := os.Stat("/etc/safeagent/proxy-enabled"); statErr == nil {
		tasks.Add(1)
		go func() {
			defer tasks.Done()
			// 导入镜像必须先完成真实发布及回执，再接纳proxy Ready和开放HTTP。
			if workspaceReady != nil {
				select {
				case <-workspaceReady:
				case <-proxyContext.Done():
					return
				}
			}
			listener := device.Listen("/dev/safeagent.proxy")
			defer listener.Close()
			done := make(chan struct{})
			stop := context.AfterFunc(proxyContext, func() { defer close(done); listener.Close() })
			defer func() {
				if !stop() {
					<-done
				}
			}()
			port, openErr := listener.Accept()
			if openErr != nil {
				control.Close()
				return
			}
			defer port.Close()
			b, bridgeErr := bridge.New(proxyContext, port, "model", func() { control.Close() })
			if bridgeErr != nil {
				control.Close()
				return
			}
			defer b.Close()
			httpListener, listenErr := net.Listen("tcp", "127.0.0.1:18080")
			if listenErr != nil {
				control.Close()
				return
			}
			// 监听器已绑定后确认Ready，Host收到确认才启动固定工作负载。
			if readyErr := proxywire.Write(port, proxywire.Frame{Kind: proxywire.Ready}, time.Now().Add(proxywire.Budget)); readyErr != nil {
				httpListener.Close()
				return
			}
			if serveErr := b.Serve(httpListener); serveErr != nil && proxyContext.Err() == nil {
				log.Print("Bridge ended: ", serveErr)
			}
		}()
	} else if !os.IsNotExist(statErr) {
		return statErr
	}
	terminal := device.Listen("/dev/safeagent.terminal")
	err = boot.Serve(ctx, control, terminal)
	stopProxy()
	// 控制断线仅撤销授权，工作负载的结束交由 Host 回收整台 VM；不在此隐式 Stop。
	if ctx.Err() == nil {
		log.Print("control connection ended; VM restart required: ", err)
		<-ctx.Done()
	}
	return err
}

// main 记录无秘密诊断，退出后禁止 init 在同 boot 自动重启服务。
func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
