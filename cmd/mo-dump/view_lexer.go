// Copyright 2026 Matrix Origin
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"fmt"
	"strings"

	"github.com/matrixorigin/matrixone/pkg/sql/parsers/dialect"
	"github.com/matrixorigin/matrixone/pkg/sql/parsers/dialect/mysql"
)

func tokenizeViewSQL(sql string, mode viewSQLMode) ([]viewToken, error) {
	tokens, err := lexViewSQL(sql, mode)
	if err != nil {
		return nil, err
	}
	var opens []int
	for i, token := range tokens {
		if token.symbol(";") {
			if i != len(tokens)-1 {
				return nil, fmt.Errorf("expected one CREATE VIEW statement")
			}
			tokens = tokens[:i]
			break
		}
		if token.symbol("(") || token.symbol("{") {
			opens = append(opens, i)
		} else if token.symbol(")") || token.symbol("}") {
			if len(opens) == 0 {
				return nil, fmt.Errorf("unbalanced view query delimiters")
			}
			open := opens[len(opens)-1]
			if token.symbol(")") != tokens[open].symbol("(") {
				return nil, fmt.Errorf("unbalanced view query delimiters")
			}
			tokens[open].close = i
			opens = opens[:len(opens)-1]
		}
	}
	if len(opens) != 0 {
		return nil, fmt.Errorf("unbalanced view query delimiters")
	}
	return tokens, nil
}

// Adapt SQL-mode-sensitive quoted tokens only at token boundaries. Everything
// unquoted is consumed by the dialect lexer in one maximal token: in particular,
// neither a digit prefix nor an internal $ can split or invalidate an ID.
func lexViewSQL(sql string, mode viewSQLMode) ([]viewToken, error) {
	scanner := mysql.NewScanner(dialect.MYSQL, sql)
	defer mysql.PutScanner(scanner)
	var tokens []viewToken
	for pos := 0; pos < len(sql); {
		if strings.ContainsRune(" \t\r\n\f", rune(sql[pos])) {
			pos++
			continue
		}
		lineComment := strings.HasPrefix(sql[pos:], "//") || strings.HasPrefix(sql[pos:], "--") && (pos+2 == len(sql) || strings.ContainsRune(" \t\r\n", rune(sql[pos+2])))
		if sql[pos] == '#' || lineComment {
			end := strings.IndexByte(sql[pos:], '\n')
			if end < 0 {
				break
			}
			pos += end + 1
			continue
		}
		if strings.HasPrefix(sql[pos:], "/*") {
			close := strings.Index(sql[pos+2:], "*/")
			if close < 0 {
				return nil, fmt.Errorf("unterminated view comment")
			}
			end := pos + close + 4
			if strings.HasPrefix(sql[pos:], "/*!") {
				body := pos + 3
				for body < end-2 && sql[body] >= '0' && sql[body] <= '9' {
					body++
				}
				inner, err := lexViewSQL(sql[body:end-2], mode)
				if err != nil {
					return nil, err
				}
				tokens = append(tokens, inner...)
			}
			pos = end
			continue
		}
		if sql[pos] == '\'' || sql[pos] == '"' || sql[pos] == '`' {
			token, next, err := viewQuotedToken(sql, pos, mode)
			if err != nil {
				return nil, err
			}
			tokens = append(tokens, token)
			pos = next
			continue
		}
		// A dollar string can begin only here, never inside the token the
		// lexer consumes below. Honor escape mode when finding its end tag.
		if sql[pos] == '$' {
			tagEnd := pos + 1
			for tagEnd < len(sql) && viewTagLetter(sql[tagEnd]) {
				tagEnd++
			}
			if tagEnd < len(sql) && sql[tagEnd] == '$' {
				tag := sql[pos : tagEnd+1]
				next, err := viewDollarEnd(sql, tagEnd+1, tag, mode)
				if err != nil {
					return nil, err
				}
				tokens = append(tokens, viewToken{kind: mysql.STRING, close: -1})
				pos = next
				continue
			}
		}
		scanner.Pos = pos
		kind, value := scanner.Scan()
		if kind == mysql.LEX_ERROR || scanner.Pos <= pos {
			return nil, fmt.Errorf("cannot tokenize view definition")
		}
		if kind == 0 {
			break
		}
		raw := sql[pos:scanner.Pos]
		if kind < 256 {
			value = string(rune(kind))
		}
		// Character shape identifies keyword spellings only; the lexer's ID
		// and QUOTE_ID identities take precedence over character heuristics.
		word := kind == mysql.ID || kind == mysql.QUOTE_ID
		if !word && kind != mysql.STRING && kind != mysql.AT_ID && kind != mysql.AT_AT_ID && len(raw) != 0 {
			word = raw[0] >= 'A' && raw[0] <= 'Z' || raw[0] >= 'a' && raw[0] <= 'z' || raw[0] == '_'
		}
		tokens = append(tokens, viewToken{kind: kind, text: value, word: word, quoted: kind == mysql.QUOTE_ID, punctuation: kind < 256, close: -1})
		pos = scanner.Pos
	}
	return tokens, nil
}

func viewQuotedToken(sql string, start int, mode viewSQLMode) (viewToken, int, error) {
	quote := sql[start]
	identifier := quote == '`' || quote == '"' && mode.ansiQuotes
	var name strings.Builder
	for pos := start + 1; pos < len(sql); pos++ {
		if sql[pos] == quote {
			if pos+1 < len(sql) && sql[pos+1] == quote {
				if identifier {
					name.WriteByte(quote)
				}
				pos++
				continue
			}
			if identifier {
				return viewToken{kind: mysql.QUOTE_ID, text: name.String(), quoted: true, word: true, close: -1}, pos + 1, nil
			}
			return viewToken{kind: mysql.STRING, close: -1}, pos + 1, nil
		}
		if !identifier && !mode.noBackslashEscapes && sql[pos] == '\\' && pos+1 < len(sql) {
			pos++
		}
		if identifier {
			name.WriteByte(sql[pos])
		}
	}
	return viewToken{}, start, fmt.Errorf("unterminated view quoted value")
}

func viewTagLetter(ch byte) bool {
	return ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '_' || ch >= 128
}

func viewDollarEnd(sql string, pos int, tag string, mode viewSQLMode) (int, error) {
	for pos < len(sql) {
		if !mode.noBackslashEscapes && sql[pos] == '\\' && pos+1 < len(sql) {
			pos += 2
		} else if strings.HasPrefix(sql[pos:], tag) {
			return pos + len(tag), nil
		} else {
			pos++
		}
	}
	return pos, fmt.Errorf("unterminated view dollar string")
}
