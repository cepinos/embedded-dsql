// Copyright (c) 2026 MiniStack Contributors. SPDX-License-Identifier: MIT
// Copies or substantial portions, including AI-assisted ports or rewrites, must retain this notice (see LICENSE).
//
// Go port of the SQL lexing primitives of ministack/core/pgproxy.py
// (MiniStack 1.5.9). MiniStack's LICENSE is reproduced in THIRD_PARTY_NOTICES
// at the root of this repository.

package pgproxy

import (
	"regexp"
	"strings"
)

// Everything that scans SQL text (statement splitting, comment stripping,
// parenthesis matching, tokenizing) goes through skipNoise so that literals,
// dollar-quoted bodies, quoted identifiers and comments can never be mistaken
// for structure. Without this a ';' or ')' inside a literal silently truncates
// the text a validation rule sees, and the rule passes on the fragment.

var dollarTagRE = regexp.MustCompile(`^\$(?:[A-Za-z_]\w*)?\$`)

func isSpace(b byte) bool {
	switch b {
	case ' ', '\t', '\n', '\r', '\f', '\v':
		return true
	}
	return false
}

// isAlnum treats every non-ASCII byte as alphanumeric: SQL structure is ASCII,
// so a UTF-8 continuation byte can only belong to an identifier or literal.
func isAlnum(b byte) bool {
	return b >= 0x80 || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

func hasPrefixAt(s string, i int, prefix string) bool {
	return i <= len(s) && strings.HasPrefix(s[i:], prefix)
}

// skipNoise returns the index just past the literal, quoted identifier or
// comment that opens at sql[i], or i unchanged when none does.
func skipNoise(sql string, i int) int {
	n := len(sql)
	ch := sql[i]
	switch ch {
	case '\'':
		// E'...' uses backslash escapes; ordinary literals only double the
		// quote (standard_conforming_strings is on).
		escapes := i > 0 && (sql[i-1] == 'E' || sql[i-1] == 'e') &&
			(i == 1 || !(isAlnum(sql[i-2]) || sql[i-2] == '_'))
		j := i + 1
		for j < n {
			if escapes && sql[j] == '\\' {
				j += 2
				continue
			}
			if sql[j] == '\'' {
				if j+1 < n && sql[j+1] == '\'' {
					j += 2
					continue
				}
				return j + 1
			}
			j++
		}
		return n
	case '"':
		j := i + 1
		for j < n {
			if sql[j] == '"' {
				if j+1 < n && sql[j+1] == '"' {
					j += 2
					continue
				}
				return j + 1
			}
			j++
		}
		return n
	case '$':
		tag := dollarTagRE.FindString(sql[i:])
		if tag == "" {
			return i
		}
		body := i + len(tag)
		closeAt := strings.Index(sql[body:], tag)
		if closeAt < 0 {
			return n
		}
		return body + closeAt + len(tag)
	}
	if hasPrefixAt(sql, i, "--") {
		nl := strings.IndexByte(sql[i:], '\n')
		if nl < 0 {
			return n
		}
		return i + nl + 1
	}
	if hasPrefixAt(sql, i, "/*") {
		depth, j := 0, i
		for j < n {
			switch {
			case hasPrefixAt(sql, j, "/*"):
				depth++
				j += 2
			case hasPrefixAt(sql, j, "*/"):
				depth--
				j += 2
				if depth == 0 {
					return j
				}
			default:
				j++
			}
		}
		return n
	}
	return i
}

// stripLeadingComments drops leading whitespace and comments. Every validation
// rule anchors on the first keyword, so a leading comment would otherwise
// disable the validator wholesale.
func stripLeadingComments(sql string) string {
	i, n := 0, len(sql)
	for i < n {
		if isSpace(sql[i]) {
			i++
			continue
		}
		j := i
		if hasPrefixAt(sql, i, "--") || hasPrefixAt(sql, i, "/*") {
			j = skipNoise(sql, i)
		}
		if j == i {
			break
		}
		i = j
	}
	return sql[i:]
}

// matchParen returns the index of the ')' matching the '(' at start, or -1.
func matchParen(sql string, start int) int {
	depth, i, n := 0, start, len(sql)
	for i < n {
		if j := skipNoise(sql, i); j != i {
			i = j
			continue
		}
		switch sql[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i
			}
		}
		i++
	}
	return -1
}

// findChar returns the index of the next byte in chars outside literals and
// comments, or -1.
func findChar(sql string, i int, chars string) int {
	n := len(sql)
	for i < n {
		if j := skipNoise(sql, i); j != i {
			i = j
			continue
		}
		if strings.IndexByte(chars, sql[i]) >= 0 {
			return i
		}
		i++
	}
	return -1
}

// One SQL identifier: double-quoted (case kept, "" escapes a quote) or bare
// (folded to lower case by the server). identPathPat covers a dotted name in
// any mix of the two, so schema."Tbl".col is one match.
const (
	identPat     = `(?:"(?:[^"]|"")*"|[^\W\d][\w$]*)`
	identPathPat = identPat + `(?:\s*\.\s*` + identPat + `)*`
)

var (
	identRE = regexp.MustCompile(identPat)
	nameRE  = regexp.MustCompile(`^[^\W\d][\w$]*`)
)

// foldIdentifier normalizes one identifier to the name the server stores: a
// quoted identifier keeps its case verbatim (with "" collapsed), a bare one
// folds to lower case. It also reports whether the identifier was quoted.
func foldIdentifier(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if len(raw) >= 2 && raw[0] == '"' && raw[len(raw)-1] == '"' {
		return strings.ReplaceAll(raw[1:len(raw)-1], `""`, `"`), true
	}
	return strings.ToLower(raw), false
}

// identifierPath returns the normalized parts of a dotted identifier.
func identifierPath(raw string) []string {
	matches := identRE.FindAllString(raw, -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		name, _ := foldIdentifier(m)
		out = append(out, name)
	}
	return out
}

const (
	opChars    = "+-*/<>=~!@#%^&|?"
	punctChars = "().,;[]:"
)

type tokenKind int

const (
	tokName tokenKind = iota
	tokQName
	tokStr
	tokOp
	tokPunct
	tokOther
)

type token struct {
	kind tokenKind
	text string
}

// sqlTokens lexes SQL into tokens, dropping whitespace and comments. Kinds:
// name (bare identifier or keyword, folded to lower case), qname (quoted
// identifier, verbatim), str (string or dollar-quoted literal), op (a maximal
// run of operator characters), punct (a single "().,;[]:" character) and
// other (numbers, $1 placeholders, anything else).
func sqlTokens(sql string) []token {
	var out []token
	i, n := 0, len(sql)
	for i < n {
		ch := sql[i]
		if isSpace(ch) {
			i++
			continue
		}
		if j := skipNoise(sql, i); j != i {
			switch ch {
			case '"':
				end := j - 1
				if end < i+1 {
					end = i + 1
				}
				out = append(out, token{tokQName, strings.ReplaceAll(sql[i+1:end], `""`, `"`)})
			case '\'', '$':
				out = append(out, token{tokStr, sql[i:j]})
			}
			i = j
			continue
		}
		if m := nameRE.FindString(sql[i:]); m != "" {
			out = append(out, token{tokName, strings.ToLower(m)})
			i += len(m)
			continue
		}
		if strings.IndexByte(opChars, ch) >= 0 {
			k := i + 1
			for k < n && strings.IndexByte(opChars, sql[k]) >= 0 {
				k++
			}
			out = append(out, token{tokOp, sql[i:k]})
			i = k
			continue
		}
		kind := tokOther
		if strings.IndexByte(punctChars, ch) >= 0 {
			kind = tokPunct
		}
		out = append(out, token{kind, sql[i : i+1]})
		i++
	}
	return out
}

// splitStatements splits a simple-query batch on ';' outside literals,
// comments and quoted names, dropping empty statements.
func splitStatements(sql string) []string {
	var parts []string
	last, i, n := 0, 0, len(sql)
	for i < n {
		if j := skipNoise(sql, i); j != i {
			i = j
			continue
		}
		if sql[i] == ';' {
			parts = append(parts, sql[last:i])
			last = i + 1
		}
		i++
	}
	parts = append(parts, sql[last:])
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if s := strings.TrimSpace(p); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// splitTopLevel splits a parenthesised body on commas at paren depth 0.
func splitTopLevel(body string) []string {
	var parts []string
	depth, last, i, n := 0, 0, 0, len(body)
	for i < n {
		if j := skipNoise(body, i); j != i {
			i = j
			continue
		}
		switch body[i] {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, body[last:i])
				last = i + 1
			}
		}
		i++
	}
	if last < len(body) {
		parts = append(parts, body[last:])
	}
	return parts
}
