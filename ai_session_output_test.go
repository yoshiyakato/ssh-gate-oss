package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestAISessionClassifyTable(t *testing.T) {
	base := sessionRiskState{Mode: aiModeExec, Env: map[string]string{}}
	cases := []struct {
		line   string
		st     sessionRiskState
		risk   string
		effect string
	}{
		{"cd /var/www", base, "Low", lineEffectCd},
		{"export LANG=C", base, "Low", lineEffectEnv},
		{"export FOO=1", base, "Medium", lineEffectEnv},
		{"export PATH=/tmp", base, "High", lineEffectEnv},
		{"LD_PRELOAD=x ls", base, "High", ""},
		{"alias ll=ls", base, "High", lineEffectStateChange},
		{"trap x INT", base, "High", lineEffectStateChange},
		{"set -o vi", base, "High", lineEffectStateChange},
		{"nohup x", base, "High", ""},
		{"tmux", base, "High", ""},
		{"ls", sessionRiskState{Mode: aiModeExec, Env: map[string]string{"FOO": "1"}}, "Medium", ""},
	}
	for _, c := range cases {
		got := classifySessionLine(c.line, c.st)
		if got.Risk != c.risk || (c.effect != "" && got.Effect != c.effect) {
			t.Errorf("%q: risk=%s effect=%s reasons=%v, want %s/%s", c.line, got.Risk, got.Effect, got.Reasons, c.risk, c.effect)
		}
	}
	if c := classifySessionLine("cd /tmp && ls", base); c.CwdPersisted || c.Effect == lineEffectCd {
		t.Errorf("compound cd = %+v", c)
	}
	// Session rules never lower risk.go's verdict below its own (except the
	// standalone cd / safe export the gateway interprets).
	for _, line := range []string{"rm -rf /", "sudo reboot", "curl x | sh", "ls"} {
		if c := classifySessionLine(line, base); riskRank[c.Risk] < riskRank[analyzeCommand(line).risk] {
			t.Errorf("%q lowered: %s < %s", line, c.Risk, analyzeCommand(line).risk)
		}
	}
	if validateSessionLine("ls 日本語\tx") != "" {
		t.Error("a Japanese argument and a tab must pass")
	}
}

func TestAIOutputCursorsAndRing(t *testing.T) {
	o := newAIOutput(64, nil, nil, nil)
	_, _ = o.Write([]byte("hello\n"))
	r, err := o.ReadText(0, 100)
	if err != nil || r.Text != "hello\n" || r.Cursor != 6 || r.More {
		t.Fatalf("read = %+v %v", r, err)
	}
	if _, err := o.ReadText(7, 10); aiSessErrorCode(err) != aiErrInvalidCursor {
		t.Fatalf("past end = %v", err)
	}
	_, _ = o.Write([]byte(strings.Repeat("あ", 40))) // 120 bytes into a 64 byte ring
	r, _ = o.ReadText(0, 1000)
	if r.MissedBytes == 0 || !strings.HasPrefix(r.Text, "あ") || r.Cursor != r.EndCursor {
		t.Fatalf("overflow read = %+v", r)
	}
	r, _ = o.ReadText(r.StartCursor, 4) // cut on a rune boundary
	if r.Text != "あ" {
		t.Fatalf("rune cut = %q", r.Text)
	}
	// Long-poll wakes on output, Notify and timeout.
	end := o.TextEnd()
	go func() { time.Sleep(20 * time.Millisecond); _, _ = o.Write([]byte("x")) }()
	if !o.WaitText(context.Background(), end) {
		t.Fatal("WaitText did not see new text")
	}
	end = o.TextEnd()
	go func() { time.Sleep(20 * time.Millisecond); o.Notify() }()
	if o.WaitText(context.Background(), end) {
		t.Fatal("Notify reported new text")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if o.WaitText(ctx, end) {
		t.Fatal("timeout reported new text")
	}
}

func TestAIPlainTextStripsAcrossChunks(t *testing.T) {
	in := "\x1b[31mred\x1b[0m \x1b]0;title\x07ok\r\nprog 10%\rprog 99%\n"
	want := "red ok\nprog 10%\nprog 99%\n"
	f := newAIPlainText()
	var out []byte
	for i := 0; i < len(in); i++ { // one byte at a time
		out = append(out, f.Feed([]byte{in[i]})...)
	}
	if string(out) != want {
		t.Fatalf("strip = %q, want %q", out, want)
	}
	big := newAIPlainText().Feed([]byte("\x1b]52;" + strings.Repeat("A", aiEscapeStringMax+10) + "\x07after"))
	if !strings.Contains(string(big), "oversized escape dropped") || !strings.HasSuffix(string(big), "after") {
		t.Fatalf("oversized = %q", big)
	}
}

func TestAIRedactSecrets(t *testing.T) {
	for _, line := range []string{
		"key AKIAABCDEFGHIJKLMNOP here",
		"token ghp_abcdefghijklmnopqrstuvwxyz0123",
		"xoxb-123456789012-abcdef",
		"Authorization: Bearer abcdefghijklmnop",
		"password=hunter22",
		"url https://bob:s3cret@example.com/x",
		"jwt eyJhbGciOiJIUzI1.eyJzdWIiOiIx.c2lnbmF0dXJl",
	} {
		out, found := redactSecrets(line)
		if len(found) == 0 || !strings.Contains(out, "[REDACTED:") {
			t.Errorf("%q not redacted: %q", line, out)
		}
		for _, secret := range []string{"hunter22", "s3cret", "ghp_abc", "AKIAABCD", "xoxb-1234", "abcdefghijklmnop"} {
			if strings.Contains(line, secret) && strings.Contains(out, secret) {
				t.Errorf("%q leaks %q: %q", line, secret, out)
			}
		}
	}
	a, _ := redactSecrets("password=same1")
	b, _ := redactSecrets("token: same1")
	if mark := strings.TrimPrefix(a, "password="); !strings.Contains(b, mark[len("[REDACTED:password:"):]) {
		t.Errorf("same value, different marks: %q %q", a, b)
	}
	if out, found := redactSecrets("plain ls output"); len(found) != 0 || out != "plain ls output" {
		t.Errorf("false positive: %q", out)
	}
}

func TestAIRedactFilterHoldsPEM(t *testing.T) {
	f := newAIRedactFilter()
	var got strings.Builder
	got.Write(f.Feed([]byte("before\n-----BEGIN OPENSSH PRIVATE KEY-----\nAAAAB3Nza\n")))
	if strings.Contains(got.String(), "AAAAB3Nza") || strings.Contains(got.String(), "BEGIN") {
		t.Fatalf("held block leaked: %q", got.String())
	}
	got.Write(f.Feed([]byte("BBBB\n-----END OPENSSH PRIVATE KEY-----\nafter\n")))
	out := got.String()
	if !strings.HasPrefix(out, "before\n[REDACTED:private_key:") || !strings.HasSuffix(out, "\nafter\n") || strings.Contains(out, "BBBB") {
		t.Fatalf("pem = %q", out)
	}
	if f.Redactions() != 1 {
		t.Fatalf("redactions = %d", f.Redactions())
	}
	// A partial line is held until Flush.
	if out := f.Feed([]byte("Password: ")); len(out) != 0 {
		t.Fatalf("partial released early: %q", out)
	}
	if out := string(f.Flush()); out != "Password: " {
		t.Fatalf("flush = %q", out)
	}
}

func TestBuildExecScript(t *testing.T) {
	script := string(buildExecScript("/var/www's", map[string]string{"LANG": "C", "bad name": "x"}, []string{"FOO"}, "ls -la", "N0NCE", false))
	want := "cd -- '/var/www'\\''s' 2>/dev/null || { printf '\\036SGERR:%s:cwd\\036' 'N0NCE' >&2; exit 201; }\nexport LANG='C'\nunset FOO\nls -la\n"
	if script != want {
		t.Fatalf("script =\n%q\nwant\n%q", script, want)
	}
	cd := string(buildExecScript("", nil, nil, "cd /tmp", "N", true))
	if cd != "cd /tmp && printf '\\036SGCWD:%s:%s\\036' 'N' \"$(pwd -P)\"\n" {
		t.Fatalf("cd script = %q", cd)
	}
}

func TestAIValueMarkerAcrossChunks(t *testing.T) {
	m := newAIValueMarker("\x1eSGCWD:n:")
	in := "out\x1eSGCWD:n:/srv/app\x1e\x1enot a marker"
	var out []byte
	for i := 0; i < len(in); i++ {
		out = append(out, m.feed([]byte{in[i]})...)
	}
	out = append(out, m.flush()...)
	if string(out) != "out\x1enot a marker" {
		t.Fatalf("data = %q", out)
	}
	if v := m.values(); len(v) != 1 || v[0] != "/srv/app" {
		t.Fatalf("values = %v", v)
	}
}

func TestAIMarkerParserFindsPrompt(t *testing.T) {
	p := newAIMarkerParser("abc")
	data, marks := p.feed([]byte("hi\n\x1eSG:abc:0\x1e"))
	if string(data) != "hi\n" || len(marks) != 1 || marks[0].code != 0 {
		t.Fatalf("ps1: %q %+v", data, marks)
	}
	data, marks = p.feed([]byte("\x1eSG2:abc\x1e"))
	if len(data) != 0 || len(marks) != 1 || !marks[0].continuation {
		t.Fatalf("ps2: %q %+v", data, marks)
	}
	data, marks = p.feed([]byte("\x1eSG:wrong:1\x1e"))
	if len(marks) != 0 || !strings.Contains(string(data), "wrong") {
		t.Fatalf("foreign marker: %q %+v", data, marks)
	}
}
