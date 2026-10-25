// Command guest-isolation 只在验收项目中运行，使用假凭据检查真实 Guest 的网络与文件边界。
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// configuration 只保存专门的宿主假秘密路径和假凭据摘要，不包含真实凭据。
type configuration struct {
	// HostSecret 指向仅在宿主创建且未导入的测试文件。
	HostSecret string `json:"hostSecret"`
	// KeyDigest 是宿主生成的假凭据 SHA256，Guest 不预先知道凭据内容。
	KeyDigest string `json:"keyDigest"`
}

// result 保存固定边界的检查结果，不能解释为完整的逃逸或秘密扫描认证。
type result struct {
	// UID 是生产 Runner 实际设置的用户编号。
	UID int `json:"uid"`
	// Checks 仅记录已完成的断言。
	Checks []string `json:"checks"`
	// FilesScanned 是实际读取的普通配置文件数。
	FilesScanned int `json:"filesScanned"`
	// ProcessFilesScanned 是实际读取的进程环境或参数文件数。
	ProcessFilesScanned int `json:"processFilesScanned"`
}

// main 输出有明确前缀的最终 JSON，任何实际失败返回非零。
func main() {
	r, err := run()
	if err != nil {
		fmt.Println("ISOLATION_FAILED:", err)
		os.Exit(1)
	}
	data, err := json.Marshal(r)
	if err != nil {
		fmt.Println("ISOLATION_FAILED:", err)
		os.Exit(1)
	}
	fmt.Printf("ISOLATION_PASSED:%s\n", data)
}

// run 在真实 CLI 的子进程中验证固定目标，不读取宿主既有凭据。
func run() (result, error) {
	r := result{UID: os.Getuid(), Checks: []string{}}
	if r.UID != 1000 || os.Getgid() != 1000 {
		return r, fmt.Errorf("验收必须由 UID/GID 1000 运行")
	}
	if len(os.Args) == 2 && os.Args[1] == "deny" {
		fmt.Println("ISOLATION_READY_FOR_DENIAL")
		client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil}}
		response, err := client.Post("http://127.0.0.1:18080/v1/unauthorized", "application/json", strings.NewReader(`{}`))
		if err != nil {
			return r, fmt.Errorf("未授权请求已中断，Host 须独立确认拒绝与撤销: %w", err)
		}
		response.Body.Close()
		return r, fmt.Errorf("预期撤销代理，但收到 HTTP %d", response.StatusCode)
	}
	data, err := os.ReadFile("acceptance.json")
	if err != nil {
		return r, err
	}
	var c configuration
	if err = json.Unmarshal(data, &c); err != nil {
		return r, err
	}
	digest, err := hex.DecodeString(c.KeyDigest)
	if err != nil || len(digest) != sha256.Size || !filepath.IsAbs(c.HostSecret) {
		return r, fmt.Errorf("验收配置无效")
	}
	if _, err := os.Stat(c.HostSecret); !errors.Is(err, fs.ErrNotExist) {
		return r, fmt.Errorf("宿主假秘密路径并未隔离: %v", err)
	}
	r.Checks = append(r.Checks, "host_fake_secret_absent")
	interfaces, err := net.Interfaces()
	if err != nil {
		return r, err
	}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagLoopback == 0 {
			return r, fmt.Errorf("发现非 loopback 网卡: %s", iface.Name)
		}
	}
	r.Checks = append(r.Checks, "no_generic_network_interface")
	// 要求内核明确报告无路由，不把远端未响应或测试超时当作隔离成功。
	for _, target := range []struct {
		// network 指定固定 IPv4 或 IPv6 的 TCP/UDP 协议。
		network string
		// address 只使用文档保留地址，避免访问任何真实服务。
		address string
	}{
		{"tcp4", "198.51.100.1:443"}, {"tcp6", "[2001:db8::1]:443"},
		{"udp4", "198.51.100.1:53"}, {"udp6", "[2001:db8::1]:53"},
	} {
		conn, err := (&net.Dialer{Timeout: time.Second}).Dial(target.network, target.address)
		if err == nil {
			conn.Close()
			return r, fmt.Errorf("未拒绝 %s", target.network)
		}
		if !errors.Is(err, syscall.ENETUNREACH) {
			return r, fmt.Errorf("%s 未获得预期的无路由拒绝: %w", target.network, err)
		}
		r.Checks = append(r.Checks, target.network+"_no_route")
	}
	var dnsNoRoute atomic.Int64
	var dnsOther atomic.Int64
	resolver := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, "udp4", "198.51.100.53:53")
		if errors.Is(err, syscall.ENETUNREACH) {
			dnsNoRoute.Add(1)
		} else {
			dnsOther.Add(1)
		}
		return conn, err
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := resolver.LookupHost(ctx, "safeagent-acceptance.invalid"); err == nil || dnsNoRoute.Load() == 0 || dnsOther.Load() != 0 {
		return r, fmt.Errorf("DNS 未获得预期的无路由拒绝: %v", err)
	}
	r.Checks = append(r.Checks, "dns_no_route")
	pattern := regexp.MustCompile(`FAKE_HOST_[a-f0-9]{32}`)
	containsKey := func(data []byte) bool {
		for _, candidate := range pattern.FindAll(data, -1) {
			sum := sha256.Sum256(candidate)
			if hex.EncodeToString(sum[:]) == c.KeyDigest {
				return true
			}
		}
		return false
	}
	if containsKey([]byte(strings.Join(os.Environ(), "\n"))) {
		return r, fmt.Errorf("当前进程环境泄露宿主假凭据")
	}
	// 只扫描明确的小型配置目录；不声称扫描全部磁盘、内核内存或不可读进程。
	for _, root := range []string{"/home/agent", "/workspace/project", "/etc/safeagent"} {
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
			if errors.Is(walkErr, fs.ErrPermission) {
				return nil
			}
			if walkErr != nil {
				return walkErr
			}
			if !entry.Type().IsRegular() {
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if info.Size() > 1<<20 {
				return nil
			}
			data, err := os.ReadFile(path)
			if errors.Is(err, fs.ErrPermission) {
				return nil
			}
			if err != nil {
				return err
			}
			r.FilesScanned++
			if containsKey(data) {
				return fmt.Errorf("Guest 配置文件泄露宿主假凭据")
			}
			return nil
		})
		if err != nil {
			return r, err
		}
	}
	processes, err := os.ReadDir("/proc")
	if err != nil {
		return r, err
	}
	for _, process := range processes {
		if _, err := strconv.Atoi(process.Name()); err != nil {
			continue
		}
		for _, name := range []string{"cmdline", "environ"} {
			data, err := os.ReadFile(filepath.Join("/proc", process.Name(), name))
			if errors.Is(err, fs.ErrPermission) || errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
				continue
			}
			if err != nil {
				return r, err
			}
			r.ProcessFilesScanned++
			if containsKey(data) {
				return r, fmt.Errorf("Guest 进程泄露宿主假凭据")
			}
		}
	}
	r.Checks = append(r.Checks, "fake_credential_absent_from_readable_config_and_processes")
	return r, nil
}
