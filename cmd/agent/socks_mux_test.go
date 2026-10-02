package main

import (
	"bufio"
	"bytes"
	"flag"
	"io"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"coreutil/internal/link"
	"coreutil/internal/proto"
)

// ---- 脚手架：一对对接的消息链路（模拟 agent ↔ 控制器）----

type tLink struct {
	out    chan *proto.Msg
	in     chan *proto.Msg
	closed chan struct{}
}

func (l *tLink) Send(m *proto.Msg) error {
	select {
	case l.out <- m:
		return nil
	case <-l.closed:
		return io.EOF
	}
}

func (l *tLink) Recv() (*proto.Msg, error) {
	m, ok := <-l.in
	if !ok {
		return nil, io.EOF
	}
	return m, nil
}

func (l *tLink) Close() error {
	select {
	case <-l.closed:
	default:
		close(l.closed)
	}
	return nil
}

func pair() (*tLink, *tLink) {
	mk := func() *tLink {
		return &tLink{out: make(chan *proto.Msg, 4096), in: make(chan *proto.Msg, 4096), closed: make(chan struct{})}
	}
	a, b := mk(), mk()
	fwd := func(from, to *tLink) {
		for {
			select {
			case m := <-from.out:
				select {
				case to.in <- m:
				case <-to.closed:
					return
				}
			case <-from.closed:
				return
			}
		}
	}
	go fwd(a, b)
	go fwd(b, a)
	return a, b
}

func echoTarget(t *testing.T) (string, func()) {
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

// TestSocksOverMuxEndToEnd —— 完整链路：
//
//	控制侧 mux 通道 → yamux 流 → agent 侧 relay.Serve → 目标
//
// 并验证 socks5h（域名为 ATYP=DOMAIN，必须由 agent 侧解析）。
func TestSocksOverMuxEndToEnd(t *testing.T) {
	_ = flag.Set("i", "test-agent")
	_ = flag.Set("t", "test-token")
	_ = flag.Set("q", "true") // 静默，避免测试输出噪音

	target, stop := echoTarget(t)
	defer stop()
	_, portStr, _ := net.SplitHostPort(target)
	port, _ := strconv.Atoi(portStr)

	agentL, ctrlL := pair()
	go handleConn(agentL) // agent 侧真实代码路径

	hub := link.NewHub(ctrlL.Send)
	results := make(chan *proto.Msg, 8)
	go func() {
		for {
			m, err := ctrlL.Recv()
			if err != nil {
				return
			}
			if hub.Handle(m) {
				continue
			}
			if m.Type == proto.MsgResult {
				select {
				case results <- m:
				default:
				}
			}
		}
	}()

	conn, err := hub.Dial(0)
	if err != nil {
		t.Fatal(err)
	}
	cfg := yamux.DefaultConfig()
	cfg.LogOutput = io.Discard
	cfg.EnableKeepAlive = false
	sess, err := yamux.Client(conn, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	// 让 agent 在这个 mux 通道上开 SOCKS5 服务
	if err := ctrlL.Send(&proto.Msg{Type: proto.MsgTask, TaskID: "t1", Task: proto.TaskSocks, MuxID: 0}); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-results:
		if !r.OK {
			t.Fatalf("TaskSocks 失败: %s", r.Err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("TaskSocks 无结果返回")
	}

	// 一条 yamux 流 = 一次 SOCKS5 会话
	st, err := sess.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	br := bufio.NewReader(st)

	chk := func(want []byte, what string) {
		got := make([]byte, len(want))
		if _, err := io.ReadFull(br, got); err != nil {
			t.Fatalf("%s: 读失败 %v", what, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("%s: % x != % x", what, got, want)
		}
	}

	if _, err := st.Write([]byte{5, 1, 0}); err != nil {
		t.Fatal(err)
	}
	chk([]byte{5, 0}, "方法协商")

	req := append([]byte{5, 1, 0, 3, byte(len("localhost"))}, []byte("localhost")...)
	req = append(req, byte(port>>8), byte(port&0xff))
	if _, err := st.Write(req); err != nil {
		t.Fatal(err)
	}
	chk([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}, "CONNECT 应答")

	// 小回环
	msg := []byte("ping-through-mux-and-socks")
	if _, err := st.Write(msg); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(br, got); err != nil {
		t.Fatalf("回环读失败: %v", err)
	}
	if !bytes.Equal(got, msg) {
		t.Fatalf("回环内容 %q", got)
	}

	// 大回环（300KB，触发多路分块）
	blob := bytes.Repeat([]byte("Z"), 300<<10)
	go func() { _, _ = st.Write(blob) }()
	gotBig := make([]byte, len(blob))
	if _, err := io.ReadFull(br, gotBig); err != nil {
		t.Fatalf("300KB 回环读失败: %v", err)
	}
	if !bytes.Equal(gotBig, blob) {
		t.Fatal("300KB 经 mux+socks 回环内容不一致")
	}
	t.Logf("端到端通过：TaskSocks → yamux 流 → relay.Serve → %s；300KB 回环一致", target)
}
