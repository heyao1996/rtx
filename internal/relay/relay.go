// Package socks5 — SOCKS5 服务端处理器（RFC 1928，仅 CONNECT）。
//
// 与 cmd/agent 里手写的 socks5 客户端（RFC 1928 CONNECT，零依赖）对称：
// 客户端用于"串联出网"（agent 经上一层跳板的 SOCKS 回连控制器），
// 本包用于"提供穿透"（agent 把本地网络栈借给控制侧使用）。
//
// 关键语义 **socks5h**：ATYP=DOMAIN 时把域名原样交给注入的 dial —— 由 agent 侧的
// 解析器在内网解析，而不是在控制侧解析（否则内网域名必然解析失败）。
//
// 全标准库，保持 rtx 零第三方依赖的设计原则。
package relay

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
)

const (
	ver5       = 0x05
	mNoAuth    = 0x00
	mNoAccept  = 0xff
	cmdConnect = 0x01
	atypIPv4   = 0x01
	atypFQDN   = 0x03
	atypIPv6   = 0x04
)

// 应答码（RFC 1928 §6）
const (
	RepSucceeded        = 0x00
	RepGeneralFailure   = 0x01
	RepNotAllowed       = 0x02
	RepNetUnreachable   = 0x03
	RepHostUnreachable  = 0x04
	RepConnRefused      = 0x05
	RepTTLExpired       = 0x06
	RepCmdNotSupported  = 0x07
	RepAddrNotSupported = 0x08
)

// Dialer 由调用方注入：agent 侧传本地拨号（net.DialTimeout / 带超时包装）。
type Dialer func(network, addr string) (net.Conn, error)

// Serve 处理一条 SOCKS5 会话：方法协商 → CONNECT → 双向转发。
// logf 可为 nil；非 nil 时记录目标（便于"隧道被谁用了"的可观测性，-q 时调用方传 nil）。
func Serve(conn net.Conn, dial Dialer, logf func(string, ...any)) error {
	defer conn.Close()
	if dial == nil {
		return errors.New("relay: nil dialer")
	}
	br := bufio.NewReader(conn)

	// ---- 1. 方法协商 ----
	var hdr [2]byte
	if _, err := io.ReadFull(br, hdr[:]); err != nil {
		return fmt.Errorf("relay: read greeting: %w", err)
	}
	if hdr[0] != ver5 {
		return fmt.Errorf("relay: bad version 0x%02x", hdr[0])
	}
	methods := make([]byte, int(hdr[1]))
	if _, err := io.ReadFull(br, methods); err != nil {
		return fmt.Errorf("relay: read methods: %w", err)
	}
	ok := false
	for _, m := range methods {
		if m == mNoAuth {
			ok = true
			break
		}
	}
	if !ok {
		_, _ = conn.Write([]byte{ver5, mNoAccept})
		return errors.New("relay: client offered no supported method")
	}
	if _, err := conn.Write([]byte{ver5, mNoAuth}); err != nil {
		return fmt.Errorf("relay: write method: %w", err)
	}

	// ---- 2. 请求 ----
	var req [4]byte
	if _, err := io.ReadFull(br, req[:]); err != nil {
		return fmt.Errorf("relay: read request: %w", err)
	}
	if req[0] != ver5 {
		return fmt.Errorf("relay: bad request version 0x%02x", req[0])
	}
	if req[1] != cmdConnect {
		_ = reply(conn, RepCmdNotSupported)
		return fmt.Errorf("relay: cmd 0x%02x not supported (only CONNECT)", req[1])
	}
	host, err := readAddr(br, req[3])
	if err != nil {
		_ = reply(conn, RepAddrNotSupported)
		return err
	}
	var pb [2]byte
	if _, err := io.ReadFull(br, pb[:]); err != nil {
		return fmt.Errorf("relay: read port: %w", err)
	}
	port := binary.BigEndian.Uint16(pb[:])
	target := net.JoinHostPort(host, strconv.Itoa(int(port)))

	// ---- 3. 拨号（host 为域名时【不在此解析】= socks5h 语义）----
	rc, err := dial("tcp", target)
	if err != nil {
		_ = reply(conn, RepHostUnreachable)
		if logf != nil {
			logf("relay: dial %s failed: %v", target, err)
		}
		return fmt.Errorf("relay: dial %s: %w", target, err)
	}
	defer rc.Close()
	if logf != nil {
		logf("relay: connect %s", target)
	}

	// ---- 4. 成功应答（BND.ADDR 填 0.0.0.0:0）----
	if err := reply(conn, RepSucceeded); err != nil {
		return fmt.Errorf("relay: write reply: %w", err)
	}

	// ---- 5. 双向转发（从 br 开始，避免丢掉已缓冲的载荷）----
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(rc, br); halfClose(rc); done <- struct{}{} }()
	go func() { _, _ = io.Copy(conn, rc); halfClose(conn); done <- struct{}{} }()
	<-done
	<-done
	return nil
}

// reply 写一条应答：VER REP RSV ATYP BND.ADDR BND.PORT
func reply(conn net.Conn, code byte) error {
	_, err := conn.Write([]byte{ver5, code, 0x00, atypIPv4, 0, 0, 0, 0, 0, 0})
	return err
}

// readAddr 按 ATYP 读地址：IPv4 / FQDN / IPv6。FQDN 原样返回（不在本层解析）。
func readAddr(br *bufio.Reader, atyp byte) (string, error) {
	switch atyp {
	case atypIPv4:
		var b [4]byte
		if _, err := io.ReadFull(br, b[:]); err != nil {
			return "", fmt.Errorf("relay: read ipv4: %w", err)
		}
		return net.IP(b[:]).String(), nil
	case atypIPv6:
		var b [16]byte
		if _, err := io.ReadFull(br, b[:]); err != nil {
			return "", fmt.Errorf("relay: read ipv6: %w", err)
		}
		return net.IP(b[:]).String(), nil
	case atypFQDN:
		var l [1]byte
		if _, err := io.ReadFull(br, l[:]); err != nil {
			return "", fmt.Errorf("relay: read fqdn len: %w", err)
		}
		if l[0] == 0 {
			return "", errors.New("relay: empty fqdn")
		}
		b := make([]byte, int(l[0]))
		if _, err := io.ReadFull(br, b); err != nil {
			return "", fmt.Errorf("relay: read fqdn: %w", err)
		}
		return string(b), nil
	default:
		return "", fmt.Errorf("relay: unsupported ATYP 0x%02x", atyp)
	}
}

// halfClose 尽力半关闭（*net.TCPConn 支持），否则整体关闭。
func halfClose(c net.Conn) {
	type closeWriter interface{ CloseWrite() error }
	if cw, ok := c.(closeWriter); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = c.Close()
}
