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

	"coreutil/internal/proto"
	"coreutil/internal/tlsx"
)

var (
	listenAddr = flag.String("l", ":9000", "agent reverse listen addr")
	ctrlAddr   = flag.String("ctrl", ":9001", "control api addr")
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
	Info   proto.Msg
	Online bool
}

func (a *Agent) send(m *proto.Msg) error {
	a.mu.Lock()
	defer a.mu.Unlock()
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
}

func NewServer(tok string) *Server {
	return &Server{agents: map[string]*Agent{}, token: tok}
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

func main() {
	flag.Parse()
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
