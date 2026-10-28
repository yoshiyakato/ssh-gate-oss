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
				if !unicode.IsPrint(r) || unicode.Is(unicode.Cf, r) || unicode.IsSpace(r) || invisibleRune(r) {
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

// invisibleRune reports characters that unicode.IsPrint accepts but that draw
// as nothing or as a blank: combining and enclosing marks (variation
// selectors, U+034F, U+17B4/5 are Mn), Hangul fillers (Lo) and the braille
// blank (So). The approval screen marks the same set (approvals.js).
func invisibleRune(r rune) bool {
	if unicode.In(r, unicode.Mn, unicode.Me) {
		return true
	}
	switch r {
	case 0x115F, 0x1160, 0x3164, 0xFFA0, 0x2800, 0x034F, 0x17B4, 0x17B5:
		return true
	}
	return (r >= 0xFE00 && r <= 0xFE0F) || (r >= 0xE0100 && r <= 0xE01EF)
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

// wrapperSpec lists every option a wrapper is known to accept. args take a
// value (as the next word, after '=' for long options, or attached to a short
// option); flags do not; shell options make the wrapper run a shell or edit
// files, so the line is High. Any option not listed is High: the classifier
// cannot tell which word is the command that runs (fail closed).
type wrapperSpec struct {
	args, flags, shell []string
}

var wrappers = map[string]wrapperSpec{
	"sudo": {
		args: []string{"-u", "--user", "-g", "--group", "-h", "--host", "-p", "--prompt", "-C", "--close-from",
			"-D", "--chdir", "-R", "--chroot", "-r", "--role", "-t", "--type", "-U", "--other-user", "-T", "--command-timeout"},
		flags: []string{"-A", "--askpass", "-b", "--background", "-B", "--bell", "-E", "--preserve-env", "-H", "--set-home",
			"-k", "--reset-timestamp", "-K", "--remove-timestamp", "-l", "--list", "-n", "--non-interactive",
			"-N", "--no-update", "-P", "--preserve-groups", "-S", "--stdin", "-v", "--validate", "-V", "--version"},
		shell: []string{"-s", "--shell", "-i", "--login", "-e", "--edit"},
	},
	"doas": {args: []string{"-u", "-C"}, flags: []string{"-n", "-L"}, shell: []string{"-s"}},
	"env": {
		args: []string{"-u", "--unset", "-C", "--chdir"},
		flags: []string{"-", "-i", "--ignore-environment", "-0", "--null", "-v", "--debug",
			"--default-signal", "--ignore-signal", "--block-signal", "--list-signal-handling"},
		shell: []string{"-S", "--split-string"},
	},
	"nice":    {args: []string{"-n", "--adjustment"}},
	"ionice":  {args: []string{"-c", "--class", "-n", "--classdata", "-p", "--pid", "-P", "--pgid", "-u", "--uid"}, flags: []string{"-t", "--ignore"}},
	"nohup":   {},
	"timeout": {args: []string{"-s", "--signal", "-k", "--kill-after"}, flags: []string{"--preserve-status", "--foreground", "-v", "--verbose"}},
	// time -o/--output writes a file, so only -p is accepted.
	"time":    {flags: []string{"-p"}},
	"command": {flags: []string{"-p"}},
	"stdbuf":  {args: []string{"-i", "--input", "-o", "--output", "-e", "--error"}},
	"xargs": {
		args: []string{"-I", "--replace", "-n", "--max-args", "-P", "--max-procs", "-L", "--max-lines", "-d", "--delimiter",
			"-E", "-s", "--max-chars", "-a", "--arg-file", "--process-slot-var"},
		flags: []string{"-0", "--null", "-r", "--no-run-if-empty", "-t", "--verbose", "-p", "--interactive",
			"-x", "--exit", "-o", "--open-tty", "--show-limits"},
	},
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// peelWrapperOptions consumes the wrapper's own options and returns the words
// that form the command it runs. ok is false when an option is unknown or
// runs a shell.
func peelWrapperOptions(name string, spec wrapperSpec, words []string) (rest []string, ok bool) {
	for len(words) > 0 {
		w := words[0]
		switch {
		case w == "--":
			words = words[1:]
			return finishWrapper(name, words)
		case name == "env" && isAssignment(w):
			words = words[1:]
			continue
		case name == "nice" && len(w) > 1 && w[0] == '-' && isNumber(w):
			words = words[1:]
			continue
		case name == "command" && (w == "-v" || w == "-V"):
			// command -v only looks the name up; nothing runs.
			return nil, true
		case !strings.HasPrefix(w, "-") || (w == "-" && name != "env"):
			return finishWrapper(name, words)
		}
		words = words[1:]
		if strings.HasPrefix(w, "--") || w == "-" {
			key, _, hasValue := strings.Cut(w, "=")
			switch {
			case contains(spec.shell, key):
				return nil, false
			case contains(spec.args, key):
				if !hasValue {
					if len(words) == 0 {
						return nil, false
					}
					words = words[1:]
				}
			case contains(spec.flags, key):
			default:
				return nil, false
			}
			continue
		}
		// A short option cluster such as -nu root or -n1.
		for i := 1; i < len(w); i++ {
			o := "-" + w[i:i+1]
			if contains(spec.shell, o) {
				return nil, false
			}
			if contains(spec.args, o) {
				if i == len(w)-1 {
					if len(words) == 0 {
						return nil, false
					}
					words = words[1:]
				}
				break
			}
			if !contains(spec.flags, o) {
				return nil, false
			}
		}
	}
	return finishWrapper(name, words)
}

// finishWrapper takes the operands a wrapper reads before the command.
func finishWrapper(name string, words []string) ([]string, bool) {
	if name == "timeout" {
		if len(words) == 0 || !isDuration(words[0]) {
			return nil, false
		}
		words = words[1:]
	}
	return words, true
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
		words, ok = peelWrapperOptions(name, opts, words[1:])
		if !ok {
			return commandAnalysis{risk: "High", usesSudo: true}
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

// isAssignment reports a shell assignment prefix, NAME=value or NAME+=value
// (bash and zsh accept both before a command). Shells only accept ASCII
// names.
func isAssignment(word string) bool {
	eq := strings.IndexByte(word, '=')
	if eq <= 0 {
		return false
	}
	name := strings.TrimSuffix(word[:eq], "+")
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !(c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (i > 0 && c >= '0' && c <= '9')) {
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

// hasArg reports any of values among args. A long option also matches its
// "=value" form and the abbreviations getopt_long accepts (--out for
// --output), so a shortened spelling cannot slip past.
func hasArg(args []string, values ...string) bool {
	for _, a := range args {
		key, _, _ := strings.Cut(a, "=")
		for _, v := range values {
			if a == v {
				return true
			}
			if strings.HasPrefix(v, "--") && strings.HasPrefix(a, "--") && len(key) > 2 && strings.HasPrefix(v, key) {
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
		"pwd", "stat", "which", "grep", "egrep", "fgrep", "uniq", "cut", "nl", "echo", "printf",
		"true", "false", "nproc", "arch", "lsblk", "lscpu", "lsof", "netstat", "printenv",
		"basename", "dirname", "realpath", "readlink", "md5sum", "sha1sum", "sha256sum", "sha512sum",
		"cksum", "tac", "rev", "column", "groups", "users", "who", "w", "last", "vmstat", "iostat",
		"mpstat", "sleep", "test", "tr", "diff", "cmp", "comm", "getent", "dig", "host", "nslookup",
		"ping", "tty", "locale", "sha256", "md5", "exit", "yes", "seq":
		// uniq writes its second operand.
		return !(name == "uniq" && len(nonOptions(args)) > 1)
	case "ss":
		// ss -K / --kill closes sockets and -D / --diag=FILE writes a file.
		return !hasShortFlag(args, 'K') && !hasShortFlag(args, 'D') && !hasArg(args, "--kill", "--diag")
	case "hostname":
		// -F/--file and -b/--boot set the name; -FX and --file=X carry the
		// value in the same word.
		return len(nonOptions(args)) == 0 && !hasShortFlag(args, 'F') && !hasShortFlag(args, 'b') &&
			!hasArg(args, "--file", "--boot")
	case "file":
		// file -C / --compile writes a .mgc file.
		return !hasShortFlag(args, 'C') && !hasArg(args, "--compile")
	case "date":
		return !hasArg(args, "--set") && !dateSetsClock(args)
	case "sort":
		return !hasShortFlag(args, 'o') && !hasArg(args, "--output", "--compress-program")
	case "ps", "pgrep", "pidof":
		return true
	case "top":
		return hasShortFlag(args, 'b')
	case "find":
		return !hasArg(args, "-delete", "-exec", "-execdir", "-ok", "-okdir", "-fprint", "-fprint0", "-fprintf", "-fls")
	case "journalctl":
		return !hasArgPrefix(args, "--vacuum") && !hasArg(args, "--vacuum-size", "--vacuum-time", "--vacuum-files",
			"--rotate", "--flush", "--sync", "--relinquish-var", "--smart-relinquish-var", "--setup-keys", "--update-catalog",
			"--cursor-file")
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
		return readOnlyIP(args)
	}
	return false
}

// ipObjects are the ip objects whose show/list output is read-only.
var ipObjects = map[string]bool{
	"a": true, "addr": true, "address": true, "l": true, "link": true, "r": true, "ro": true, "route": true,
	"n": true, "neigh": true, "neighbor": true, "neighbour": true, "ru": true, "rule": true,
	"maddr": true, "maddress": true, "mr": true, "mroute": true, "tunnel": true, "ntable": true, "netconf": true,
}

// ipOptions are the global options that only change the output format.
// -batch, -force and -netns are not here: -batch runs every command in a
// file, which the classifier cannot see.
var ipOptions = map[string]bool{
	"-4": true, "-6": true, "-0": true, "-s": true, "-stats": true, "-statistics": true, "-d": true, "-details": true,
	"-br": true, "-brief": true, "-c": true, "-color": true, "-j": true, "-json": true, "-p": true, "-pretty": true,
	"-o": true, "-oneline": true, "-h": true, "-human": true, "-human-readable": true, "-r": true, "-resolve": true,
}

// readOnlyIP accepts `ip [format options] OBJECT [show|list|get ...]`. Verbs
// are matched in full because ip accepts abbreviations (`ip r f` is route
// flush), so anything else is not Low.
func readOnlyIP(args []string) bool {
	i := 0
	for ; i < len(args) && strings.HasPrefix(args[i], "-"); i++ {
		if args[i] == "-f" || args[i] == "-family" {
			i++
			continue
		}
		if !ipOptions[args[i]] {
			return false
		}
	}
	if i >= len(args) || !ipObjects[args[i]] {
		return false
	}
	rest := args[i+1:]
	if len(rest) == 0 {
		return true
	}
	switch rest[0] {
	case "show", "list", "lst", "get":
		return true
	}
	return false
}

// dateSetsClock reports -s in a short option cluster (-s, -us) and any
// operand that is not a +FORMAT (date MMDDhhmm[[CC]YY] sets the clock).
// -d, -f and -r take the rest of the cluster, or the next word, as their
// value; -I takes only the rest of the cluster.
func dateSetsClock(args []string) bool {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			for _, op := range args[i+1:] {
				if !strings.HasPrefix(op, "+") {
					return true
				}
			}
			return false
		}
		if strings.HasPrefix(a, "--") {
			key, _, hasValue := strings.Cut(a, "=")
			if !hasValue && len(key) > 2 {
				for _, v := range []string{"--date", "--file", "--reference"} {
					if strings.HasPrefix(v, key) {
						i++
						break
					}
				}
			}
			continue
		}
		if len(a) < 2 || a[0] != '-' {
			if !strings.HasPrefix(a, "+") {
				return true
			}
			continue
		}
		for j := 1; j < len(a); j++ {
			c := a[j]
			if c == 's' {
				return true
			}
			if c == 'I' {
				break
			}
			if strings.IndexByte("dfr", c) >= 0 {
				if j == len(a)-1 {
					i++
				}
				break
			}
		}
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
