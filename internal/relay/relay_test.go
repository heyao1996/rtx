package relay

import (
	"bufio"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 起一个 echo 服务当"内网目标"
func startEcho(t *testing.T) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(c, c); _ = c.Close() }()
		}
	}()
	return ln.Addr().String(), func() { _ = ln.Close() }
}

// 起一个中继，返回地址与"dial 收到过哪些 target"的通道
func startRelay(t *testing.T, seen chan string) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				_ = Serve(c, func(network, addr string) (net.Conn, error) {
					select {
					case seen <- addr:
					default:
					}
					return net.DialTimeout(network, addr, 3*time.Second)
				}, nil)
			}()
		}
	}()
	return ln.Addr().String(), func() { _ = ln.Close() }
}

func expect(t *testing.T, br *bufio.Reader, want []byte, what string) {
	t.Helper()
	got := make([]byte, len(want))
	if _, err := io.ReadFull(br, got); err != nil {
		t.Fatalf("%s: 读应答失败: %v", what, err)
	}
	if string(got) != string(want) {
		t.Fatalf("%s: 应答 % x，期望 % x", what, got, want)
	}
}

// TestConnectFQDNRemoteResolve —— 核心语义：ATYP=DOMAIN 必须【原样】交给 dial（socks5h）
func TestConnectFQDNRemoteResolve(t *testing.T) {
	target, stopEcho := startEcho(t)
	defer stopEcho()
	_, portStr, _ := net.SplitHostPort(target)
	port, _ := strconv.Atoi(portStr)

	seen := make(chan string, 4)
	proxy, stopProxy := startRelay(t, seen)
	defer stopProxy()

	c, err := net.Dial("tcp", proxy)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	br := bufio.NewReader(c)

	if _, err := c.Write([]byte{5, 1, 0}); err != nil {
		t.Fatal(err)
	}
	expect(t, br, []byte{5, 0}, "方法协商")

	// CONNECT localhost:<port>，ATYP=0x03(FQDN)
	req := append([]byte{5, 1, 0, 3, byte(len("localhost"))}, []byte("localhost")...)
	req = append(req, byte(port>>8), byte(port&0xff))
	if _, err := c.Write(req); err != nil {
		t.Fatal(err)
	}
	expect(t, br, []byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}, "CONNECT 应答")

	// 双向透传
	if _, err := c.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(br, buf); err != nil {
		t.Fatalf("回环读失败: %v", err)
	}
	if string(buf) != "hello" {
		t.Fatalf("回环内容 %q，期望 hello", buf)
	}

	select {
	case addr := <-seen:
		if !strings.HasPrefix(addr, "localhost:") {
			t.Fatalf("socks5h 语义失败：dial 收到 %q —— 域名被本层解析了，必须原样下发", addr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("dial 未被调用")
	}
}

// TestRejectNonConnect —— BIND / UDP ASSOCIATE 必须回 0x07，不得静默
func TestRejectNonConnect(t *testing.T) {
	seen := make(chan string, 1)
	proxy, stop := startRelay(t, seen)
	defer stop()

	c, err := net.Dial("tcp", proxy)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	br := bufio.NewReader(c)
	if _, err := c.Write([]byte{5, 1, 0}); err != nil {
		t.Fatal(err)
	}
	expect(t, br, []byte{5, 0}, "方法协商")
	// CMD=0x02(BIND)
	if _, err := c.Write([]byte{5, 2, 0, 1, 127, 0, 0, 1, 0, 80}); err != nil {
		t.Fatal(err)
	}
	expect(t, br, []byte{5, RepCmdNotSupported, 0, 1, 0, 0, 0, 0, 0, 0}, "非 CONNECT 拒绝码")
	select {
	case addr := <-seen:
		t.Fatalf("非 CONNECT 请求不应触发 dial，却拨了 %s", addr)
	default:
	}
}
