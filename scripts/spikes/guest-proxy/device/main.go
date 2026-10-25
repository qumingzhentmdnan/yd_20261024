// Command deviceprobe 验证真实virtio设备的截止时间与并发关闭。
package main

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"net"
	"time"

	"safeagent/internal/guest/device"
)

// accept 等待Host连接当前固定设备。
func accept(name string) net.Conn {
	listener := device.Listen("/dev/safeagent." + name)
	defer listener.Close()
	c, err := listener.Accept()
	if err != nil {
		log.Fatal(err)
	}
	return c
}

// main 串行处理探针命令，所有设备写者均显式等待退出。
func main() {
	control := accept("control")
	defer control.Close()
	proxy := accept("proxy")
	defer proxy.Close()
	go func() { terminal := accept("terminal"); defer terminal.Close(); io.Copy(terminal, terminal) }()
	scanner := bufio.NewScanner(control)
	var done chan error
	for scanner.Scan() {
		switch scanner.Text() {
		case "ping":
			fmt.Fprintln(control, "pong")
		case "terminal":
			fmt.Fprintln(control, "TERMINAL_STARTED")
		case "echo":
			data := make([]byte, 65536)
			if _, err := io.ReadFull(proxy, data); err != nil {
				log.Fatal(err)
			}
			if err := writeAll(proxy, data); err != nil {
				log.Fatal(err)
			}
			fmt.Fprintln(control, "ECHO_DONE")
		case "read-deadline":
			proxy.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
			_, err := proxy.Read(make([]byte, 1))
			if e, ok := err.(net.Error); !ok || !e.Timeout() {
				log.Fatal("读取截止时间失效: ", err)
			}
			proxy.SetReadDeadline(time.Time{})
			fmt.Fprintln(control, "READ_DEADLINE")
		case "flood":
			done = make(chan error, 1)
			go func() {
				block := make([]byte, 16384)
				for i := 0; i < 4096; i++ {
					if err := writeAll(proxy, block); err != nil {
						done <- err
						return
					}
				}
				done <- nil
			}()
			fmt.Fprintln(control, "FLOOD_STARTED")
		case "cancel":
			if err := proxy.Close(); err != nil {
				log.Fatal(err)
			}
			select {
			case err := <-done:
				if err == nil {
					log.Fatal("未观察到阻塞写取消")
				}
				fmt.Fprintln(control, "CANCELED_CLOSED")
			case <-time.After(time.Second):
				log.Fatal("设备Close未解除写等待")
			}
		}
	}
}

// writeAll 处理真实字符设备允许的短写，直到完整块或明确错误。
func writeAll(c net.Conn, data []byte) error {
	for len(data) > 0 {
		n, err := c.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}
