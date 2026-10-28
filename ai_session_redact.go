package main

// Secret redaction for AI session text (phase7-ai-session.md §6.3). The AI
// text stream, the per-line records and the audit only ever see redacted
// text; the raw ring for the human viewer stays as it was (memory only).
//
// Detection is line based. A PEM private key block is held from its BEGIN
// line to its END line (at most aiPEMHoldLines lines) and replaced as one
// piece; while it is held, nothing after it is released either, so a reader
// never sees part of a key.

import (
	"bytes"
	"regexp"
	"strings"
)

// aiPEMHoldLines bounds how many lines a PEM block may be held.
var aiPEMHoldLines = 64

// aiPartialLineMax is how long an unterminated line may get before it is
// released (redacted on its own) without waiting for its newline.
var aiPartialLineMax = 4096

type aiRedaction struct {
	Kind string
	Mark string
}

type aiSecretPattern struct {
	kind string
	re   *regexp.Regexp
	// group is the submatch that holds the secret (0 = whole match).
	group int
}

var aiSecretPatterns = []aiSecretPattern{
	{kind: "aws_key", re: regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{kind: "github", re: regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})`)},
	{kind: "slack", re: regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`)},
	{kind: "jwt", re: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}`)},
	{kind: "bearer", re: regexp.MustCompile(`(?i)\b(?:authorization:\s*)?bearer\s+([A-Za-z0-9._~+/=-]{8,})`), group: 1},
	{kind: "url_password", re: regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^\s:/@]+:([^\s@/]+)@`), group: 1},
	{kind: "aws_secret", re: regexp.MustCompile(`(?i)aws_?secret_?access_?key["']?\s*[=:]\s*["']?([A-Za-z0-9/+=]{40})`), group: 1},
	{kind: "password", re: regexp.MustCompile(`(?i)\b(?:password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key|secret[_-]?key|client[_-]?secret)["']?\s*[=:]\s*("[^"\n]*"|'[^'\n]*'|[^\s"',;]+)`), group: 1},
}

var (
	aiPEMBegin = regexp.MustCompile(`-----BEGIN ((?:[A-Z0-9]+ )*PRIVATE KEY(?: BLOCK)?)-----`)
	aiPEMEnd   = regexp.MustCompile(`-----END ((?:[A-Z0-9]+ )*PRIVATE KEY(?: BLOCK)?)-----`)
)

// redactSecrets replaces every secret-looking value in line with
// [REDACTED:<kind>:<sha256 prefix>]. The same value always gets the same
// mark. Marks already present are left alone.
func redactSecrets(line string) (string, []aiRedaction) {
	var found []aiRedaction
	for _, p := range aiSecretPatterns {
		matches := p.re.FindAllStringSubmatchIndex(line, -1)
		if len(matches) == 0 {
			continue
		}
		var b strings.Builder
		prev := 0
		for _, m := range matches {
			start, end := m[2*p.group], m[2*p.group+1]
			if start < 0 {
				continue
			}
			value := line[start:end]
			trimmed := strings.Trim(value, `"'`)
			if trimmed == "" || strings.HasPrefix(trimmed, "[REDACTED:") || strings.Contains(trimmed, "[REDACTED:") {
				continue
			}
			mark := aiRedactionMark(p.kind, trimmed)
			if value != trimmed && len(value) >= 2 {
				mark = value[:1] + mark + value[len(value)-1:]
			}
			b.WriteString(line[prev:start])
			b.WriteString(mark)
			prev = end
			found = append(found, aiRedaction{Kind: p.kind, Mark: mark})
		}
		if prev > 0 {
			b.WriteString(line[prev:])
			line = b.String()
		}
	}
	return line, found
}

// aiRedactFilter is the AI text pipeline: terminal stripping (aiPlainText),
// then line based redaction with the PEM hold (secretLineBuffer in the
// design). It implements aiTextFilter, aiTextFlusher and aiRedactionCounter.
type aiRedactFilter struct {
	strip   *aiPlainText
	partial []byte   // text after the last newline
	pem     []string // held PEM lines (with their newlines)
	count   int
}

func newAIRedactFilter() *aiRedactFilter { return &aiRedactFilter{strip: newAIPlainText()} }

// Feed implements aiTextFilter.
func (f *aiRedactFilter) Feed(p []byte) []byte {
	text := f.strip.Feed(p)
	if len(text) == 0 {
		return nil
	}
	f.partial = append(f.partial, text...)
	var out []byte
	for {
		i := bytes.IndexByte(f.partial, '\n')
		if i < 0 {
			break
		}
		line := string(f.partial[:i+1])
		f.partial = f.partial[i+1:]
		out = append(out, f.line(line)...)
	}
	if len(f.pem) == 0 && len(f.partial) > aiPartialLineMax {
		out = append(out, f.line(string(f.partial))...)
		f.partial = nil
	}
	if len(f.partial) == 0 {
		f.partial = nil
	}
	return out
}

// line handles one complete line (or a forced partial one).
func (f *aiRedactFilter) line(line string) []byte {
	if len(f.pem) > 0 {
		f.pem = append(f.pem, line)
		if aiPEMEnd.MatchString(line) || len(f.pem) >= aiPEMHoldLines {
			return f.releasePEM()
		}
		return nil
	}
	if loc := aiPEMBegin.FindStringIndex(line); loc != nil {
		if aiPEMEnd.MatchString(line[loc[1]:]) {
			// A whole block on one line.
			end := aiPEMEnd.FindStringIndex(line[loc[1]:])
			stop := loc[1] + end[1]
			head, _ := f.redact(line[:loc[0]])
			mark := aiRedactionMark("private_key", line[loc[0]:stop])
			f.count++
			tail, _ := f.redact(line[stop:])
			return []byte(head + mark + tail)
		}
		head, _ := f.redact(line[:loc[0]])
		f.pem = append(f.pem, line[loc[0]:])
		if head == "" {
			return nil
		}
		return []byte(head)
	}
	red, _ := f.redact(line)
	return []byte(red)
}

func (f *aiRedactFilter) redact(s string) (string, []aiRedaction) {
	out, found := redactSecrets(s)
	f.count += len(found)
	return out, found
}

// releasePEM replaces the held block with one mark. A block that never
// ended within aiPEMHoldLines is redacted all the same.
func (f *aiRedactFilter) releasePEM() []byte {
	block := strings.Join(f.pem, "")
	f.pem = nil
	tail := ""
	if loc := aiPEMEnd.FindStringIndex(block); loc != nil {
		tail = block[loc[1]:]
		block = block[:loc[1]]
	} else if strings.HasSuffix(block, "\n") {
		block, tail = strings.TrimSuffix(block, "\n"), "\n"
	}
	f.count++
	rest, _ := f.redact(tail)
	return []byte(aiRedactionMark("private_key", block) + rest)
}

// Flush implements aiTextFlusher: the end of a line releases the partial
// line, and a PEM block that is still open is redacted whole.
func (f *aiRedactFilter) Flush() []byte {
	var out []byte
	if len(f.pem) > 0 {
		if len(f.partial) > 0 {
			f.pem = append(f.pem, string(f.partial))
			f.partial = nil
		}
		out = append(out, f.releasePEM()...)
	}
	if len(f.partial) > 0 {
		red, _ := f.redact(string(f.partial))
		out = append(out, red...)
		f.partial = nil
	}
	return out
}

// Redactions implements aiRedactionCounter.
func (f *aiRedactFilter) Redactions() int { return f.count }
