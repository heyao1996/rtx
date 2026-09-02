// Package proto 定义 rtx 控制器 <-> 执行器 的通信协议。
// 线格式: 4 字节大端长度 + JSON 载荷。全 stdlib，无第三方依赖。
package proto

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
)

// MsgType 消息方向: 上行 = agent→server, 下行 = server→agent
type MsgType string

const (
	// 上行（agent -> server）
	MsgHello  MsgType = "hello" // 注册: agent 信息 + token
	MsgResult MsgType = "result"
	MsgBeat   MsgType = "beat" // 心跳（可选）
	// 下行（server -> agent）
	MsgTask MsgType = "task"
	MsgPing MsgType = "ping"
)

// TaskType 任务类型
type TaskType string

const (
	TaskExec     TaskType = "exec"
	TaskRead     TaskType = "read"
	TaskWrite    TaskType = "write"
	TaskList     TaskType = "list"
	TaskInfo     TaskType = "info"
	TaskUpload   TaskType = "upload"
	TaskDownload TaskType = "download"
	TaskKill     TaskType = "kill" // 执行器退出/自清理
)

// Msg 统一消息信封
type Msg struct {
	Type MsgType `json:"t"`
	// 注册/探活
	AgentID  string `json:"aid,omitempty"`
	Hostname string `json:"hn,omitempty"`
	OS       string `json:"os,omitempty"`
	Arch     string `json:"ar,omitempty"`
	User     string `json:"u,omitempty"`
	PID      int    `json:"pid,omitempty"`
	Token    string `json:"tok,omitempty"`
	// 任务
	TaskID string   `json:"tid,omitempty"`
	Task   TaskType `json:"task,omitempty"`
	// 任务参数
	Cmd    string `json:"cmd,omitempty"`
	Path   string `json:"p,omitempty"`
	Data   string `json:"d,omitempty"` // upload/write 用 base64
	Append bool   `json:"ap,omitempty"`
	Offset int64  `json:"of,omitempty"`
	Limit  int    `json:"li,omitempty"`
	// 结果
	OK       bool     `json:"ok,omitempty"`
	Stdout   string   `json:"so,omitempty"`
	Stderr   string   `json:"se,omitempty"`
	ExitCode int      `json:"ec,omitempty"`
	Entries  []string `json:"en,omitempty"`
	Err      string   `json:"err,omitempty"`
}

// WriteMsg 写一条消息（4 字节大端长度 + JSON）
func WriteMsg(w io.Writer, m *Msg) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(b)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

// ReadMsg 读一条消息
func ReadMsg(r *bufio.Reader) (*Msg, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > 64<<20 { // 64MB 上限
		return nil, fmt.Errorf("msg too large: %d", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	var m Msg
	if err := json.Unmarshal(buf, &m); err != nil {
		return nil, err
	}
	return &m, nil
}
