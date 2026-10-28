package main

// The sshtest server side of exec mode (phase7 §9.1): a tiny interpreter for
// the `/bin/sh -s` scripts buildExecScript writes, so the transport is
// tested over real SSH channels without depending on the local /bin/sh.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

var (
	aiTestCdPrefix = regexp.MustCompile(`^cd -- '((?:[^']|'\\'')*)' 2>/dev/null \|\| \{ printf '\\036SGERR:%s:cwd\\036' '([0-9a-f]+)' >&2; exit 201; \}$`)
	aiTestCdProbe  = regexp.MustCompile(`^(cd(?: [^ &]+)?) && printf '\\036SGCWD:%s:%s\\036' '([0-9a-f]+)' "\$\(pwd -P\)"$`)
	aiTestExport   = regexp.MustCompile(`^export ([A-Za-z_][A-Za-z0-9_]*)='((?:[^']|'\\'')*)'$`)
)

var aiTestDirs = map[string]bool{"/": true, "/home": true, "/home/tester": true, "/tmp": true, "/var": true, "/var/www": true, "/etc": true, "/etc/nginx": true}

func aiTestUnquote(s string) string { return strings.ReplaceAll(s, `'\''`, `'`) }

// runAITestSh reads the whole script from stdin (the client closes it) and
// runs it line by line.
func runAITestSh(ch ssh.Channel) int {
	script, _ := io.ReadAll(ch)
	cwd := "/home/tester"
	env := map[string]string{}
	cd := func(target string) bool {
		switch {
		case target == "":
			target = "/home/tester"
		case !strings.HasPrefix(target, "/"):
			target = path.Join(cwd, target)
		}
		target = path.Clean(target)
		if !aiTestDirs[target] {
			fmt.Fprintf(ch.Stderr(), "sh: cd: %s: No such file or directory\n", target)
			return false
		}
		cwd = target
		return true
	}
	for _, line := range strings.Split(strings.TrimRight(string(script), "\n"), "\n") {
		if m := aiTestCdPrefix.FindStringSubmatch(line); m != nil {
			if !cd(aiTestUnquote(m[1])) {
				fmt.Fprintf(ch.Stderr(), "\x1eSGERR:%s:cwd\x1e", m[2])
				return 201
			}
			continue
		}
		if m := aiTestCdProbe.FindStringSubmatch(line); m != nil {
			if !cd(strings.TrimSpace(strings.TrimPrefix(m[1], "cd"))) {
				return 1
			}
			fmt.Fprintf(ch, "\x1eSGCWD:%s:%s\x1e", m[2], cwd)
			return 0
		}
		if m := aiTestExport.FindStringSubmatch(line); m != nil {
			env[m[1]] = aiTestUnquote(m[2])
			continue
		}
		name, arg, _ := strings.Cut(line, " ")
		switch name {
		case "printenv":
			fmt.Fprintln(ch, env[arg])
		case "unset":
			delete(env, arg)
		case "pwd":
			fmt.Fprintln(ch, cwd)
		case "echo":
			if strings.HasPrefix(arg, "$") {
				arg = env[arg[1:]]
			}
			fmt.Fprintln(ch, arg)
		case "cat":
			rest, _ := io.ReadAll(ch)
			_, _ = ch.Write(rest)
		case "sleep":
			// Ignores the signal request (as old OpenSSH and Dropbear do):
			// runs until the client closes the channel.
			for {
				time.Sleep(20 * time.Millisecond)
				if _, err := ch.Write([]byte{}); err != nil {
					return 0
				}
				if _, err := ch.SendRequest("keepalive@test", true, nil); err != nil {
					return 0
				}
			}
		case "alias", "true":
		case "head":
			fmt.Fprintln(ch, "db password=hunter22-raw")
		case "exit":
			n, _ := strconv.Atoi(arg)
			return n
		default:
			fmt.Fprintf(ch.Stderr(), "sh: %s: not found\n", name)
			return 127
		}
	}
	return 0
}

func TestAISessionExecTransportOverSSH(t *testing.T) {
	srv := newSSHTestServer(t)
	app := newTestApp(t)
	conn := addTrustedConnection(t, app, srv.Connection("lab"))
	issueAgentToken(t, app, aiTestAgent)
	settings := defaultAISessionSettings()
	settings.Enabled = true
	if r := app.SaveAISessionSettings(settings); !r.OK {
		t.Fatal(r)
	}
	if r := app.setAgentApprovalBypass(aiTestAgent, true); !r.OK {
		t.Fatal(r)
	}
	call := func(name string, args map[string]any) (map[string]any, bool) {
		t.Helper()
		raw, _ := json.Marshal(args)
		out, rpcErr, handled := app.callAISessionTool(context.Background(), aiTestCaller(app), toolCallParams{Name: name, Arguments: raw})
		if !handled || rpcErr != nil {
			t.Fatalf("%s: %v", name, rpcErr)
		}
		var res struct {
			IsError           bool           `json:"isError"`
			StructuredContent map[string]any `json:"structuredContent"`
		}
		_ = json.Unmarshal([]byte(mustJSON(t, out)), &res)
		return res.StructuredContent, res.IsError
	}
	open, isErr := call("open_session", map[string]any{"host": conn.Name, "reason": "test"})
	if isErr || open["cwd"] != "/home/tester" {
		t.Fatalf("open = %v", open)
	}
	id := open["session_id"].(string)
	n := 0
	send := func(line string) map[string]any {
		t.Helper()
		n++
		out, _ := call("send_line", map[string]any{"session_id": id, "line": line, "client_request_id": fmt.Sprint("l", n)})
		return out
	}
	if v := send("cd /var/www"); v["state"] != stateAutoSucceeded || v["cwd"] != "/var/www" {
		t.Fatalf("cd = %v", v)
	}
	if v := send("pwd"); !strings.Contains(v["output"].(string), "/var/www\n") || strings.Contains(v["output"].(string), "SGCWD") {
		t.Fatalf("pwd = %v", v)
	}
	send("export LANG=ja_JP.UTF-8")
	if v := send("printenv LANG"); !strings.Contains(v["output"].(string), "ja_JP.UTF-8") {
		t.Fatalf("printenv LANG = %v", v)
	}
	if v := send("cat"); v["state"] != stateAutoSucceeded {
		t.Fatalf("cat (stdin EOF) = %v", v)
	}
	if v := send("cd /nope"); v["state"] != stateAutoFailed || v["cwd"] != "/var/www" {
		t.Fatalf("cd /nope = %v", v)
	}
	if v := send("head config.ini"); strings.Contains(mustJSON(t, v), "hunter22") || !strings.Contains(v["output"].(string), "[REDACTED:password:") {
		t.Fatalf("secret = %v", v)
	}
	read, _ := call("read_output", map[string]any{"session_id": id})
	text := read["text"].(string)
	if strings.Contains(text, "hunter22") || strings.Contains(text, "\x1e") || !strings.Contains(text, "$ cd /var/www\n") {
		t.Fatalf("read_output = %q", text)
	}
	if r, _ := read["redactions"].(float64); r < 1 {
		t.Fatalf("redactions = %v", read["redactions"])
	}
	// The agent's line goes to stdin, never into the exec command string.
	for _, cmd := range srv.Execs() {
		if cmd != aiExecCommand(aiShellSh) {
			t.Fatalf("exec command = %q", cmd)
		}
	}
	if closed, _ := call("close_session", map[string]any{"session_id": id}); closed["close_reason"] != "agent_closed" {
		t.Fatalf("close = %v", closed)
	}

	// Audit: the session, its lines and its (redacted) output are stored;
	// the raw secret is in neither the DB file nor the WAL.
	row, ok, err := app.store.loadAISessionRow(id)
	if err != nil || !ok || row.open() || row.CloseReason != aiCloseAgent || row.LineCount != 7 {
		t.Fatalf("row = %+v %v %v", row, ok, err)
	}
	chunks, err := app.store.loadAISessionOutput(id, 0, 1<<20)
	if err != nil || len(chunks) == 0 {
		t.Fatalf("output chunks = %v %v", chunks, err)
	}
	var all strings.Builder
	for _, c := range chunks {
		all.WriteString(c.Text)
	}
	if !strings.Contains(all.String(), "$ pwd\n/var/www") || strings.Contains(all.String(), "hunter22") {
		t.Fatalf("stored output = %q", all.String())
	}
	for _, p := range []string{app.dbPath, app.dbPath + "-wal"} {
		if data, err := os.ReadFile(p); err == nil && bytes.Contains(data, []byte("hunter22-raw")) {
			t.Fatalf("raw secret in %s", p)
		}
	}
}

// A previous run that left a session open: NewApp closes it (app_restart),
// withdraws its pending line, and the line can never be approved.
func TestAISessionStartupRecovery(t *testing.T) {
	isolateEnv(t)
	store, err := openStore(defaultDatabasePath())
	if err != nil {
		t.Fatal(err)
	}
	const sid = "AIS-AAAAAAAAAAAAAAAAAAAAAAAAAA"
	if err := store.insertAISession(aiSessionRow{ID: sid, AgentName: aiTestAgent, ConnectionID: "c1", HostName: "web", TargetFP: "u@h:22",
		Mode: aiModeExec, Shell: aiShellSh, Reason: "r", OpenedAt: aiStoreTime(newFakeAIHost().now())}); err != nil {
		t.Fatal(err)
	}
	req := newCommandRequest(Connection{ID: "c1", Name: "web", Host: "h", Port: 22, User: "u"}, "ls", "r", aiTestAgent, "Low", statePending)
	req.ExpiresAt = "2999-01-01T00:00:00Z"
	if err := store.insertAISessionLineRequest(req, aiSessionLineRow{RequestID: req.ID, SessionID: sid, Seq: 1, Effect: "none"}); err != nil {
		t.Fatal(err)
	}
	store.Close()

	app := NewApp()
	t.Cleanup(func() { app.shutdown(context.Background()) })
	got, ok := app.findRequest(req.ID)
	if !ok || got.Status != stateWithdrawn || got.SessionID != sid || got.SessionSeq != 1 {
		t.Fatalf("request = %+v %v", got, ok)
	}
	if r := app.ApproveCommand(req.ID, ""); r.OK {
		t.Fatalf("approve = %+v", r)
	}
	row, _, _ := app.store.loadAISessionRow(sid)
	if row.open() || row.CloseReason != aiCloseAppRestart {
		t.Fatalf("row = %+v", row)
	}
}
