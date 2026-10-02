package link

import (
	"bytes"
	"crypto/md5"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"coreutil/internal/proto"
)

// ---- 测试脚手架：一对对接的消息链路 + 各自的【唯一读循环】----

type testLink struct {
	out chan *proto.Msg
	in  chan *proto.Msg
}

func (l *testLink) Send(m *proto.Msg) error { l.out <- m; return nil }
func (l *testLink) Recv() (*proto.Msg, error) {
	m, ok := <-l.in
	if !ok {
		return nil, io.EOF
	}
	return m, nil
}

func newLinkPair() (*testLink, *testLink) {
	a := &testLink{out: make(chan *proto.Msg, 8192), in: make(chan *proto.Msg, 8192)}
	b := &testLink{out: make(chan *proto.Msg, 8192), in: make(chan *proto.Msg, 8192)}
	go func() {
		for m := range a.out {
			b.in <- m
		}
	}()
	go func() {
		for m := range b.out {
			a.in <- m
		}
	}()
	return a, b
}

// runLoop 模拟 agent/server 的读循环：全部消息进 Hub，非 mux 的交给 control
func runLoop(l *testLink, h *Hub, control func(*proto.Msg)) {
	go func() {
		for {
			m, err := l.Recv()
			if err != nil {
				return
			}
			if h.Handle(m) {
				continue
			}
			if control != nil {
				control(m)
			}
		}
	}()
}

func yamuxCfg() *yamux.Config {
	c := yamux.DefaultConfig()
	c.LogOutput = io.Discard
	c.EnableKeepAlive = false
	return c
}

// ---- ① 通道基本语义：往返 + 关闭后 EOF ----

func TestMuxConnRoundTripAndEOF(t *testing.T) {
	a, b := newLinkPair()
	ha, hb := NewHub(a.Send), NewHub(b.Send)
	runLoop(a, ha, nil)
	runLoop(b, hb, nil)

	srvConn := make(chan *Conn, 1)
	go func() {
		c, err := hb.Accept()
		if err == nil {
			srvConn <- c
		}
	}()
	cli, err := ha.Dial(0)
	if err != nil {
		t.Fatal(err)
	}
	var srv *Conn
	select {
	case srv = <-srvConn:
	case <-time.After(3 * time.Second):
		t.Fatal("对端未 Accept 到通道")
	}

	payload := bytes.Repeat([]byte("rtx-mux-payload-"), 65536) // 1 MB
	want := md5.Sum(payload)

	go func() { _, _ = cli.Write(payload) }()
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(srv, got); err != nil {
		t.Fatalf("读失败: %v", err)
	}
	if md5.Sum(got) != want {
		t.Fatal("1MB 透传内容不一致")
	}

	if err := cli.Close(); err != nil {
		t.Fatal(err)
	}
	srv.SetReadDeadline(time.Now()) // 无操作，仅占位；真正的等待靠超时 goroutine
	done := make(chan error, 1)
	go func() {
		buf := make([]byte, 16)
		_, err := srv.Read(buf)
		done <- err
	}()
	select {
	case err := <-done:
		if err != io.EOF {
			t.Fatalf("对端关闭后应得 io.EOF，实得 %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("对端关闭后 Read 未返回")
	}
}

// ---- ② 核心回归：yamux 流满载时，控制消息不得被拖死（用户最初担心的问题）----

// controlProbe 发 n 条控制消息并测 RTT。
// ⛔ 关键：回包由【读循环的控制回调】投递到 echoCh，探针绝不直接读 l.in
// （否则与读循环双重消费同一 channel —— 实测会挂死）。
func controlProbe(a *testLink, echoCh <-chan struct{}, n int, timeout time.Duration) []time.Duration {
	var rtts []time.Duration
	for i := 0; i < n; i++ {
		start := time.Now()
		if err := a.Send(&proto.Msg{Type: "ctl", Data: "ping"}); err != nil {
			return rtts
		}
		select {
		case <-echoCh:
			rtts = append(rtts, time.Since(start))
		case <-time.After(timeout):
			return rtts // 超时即视为被拖死，由调用方断言
		}
	}
	return rtts
}

func TestControlNotStarvedWhileStreaming(t *testing.T) {
	a, b := newLinkPair()
	ha, hb := NewHub(a.Send), NewHub(b.Send)
	echoCh := make(chan struct{}, 256)
	runLoop(a, ha, func(m *proto.Msg) {
		if m.Type == "ctl-echo" {
			select {
			case echoCh <- struct{}{}:
			default:
			}
		}
	})
	runLoop(b, hb, func(m *proto.Msg) {
		if m.Type == "ctl" {
			_ = b.Send(&proto.Msg{Type: "ctl-echo"})
		}
	})

	srvCh := make(chan *Conn, 1)
	go func() {
		if c, err := hb.Accept(); err == nil {
			srvCh <- c
		}
	}()
	cli, err := ha.Dial(0)
	if err != nil {
		t.Fatal(err)
	}
	srv := <-srvCh
	cliSess, err := yamux.Client(cli, yamuxCfg())
	if err != nil {
		t.Fatal(err)
	}
	srvSess, err := yamux.Server(srv, yamuxCfg())
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			st, err := srvSess.AcceptStream()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(st, st); _ = st.Close() }()
		}
	}()

	idle := controlProbe(a, echoCh, 10, 2*time.Second)
	if len(idle) != 10 {
		t.Fatalf("空载控制消息就不齐: %d/10", len(idle))
	}

	stop := make(chan struct{})
	var sent int64
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		st, err := cliSess.Open()
		if err != nil {
			return
		}
		defer st.Close()
		go func() { _, _ = io.Copy(io.Discard, st) }()
		chunk := bytes.Repeat([]byte("X"), 16<<10)
		for {
			select {
			case <-stop:
				return
			default:
			}
			n, err := st.Write(chunk)
			if err != nil {
				return
			}
			atomic.AddInt64(&sent, int64(n))
		}
	}()
	time.Sleep(150 * time.Millisecond)

	loaded := controlProbe(a, echoCh, 20, 3*time.Second)
	close(stop)
	wg.Wait()

	maxOf := func(d []time.Duration) time.Duration {
		var m time.Duration
		for _, x := range d {
			if x > m {
				m = x
			}
		}
		return m
	}
	mb := atomic.LoadInt64(&sent) >> 20
	t.Logf("控制消息 RTT: 空载 max=%v | 满载(yamux 流进行中已推 %d MB) max=%v | 样本=%d/%d",
		maxOf(idle), mb, maxOf(loaded), len(idle), len(loaded))
	if mb < 1 {
		t.Fatalf("满载不成立: 期间只推了 %d MB", mb)
	}
	if len(loaded) != 20 {
		t.Fatalf("满载时控制消息被拖死: 只有 %d/20 返回", len(loaded))
	}
	if m := maxOf(loaded); m > 100*time.Millisecond {
		t.Fatalf("满载时控制消息延迟过高: max=%v (判据 100ms)", m)
	}
}

// ---- ③ 溢出必须显式失败（不静默丢数据、不反压读循环）----

func TestOverflowFailsLoudly(t *testing.T) {
	a, b := newLinkPair()
	ha, hb := NewHub(a.Send), NewHub(b.Send)
	runLoop(a, ha, nil)
	runLoop(b, hb, nil)

	srvCh := make(chan *Conn, 1)
	go func() {
		if c, err := hb.Accept(); err == nil {
			srvCh <- c
		}
	}()
	cli, err := ha.Dial(0)
	if err != nil {
		t.Fatal(err)
	}
	srv := <-srvCh

	// ⛔ 先写满、且【完全不读】，才可能真正触发溢出（边写边读则永远不溢出）
	blob := bytes.Repeat([]byte("Y"), ChunkSize)
	burst := (MaxInboundBytes / ChunkSize) + 32
	for i := 0; i < burst; i++ {
		if _, err := cli.Write(blob); err != nil {
			t.Fatalf("写入第 %d 块失败: %v", i, err)
		}
	}
	time.Sleep(300 * time.Millisecond) // 等投递完成

	// 再读：应先把已入队的数据读完，最后返回 ErrOverflow（不静默丢）
	buf := make([]byte, 64<<10)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		type res struct {
			n   int
			err error
		}
		ch := make(chan res, 1)
		go func() {
			n, err := srv.Read(buf)
			ch <- res{n, err}
		}()
		select {
		case r := <-ch:
			if r.err == ErrOverflow {
				return // 期望
			}
			if r.err != nil {
				t.Fatalf("期望 ErrOverflow，实得 %v", r.err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("读挂住了（缓冲耗尽后应显式报溢出，而非无限等待）")
		}
	}
	t.Fatal("溢出后未在预期时间内报错（可能静默丢数据）")
}
