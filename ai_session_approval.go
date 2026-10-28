package main

// AI session lines: per-line checks, classification and the approval flow
// (phase7-ai-session.md §3). Everything here is new code; the existing
// approval machinery (command_requests, transitionRequest's CAS, expiry,
// needsHumanApproval) is used unchanged.
//
// Invariant (§3.5): a line reaches the remote side only through
// (*aiLineGate).startLine, and startLine is called only by
//   - sendSessionLine, right after the auto_running request was recorded, and
//   - approveAISessionLine, right after the pending→running CAS succeeded.
// startLine re-checks under the gate lock that the line is still the one in
// flight and the session is still open, so a line that was rejected,
// expired, withdrawn or whose session is closing never gets a byte through.
//
// Lock rules (§5.0 addendum): aiLineGate.mu and aiLineRegistry.mu are leaf
// locks. Neither is held while calling into App (a.mu, transitionRequest,
// needsHumanApproval), the runner (network I/O) or any hook.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// Constants
// ---------------------------------------------------------------------------

const (
	// maxSessionLineBytes is the longest line send_line accepts (§3.1).
	maxSessionLineBytes = 4096
	// aiLineHistoryKeep is how many finished lines a gate remembers for the
	// approval screen (it shows the last aiLineContextPrevious of them).
	aiLineHistoryKeep     = 8
	aiLineContextPrevious = 3
	aiLineContextTail     = 20
	aiLineEnvDisplayMax   = 64
	aiLineEventLineMax    = 200
	aiLineMaxWait         = 60 * time.Second
	aiLineDefaultTimeout  = 30 * time.Second
	aiLineMaxTimeout      = 3600 * time.Second
	decidedBySystem       = "system"
)

// reject_reason values of validateSessionLine (§3.1).
const (
	lineRejectEmpty            = "empty"
	lineRejectTooLong          = "too_long"
	lineRejectInvalidUTF8      = "invalid_utf8"
	lineRejectMultiline        = "multiline"
	lineRejectControlChar      = "control_char"
	lineRejectHeredoc          = "heredoc"
	lineRejectLineContinuation = "line_continuation"
	lineRejectUnbalancedQuote  = "unbalanced_quote"
)

// effect values of sessionLineClass (§3.2).
const (
	lineEffectNone        = "none"
	lineEffectCd          = "cd"
	lineEffectEnv         = "env"
	lineEffectStateChange = "state_change"
)

// session_state values the gate reports (§2.9). "opening" belongs to the
// lifecycle owner, which creates the gate once the transport is up.
const (
	aiGateIdle            = "idle"
	aiGatePendingApproval = "pending_approval"
	aiGateBusy            = "busy"
	aiGateClosing         = "closing"
	aiGateClosed          = "closed"
)

// Phases of the line in flight.
const (
	aiLineReserved = "reserved" // classified, not yet recorded
	aiLinePending  = "pending"  // recorded as pending, waiting for a human
	aiLineRunning  = "running"  // handed to the runner
)

// errAISessionClosed is the cancel cause of a line stopped because its
// session closed. classifyExecError maps it to "canceled" through the
// wrapped context.Canceled.
var errAISessionClosed = errors.New("ai session closed")

// ---------------------------------------------------------------------------
// §3.1 validateSessionLine
// ---------------------------------------------------------------------------

// validateSessionLine returns "" when line may be queued or run, otherwise a
// reject_reason. Rejected lines never enter command_requests: queuing them
// would let a human approve something that cannot be sent safely (a second
// line would break the shell-mode prompt protocol).
func validateSessionLine(line string) string {
	if !utf8.ValidString(line) {
		return lineRejectInvalidUTF8
	}
	if strings.TrimSpace(line) == "" {
		return lineRejectEmpty
	}
	if len(line) > maxSessionLineBytes {
		return lineRejectTooLong
	}
	for _, r := range line {
		switch r {
		case '\n', '\r', ' ', ' ', '\u0085':
			return lineRejectMultiline
		}
	}
	for _, r := range line {
		if r == '\t' {
			continue
		}
		// C0 (Ctrl-C included: it has its own tool), DEL, C1, and format
		// characters such as zero-width spaces and bidi overrides.
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) || unicode.Is(unicode.Cf, r) {
			return lineRejectControlChar
		}
	}
	return scanSessionLine(line)
}

// scanSessionLine is the small lexer behind the heredoc, continuation and
// balance checks. It errs on the side of rejecting: a line it cannot balance
// is refused even if some shell would accept it.
func scanSessionLine(line string) string {
	b := []byte(line)
	var stack []byte // '"', '`', '$' ($( ), '(' , '{'
	top := func() byte {
		if len(stack) == 0 {
			return 0
		}
		return stack[len(stack)-1]
	}
	pop := func() { stack = stack[:len(stack)-1] }
	opEnd := -1 // end of the last unquoted top-level | or &&
	for i := 0; i < len(b); i++ {
		c := b[i]
		var next byte
		if i+1 < len(b) {
			next = b[i+1]
		}
		if top() == '"' {
			switch c {
			case '\\':
				if i+1 >= len(b) {
					return lineRejectLineContinuation
				}
				i++
			case '"':
				pop()
			case '`':
				stack = append(stack, '`')
			case '$':
				switch next {
				case '(':
					stack = append(stack, '$')
					i++
				case '{':
					stack = append(stack, '{')
					i++
				}
			}
			continue
		}
		// Code contexts: top level, $( ), ( ), { } and backticks.
		switch c {
		case '\\':
			if i+1 >= len(b) {
				return lineRejectLineContinuation
			}
			i++
		case '\'':
			j := strings.IndexByte(line[i+1:], '\'')
			if j < 0 {
				return lineRejectUnbalancedQuote
			}
			i += j + 1
		case '"':
			stack = append(stack, '"')
		case '`':
			if top() == '`' {
				pop()
			} else {
				stack = append(stack, '`')
			}
		case '$':
			switch next {
			case '(':
				stack = append(stack, '$')
				i++
			case '{':
				stack = append(stack, '{')
				i++
			case '\'':
				// $'...' (ANSI-C quoting): backslash escapes the quote.
				j := i + 2
				for ; j < len(b); j++ {
					if b[j] == '\\' {
						j++
						continue
					}
					if b[j] == '\'' {
						break
					}
				}
				if j >= len(b) {
					return lineRejectUnbalancedQuote
				}
				i = j
			}
		case '(':
			stack = append(stack, '(')
		case ')':
			if t := top(); t == '$' || t == '(' {
				pop()
			} else {
				return lineRejectUnbalancedQuote
			}
		case '{':
			stack = append(stack, '{')
		case '}':
			if top() != '{' {
				return lineRejectUnbalancedQuote
			}
			pop()
		case '<':
			// <<, <<- and <<< all read more input than this line; $((1<<2))
			// is refused too, which is acceptable.
			if next == '<' {
				return lineRejectHeredoc
			}
		case '|':
			if len(stack) == 0 {
				opEnd = i + 1
				if next == '|' {
					opEnd = i + 2
					i++
				}
			}
		case '&':
			if len(stack) == 0 && next == '&' {
				opEnd = i + 2
				i++
			}
		}
	}
	if len(stack) > 0 {
		return lineRejectUnbalancedQuote
	}
	if opEnd >= 0 && opEnd == len(strings.TrimRight(line, " \t")) {
		return lineRejectLineContinuation
	}
	return ""
}

// ---------------------------------------------------------------------------
// §3.2 classifySessionLine
// ---------------------------------------------------------------------------

// sessionRiskState is what classification needs to know about the session.
type sessionRiskState struct {
	Mode    string            // exec | shell
	Env     map[string]string // the gateway's env overlay (set entries)
	Tainted bool
}

// sessionLineClass is the outcome of classifySessionLine.
type sessionLineClass struct {
	Risk     string   `json:"risk"`
	BaseRisk string   `json:"baseRisk"` // risk.go on its own
	Reasons  []string `json:"reasons"`
	Effect   string   `json:"effect"`
	// CdTarget is the operand of a standalone cd ("" = home, "-" = previous).
	CdTarget string            `json:"cdTarget,omitempty"`
	EnvSet   map[string]string `json:"envSet,omitempty"`
	EnvUnset []string          `json:"envUnset,omitempty"`
	UsesSudo bool              `json:"usesSudo"`
	// CwdPersisted is false for an exec-mode line that changes directory
	// only within itself (cd /tmp && ls).
	CwdPersisted bool   `json:"cwdPersisted"`
	Notice       string `json:"notice,omitempty"`
}

// sessionSafeEnv are the names whose standalone export is Low (§3.2).
var sessionSafeEnv = map[string]bool{
	"LANG": true, "TZ": true, "NO_COLOR": true, "COLUMNS": true, "LINES": true, "TERM": true,
}

// sessionDangerousEnv are names that make later commands load or run other
// code. The spec's list (§3.2) plus a few with the same effect (HOME and
// XDG_CONFIG_HOME pick the dotfiles that can name pagers and hooks, ZDOTDIR,
// GCONV_PATH, LOCPATH, NLSPATH, TERMINFO*).
var sessionDangerousEnv = map[string]bool{
	"PATH": true, "BASH_ENV": true, "ENV": true, "PROMPT_COMMAND": true, "IFS": true, "CDPATH": true,
	"SHELLOPTS": true, "BASHOPTS": true, "HISTFILE": true, "PAGER": true, "MANPAGER": true,
	"EDITOR": true, "VISUAL": true, "LESSOPEN": true, "LESSCLOSE": true, "RUBYOPT": true,
	"NODE_OPTIONS": true, "SSH_ASKPASS": true, "SUDO_ASKPASS": true, "TMPDIR": true,
	"HOME": true, "XDG_CONFIG_HOME": true, "ZDOTDIR": true, "GCONV_PATH": true, "LOCPATH": true,
	"NLSPATH": true, "TERMINFO": true, "TERMINFO_DIRS": true, "INPUTRC": true,
}

var sessionDangerousEnvPrefixes = []string{"LD_", "DYLD_", "GIT_", "PYTHON", "PERL5", "BASH_FUNC_", "LESS"}

// sessionEnvNameClass is "safe", "dangerous" or "other".
func sessionEnvNameClass(name string) string {
	upper := strings.ToUpper(name)
	if sessionDangerousEnv[upper] {
		return "dangerous"
	}
	for _, p := range sessionDangerousEnvPrefixes {
		if strings.HasPrefix(upper, p) {
			return "dangerous"
		}
	}
	if len(upper) == 3 && strings.HasPrefix(upper, "PS") && upper[2] >= '0' && upper[2] <= '4' {
		return "dangerous"
	}
	if sessionSafeEnv[name] || strings.HasPrefix(name, "LC_") {
		return "safe"
	}
	return "other"
}

// envOverlayUnsafe reports whether the overlay sets any name that is not
// safe; while it does, every line is at least Medium (§3.2).
func envOverlayUnsafe(env map[string]string) bool {
	for name := range env {
		if sessionEnvNameClass(name) != "safe" {
			return true
		}
	}
	return false
}

// sessionStateBuiltins change the shell itself: in shell mode their effect
// outlives the line (§3.2 row 5).
var sessionStateBuiltins = map[string]bool{
	"alias": true, "unalias": true, "set": true, "shopt": true, "trap": true, "enable": true,
	"builtin": true, "hash": true, "bind": true, "complete": true, "compopt": true, "declare": true,
	"typeset": true, "readonly": true, "local": true, "stty": true, "ulimit": true, "umask": true,
	"pushd": true, "popd": true, "exec": true, "source": true, ".": true, "eval": true, "let": true,
	"function": true,
}

// sessionDetaching outlive the line or the session (§3.2 row 6).
var sessionDetaching = map[string]bool{
	"setsid": true, "disown": true, "screen": true, "tmux": true, "at": true, "batch": true,
	"systemd-run": true, "nohup": true,
}

// sessionReservedWords may precede a command in a compound statement.
var sessionReservedWords = map[string]bool{
	"if": true, "then": true, "else": true, "elif": true, "fi": true, "do": true, "done": true,
	"while": true, "until": true, "for": true, "in": true, "case": true, "esac": true, "!": true,
	"time": true, "select": true, "coproc": true, "[[": true, "]]": true,
}

var sessionFuncDef = regexp.MustCompile(`(^|[;&|(){}\s])[A-Za-z_][A-Za-z0-9_.:-]*\s*\(\s*\)`)

// classifySessionLine adds the session-specific rules to analyzeCommand.
// It only ever raises the risk, except for the two forms the gateway runs
// itself (standalone cd, and export/unset of safe names), which are Low.
func classifySessionLine(line string, st sessionRiskState) sessionLineClass {
	line = strings.TrimSpace(line)
	base := analyzeCommand(line)
	c := sessionLineClass{
		Risk:         base.risk,
		BaseRisk:     base.risk,
		Effect:       lineEffectNone,
		UsesSudo:     commandUsesSudo(line),
		CwdPersisted: true,
	}
	c.addReason("base:" + base.risk)

	fields := strings.Fields(line)
	if !opaqueCommand(line) {
		segments := splitSegments(line)
		if len(segments) == 1 && len(segments[0]) == len(fields) {
			words := segments[0]
			if target, ok := standaloneCd(words); ok {
				c.Risk = "Low"
				c.Effect = lineEffectCd
				c.CdTarget = target
				c.addReason("cd_builtin")
				return c.withSessionFloor(st)
			}
			if set, unset, ok := standaloneEnv(words); ok {
				c.Risk = "Low"
				c.Effect = lineEffectEnv
				c.EnvSet = set
				c.EnvUnset = unset
				c.addReason("env_overlay")
				names := make([]string, 0, len(set)+len(unset))
				for name := range set {
					names = append(names, name)
				}
				names = append(names, unset...)
				for _, name := range names {
					switch sessionEnvNameClass(name) {
					case "dangerous":
						c.raise("High", "dangerous_env")
					case "other":
						c.raise("Medium", "env_overlay_unsafe")
					}
				}
				return c.withSessionFloor(st)
			}
		}
	}

	stateChange, detaches, dangerousEnv, hasCd := scanSessionCommands(line, st.Mode == aiModeShell)
	if dangerousEnv {
		c.raise("High", "dangerous_env")
	}
	if detaches {
		c.raise("High", "detaches")
	}
	if stateChange {
		c.raise("High", "shell_state_change")
		c.Effect = lineEffectStateChange
	}
	if hasCd && st.Mode != aiModeShell {
		c.CwdPersisted = false
		c.Notice = "cd inside a compound line applies only to that line; send cd on its own line to change the session directory"
	}
	return c.withSessionFloor(st)
}

func (c *sessionLineClass) addReason(reason string) {
	if !slices.Contains(c.Reasons, reason) {
		c.Reasons = append(c.Reasons, reason)
	}
}

func (c *sessionLineClass) raise(risk string, reason string) {
	if riskRank[risk] > riskRank[c.Risk] {
		c.Risk = risk
	}
	c.addReason(reason)
}

func (c sessionLineClass) withSessionFloor(st sessionRiskState) sessionLineClass {
	if envOverlayUnsafe(st.Env) {
		c.raise("Medium", "env_overlay_unsafe")
	}
	if st.Tainted {
		c.addReason("session_tainted")
	}
	return c
}

// standaloneCd matches "cd", "cd -" and "cd <plain word>".
func standaloneCd(words []string) (string, bool) {
	if len(words) == 0 || words[0] != "cd" || len(words) > 2 {
		return "", false
	}
	if len(words) == 1 {
		return "", true
	}
	arg := words[1]
	if arg == "-" {
		return arg, true
	}
	if strings.HasPrefix(arg, "-") || strings.ContainsAny(arg, "*?[]") {
		return "", false
	}
	return arg, true
}

// standaloneEnv matches "export N=v ...", "N=v ..." and "unset N ...". The
// gateway re-quotes the values when it replays them, so only values whose
// meaning does not depend on the shell are accepted (no ~ or globs; quotes
// and $ are already excluded by opaqueCommand).
func standaloneEnv(words []string) (map[string]string, []string, bool) {
	if len(words) == 0 {
		return nil, nil, false
	}
	switch words[0] {
	case "export":
		if len(words) < 2 {
			return nil, nil, false
		}
		set, ok := parseAssignments(words[1:])
		return set, nil, ok
	case "unset":
		if len(words) < 2 {
			return nil, nil, false
		}
		var unset []string
		for _, name := range words[1:] {
			if !validEnvName(name) {
				return nil, nil, false
			}
			unset = append(unset, name)
		}
		return nil, unset, true
	}
	set, ok := parseAssignments(words)
	return set, nil, ok
}

func parseAssignments(words []string) (map[string]string, bool) {
	set := map[string]string{}
	for _, w := range words {
		eq := strings.IndexByte(w, '=')
		if eq <= 0 || !validEnvName(w[:eq]) || !plainEnvValue(w[eq+1:]) {
			return nil, false
		}
		set[w[:eq]] = w[eq+1:]
	}
	return set, true
}

func validEnvName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		if !(r == '_' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (i > 0 && r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}

func plainEnvValue(v string) bool {
	return !strings.ContainsAny(v, "~*?[]")
}

// scanSessionCommands looks at every word in command position, including
// inside groups, subshells and substitutions. Single-quoted text is literal
// and skipped. In shell mode a command name the gateway cannot read
// (built from quotes or expansions) counts as a state change: it might be a
// builtin.
func scanSessionCommands(line string, shell bool) (stateChange, detaches, dangerousEnv, hasCd bool) {
	blanked := blankSingleQuoted(line)
	if sessionFuncDef.MatchString(blanked) {
		stateChange = true
	}
	splitter := strings.NewReplacer(
		"$(", " ; ", "${", " ; ", "&&", " ; ", "||", " ; ",
		";", " ; ", "|", " ; ", "&", " ; ", "(", " ; ", ")", " ; ",
		"{", " ; ", "}", " ; ", "`", " ; ",
	)
	for _, chunk := range strings.Split(splitter.Replace(blanked), ";") {
		words := strings.Fields(chunk)
		for len(words) > 0 && sessionReservedWords[words[0]] {
			words = words[1:]
		}
		// Assignments and wrappers in front of the command.
		assigned, wrapped := false, false
		for len(words) > 0 {
			w := words[0]
			if isAssignment(w) {
				if sessionEnvNameClass(w[:strings.IndexByte(w, '=')]) == "dangerous" {
					dangerousEnv = true
				}
				assigned = true
				words = words[1:]
				continue
			}
			name := commandWord(w)
			if sessionDetaching[name] {
				detaches = true
			}
			opts, ok := wrappers[name]
			if !ok {
				break
			}
			wrapped = true
			words = words[1:]
			for len(words) > 0 && (strings.HasPrefix(words[0], "-") || (name == "env" && isAssignment(words[0])) || (name == "timeout" && isDuration(words[0])) || (name == "nice" && isNumber(words[0]))) {
				if name == "env" && isAssignment(words[0]) && sessionEnvNameClass(words[0][:strings.IndexByte(words[0], '=')]) == "dangerous" {
					dangerousEnv = true
				}
				if opts[words[0]] && len(words) > 1 {
					words = words[1:]
				}
				words = words[1:]
			}
		}
		if len(words) == 0 {
			// A bare assignment (N="v", N=$x) the standalone parser could
			// not take changes the shell's variables: the gateway no longer
			// knows the session's environment.
			if shell && assigned && !wrapped {
				stateChange = true
			}
			continue
		}
		raw := words[0]
		name := commandWord(raw)
		if name == "" {
			continue
		}
		args := words[1:]
		if sessionVarDeclarers[name] {
			declared := false
			for _, a := range args {
				a = unquoteWord(a)
				if a == "" || strings.HasPrefix(a, "-") {
					continue
				}
				declared = true
				n := a
				if eq := strings.IndexByte(a, '='); eq >= 0 {
					n = a[:eq]
				}
				if sessionEnvNameClass(n) == "dangerous" {
					dangerousEnv = true
				}
			}
			if shell && declared {
				stateChange = true
			}
		}
		switch {
		case sessionDetaching[name]:
			detaches = true
		case sessionStateBuiltins[name]:
			stateChange = true
		case name == "export" && hasArgPrefix(args, "-"):
			stateChange = true
		case name == "unset" && hasArgPrefix(args, "-f"):
			stateChange = true
		case shell && strings.ContainsAny(name, "$`"):
			stateChange = true
		}
		if name == "cd" || name == "pushd" || name == "popd" {
			hasCd = true
		}
	}
	return
}

// sessionVarDeclarers set or export variables through their arguments.
var sessionVarDeclarers = map[string]bool{
	"export": true, "declare": true, "typeset": true, "readonly": true, "local": true,
}

// unquoteWord removes quote and backslash characters (a rough reading of
// "N=v", 'N'=v and N\=v that is enough to find the variable name).
func unquoteWord(w string) string {
	return strings.NewReplacer("'", "", "\"", "", "\\", "").Replace(w)
}

// commandWord removes quoting so 'alias', \alias and al""ias are all alias.
func commandWord(w string) string {
	w = strings.NewReplacer("'", "", "\"", "", "\\", "").Replace(w)
	if i := strings.LastIndexByte(w, '/'); i >= 0 && i < len(w)-1 {
		w = w[i+1:]
	}
	return w
}

// blankSingleQuoted replaces the contents of '...' with x so separators in
// literal text do not start new commands for scanSessionCommands.
func blankSingleQuoted(line string) string {
	b := []byte(line)
	in := false
	for i := range b {
		switch {
		case b[i] == '\'':
			in = !in
		case in:
			b[i] = 'x'
		}
	}
	return string(b)
}

// ---------------------------------------------------------------------------
// §3.4 sessionNeedsApproval
// ---------------------------------------------------------------------------

// normalizeSessionBypassMaxRisk clamps AISessionSettings.BypassMaxRisk to
// Low (default) or Medium. High is never allowed.
func normalizeSessionBypassMaxRisk(risk string) string {
	if risk == "Medium" {
		return "Medium"
	}
	return "Low"
}

// sessionNeedsApproval reports the session-only conditions that queue a line
// even when needsHumanApproval would let it run. It only ever adds approval.
func sessionNeedsApproval(st sessionRiskState, c sessionLineClass, bypassMaxRisk string) bool {
	if c.Risk == "High" || c.Effect == lineEffectStateChange {
		return true
	}
	if riskRank[c.Risk] > riskRank[normalizeSessionBypassMaxRisk(bypassMaxRisk)] {
		return true
	}
	if st.Tainted {
		return true
	}
	if c.Effect == lineEffectEnv {
		for name := range c.EnvSet {
			if sessionEnvNameClass(name) != "safe" {
				return true
			}
		}
		for _, name := range c.EnvUnset {
			if sessionEnvNameClass(name) != "safe" {
				return true
			}
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Runner: what the gate needs from the transport
// ---------------------------------------------------------------------------

// aiLinePlan is one line as the runner must send it. In exec mode the
// runner builds the stdin script from Cwd and the overlay (buildExecScript);
// in shell mode it writes Line to the PTY and waits for the prompt marker.
type aiLinePlan struct {
	RequestID string
	SessionID string
	Seq       int
	Line      string
	Mode      string
	Cwd       string
	Env       map[string]string
	Unset     []string
	// CdProbe asks the runner to report the directory after the line
	// (exec mode: the SGCWD marker; shell mode: whatever it tracks).
	CdProbe bool
	Timeout time.Duration
}

// aiLineOutcome is what the runner reports. Stdout and Stderr are the AI
// text of this line (escape sequences stripped, secrets redacted).
type aiLineOutcome struct {
	ExitCode int
	Stdout   string
	Stderr   string
	// NewCwd is the directory after the line, when known.
	NewCwd   string
	Duration time.Duration
	// CursorStart / CursorEnd bound the line in the AI text stream
	// (CursorStart is before the gateway echo).
	CursorStart uint64
	CursorEnd   uint64
}

// aiLineRunner sends one approved line and waits for it. A non-zero exit is
// reported in ExitCode with a nil error; an error means the line did not
// complete (transport failure, ctx cancelled or timed out).
type aiLineRunner interface {
	RunLine(ctx context.Context, plan aiLinePlan) (aiLineOutcome, error)
}

// aiLineHooks connect the gate to the lifecycle owner, storage and output.
// All are optional and are called without any lock held.
type aiLineHooks struct {
	// Event records an ai_session_events row (line_rejected, tainted, ...).
	Event func(kind string, detail map[string]any)
	// LineQueued persists the ai_session_lines row before the request is
	// recorded. An error aborts the send with code storage.
	LineQueued func(rec aiLineRecord) error
	// LineFinished updates the ai_session_lines row (cursor_end etc.).
	LineFinished func(rec aiLineRecord)
	// OutputTail returns the last n lines of redacted AI text.
	OutputTail func(n int) string
	// CloseSession asks the owner to close the whole session (for example
	// target_changed). When nil the gate settles its own line and closes.
	CloseSession func(reason string)
	// Changed is called when the gate's state changes (aisessions:changed).
	Changed func()
	// Cursor is the current end of the AI text stream (cursor_before).
	Cursor func() uint64
}

// ---------------------------------------------------------------------------
// aiLineGate: the per-session line state
// ---------------------------------------------------------------------------

// aiLineRecord is a line as it was queued: the context shown on the
// approval screen is exactly the context it runs with, because only one
// line is in flight and session state changes only when it finishes.
type aiLineRecord struct {
	RequestID string            `json:"requestId"`
	SessionID string            `json:"sessionId"`
	Seq       int               `json:"seq"`
	Line      string            `json:"line"`
	Mode      string            `json:"mode"`
	Cwd       string            `json:"cwd"`
	Env       map[string]string `json:"env"`
	Unset     []string          `json:"unset"`
	Tainted   bool              `json:"tainted"`
	Class     sessionLineClass  `json:"class"`
	Phase     string            `json:"phase"`
	State     string            `json:"state"`
	ExitCode  *int              `json:"exitCode,omitempty"`
	QueuedAt  time.Time         `json:"queuedAt"`
	Timeout   time.Duration     `json:"-"`
	// CursorStart is the text cursor when the line was queued (the agent's
	// cursor_before); CursorEnd is set when it finished.
	CursorStart uint64 `json:"cursorStart"`
	CursorEnd   uint64 `json:"cursorEnd"`
}

// aiLineGateConfig is what the lifecycle owner knows when it opens a
// session.
type aiLineGateConfig struct {
	SessionID string
	Owner     string // caller.Agent of open_session
	ConnID    string
	TargetFP  string
	Mode      string
	Cwd       string
	ExpiresAt time.Time
	Runner    aiLineRunner
	Hooks     aiLineHooks
	// Now is the clock (a.clock); nil means time.Now.
	Now func() time.Time
}

// aiLineGate owns the line state of one AI session: cwd, env overlay,
// taint, sequence numbers and the single line in flight. The lifecycle
// owner (aisession.go) keeps one per session.
type aiLineGate struct {
	id       string
	owner    string
	connID   string
	targetFP string
	mode     string
	runner   aiLineRunner
	hooks    aiLineHooks
	now      func() time.Time

	mu          sync.Mutex
	state       string
	closeReason string
	cwd         string
	env         map[string]string
	unset       map[string]bool
	tainted     bool
	seq         int
	inflight    *aiLineRecord
	cancelRun   context.CancelCauseFunc
	runDone     chan struct{}
	history     []aiLineRecord
	expiresAt   time.Time
	lastActive  time.Time
}

func newAILineGate(cfg aiLineGateConfig) *aiLineGate {
	mode := cfg.Mode
	if mode != aiModeShell {
		mode = aiModeExec
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &aiLineGate{
		now:        now,
		id:         cfg.SessionID,
		owner:      cfg.Owner,
		connID:     cfg.ConnID,
		targetFP:   cfg.TargetFP,
		mode:       mode,
		runner:     cfg.Runner,
		hooks:      cfg.Hooks,
		state:      aiGateIdle,
		cwd:        cfg.Cwd,
		env:        map[string]string{},
		unset:      map[string]bool{},
		expiresAt:  cfg.ExpiresAt,
		lastActive: now(),
	}
}

// runDoneChan is closed when the running line has recorded its outcome
// (nil when no line is running).
func (g *aiLineGate) runDoneChan() <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.runDone == nil {
		return nil
	}
	return g.runDone
}

// record finds a line in flight or in the history.
func (g *aiLineGate) record(requestID string) (aiLineRecord, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.inflight != nil && g.inflight.RequestID == requestID {
		return *g.inflight, true
	}
	for i := len(g.history) - 1; i >= 0; i-- {
		if g.history[i].RequestID == requestID {
			return g.history[i], true
		}
	}
	return aiLineRecord{}, false
}

func (g *aiLineGate) riskStateLocked() sessionRiskState {
	return sessionRiskState{Mode: g.mode, Env: cloneEnv(g.env), Tainted: g.tainted}
}

func cloneEnv(in map[string]string) map[string]string {
	out := maps.Clone(in)
	if out == nil {
		out = map[string]string{}
	}
	return out
}

func (g *aiLineGate) unsetListLocked() []string {
	out := make([]string, 0, len(g.unset))
	for name := range g.unset {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// aiLineBusyError is session_busy: another line is pending or running.
type aiLineBusyError struct {
	RequestID string
	Phase     string
}

func (e *aiLineBusyError) Error() string {
	return fmt.Sprintf("session is busy with %s (%s)", e.RequestID, e.Phase)
}

// aiLineClosedError is session_closed.
type aiLineClosedError struct{ Reason string }

func (e *aiLineClosedError) Error() string { return "session is closed: " + e.Reason }

// reserve classifies line against the current state and makes it the line
// in flight. Classification happens under the lock so no other line can
// change cwd, env or taint between the decision and the run.
func (g *aiLineGate) reserve(requestID string, line string, now time.Time, timeout time.Duration) (aiLineRecord, error) {
	var cursor uint64
	if g.hooks.Cursor != nil {
		cursor = g.hooks.Cursor()
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.state == aiGateClosing || g.state == aiGateClosed {
		return aiLineRecord{}, &aiLineClosedError{Reason: g.closeReason}
	}
	if g.inflight != nil {
		return aiLineRecord{}, &aiLineBusyError{RequestID: g.inflight.RequestID, Phase: g.inflight.Phase}
	}
	st := g.riskStateLocked()
	g.seq++
	rec := aiLineRecord{
		RequestID: requestID,
		SessionID: g.id,
		Seq:       g.seq,
		Line:      line,
		Mode:      g.mode,
		Cwd:       g.cwd,
		Env:       st.Env,
		Unset:     g.unsetListLocked(),
		Tainted:   g.tainted,
		Class:     classifySessionLine(line, st),
		Phase:     aiLineReserved,
		QueuedAt:  now,
		Timeout:   timeout,

		CursorStart: cursor,
	}
	g.inflight = &rec
	g.lastActive = now
	return rec, nil
}

// release drops a reserved line that was never recorded.
func (g *aiLineGate) release(requestID string) {
	g.mu.Lock()
	if g.inflight != nil && g.inflight.RequestID == requestID && g.inflight.Phase == aiLineReserved {
		g.inflight = nil
		g.seq--
		if g.state != aiGateClosing && g.state != aiGateClosed {
			g.state = aiGateIdle
		}
	}
	g.mu.Unlock()
}

// markPending records that the reserved line is now a pending request. It
// reports false when the session started closing meanwhile.
func (g *aiLineGate) markPending(requestID string) bool {
	g.mu.Lock()
	ok := g.inflight != nil && g.inflight.RequestID == requestID && g.inflight.Phase == aiLineReserved &&
		g.state != aiGateClosing && g.state != aiGateClosed
	if ok {
		g.inflight.Phase = aiLinePending
		g.state = aiGatePendingApproval
	}
	g.mu.Unlock()
	g.changed()
	return ok
}

// beginDelivery is the last check before bytes go out: the line is still the
// one in flight, it was not delivered before, and the session is open.
func (g *aiLineGate) beginDelivery(requestID string, cancel context.CancelCauseFunc) (aiLineRecord, chan struct{}, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.state == aiGateClosing || g.state == aiGateClosed || g.inflight == nil || g.inflight.RequestID != requestID {
		return aiLineRecord{}, nil, false
	}
	if g.inflight.Phase != aiLineReserved && g.inflight.Phase != aiLinePending {
		return aiLineRecord{}, nil, false
	}
	g.inflight.Phase = aiLineRunning
	g.state = aiGateBusy
	g.cancelRun = cancel
	g.runDone = make(chan struct{})
	g.lastActive = g.now()
	return *g.inflight, g.runDone, true
}

// finishLine applies the line's effect on the session and frees the slot.
// It reports whether the session became tainted by this line.
func (g *aiLineGate) finishLine(rec aiLineRecord, delivered bool, outcome aiLineOutcome, ok bool, final CommandRequest) (aiLineRecord, bool) {
	g.mu.Lock()
	newlyTainted := false
	if delivered {
		switch rec.Class.Effect {
		case lineEffectCd:
			if ok && outcome.NewCwd != "" {
				g.cwd = outcome.NewCwd
			}
		case lineEffectEnv:
			if ok {
				for name, value := range rec.Class.EnvSet {
					g.env[name] = value
					delete(g.unset, name)
				}
				for _, name := range rec.Class.EnvUnset {
					delete(g.env, name)
					g.unset[name] = true
				}
			}
		case lineEffectStateChange:
			// The shell may keep part of the change even if the line
			// failed, so taint on delivery, not on success.
			if g.mode == aiModeShell && !g.tainted {
				g.tainted = true
				newlyTainted = true
			}
		}
		if g.mode == aiModeShell && rec.Class.Effect != lineEffectCd && outcome.NewCwd != "" {
			g.cwd = outcome.NewCwd
		}
	}
	rec.State = final.Status
	rec.ExitCode = final.ExitCode
	if outcome.CursorEnd > 0 {
		rec.CursorEnd = outcome.CursorEnd
	}
	if g.inflight != nil && g.inflight.RequestID == rec.RequestID {
		g.inflight = nil
		g.cancelRun = nil
		if g.runDone != nil {
			close(g.runDone)
			g.runDone = nil
		}
	}
	g.history = append(g.history, rec)
	if len(g.history) > aiLineHistoryKeep {
		g.history = g.history[len(g.history)-aiLineHistoryKeep:]
	}
	if g.state != aiGateClosing && g.state != aiGateClosed {
		g.state = aiGateIdle
	}
	g.lastActive = g.now()
	g.mu.Unlock()
	if newlyTainted {
		g.event("tainted", map[string]any{"seq": rec.Seq, "request_id": rec.RequestID})
	}
	g.changed()
	return rec, newlyTainted
}

// dropPending frees the slot of a pending line that ended without running
// (rejected, expired, withdrawn).
func (g *aiLineGate) dropPending(requestID string, status string) bool {
	g.mu.Lock()
	if g.inflight == nil || g.inflight.RequestID != requestID || g.inflight.Phase != aiLinePending {
		g.mu.Unlock()
		return false
	}
	rec := *g.inflight
	rec.State = status
	g.inflight = nil
	g.history = append(g.history, rec)
	if len(g.history) > aiLineHistoryKeep {
		g.history = g.history[len(g.history)-aiLineHistoryKeep:]
	}
	if g.state != aiGateClosing && g.state != aiGateClosed {
		g.state = aiGateIdle
	}
	g.mu.Unlock()
	if g.hooks.LineFinished != nil {
		g.hooks.LineFinished(rec)
	}
	g.changed()
	return true
}

// beginClose marks the gate closing (once) and returns the line in flight.
func (g *aiLineGate) beginClose(reason string) (rec *aiLineRecord, cancel context.CancelCauseFunc, done chan struct{}, first bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.state == aiGateClosing || g.state == aiGateClosed {
		return nil, nil, nil, false
	}
	g.state = aiGateClosing
	g.closeReason = reason
	if g.inflight != nil {
		copied := *g.inflight
		rec = &copied
	}
	return rec, g.cancelRun, g.runDone, true
}

func (g *aiLineGate) markClosed() {
	g.mu.Lock()
	g.state = aiGateClosed
	g.mu.Unlock()
	g.changed()
}

func (g *aiLineGate) event(kind string, detail map[string]any) {
	if g.hooks.Event != nil {
		g.hooks.Event(kind, detail)
	}
}

func (g *aiLineGate) changed() {
	if g.hooks.Changed != nil {
		g.hooks.Changed()
	}
}

// touch records owner activity (send_line, read_output, interrupt).
func (g *aiLineGate) touch(now time.Time) {
	g.mu.Lock()
	g.lastActive = now
	g.mu.Unlock()
}

// aiLineGateView is a consistent copy of the gate for callers outside it.
type aiLineGateView struct {
	SessionID    string
	Owner        string
	ConnID       string
	TargetFP     string
	Mode         string
	State        string
	CloseReason  string
	Cwd          string
	Env          map[string]string
	Unset        []string
	Tainted      bool
	Seq          int
	Inflight     *aiLineRecord
	History      []aiLineRecord
	ExpiresAt    time.Time
	LastActivity time.Time
}

func (g *aiLineGate) view() aiLineGateView {
	g.mu.Lock()
	defer g.mu.Unlock()
	v := aiLineGateView{
		SessionID: g.id, Owner: g.owner, ConnID: g.connID, TargetFP: g.targetFP, Mode: g.mode,
		State: g.state, CloseReason: g.closeReason, Cwd: g.cwd, Env: cloneEnv(g.env),
		Unset: g.unsetListLocked(), Tainted: g.tainted, Seq: g.seq,
		History: slices.Clone(g.history), ExpiresAt: g.expiresAt, LastActivity: g.lastActive,
	}
	if g.inflight != nil {
		copied := *g.inflight
		v.Inflight = &copied
	}
	return v
}

// ---------------------------------------------------------------------------
// Request ↔ session links
// ---------------------------------------------------------------------------

// aiLineLink ties a command_requests row to its AI session.
type aiLineLink struct {
	SessionID string
	Seq       int
	// gate is nil for links loaded from storage whose session is gone; such
	// a line can never be approved.
	gate *aiLineGate
	rec  aiLineRecord
}

type aiLineRegistry struct {
	mu    sync.Mutex
	links map[string]aiLineLink
}

// aiLineRegistries holds one registry per App. The integrator may move it
// into App (aiSessionState) and replace aiLines() with a field access.
var aiLineRegistries sync.Map // *App → *aiLineRegistry

func (a *App) aiLines() *aiLineRegistry {
	if v, ok := aiLineRegistries.Load(a); ok {
		return v.(*aiLineRegistry)
	}
	v, _ := aiLineRegistries.LoadOrStore(a, &aiLineRegistry{links: map[string]aiLineLink{}})
	return v.(*aiLineRegistry)
}

func (a *App) putAILineLink(link aiLineLink) {
	reg := a.aiLines()
	reg.mu.Lock()
	reg.links[link.rec.RequestID] = link
	reg.mu.Unlock()
}

// markAISessionLinesKnown registers request IDs loaded from ai_session_lines
// at startup (loadAISessionLinks). Their session is gone, so approve refuses
// them instead of running them as plain commands.
func (a *App) markAISessionLinesKnown(links map[string]aiLineLink) {
	reg := a.aiLines()
	reg.mu.Lock()
	defer reg.mu.Unlock()
	for id, link := range links {
		if _, exists := reg.links[id]; exists {
			continue
		}
		link.gate = nil
		link.rec.RequestID = id
		link.rec.SessionID = link.SessionID
		link.rec.Seq = link.Seq
		reg.links[id] = link
	}
}

// aiLineLink reports whether requestID is a line of an AI session. approve()
// branches on it (§3.6) so a session line is never executed as a plain
// command.
func (a *App) aiLineLink(requestID string) (aiLineLink, bool) {
	reg := a.aiLines()
	reg.mu.Lock()
	defer reg.mu.Unlock()
	link, ok := reg.links[strings.TrimSpace(requestID)]
	return link, ok
}

// aiLineOpen reports whether requestID belongs to a session that is still
// open; DeleteCommandRequest refuses those (§7.3).
func (a *App) aiLineOpen(requestID string) bool {
	link, ok := a.aiLineLink(requestID)
	if !ok || link.gate == nil {
		return false
	}
	state := link.gate.view().State
	return state != aiGateClosed
}

// reconcileAISessionLine frees the session slot when its pending line was
// decided elsewhere (RejectCommand, expiry, deletion). The approval code
// needs no callback for those: the session checks the request each time.
func (a *App) reconcileAISessionLine(g *aiLineGate) {
	v := g.view()
	if v.Inflight == nil || v.Inflight.Phase != aiLinePending {
		return
	}
	req, found := a.findRequest(v.Inflight.RequestID)
	switch {
	case !found:
		g.dropPending(v.Inflight.RequestID, stateWithdrawn)
	case req.Status != statePending:
		if req.Status == stateRunning {
			return // approveAISessionLine is about to deliver it
		}
		g.dropPending(v.Inflight.RequestID, req.Status)
	case !a.clock().Before(requestExpiresAt(req)):
		a.expireRequest(req.ID)
		if latest, ok := a.findRequest(req.ID); ok && latest.Status != statePending {
			g.dropPending(req.ID, latest.Status)
		}
	}
}

// ---------------------------------------------------------------------------
// Errors returned to the MCP layer
// ---------------------------------------------------------------------------

// aiLineError is a tool error: Code is the §2 error code and Extra goes
// into structuredContent.
type aiLineError struct {
	Code    string
	Message string
	Extra   map[string]any
}

func (e *aiLineError) Error() string { return e.Code + ": " + e.Message }

func newAILineError(code string, message string, extra map[string]any) *aiLineError {
	return &aiLineError{Code: code, Message: message, Extra: extra}
}

// ---------------------------------------------------------------------------
// §2.3 send_line core
// ---------------------------------------------------------------------------

// aiSendLineInput is send_line after argument parsing (mcp_aisession.go).
type aiSendLineInput struct {
	Line            string
	Reason          string
	ClientRequestID string
	ForceQueue      bool
	TimeoutSeconds  int
	Wait            time.Duration
	// BypassMaxRisk is AISessionSettings.BypassMaxRisk.
	BypassMaxRisk string
}

// aiSendLineResult is what send_line reports.
type aiSendLineResult struct {
	Request          CommandRequest
	Class            sessionLineClass
	Seq              int
	Deduplicated     bool
	RequiresApproval bool
	Cwd              string
	SessionState     string
}

// sendSessionLine runs the §2.3 steps for one line: owner and state checks,
// validateSessionLine, classifySessionLine, needsHumanApproval plus
// sessionNeedsApproval, then either queueing or auto-running it.
func (a *App) sendSessionLine(ctx context.Context, caller mcpCaller, g *aiLineGate, in aiSendLineInput) (aiSendLineResult, *aiLineError) {
	if caller.Agent == "" {
		return aiSendLineResult{}, newAILineError("agent_token_required", "AI sessions need the agent's own token", nil)
	}
	if g == nil {
		return aiSendLineResult{}, newAILineError("session_not_found", "session not found", nil)
	}
	if caller.Agent != g.owner {
		return aiSendLineResult{}, newAILineError("not_owner", "this session belongs to another agent", nil)
	}
	in.ClientRequestID = strings.TrimSpace(in.ClientRequestID)
	if in.ClientRequestID == "" {
		return aiSendLineResult{}, newAILineError("invalid_params", "client_request_id is required for send_line", nil)
	}
	if len(in.ClientRequestID) > maxClientRequestIDLen {
		return aiSendLineResult{}, newAILineError("invalid_params", fmt.Sprintf("client_request_id must be at most %d bytes", maxClientRequestIDLen), nil)
	}
	if in.TimeoutSeconds < 0 || in.TimeoutSeconds > 3600 {
		return aiSendLineResult{}, newAILineError("invalid_params", "timeout_seconds must be 1-3600", nil)
	}
	line := strings.TrimSpace(in.Line)
	now := a.clock()
	g.touch(now)

	unlock := a.mcpRuntime.lockKey(caller.Agent + "\x00" + in.ClientRequestID)
	defer func() { unlock() }()
	if existing, ok := a.findRequestByClientID(caller.Agent, in.ClientRequestID); ok {
		link, linked := a.aiLineLink(existing.ID)
		if !linked || link.SessionID != g.id || existing.Command != line {
			return aiSendLineResult{}, newAILineError("idempotency_conflict",
				"client_request_id was already used for a different line or session", map[string]any{"request_id": existing.ID})
		}
		v := g.view()
		return aiSendLineResult{
			Request: existing, Class: link.rec.Class, Seq: link.Seq, Deduplicated: true,
			RequiresApproval: !strings.HasPrefix(existing.DecidedBy, "bypass:"),
			Cwd:              v.Cwd, SessionState: v.State,
		}, nil
	}

	a.reconcileAISessionLine(g)
	v := g.view()
	if v.State == aiGateClosing || v.State == aiGateClosed {
		return aiSendLineResult{}, newAILineError("session_closed", "session is closed", map[string]any{"close_reason": v.CloseReason})
	}
	if v.Inflight != nil {
		return aiSendLineResult{}, busyLineError(v.Inflight.RequestID, v.Inflight.Phase)
	}
	if reason := validateSessionLine(in.Line); reason != "" {
		g.event("line_rejected", map[string]any{"line": truncateRunes(in.Line, aiLineEventLineMax), "reason": reason})
		return aiSendLineResult{}, newAILineError("line_rejected", "line rejected: "+reason, map[string]any{"reject_reason": reason})
	}

	conn, ok := a.connectionByID(g.connID)
	if !ok {
		a.closeAISessionFromLine(g, "connection_deleted")
		return aiSendLineResult{}, newAILineError("session_closed", "the session's connection was deleted", map[string]any{"close_reason": "connection_deleted"})
	}
	if targetFingerprint(conn) != g.targetFP {
		a.closeAISessionFromLine(g, "target_changed")
		return aiSendLineResult{}, newAILineError("target_changed", "the connection's user@host:port changed; the session was closed", nil)
	}

	timeout := timeoutSeconds(conn.CommandTimeout, int(aiLineDefaultTimeout/time.Second))
	if in.TimeoutSeconds > 0 {
		timeout = time.Duration(in.TimeoutSeconds) * time.Second
	}
	timeout = min(timeout, aiLineMaxTimeout)

	// The request ID is fixed before reserving so the gate and the record
	// agree on it; the risk is filled in from the classification.
	draft := newCommandRequest(conn, line, in.Reason, caller.Agent, "High", statePending)
	rec, err := g.reserve(draft.ID, line, now, timeout)
	if err != nil {
		var busy *aiLineBusyError
		if errors.As(err, &busy) {
			return aiSendLineResult{}, busyLineError(busy.RequestID, busy.Phase)
		}
		var closed *aiLineClosedError
		errors.As(err, &closed)
		return aiSendLineResult{}, newAILineError("session_closed", "session is closed", map[string]any{"close_reason": closed.Reason})
	}
	draft.SessionID, draft.SessionSeq = g.id, rec.Seq
	class := rec.Class
	st := sessionRiskState{Mode: rec.Mode, Env: rec.Env, Tainted: rec.Tainted}
	needs := a.needsHumanApproval(conn, class.Risk, line, caller, caller.Agent, in.ForceQueue) ||
		sessionNeedsApproval(st, class, in.BypassMaxRisk)

	if g.hooks.LineQueued != nil {
		if err := g.hooks.LineQueued(rec); err != nil {
			g.release(rec.RequestID)
			return aiSendLineResult{}, newAILineError("storage", err.Error(), nil)
		}
	}

	if needs {
		req := draft
		req.Risk = class.Risk
		req.ClientRequestID = in.ClientRequestID
		// A line cannot outlive its session waiting for approval.
		expires := now.Add(pendingRequestTTL)
		if !v.ExpiresAt.IsZero() && v.ExpiresAt.Before(expires) {
			expires = v.ExpiresAt
		}
		req.ExpiresAt = expires.Format(time.RFC3339)
		a.putAILineLink(aiLineLink{SessionID: g.id, Seq: rec.Seq, gate: g, rec: rec})
		if err := a.recordCommandRequest(req); err != nil {
			g.release(rec.RequestID)
			return aiSendLineResult{}, newAILineError("storage", err.Error(), nil)
		}
		if !g.markPending(req.ID) {
			// The session closed between reserve and record: withdraw the
			// line so it does not wait for an approval that cannot run.
			a.withdrawAISessionRequest(req.ID)
			g.dropPendingOnClose(req.ID, stateWithdrawn)
			v := g.view()
			return aiSendLineResult{}, newAILineError("session_closed", "session is closed", map[string]any{"close_reason": v.CloseReason})
		}
		after := g.view()
		return aiSendLineResult{
			Request: req, Class: class, Seq: rec.Seq, RequiresApproval: true,
			Cwd: after.Cwd, SessionState: after.State,
		}, nil
	}

	// Auto-run. Registered with beginExec first so DisconnectConnection
	// cancels it and shutdown waits for its outcome.
	if !a.mcpRuntime.acquireExec() {
		g.release(rec.RequestID)
		return aiSendLineResult{}, newAILineError("server_busy", fmt.Sprintf("too many commands running (max %d); retry later or use force_queue", maxMCPExec), nil)
	}
	execCtx, finishExec, accepted := a.beginExec(a.appCtx, conn.ID)
	release := func() {
		finishExec()
		a.mcpRuntime.releaseExec()
	}
	if !accepted {
		release()
		g.release(rec.RequestID)
		return aiSendLineResult{}, newAILineError("shutting_down", "ssh-gete is shutting down", nil)
	}
	req := draft
	req.Status = stateAutoRunning
	req.ExpiresAt = ""
	req.Risk = class.Risk
	req.ApprovedRisk = class.Risk
	req.ExecutedCommand = line
	req.DecidedBy = "bypass:" + caller.Agent
	req.DecidedAt = req.RequestedAt
	req.ClientRequestID = in.ClientRequestID
	a.putAILineLink(aiLineLink{SessionID: g.id, Seq: rec.Seq, gate: g, rec: rec})
	if err := a.recordCommandRequest(req); err != nil {
		release()
		g.release(rec.RequestID)
		return aiSendLineResult{}, newAILineError("storage", err.Error(), nil)
	}
	unlock()

	done := make(chan CommandRequest, 1)
	go func() {
		defer release()
		done <- g.startLine(a, execCtx, req)
	}()
	wait := min(max(in.Wait, 0), aiLineMaxWait)
	final := req
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case final = <-done:
	case <-timer.C:
	case <-ctx.Done():
	}
	if final.Status == stateAutoRunning {
		if latest, ok := a.findRequest(req.ID); ok {
			final = latest
		}
	}
	after := g.view()
	return aiSendLineResult{
		Request: final, Class: class, Seq: rec.Seq, RequiresApproval: false,
		Cwd: after.Cwd, SessionState: after.State,
	}, nil
}

func busyLineError(requestID string, phase string) *aiLineError {
	state := statePending
	if phase == aiLineRunning || phase == aiLineReserved {
		state = stateRunning
	}
	return newAILineError("session_busy", "another line of this session is pending or running",
		map[string]any{"request_id": requestID, "state": state})
}

// ---------------------------------------------------------------------------
// §3.5 startLine — the only path to the remote side
// ---------------------------------------------------------------------------

// startLine hands req to the runner and records the outcome. req must have
// just won the CAS to running (approval) or been recorded as auto_running
// (bypass); anything else is recorded as canceled without sending a byte.
// execCtx is the context from beginExec.
func (g *aiLineGate) startLine(a *App, execCtx context.Context, req CommandRequest) CommandRequest {
	approved := req.Status == stateRunning
	if req.Status != stateRunning && req.Status != stateAutoRunning {
		return req
	}
	runCtx, cancel := context.WithCancelCause(execCtx)
	defer cancel(nil)
	rec, _, ok := g.beginDelivery(req.ID, cancel)
	if !ok {
		final := a.finishAISessionRequest(req, failedState(approved), aiLineOutcome{}, fmt.Errorf("%w: %w", errAISessionClosed, context.Canceled))
		if v := g.view(); v.Inflight != nil && v.Inflight.RequestID == req.ID {
			g.finishLine(*v.Inflight, false, aiLineOutcome{}, false, final)
		}
		return final
	}

	var outcome aiLineOutcome
	var err error
	started := time.Now()
	if rec.Class.Effect == lineEffectEnv && rec.Mode == aiModeExec {
		// The gateway keeps the overlay; nothing runs remotely (§1.1).
		outcome = aiLineOutcome{ExitCode: 0}
	} else if g.runner == nil {
		err = errors.New("ai session has no transport")
	} else {
		lineCtx, lineCancel := context.WithTimeout(runCtx, max(rec.Timeout, time.Second))
		outcome, err = g.runner.RunLine(lineCtx, aiLinePlan{
			RequestID: rec.RequestID, SessionID: rec.SessionID, Seq: rec.Seq, Line: rec.Line,
			Mode: rec.Mode, Cwd: rec.Cwd, Env: rec.Env, Unset: rec.Unset,
			CdProbe: rec.Class.Effect == lineEffectCd, Timeout: rec.Timeout,
		})
		lineCancel()
		if err != nil && runCtx.Err() != nil {
			cause := context.Cause(runCtx)
			if errors.Is(context.Cause(a.appCtx), errShuttingDown) {
				cause = errShuttingDown
			}
			if isStopCause(cause) || errors.Is(cause, errAISessionClosed) {
				err = fmt.Errorf("%w: %w", cause, err)
			}
		}
	}
	if outcome.Duration == 0 {
		outcome.Duration = time.Since(started)
	}
	state := succeededState(approved)
	if err != nil || outcome.ExitCode != 0 {
		state = failedState(approved)
	}
	final := a.finishAISessionRequest(req, state, outcome, err)
	if final.ID == "" {
		final = req
		final.Status = state
	}
	finished, _ := g.finishLine(rec, true, outcome, err == nil && outcome.ExitCode == 0, final)
	if g.hooks.LineFinished != nil {
		g.hooks.LineFinished(finished)
	}
	return final
}

func succeededState(approved bool) string {
	if approved {
		return stateSucceeded
	}
	return stateAutoSucceeded
}

func failedState(approved bool) string {
	if approved {
		return stateFailed
	}
	return stateAutoFailed
}

// finishAISessionRequest is finishRequest for a session line: the exit code
// comes from the runner instead of an *ssh.ExitError.
func (a *App) finishAISessionRequest(running CommandRequest, state string, outcome aiLineOutcome, runErr error) CommandRequest {
	if errors.Is(runErr, errShuttingDown) {
		state = stateInterrupted
	}
	a.mu.RLock()
	outputCap := a.mcp.MaxOutputKB * 1024
	a.mu.RUnlock()
	if outputCap <= 0 {
		outputCap = defaultOutputCap
	}
	updated, err := a.transitionRequest(running.ID, []string{running.Status}, func(r *CommandRequest) {
		r.Status = state
		r.Stdout = truncateUTF8(outcome.Stdout, outputCap)
		r.Stderr = truncateUTF8(outcome.Stderr, outputCap)
		r.FinishedAt = nowRFC3339()
		if outcome.Duration > 0 {
			r.Duration = outcome.Duration.Round(100 * time.Millisecond).String()
		}
		if runErr == nil {
			code := outcome.ExitCode
			r.ExitCode = &code
			if code != 0 {
				r.ErrorKind = "exit_nonzero"
			}
			return
		}
		r.ErrorKind = aiLineErrorKind(runErr)
		if r.Stderr != "" {
			r.Stderr += "\n"
		}
		r.Stderr += execErrorMarker + runErr.Error()
	})
	if err != nil {
		log.Printf("finish ai session line %s: %v", running.ID, err)
	}
	return updated
}

// aiLineErrorKind is error_kind for a session line that did not complete.
func aiLineErrorKind(err error) string {
	switch {
	case errors.Is(err, errAIIncompleteLine):
		return aiErrorKindIncomplete
	case errors.Is(err, errAITransportBusy):
		return aiErrorKindTransportBusy
	case errors.Is(err, errAICwdGone):
		return aiErrorKindCwdGone
	case errors.Is(err, errAILineTimeout):
		return aiErrorKindTimeout
	case errors.Is(err, errAISessionClosed), errors.Is(err, errAITransportClosed):
		return aiErrorKindCanceled
	}
	return classifyExecError(err)
}

// truncateUTF8 cuts s to at most n bytes on a rune boundary.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// ---------------------------------------------------------------------------
// §3.6 approveAISessionLine
// ---------------------------------------------------------------------------

// approveAISessionLine is approve() for a session line. approve() branches
// here first: if link, ok := a.aiLineLink(id); ok { return
// a.approveAISessionLine(id, command, grantBypass, link) }.
func (a *App) approveAISessionLine(id string, command string, grantBypass bool, link aiLineLink) ActionResult {
	id = strings.TrimSpace(id)
	command = strings.TrimSpace(command)
	req, ok := a.findRequest(id)
	if !ok {
		return ActionResult{OK: false, Message: "承認対象のリクエストが見つかりません"}
	}
	// 1. Session lines cannot be edited at approval time: the agent's
	// context (cwd, previous lines) was built around the line it sent.
	if command != "" && command != req.Command {
		return ActionResult{OK: false, Code: "edit_not_allowed", Message: "AI セッション内の行は修正して承認できません。拒否して、エージェントに送り直させてください"}
	}
	if req.Status != statePending {
		return ActionResult{OK: false, Code: "not_pending", Message: fmt.Sprintf("%s は既に処理済みです（%s）", id, req.Status)}
	}
	g := link.gate
	if !a.clock().Before(requestExpiresAt(req)) {
		a.expireRequest(id)
		if g != nil {
			a.reconcileAISessionLine(g)
		}
		return ActionResult{OK: false, Code: "expired", Message: fmt.Sprintf("%s は承認期限（%s）を過ぎたため失効しました", id, requestExpiresAt(req).Format(time.RFC3339))}
	}
	// 2. The session must be open and this must be its line in flight.
	if g == nil {
		return ActionResult{OK: false, Code: "session_closed", Message: "この行の AI セッションは終了しています。拒否してください"}
	}
	v := g.view()
	if v.State == aiGateClosing || v.State == aiGateClosed {
		return ActionResult{OK: false, Code: "session_closed", Message: fmt.Sprintf("この行の AI セッションは終了しています（%s）", v.CloseReason)}
	}
	if v.Inflight == nil || v.Inflight.RequestID != id || v.Inflight.Phase == aiLineRunning {
		return ActionResult{OK: false, Code: "session_closed", Message: "この行は AI セッションの処理中の行ではありません。拒否してください"}
	}
	// 3. Same user@host:port as when the session opened.
	conn, connOK := a.connectionByID(v.ConnID)
	if !connOK {
		return ActionResult{OK: false, Code: "session_closed", Message: "AI セッションの接続先が削除されています。拒否してください"}
	}
	if fp := targetFingerprint(conn); fp != v.TargetFP || (req.TargetFP != "" && req.TargetFP != fp) {
		a.closeAISessionFromLine(g, "target_changed")
		return ActionResult{OK: false, Code: "target_changed", Message: fmt.Sprintf("セッション開始後に実行先が変わったため承認できません（%s → %s）。セッションを終了しました", v.TargetFP, fp)}
	}
	agentName := strings.TrimSpace(req.RequestedBy)
	if grantBypass {
		if agentName == "" {
			return ActionResult{OK: false, Message: "承認省略にはエージェント名が必要です"}
		}
		if !a.GetMCPSettings().BypassAllowed && !a.agentHasToken(agentName) {
			return ActionResult{OK: false, Code: "bearer_required", Message: "承認省略を使うには、先にMCP設定でBearerトークンまたはエージェント専用トークンを発行してください（名乗りだけでは本人確認ができないため）"}
		}
	}

	// 4. Register the run so DisconnectConnection and shutdown see it.
	execCtx, finishExec, accepted := a.beginExec(a.appCtx, conn.ID)
	defer finishExec()
	if !accepted {
		return ActionResult{OK: false, Code: "shutting_down", Message: "終了処理中のため承認できません"}
	}
	// 5. CAS pending→running; only the winner delivers the line.
	running, err := a.transitionRequest(id, []string{statePending}, func(r *CommandRequest) {
		r.Status = stateRunning
		r.ExecutedCommand = r.Command
		r.ApprovedRisk = r.Risk
		r.DecidedBy = decidedByGUI
		r.DecidedAt = nowRFC3339()
	})
	if errors.Is(err, errStateConflict) {
		a.reconcileAISessionLine(g)
		return ActionResult{OK: false, Code: "not_pending", Message: fmt.Sprintf("%s は既に処理済みです（%s）", id, running.Status)}
	}
	if err != nil {
		return ActionResult{OK: false, Message: fmt.Sprintf("%s の状態を更新できません: %v", id, err)}
	}

	bypassNote := ""
	if grantBypass {
		if result := a.setAgentApprovalBypass(agentName, true); result.OK {
			bypassNote = fmt.Sprintf("、%s の承認省略を許可しました（AI セッションでは、上限を超える行・汚染されたセッションの行・安全でない環境変数の行は引き続き承認が必要です）", agentName)
		} else {
			bypassNote = "（承認省略の保存に失敗: " + result.Message + "）"
		}
	}

	// 6. Deliver and record the outcome.
	final := g.startLine(a, execCtx, running)
	switch final.Status {
	case stateSucceeded:
		return ActionResult{OK: true, Message: fmt.Sprintf("%s を承認し、AI セッションで実行しました%s", id, bypassNote)}
	case stateInterrupted:
		return ActionResult{OK: false, Message: fmt.Sprintf("%s は終了処理のため中断されました%s", id, bypassNote)}
	case stateFailed:
		if final.ErrorKind == "exit_nonzero" {
			return ActionResult{OK: true, Message: fmt.Sprintf("%s を承認し、AI セッションで実行しました（終了コード %d）%s", id, derefExit(final.ExitCode), bypassNote)}
		}
		return ActionResult{OK: false, Message: fmt.Sprintf("%s の実行に失敗しました（%s）%s", id, final.ErrorKind, bypassNote)}
	}
	return ActionResult{OK: true, Message: fmt.Sprintf("%s を承認しました%s", id, bypassNote)}
}

func derefExit(code *int) int {
	if code == nil {
		return -1
	}
	return *code
}

// ---------------------------------------------------------------------------
// Closing: settle the line in flight
// ---------------------------------------------------------------------------

// settleAISessionLines is step 1-2 of closeAISession (§5) for the line
// state: mark the gate closing, withdraw a pending line and cancel a
// running one, then wait up to grace for the run to record its outcome. The
// lifecycle owner then closes the transport and calls finishAISessionClose.
// It reports whether this call started the close.
func (a *App) settleAISessionLines(g *aiLineGate, reason string, grace time.Duration) bool {
	rec, cancel, done, first := g.beginClose(reason)
	if !first {
		return false
	}
	g.changed()
	if rec == nil {
		return true
	}
	switch rec.Phase {
	case aiLinePending, aiLineReserved:
		a.withdrawAISessionRequest(rec.RequestID)
		status := stateWithdrawn
		if latest, ok := a.findRequest(rec.RequestID); ok {
			status = latest.Status
		}
		// A request that just won the CAS to running is left to startLine,
		// which sees the gate closing and records it canceled.
		if status != stateRunning && status != stateAutoRunning {
			g.dropPendingOnClose(rec.RequestID, status)
		}
	case aiLineRunning:
		if cancel != nil {
			cause := errAISessionClosed
			if reason == "app_shutdown" {
				cause = errShuttingDown
			}
			cancel(cause)
		}
		if done != nil && grace > 0 {
			select {
			case <-done:
			case <-time.After(grace):
			}
		}
	}
	return true
}

// withdrawAISessionRequest moves a pending session line to withdrawn
// (DecidedBy=system) through the CAS.
func (a *App) withdrawAISessionRequest(id string) {
	_, err := a.transitionRequest(id, []string{statePending}, func(r *CommandRequest) {
		r.Status = stateWithdrawn
		r.DecidedBy = decidedBySystem
		r.DecidedAt = nowRFC3339()
		r.FinishedAt = r.DecidedAt
	})
	if err != nil && !errors.Is(err, errStateConflict) && err.Error() != "request not found" {
		log.Printf("withdraw ai session line %s: %v", id, err)
	}
}

// dropPendingOnClose frees a reserved or pending slot during close.
func (g *aiLineGate) dropPendingOnClose(requestID string, status string) {
	g.mu.Lock()
	if g.inflight == nil || g.inflight.RequestID != requestID || g.inflight.Phase == aiLineRunning {
		g.mu.Unlock()
		return
	}
	rec := *g.inflight
	rec.State = status
	g.inflight = nil
	g.history = append(g.history, rec)
	g.mu.Unlock()
	if g.hooks.LineFinished != nil {
		g.hooks.LineFinished(rec)
	}
}

// finishAISessionClose marks the gate closed after the transport is gone.
func (a *App) finishAISessionClose(g *aiLineGate) {
	g.markClosed()
}

// closeAISessionFromLine closes the session for a reason found while
// handling a line (target_changed, connection_deleted).
func (a *App) closeAISessionFromLine(g *aiLineGate, reason string) {
	if g.hooks.CloseSession != nil {
		g.hooks.CloseSession(reason)
		return
	}
	if a.settleAISessionLines(g, reason, 2*time.Second) {
		a.finishAISessionClose(g)
	}
}

// ---------------------------------------------------------------------------
// §3.7 approval screen context
// ---------------------------------------------------------------------------

// AISessionEnvView is one overlay entry as the approval screen shows it.
type AISessionEnvView struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Masked bool   `json:"masked"`
	Unsafe bool   `json:"unsafe"`
	Unset  bool   `json:"unset"`
}

// AISessionLineSummary is a previous line of the session.
type AISessionLineSummary struct {
	Seq      int    `json:"seq"`
	Line     string `json:"line"`
	State    string `json:"state"`
	ExitCode *int   `json:"exitCode,omitempty"`
}

// AISessionLineContext is what the approval screen shows for a session
// line (GetAISessionLineContext). The command is read-only.
type AISessionLineContext struct {
	RequestID     string                 `json:"requestId"`
	Found         bool                   `json:"found"`
	SessionID     string                 `json:"sessionId"`
	Agent         string                 `json:"agent"`
	Mode          string                 `json:"mode"`
	Seq           int                    `json:"seq"`
	Line          string                 `json:"line"`
	Cwd           string                 `json:"cwd"`
	CwdPersisted  bool                   `json:"cwdPersisted"`
	Env           []AISessionEnvView     `json:"env"`
	Tainted       bool                   `json:"tainted"`
	Risk          string                 `json:"risk"`
	BaseRisk      string                 `json:"baseRisk"`
	Effect        string                 `json:"effect"`
	Reasons       []string               `json:"reasons"`
	Notice        string                 `json:"notice,omitempty"`
	SessionState  string                 `json:"sessionState"`
	CloseReason   string                 `json:"closeReason,omitempty"`
	PreviousLines []AISessionLineSummary `json:"previousLines"`
	OutputTail    string                 `json:"outputTail"`
	Editable      bool                   `json:"editable"`
	Message       string                 `json:"message,omitempty"`
}

// GetAISessionLineContext is the Wails binding the approval screen calls for
// a request with a sessionId. It never touches the network.
func (a *App) GetAISessionLineContext(requestID string) AISessionLineContext {
	requestID = strings.TrimSpace(requestID)
	out := AISessionLineContext{RequestID: requestID, Env: []AISessionEnvView{}, Reasons: []string{}, PreviousLines: []AISessionLineSummary{}}
	link, ok := a.aiLineLink(requestID)
	if !ok {
		out.Message = "AI セッションの行ではありません"
		return out
	}
	rec := link.rec
	out.Found = true
	out.SessionID = link.SessionID
	out.Seq = link.Seq
	out.Line = rec.Line
	out.Mode = rec.Mode
	out.Cwd = rec.Cwd
	out.CwdPersisted = rec.Class.CwdPersisted
	out.Tainted = rec.Tainted
	out.Risk = rec.Class.Risk
	out.BaseRisk = rec.Class.BaseRisk
	out.Effect = rec.Class.Effect
	out.Notice = rec.Class.Notice
	if rec.Class.Reasons != nil {
		out.Reasons = slices.Clone(rec.Class.Reasons)
	}
	out.Env = envViews(rec.Env, rec.Unset)
	if req, found := a.findRequest(requestID); found {
		out.Agent = req.RequestedBy
	}
	if link.gate == nil {
		out.SessionState = aiGateClosed
		out.Message = "この行の AI セッションは終了しています"
		return out
	}
	v := link.gate.view()
	out.SessionState = v.State
	out.CloseReason = v.CloseReason
	if out.Agent == "" {
		out.Agent = v.Owner
	}
	var previous []AISessionLineSummary
	for _, h := range v.History {
		if h.Seq < rec.Seq {
			previous = append(previous, AISessionLineSummary{Seq: h.Seq, Line: h.Line, State: h.State, ExitCode: h.ExitCode})
		}
	}
	if len(previous) > aiLineContextPrevious {
		previous = previous[len(previous)-aiLineContextPrevious:]
	}
	if previous != nil {
		out.PreviousLines = previous
	}
	if link.gate.hooks.OutputTail != nil {
		out.OutputTail = link.gate.hooks.OutputTail(aiLineContextTail)
	}
	return out
}

// envViews lists the overlay for display: values are cut to 64 characters
// and hidden when the name looks like it holds a secret.
func envViews(env map[string]string, unset []string) []AISessionEnvView {
	names := make([]string, 0, len(env))
	for name := range env {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]AISessionEnvView, 0, len(names)+len(unset))
	for _, name := range names {
		view := AISessionEnvView{Name: name, Unsafe: sessionEnvNameClass(name) != "safe"}
		if secretLikeEnvName(name) {
			view.Value = "[REDACTED]"
			view.Masked = true
		} else {
			view.Value = env[name]
			if utf8.RuneCountInString(view.Value) > aiLineEnvDisplayMax {
				view.Value = truncateRunes(view.Value, aiLineEnvDisplayMax) + "…"
			}
		}
		out = append(out, view)
	}
	for _, name := range unset {
		out = append(out, AISessionEnvView{Name: name, Unset: true, Unsafe: sessionEnvNameClass(name) != "safe"})
	}
	return out
}

func secretLikeEnvName(name string) bool {
	upper := strings.ToUpper(name)
	for _, marker := range []string{"TOKEN", "SECRET", "PASSWORD", "PASSWD", "KEY", "CREDENTIAL", "AUTH"} {
		if strings.Contains(upper, marker) {
			return true
		}
	}
	return false
}
