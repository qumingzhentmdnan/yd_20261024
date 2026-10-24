// Command guest-http-probe 只访问镜像固定回环Bridge，不模拟实际Agent CLI。
package main

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// run 以非root固定输入验证首块、二进制和HTTP并发资格。
func run() error {
	fmt.Printf("HTTP_UID:%d\n", os.Getuid())
	client := &http.Client{Timeout: 15 * time.Second}
	mode := ""
	if len(os.Args) == 2 {
		mode = os.Args[1]
	}
	if mode == "total" {
		client.Timeout = 75 * time.Second
	}
	if mode == "upload" {
		c, err := net.Dial("tcp", "127.0.0.1:18080")
		if err != nil {
			return err
		}
		c.SetDeadline(time.Now().Add(4 * time.Second))
		if _, err = io.WriteString(c, "POST /v1/responses HTTP/1.1\r\nHost: loopback\r\nContent-Length: 100\r\n\r\nx"); err != nil {
			c.Close()
			return err
		}
		// 只验证有界结束；超时上传可能返回400或HTTP传输EOF。
		_, readErr := io.ReadAll(c)
		c.Close()
		if e, ok := readErr.(net.Error); ok && e.Timeout() {
			return fmt.Errorf("上传未有界结束: %w", readErr)
		}
		fmt.Println("HTTP_UPLOAD_TIMED_OUT")
	}
	request, err := http.NewRequest("POST", "http://127.0.0.1:18080/v1/responses", bytes.NewBufferString("guest-actual-body"))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "guest-spoof")
	request.Header.Set("X-Api-Key", "guest-spoof")
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return fmt.Errorf("status %d", response.StatusCode)
	}
	first := make([]byte, 5)
	if _, err = io.ReadFull(response.Body, first); err != nil {
		return err
	}
	if !bytes.Equal(first, []byte{0, 255, 10, 13, 128}) {
		return fmt.Errorf("binary first chunk %x", first)
	}
	if response.Header.Get("Authorization") != "" {
		return fmt.Errorf("credential response leaked")
	}
	fmt.Println("HTTP_FIRST_BINARY")
	if mode == "slow" {
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, syscall.SIGTERM)
		defer signal.Stop(signals)
		// 保持真实下游不消费，等待Host写入预算和VM回收，不把关闭当正常EOF。
		second, err := client.Post("http://127.0.0.1:18080/v1/responses", "application/json", bytes.NewBufferString("second"))
		if err == nil {
			second.Body.Close()
			if second.StatusCode != 503 {
				return fmt.Errorf("concurrent status %d", second.StatusCode)
			}
		}
		if err != nil {
			return err
		}
		fmt.Println("HTTP_SLOW_READER")
		<-signals
		if err := os.WriteFile("/tmp/http-stop-result.tmp", []byte("HTTP_STOP_TERM\n"), 0600); err != nil {
			return err
		}
		if err := os.Rename("/tmp/http-stop-result.tmp", "/tmp/http-stop-result"); err != nil {
			return err
		}
		return nil
	}
	if mode == "cancel" {
		response.Body.Close()
		fmt.Println("HTTP_CLIENT_CLOSED")
		return nil
	}
	second, err := client.Post("http://127.0.0.1:18080/v1/responses", "application/json", bytes.NewBufferString("second"))
	if err != nil {
		return err
	}
	second.Body.Close()
	if second.StatusCode != 503 {
		return fmt.Errorf("concurrent status %d", second.StatusCode)
	}
	fmt.Println("HTTP_SECOND_503")
	tail, err := io.ReadAll(response.Body)
	if mode == "abort" || mode == "total" {
		if err == nil {
			return fmt.Errorf("异常响应被当成成功EOF")
		}
		fmt.Println("HTTP_ABORT_ERROR")
		if err := os.WriteFile("/tmp/http-abort-result.tmp", []byte("HTTP_ABORT_ERROR\n"), 0600); err != nil {
			return err
		}
		if err := os.Rename("/tmp/http-abort-result.tmp", "/tmp/http-abort-result"); err != nil {
			return err
		}
		return nil
	}
	if err != nil {
		return err
	}
	if string(tail) != "tail" {
		return fmt.Errorf("tail %q", tail)
	}
	fmt.Println("HTTP_DONE")
	return nil
}

// main 输出固定验收标记，不读取Host凭据或动态目标。
func main() {
	if err := run(); err != nil {
		fmt.Println("HTTP_ERROR:", err)
		os.Exit(1)
	}
}
