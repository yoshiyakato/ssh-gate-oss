package main

import (
	"path"
	"strings"
	"unicode"
)

// Risk classification is allow-list based: a command is Low only when every
// part of it is a known read-only command called with plain arguments.
// Anything the classifier cannot read with certainty (quoting, escapes,
// expansions, redirections, control or invisible characters, code handed to
// an interpreter) is High, so new shell tricks fail closed instead of
// slipping through as Low. Commands that are neither allow-listed nor
// recognized as destructive are Medium.

// harmlessRedirects are the only redirections accepted; they discard output
// or merge stderr and cannot write anywhere else.
var harmlessRedirects = map[string]bool{
	"2>/dev/null": true, ">/dev/null": true, "1>/dev/null": true, "2>&1": true,
}

// commandAnalysis is the parsed view of a command line.
type commandAnalysis struct {
	risk     string
	usesSudo bool
}

func classifyRisk(command string) string {
	return analyzeCommand(command).risk
}

// commandUsesSudo reports whether the command elevates privileges. A command
// the classifier cannot read is assumed to.
func commandUsesSudo(command string) bool {
	a := analyzeCommand(command)
	return a.usesSudo || a.risk == "High"
}

func analyzeCommand(command string) commandAnalysis {
	command = strings.TrimSpace(command)
	if command == "" || opaqueCommand(command) {
		return commandAnalysis{risk: "High", usesSudo: true}
	}
	result := commandAnalysis{risk: "Low"}
	for _, segment := range splitSegments(command) {
		seg := analyzeSegment(segment)
		if seg.usesSudo {
			result.usesSudo = true
		}
		if riskRank[seg.risk] > riskRank[result.risk] {
			result.risk = seg.risk
		}
	}
	return result
}

// opaqueCommand reports characters whose meaning depends on shell parsing the
// classifier does not do. Plain arguments in other scripts (for example a
// Japanese search word) are fine; control, format and non-space whitespace
// characters are not.
func opaqueCommand(command string) bool {
	for _, field := range strings.Fields(command) {
		if harmlessRedirects[field] {
			continue
		}
		for _, r := range field {
			if r > unicode.MaxASCII {
				if !unicode.IsPrint(r) || unicode.Is(unicode.Cf, r) || unicode.IsSpace(r) {
					return true
				}
				continue
			}
			if r < 0x20 || r == 0x7f {
				return true
			}
			switch r {
			case '\'', '"', '\\', '$', '`', '(', ')', '{', '}', '<', '>', '!', '#':
				return true
			}
		}
	}
	// Only spaces and tabs may separate words; newlines, CR and the like
	// would start new commands the splitter does not see.
	for _, r := range command {
		if unicode.IsSpace(r) && r != ' ' && r != '\t' {
			return true
		}
	}
	// A lone & runs a command in the background, which also separates
	// commands; && is handled by splitSegments.
	stripped := strings.ReplaceAll(strings.ReplaceAll(command, "&&", ""), "2>&1", "")
	return strings.Contains(stripped, "&")
}

// splitSegments splits on ; | && ||. Each segment is one simple command.
func splitSegments(command string) [][]string {
	var segments [][]string
	var current []string
	for _, field := range strings.Fields(strings.NewReplacer(
		"&&", " ; ", "||", " ; ", "|", " ; ", ";", " ; ",
	).Replace(command)) {
		if field == ";" {
			segments = append(segments, current)
			current = nil
			continue
		}
		if harmlessRedirects[field] {
			continue
		}
		current = append(current, field)
	}
	return append(segments, current)
}

// wrappers run the rest of the line as a command. The value is the set of
// their options that take a separate argument.
var wrappers = map[string]map[string]bool{
	"sudo":    {"-u": true, "-g": true, "-h": true, "-p": true, "-C": true, "-D": true, "-r": true, "-t": true, "-U": true},
	"doas":    {"-u": true, "-C": true},
	"env":     {"-u": true, "-C": true, "-S": true},
	"nice":    {"-n": true},
	"ionice":  {"-c": true, "-n": true, "-p": true},
	"nohup":   {},
	"timeout": {"-s": true, "-k": true},
	"time":    {},
	"command": {},
	"stdbuf":  {"-i": true, "-o": true, "-e": true},
	"xargs":   {"-I": true, "-n": true, "-P": true, "-L": true, "-d": true, "-E": true, "-s": true, "-a": true},
}

// interpreters run code given as an argument, a file or stdin.
var interpreters = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "csh": true, "tcsh": true, "fish": true,
	"python": true, "python2": true, "python3": true, "perl": true, "ruby": true, "node": true, "php": true,
	"lua": true, "awk": true, "gawk": true, "mawk": true, "nawk": true, "tclsh": true, "expect": true,
	"eval": true, "exec": true, "source": true, ".": true, "busybox": true, "su": true, "script": true,
}

func analyzeSegment(words []string) commandAnalysis {
	result := commandAnalysis{risk: "Low"}
	raise := func(risk string) {
		if riskRank[risk] > riskRank[result.risk] {
			result.risk = risk
		}
	}
	if len(words) == 0 {
		return result
	}
	// Leading VAR=value assignments change the command's environment.
	for len(words) > 0 && isAssignment(words[0]) {
		raise("Medium")
		words = words[1:]
	}
	// Peel wrappers: sudo, env, xargs and the like run the next command.
	for len(words) > 0 {
		name := path.Base(words[0])
		opts, ok := wrappers[name]
		if !ok {
			break
		}
		if name == "sudo" || name == "doas" {
			result.usesSudo = true
		}
		// Running something through a wrapper is never Low on its own.
		raise("Medium")
		words = words[1:]
		for len(words) > 0 && (strings.HasPrefix(words[0], "-") || (name == "env" && isAssignment(words[0])) || (name == "timeout" && isDuration(words[0])) || (name == "nice" && isNumber(words[0]))) {
			if opts[words[0]] && len(words) > 1 {
				words = words[1:]
			}
			words = words[1:]
		}
	}
	if len(words) == 0 {
		return result
	}
	name := words[0]
	if strings.ContainsAny(name, "*?[]") {
		// A glob in command position lets the shell pick the program.
		return commandAnalysis{risk: "High", usesSudo: true}
	}
	base := path.Base(name)
	args := words[1:]
	switch {
	case interpreters[base]:
		raise("High")
	case destructiveCommand(base, args):
		raise("High")
	case readOnlyCommand(base, args):
		// stays at the level the wrappers set
	default:
		raise("Medium")
	}
	return result
}

func isAssignment(word string) bool {
	eq := strings.IndexByte(word, '=')
	if eq <= 0 {
		return false
	}
	for i, r := range word[:eq] {
		if !(r == '_' || unicode.IsLetter(r) || (i > 0 && unicode.IsDigit(r))) {
			return false
		}
	}
	return true
}

func isNumber(word string) bool {
	word = strings.TrimPrefix(word, "-")
	return word != "" && strings.Trim(word, "0123456789") == ""
}

func isDuration(word string) bool {
	return word != "" && strings.Trim(strings.TrimRight(word, "smhd"), "0123456789.") == ""
}

func hasArg(args []string, values ...string) bool {
	for _, a := range args {
		for _, v := range values {
			if a == v || (strings.HasPrefix(v, "--") && strings.HasPrefix(a, v+"=")) {
				return true
			}
		}
	}
	return false
}

func hasArgPrefix(args []string, prefixes ...string) bool {
	for _, a := range args {
		for _, p := range prefixes {
			if strings.HasPrefix(a, p) {
				return true
			}
		}
	}
	return false
}

// hasShortFlag reports a single-dash option cluster containing flag, so
// "-Rf" contains R.
func hasShortFlag(args []string, flag byte) bool {
	for _, a := range args {
		if len(a) > 1 && a[0] == '-' && a[1] != '-' && strings.IndexByte(a[1:], flag) >= 0 {
			return true
		}
	}
	return false
}

// firstOperand is the first argument that is not an option.
func firstOperand(args []string) string {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			return a
		}
	}
	return ""
}

func destructiveCommand(name string, args []string) bool {
	switch name {
	case "rm", "rmdir", "shred", "unlink", "shutdown", "reboot", "halt", "poweroff", "init", "telinit",
		"iptables", "ip6tables", "nft", "ufw", "firewall-cmd", "userdel", "usermod", "groupdel",
		"passwd", "chpasswd", "dd", "fdisk", "sfdisk", "parted", "wipefs", "mkswap", "swapoff",
		"kill", "killall", "pkill", "zfs", "zpool", "lvremove", "vgremove", "pvremove", "umount":
		return true
	case "chmod", "chown", "chgrp", "setfacl":
		return hasShortFlag(args, 'R') || hasArg(args, "--recursive")
	case "crontab":
		return hasShortFlag(args, 'r')
	case "find":
		return hasArg(args, "-delete", "-exec", "-execdir", "-ok", "-okdir")
	case "systemctl", "service":
		return !readOnlyCommand(name, args) && hasArg(args, "stop", "restart", "disable", "mask", "kill",
			"isolate", "poweroff", "reboot", "halt", "suspend", "hibernate", "rescue", "emergency", "daemon-reexec")
	}
	return strings.HasPrefix(name, "mkfs")
}

// readOnlyCommand is the allow list. Each entry rejects the options that
// write files, change state or run other programs.
func readOnlyCommand(name string, args []string) bool {
	switch name {
	case "ls", "cat", "head", "tail", "wc", "df", "du", "free", "uptime", "uname", "whoami", "id",
		"pwd", "stat", "file", "which", "grep", "egrep", "fgrep", "uniq", "cut", "nl", "echo", "printf",
		"true", "false", "nproc", "arch", "lsblk", "lscpu", "lsof", "ss", "netstat", "printenv",
		"basename", "dirname", "realpath", "readlink", "md5sum", "sha1sum", "sha256sum", "sha512sum",
		"cksum", "tac", "rev", "column", "groups", "users", "who", "w", "last", "vmstat", "iostat",
		"mpstat", "sleep", "test", "tr", "diff", "cmp", "comm", "getent", "dig", "host", "nslookup",
		"ping", "tty", "locale", "sha256", "md5", "exit", "yes", "seq":
		// uniq writes its second operand.
		return !(name == "uniq" && len(nonOptions(args)) > 1)
	case "hostname":
		return len(nonOptions(args)) == 0
	case "date":
		return !hasArg(args, "-s", "--set") && !hasArgPrefix(args, "--set=")
	case "sort":
		return !hasShortFlag(args, 'o') && !hasArg(args, "--output") && !hasArgPrefix(args, "--compress-program")
	case "ps", "pgrep", "pidof":
		return true
	case "top":
		return hasShortFlag(args, 'b')
	case "find":
		return !hasArg(args, "-delete", "-exec", "-execdir", "-ok", "-okdir", "-fprint", "-fprint0", "-fprintf", "-fls")
	case "journalctl":
		return !hasArgPrefix(args, "--vacuum", "--rotate", "--flush", "--sync", "--relinquish", "--setup-keys", "--update-catalog")
	case "systemctl":
		switch firstOperand(args) {
		case "status", "is-active", "is-enabled", "is-failed", "list-units", "list-unit-files", "list-timers", "list-sockets", "show", "cat":
			return true
		}
	case "service":
		ops := nonOptions(args)
		return len(ops) == 2 && ops[1] == "status"
	case "docker", "podman":
		switch firstOperand(args) {
		case "ps", "images", "logs", "inspect", "version", "info", "top":
			return true
		case "stats":
			return hasArg(args, "--no-stream")
		}
	case "git":
		// Global options (-c, -C, --exec-path, ...) come before the
		// subcommand and can run programs, so the subcommand must be first.
		if len(args) == 0 || hasArgPrefix(args, "--output", "--ext-diff", "--upload-pack", "--exec") {
			return false
		}
		switch args[0] {
		case "status", "log", "diff", "show", "rev-parse", "describe", "shortlog", "blame", "ls-files":
			return true
		case "branch", "tag", "remote":
			for _, a := range args[1:] {
				switch a {
				case "-v", "-vv", "-a", "-r", "-l", "--list":
				default:
					return false
				}
			}
			return true
		}
	case "ip":
		for _, a := range args {
			switch a {
			case "add", "del", "delete", "set", "flush", "change", "replace", "append", "prepend", "exec", "netns":
				return false
			}
		}
		return true
	}
	return false
}

func nonOptions(args []string) []string {
	var out []string
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			out = append(out, a)
		}
	}
	return out
}
