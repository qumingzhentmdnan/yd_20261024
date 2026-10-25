package runner

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"testing"

	"safeagent/internal/protocol"
)

// TestGroupProcessChurn 验证扫描所有进程时，无关短命进程的退出不会使组查询失败。
func TestGroupProcessChurn(t *testing.T) {
	var workers sync.WaitGroup
	for range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for range 500 {
				if err := exec.Command("/bin/true").Run(); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	defer workers.Wait()
	for range 1000 {
		// Linux PID 上限小于此值；每次构造新观察者，避免组空缓存跳过后续扫描。
		p := &Process{cmd: &exec.Cmd{Process: &os.Process{Pid: 2147483647}}}
		state, err := p.Group()
		if err != nil || state != protocol.GroupStateEmpty {
			t.Fatalf("Group: %s, %T: %+v", state, err, err)
		}
	}
}

// TestProcessInGroupAfterReap 验证 proc stat 已打开但目标已回收时的真实 ESRCH 读取竞态。
func TestProcessInGroupAfterReap(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "read line")
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { input.Close(); cmd.Process.Kill(); cmd.Wait() }()
	file, err := os.Open(fmt.Sprintf("/proc/%d/stat", cmd.Process.Pid))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	input.Close()
	if err = cmd.Wait(); err == nil {
		t.Fatal("预期 read 遇到 EOF")
	}
	// 通过 fd 路径重新打开同一 proc inode，固定在 open 与 read 之间回收的内核状态。
	path := fmt.Sprintf("/proc/self/fd/%d", file.Fd())
	if _, err = os.ReadFile(path); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("预期 ESRCH，实际 %v", err)
	}
	running, err := processInGroup(path, strconv.Itoa(cmd.Process.Pid))
	if err != nil || running {
		t.Fatalf("已回收进程: running=%v err=%v", running, err)
	}
}

// TestProcessInGroupErrors 验证只有消失的进程被忽略，空文件、短 stat 和资源读取错误仍明确失败。
func TestProcessInGroupErrors(t *testing.T) {
	dir := t.TempDir()
	for _, data := range []string{"", "1 (name) S", "1 (name) S 1 42", "1 (name) Z 1 42", "1 (name) X 1 42"} {
		path := filepath.Join(dir, "stat")
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		running, err := processInGroup(path, "42")
		if data == "" || data == "1 (name) S" {
			if err == nil {
				t.Fatalf("格式错误被忽略: %q", data)
			}
		} else if err != nil || running != (data == "1 (name) S 1 42") {
			t.Fatalf("stat=%q running=%v err=%v", data, running, err)
		}
	}
	if _, err := processInGroup(dir, "42"); !errors.Is(err, syscall.EISDIR) {
		t.Fatalf("读取错误被改写: %v", err)
	}
	if running, err := processInGroup(filepath.Join(dir, "missing"), "42"); err != nil || running {
		t.Fatalf("已消失路径: %v %v", running, err)
	}
}
