// Package ws — 极简 WebSocket（RFC 6455）binary 帧实现，全标准库。
// 用于 agent 以 ws:// / wss:// 自包含方式反连 server（穿透 HTTP 白名单/DPI），
// 免去在目标机额外部署隧道工具。加密走 wss（TLS），不做应用层加密。
//
// 能力：客户端握手 + 服务端握手；binary 帧读写（客户端帧掩码、服务端解掩码）；
// 单消息模式（Read 返回一个完整 frame 的 payload，Write 发一个 binary frame）。
package ws

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
)

const guid = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// Conn 包装底层连接为 binary frame 消息流。
type Conn struct {
	conn net.Conn
	br   *bufio.Reader
	// 当前帧剩余 payload 字节（未读完时续读）
	remain int
}

func (c *Conn) Close() error { return c.conn.Close() }

// Dial 建立 ws:// 或 wss:// 连接（wss 时带 tls 配置）。
func Dial(rawurl string, tlsCfg *tls.Config, path string) (*Conn, error) {
	u, err := url.Parse(rawurl)
	if err != nil {
		return nil, err
	}
	host := u.Host
	if !strings.Contains(host, ":") {
		if u.Scheme == "wss" {
			host += ":443"
		} else {
			host += ":80"
		}
	}
	var nc net.Conn
	if u.Scheme == "wss" {
		nc, err = tls.Dial("tcp", host, tlsCfg)
	} else {
		nc, err = net.Dial("tcp", host)
	}
	if err != nil {
		return nil, err
	}
	if path == "" {
		path = "/"
	}
	key := make([]byte, 16)
	_, _ = rand.Read(key)
	keyB64 := base64.StdEncoding.EncodeToString(key)
	req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n",
		path, u.Host, keyB64)
	if _, err := nc.Write([]byte(req)); err != nil {
		nc.Close()
		return nil, err
	}
	br := bufio.NewReader(nc)
	// 读状态行 + 头
	status, err := br.ReadString('\n')
	if err != nil {
		nc.Close()
		return nil, err
	}
	if !strings.Contains(status, " 101 ") {
		nc.Close()
		return nil, fmt.Errorf("ws handshake: %s", strings.TrimSpace(status))
	}
	accept := ""
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			nc.Close()
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if strings.HasPrefix(strings.ToLower(line), "sec-websocket-accept:") {
			accept = strings.TrimSpace(strings.SplitN(line, ":", 2)[1])
		}
	}
	expect := acceptKey(keyB64)
	if accept != expect {
		nc.Close()
		return nil, errors.New("ws handshake: bad accept")
	}
	return &Conn{conn: nc, br: br}, nil
}

func acceptKey(key string) string {
	h := sha1.Sum([]byte(key + guid))
	return base64.StdEncoding.EncodeToString(h[:])
}

// Server 服务端握手：从已建立的连接（明文或 TLS）读 Upgrade 请求，返回 ws.Conn。
func Server(conn net.Conn) (*Conn, error) {
	br := bufio.NewReader(conn)
	req, err := http.ReadRequest(br)
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(req.Header.Get("Upgrade"), "websocket") {
		return nil, errors.New("not a websocket upgrade")
	}
	key := req.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		return nil, errors.New("missing Sec-WebSocket-Key")
	}
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + acceptKey(key) + "\r\n\r\n"
	if _, err := conn.Write([]byte(resp)); err != nil {
		return nil, err
	}
	return &Conn{conn: conn, br: br}, nil
}

// ReadFrame 读一个完整 binary/text frame 的 payload。
func (c *Conn) ReadFrame() ([]byte, error) {
	// 若上一帧没读完，先丢弃剩余（简单模型：单帧小消息）
	if c.remain > 0 {
		_, _ = io.CopyN(io.Discard, c.br, int64(c.remain))
		c.remain = 0
	}
	var h [2]byte
	if _, err := io.ReadFull(c.br, h[:]); err != nil {
		return nil, err
	}
	fin := h[0] & 0x80
	opcode := h[0] & 0x0f
	masked := h[1]&0x80 != 0
	length := uint64(h[1] & 0x7f)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return nil, err
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return nil, err
		}
		length = binary.BigEndian.Uint64(ext[:])
	}
	// 控制帧（ping/pong/close）简短处理
	if opcode >= 0x8 {
		if masked {
			var mk [4]byte
			if _, err := io.ReadFull(c.br, mk[:]); err != nil {
				return nil, err
			}
		}
		_, _ = io.CopyN(io.Discard, c.br, int64(length))
		// ping 回 pong；close 返回错误结束
		if opcode == 0x8 {
			return nil, io.EOF
		}
		return c.ReadFrame()
	}
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(c.br, mask[:]); err != nil {
			return nil, err
		}
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(c.br, payload); err != nil {
		return nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	_ = fin
	return payload, nil
}

// WriteFrame 发一个 binary 帧。isClient=true 时加掩码（客户端帧必须掩码）。
func (c *Conn) WriteFrame(payload []byte, isClient bool) error {
	n := len(payload)
	var hdr []byte
	if n < 126 {
		hdr = []byte{0x82, byte(n)}
	} else if n < 65536 {
		hdr = []byte{0x82, 126, byte(n >> 8), byte(n & 0xff)}
	} else {
		hdr = []byte{0x82, 127,
			byte(n >> 56), byte(n >> 48), byte(n >> 40), byte(n >> 32),
			byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}
	}
	if isClient {
		hdr[1] |= 0x80
	}
	if _, err := c.conn.Write(hdr); err != nil {
		return err
	}
	if isClient {
		var mk [4]byte
		_, _ = rand.Read(mk[:])
		if _, err := c.conn.Write(mk[:]); err != nil {
			return err
		}
		out := make([]byte, n)
		for i := range payload {
			out[i] = payload[i] ^ mk[i%4]
		}
		_, err := c.conn.Write(out)
		return err
	}
	_, err := c.conn.Write(payload)
	return err
}
