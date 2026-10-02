// rtx server — 外部控制器：监听执行器 reverse 回连 + 控制 API 派发任务
// 用法: server -l :9000 -t <token> [--ctrl :9001]
// 控制 API（主 agent / rtx CLI 通过它派任务）:
//
//	GET  /agents                    列出在线执行器
//	POST /task {agent_id,type,...}  派发任务并同步等结果
package main

import (
	"bufio"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"coreutil/internal/link"
	"coreutil/internal/proto"

	"coreutil/internal/tlsx"
	"coreutil/internal/ws"
	"github.com/hashicorp/yamux"
)

var (
	listenAddr = flag.String("l", ":9000", "agent reverse listen addr")
	ctrlAddr   = flag.String("ctrl", ":9001", "control api addr")
	wsAddr     = flag.String("wsl", "", "websocket listen addr (ws/wss agent dial-back, optional)")
	token      = flag.String("t", "", "auth token (agent 与控制 API 共用)")
	tlsEnable  = flag.Bool("tls", false, "")
	certFile   = flag.String("tls-cert", "", "")
	keyFile    = flag.String("tls-key", "", "")
)

// Agent 表示一个在线的执行器连接
type Agent struct {
	mu     sync.Mutex
	conn   net.Conn
	writer *bufio.Writer
	wsSend func(*proto.Msg) error // WS agent 的消息发送
	Info   proto.Msg
	Online bool
	hub    *link.Hub // mux 通道（穿透）
}

func (a *Agent) send(m *proto.Msg) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.wsSend != nil {
		if err := a.wsSend(m); err != nil {
			a.Online = false
			return err
		}
		return nil
	}
	if err := proto.WriteMsg(a.writer, m); err != nil {
		a.Online = false
		return err
	}
	return a.writer.Flush()
}

type Server struct {
	mu      sync.Mutex
	agents  map[string]*Agent // id -> agent
	pending sync.Map          // task_id -> chan *proto.Msg
	seq     uint64
	token   string

	// 穿透（控制侧监听 → yamux 流 → agent 侧 SOCKS5）
	socksMu  sync.Mutex
	socks    map[uint32]*socksEntry
	socksSeq uint32
}

// socksEntry 一个已启动的穿透监听
type socksEntry struct {
	muxID uint32
	agent string
	ln    net.Listener
	sess  *yamux.Session
}

func NewServer(tok string) *Server {
	return &Server{agents: map[string]*Agent{}, token: tok, socks: map[uint32]*socksEntry{}}
}

func (s *Server) newTaskID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// agentLoop 读某 agent 连接的上行消息
func (s *Server) agentLoop(a *Agent) {
	r := bufio.NewReader(a.conn)
	for {
		m, err := proto.ReadMsg(r)
		if err != nil {
			a.mu.Lock()
			a.Online = false
			a.mu.Unlock()
			s.mu.Lock()
			delete(s.agents, a.Info.AgentID)
			s.mu.Unlock()
			fmt.Printf("[server] agent %s offline\n", a.Info.AgentID)
			return
		}
		if a.hub.Handle(m) {
			continue // mux 流式承载：已由 link.Hub 消费
		}
		switch m.Type {
		case proto.MsgResult:
			if ch, ok := s.pending.Load(m.TaskID); ok {
				ch.(chan *proto.Msg) <- m
				s.pending.Delete(m.TaskID)
			}
		default:
			// hello 之外的未知上行忽略
		}
	}
}

func (s *Server) handleAgentConn(conn net.Conn) {
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	r := bufio.NewReader(conn)
	hello, err := proto.ReadMsg(r)
	if err != nil || hello.Type != proto.MsgHello {
		return
	}
	if hello.Token != s.token {
		fmt.Printf("[server] bad token from %s\n", conn.RemoteAddr())
		return
	}
	conn.SetReadDeadline(time.Time{})
	id := hello.AgentID
	a := &Agent{conn: conn, writer: bufio.NewWriter(conn), Info: *hello, Online: true}
	a.hub = link.NewHub(a.send)
	s.mu.Lock()
	if old, ok := s.agents[id]; ok { // 旧连接（重连）先断掉
		old.mu.Lock()
		old.conn.Close()
		old.mu.Unlock()
	}
	s.agents[id] = a
	s.mu.Unlock()
	fmt.Printf("[server] agent online: id=%s os=%s arch=%s host=%s user=%s\n",
		id, hello.OS, hello.Arch, hello.Hostname, hello.User)
	s.agentLoop(a)
}

// dispatch 派发任务并同步等待结果（timeout）
func (s *Server) dispatch(req proto.Msg, timeout time.Duration) (*proto.Msg, error) {
	s.mu.Lock()
	a, ok := s.agents[req.AgentID]
	s.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("agent not online: %s", req.AgentID)
	}
	tid := s.newTaskID()
	ch := make(chan *proto.Msg, 1)
	s.pending.Store(tid, ch)
	defer s.pending.Delete(tid)

	task := &proto.Msg{
		Type: proto.MsgTask, TaskID: tid, Task: req.Task,
		Cmd: req.Cmd, Path: req.Path, Data: req.Data, Append: req.Append,
		BgID: req.BgID, Limit: req.Limit, // Phase B: 后台任务原语透传
		MuxID: req.MuxID, MuxOp: req.MuxOp, // 穿透：socks 通道 id
	}
	if err := a.send(task); err != nil {
		return nil, fmt.Errorf("send to agent failed: %v", err)
	}
	select {
	case res := <-ch:
		return res, nil
	case <-time.After(timeout):
		return nil, fmt.Errorf("task timeout (%s)", timeout)
	}
}

// ---- 穿透：控制侧监听 → yamux 流 → agent 侧 SOCKS5 ----
//
// 目标机上不开监听端口、不新增连接、不新增第三方二进制；隧道流量复用已有控制连接。
// 每条接入的本地连接 = 一条 yamux 流 = 一次 SOCKS5 会话（含 socks5h 远端解析）。

func (s *Server) startSocks(agentID, listen string) (uint32, string, error) {
	s.mu.Lock()
	a, ok := s.agents[agentID]
	s.mu.Unlock()
	if !ok {
		return 0, "", fmt.Errorf("agent not online: %s", agentID)
	}
	if a.hub == nil {
		return 0, "", fmt.Errorf("agent %s has no mux hub", agentID)
	}

	s.socksMu.Lock()
	s.socksSeq++
	id := s.socksSeq
	s.socksMu.Unlock()

	conn, err := a.hub.Dial(id)
	if err != nil {
		return 0, "", fmt.Errorf("mux open: %w", err)
	}
	cfg := yamux.DefaultConfig()
	cfg.LogOutput = io.Discard
	cfg.EnableKeepAlive = false
	sess, err := yamux.Client(conn, cfg)
	if err != nil {
		_ = conn.Close()
		return 0, "", fmt.Errorf("yamux client: %w", err)
	}
	// 让 agent 在这个通道上开 SOCKS5 服务
	if _, err := s.dispatch(proto.Msg{AgentID: agentID, Task: proto.TaskSocks, MuxID: id}, 20*time.Second); err != nil {
		_ = sess.Close()
		return 0, "", fmt.Errorf("agent 拒绝开 socks: %w", err)
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		_ = sess.Close()
		return 0, "", err
	}
	e := &socksEntry{muxID: id, agent: agentID, ln: ln, sess: sess}
	s.socksMu.Lock()
	s.socks[id] = e
	s.socksMu.Unlock()
	go s.socksAcceptLoop(e)
	fmt.Printf("[server] socks on %s via agent %s (mux %d)\n", ln.Addr(), agentID, id)
	return id, ln.Addr().String(), nil
}

func (s *Server) socksAcceptLoop(e *socksEntry) {
	for {
		c, err := e.ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			st, err := e.sess.OpenStream()
			if err != nil {
				return
			}
			defer st.Close()
			done := make(chan struct{}, 2)
			go func() { _, _ = io.Copy(st, c); done <- struct{}{} }()
			go func() { _, _ = io.Copy(c, st); done <- struct{}{} }()
			<-done
			<-done
		}()
	}
}

func (s *Server) stopSocks(id uint32) error {
	s.socksMu.Lock()
	e := s.socks[id]
	delete(s.socks, id)
	s.socksMu.Unlock()
	if e == nil {
		return fmt.Errorf("no such socks mux: %d", id)
	}
	_ = e.ln.Close()
	_ = e.sess.Close()
	fmt.Printf("[server] socks stopped (mux %d)\n", id)
	return nil
}

func (s *Server) listSocks() []map[string]any {
	s.socksMu.Lock()
	defer s.socksMu.Unlock()
	var out []map[string]any
	for _, e := range s.socks {
		out = append(out, map[string]any{
			"mux": e.muxID, "agent": e.agent, "listen": e.ln.Addr().String(),
		})
	}
	return out
}

func (s *Server) httpAPI() http.Handler {
	mux := http.NewServeMux()
	// 简单鉴权中间件
	auth := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-Rtx-Token") != s.token {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next(w, r)
		}
	}
	mux.HandleFunc("/agents", auth(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		type item struct {
			ID, Host, OS, Arch, User string
			PID                      int
			Online                   bool
		}
		var out []item
		for id, a := range s.agents {
			info := a.Info
			out = append(out, item{id, info.Hostname, info.OS, info.Arch, info.User, info.PID, a.Online})
		}
		json.NewEncoder(w).Encode(map[string]any{"agents": out})
	}))
	mux.HandleFunc("/socks", auth(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			var req struct{ Agent, Listen string }
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.Listen == "" {
				req.Listen = "127.0.0.1:1080"
			}
			id, addr, err := s.startSocks(req.Agent, req.Listen)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "mux": id, "listen": addr})
		case http.MethodDelete:
			var id uint32
			_, _ = fmt.Sscanf(r.URL.Query().Get("mux"), "%d", &id)
			if err := s.stopSocks(id); err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"ok": true})
		default:
			json.NewEncoder(w).Encode(map[string]any{"socks": s.listSocks()})
		}
	}))
	mux.HandleFunc("/task", auth(func(w http.ResponseWriter, r *http.Request) {
		var req proto.Msg
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		res, err := s.dispatch(req, 120*time.Second)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(res)
	}))
	return mux
}

// serveWS 监听 WS(HTTP Upgrade) 连接：握手后按 WS 会话处理 agent 消息
func (s *Server) serveWS(addr string) {
	var ln net.Listener
	var err error
	if *tlsEnable {
		cf, kf := *certFile, *keyFile
		if cf == "" {
			cf = "rtx-server.crt"
		}
		if kf == "" {
			kf = "rtx-server.key"
		}
		tcfg, fp, err2 := tlsx.ServerConfig(cf, kf)
		if err2 != nil {
			fmt.Fprintln(os.Stderr, "ws tls:", err2)
			return
		}
		ln, err = tls.Listen("tcp", addr, tcfg)
		_ = fp
	} else {
		ln, err = net.Listen("tcp", addr)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ws listen:", err)
		return
	}
	fmt.Printf("[server] ws listen on %s (tls=%v)\n", addr, *tlsEnable)
	for {
		conn, err := ln.Accept()
		if err != nil {
			continue
		}
		go func(c net.Conn) {
			wc, err := ws.Server(c)
			if err != nil {
				c.Close()
				return
			}
			link := &serverWSLink{c: wc, r: bufio.NewReader(nil)}
			s.handleAgentConn2(link)
		}(conn)
	}
}

// ---- server 侧消息链路（TCP/TLS 用长度帧；WS 用 frame JSON）----

type serverWSLink struct {
	c *ws.Conn
	r interface{} // 占位
}

func (l *serverWSLink) Send(m *proto.Msg) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return l.c.WriteFrame(b, false) // 服务端帧不掩码
}
func (l *serverWSLink) Recv() (*proto.Msg, error) {
	payload, err := l.c.ReadFrame()
	if err != nil {
		return nil, err
	}
	var m proto.Msg
	if err := json.Unmarshal(payload, &m); err != nil {
		return nil, err
	}
	return &m, nil
}
func (l *serverWSLink) Close() error { return l.c.Close() }

// handleAgentConn2 与 handleAgentConn 同逻辑，但走 msgLink 抽象
func (s *Server) handleAgentConn2(ln *serverWSLink) {
	defer ln.Close()
	hello, err := ln.Recv()
	if err != nil {
		return
	}
	if hello.Type != proto.MsgHello {
		return
	}
	if hello.Token != s.token {
		fmt.Printf("[server] bad token (ws) from %s\n", "ws")
		return
	}
	a := &Agent{conn: nil, writer: nil, Info: *hello, Online: true, wsSend: ln.Send}
	a.hub = link.NewHub(ln.Send)
	s.mu.Lock()
	s.agents[hello.AgentID] = a
	s.mu.Unlock()
	fmt.Printf("[server] agent online (ws): id=%s os=%s arch=%s host=%s user=%s\n",
		hello.AgentID, hello.OS, hello.Arch, hello.Hostname, hello.User)
	// 任务循环
	for {
		m, err := ln.Recv()
		if err != nil {
			break
		}
		if a.hub.Handle(m) {
			continue
		}
		if m.Type == proto.MsgResult {
			if ch, ok := s.pending.Load(m.TaskID); ok {
				ch.(chan *proto.Msg) <- m
				s.pending.Delete(m.TaskID)
			}
		}
	}
	s.mu.Lock()
	delete(s.agents, hello.AgentID)
	s.mu.Unlock()
	fmt.Printf("[server] agent offline (ws): %s\n", hello.AgentID)
}

func main() {
	flag.Parse()
	// token 可走环境变量：避免出现在进程 cmdline 里
	if *token == "" {
		*token = os.Getenv("RTX_TOKEN")
	}
	if *token == "" {
		fmt.Fprintln(os.Stderr, "usage: server -l :9000 -t <token> [--ctrl :9001]")
		os.Exit(1)
	}
	s := NewServer(*token)

	// 控制 API
	go func() {
		fmt.Printf("[server] control api on %s\n", *ctrlAddr)
		if err := http.ListenAndServe(*ctrlAddr, s.httpAPI()); err != nil {
			fmt.Fprintln(os.Stderr, "ctrl api:", err)
			os.Exit(1)
		}
	}()

	// agent reverse 监听
	ln, err := net.Listen("tcp", *listenAddr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "listen:", err)
		os.Exit(1)
	}
	if *tlsEnable {
		cf, kf := *certFile, *keyFile
		if cf == "" {
			cf = "rtx-server.crt"
		}
		if kf == "" {
			kf = "rtx-server.key"
		}
		tcfg, fp, err := tlsx.ServerConfig(cf, kf)
		if err != nil {
			fmt.Fprintln(os.Stderr, "tls:", err)
			os.Exit(1)
		}
		ln = tlsx.WrapServer(ln, tcfg)
		fmt.Printf("[server] TLS on, cert=%s/%s pin(fp)=%s\n", cf, kf, fp)
	}
	fmt.Printf("[server] agent listen on %s (token %s...)\n", *listenAddr, truncate(*token, 4))
	if *wsAddr != "" {
		go s.serveWS(*wsAddr)
	}
	for {
		conn, err := ln.Accept()
		if err != nil {
			fmt.Fprintln(os.Stderr, "accept:", err)
			continue
		}
		go s.handleAgentConn(conn)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

var _ = strings.TrimSpace
var _ io.Reader
