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
	"context"
	"fmt"
	"strings"

	"github.com/matrixorigin/matrixone/pkg/sql/parsers/dialect"
	"github.com/matrixorigin/matrixone/pkg/sql/parsers/dialect/mysql"
)

type viewSQLMode struct {
	lowerCaseTableNames int64
	ansiQuotes          bool
	noBackslashEscapes  bool
}

func (m viewSQLMode) name(name string) string {
	// Match the server lexer's GetDbOrTblName: only mode 1 folds catalog
	// names. Mode 2 preserves them in MatrixOne, including during lookup.
	if m.lowerCaseTableNames != 1 {
		return name
	}
	return strings.ToLower(name)
}

func getViewSQLMode(ctx context.Context) (viewSQLMode, error) {
	var mode viewSQLMode
	var sqlMode string
	err := conn.QueryRowContext(ctx, "select @@lower_case_table_names, @@sql_mode").Scan(&mode.lowerCaseTableNames, &sqlMode)
	if err != nil {
		return mode, err
	}
	if mode.lowerCaseTableNames < 0 || mode.lowerCaseTableNames > 2 {
		return mode, fmt.Errorf("unsupported lower_case_table_names: %d", mode.lowerCaseTableNames)
	}
	for _, option := range strings.Split(sqlMode, ",") {
		switch strings.ToUpper(strings.TrimSpace(option)) {
		case "ANSI_QUOTES":
			mode.ansiQuotes = true
		case "NO_BACKSLASH_ESCAPES":
			mode.noBackslashEscapes = true
		}
	}
	return mode, nil
}

// adjustViewOrder orders only the view suffix. Neither slice is changed on error.
func adjustViewOrder(ctx context.Context, database string, createTable []string, tables Tables, start int, mode viewSQLMode) error {
	n := len(tables) - start
	positions := make(map[string]int, n)
	for i, table := range tables[start:] {
		name := mode.name(table.Name)
		if _, exists := positions[name]; exists {
			return fmt.Errorf("duplicate view identity in database %s: %s", database, table.Name)
		}
		positions[name] = i
	}
	counts := make([]int, n)
	dependents := make([][]int, n)
	for i, ddl := range createTable[start:] {
		if err := ctx.Err(); err != nil {
			return err
		}
		refs, err := viewDependencies(database, ddl, mode)
		if err != nil {
			return fmt.Errorf("cannot order view %s.%s: %w", database, tables[start+i].Name, err)
		}
		for _, name := range refs {
			if dependency, ok := positions[name]; ok {
				counts[i]++
				dependents[dependency] = append(dependents[dependency], i)
			}
		}
	}

	// A FIFO ready queue makes progress bounded and preserves input order among
	// initially independent views. Each vertex and each edge is processed once.
	order := make([]int, 0, n)
	for i, count := range counts {
		if count == 0 {
			order = append(order, i)
		}
	}
	for next := 0; next < len(order); next++ {
		for _, dependent := range dependents[order[next]] {
			counts[dependent]--
			if counts[dependent] == 0 {
				order = append(order, dependent)
			}
		}
	}
	if len(order) != n {
		blocked := make([]string, 0, n-len(order))
		for i, count := range counts {
			if count != 0 {
				blocked = append(blocked, tables[start+i].Name)
			}
		}
		return fmt.Errorf("cyclic view dependencies in database %s; blocked views: %s", database, strings.Join(blocked, ", "))
	}

	orderedDDL := make([]string, n)
	orderedTables := make(Tables, n)
	for i, old := range order {
		orderedDDL[i] = createTable[start+old]
		orderedTables[i] = tables[start+old]
	}
	copy(createTable[start:], orderedDDL)
	copy(tables[start:], orderedTables)
	return nil
}

type viewToken struct {
	text        string
	quoted      bool
	word        bool
	punctuation bool
	close       int
}

func (t viewToken) keyword(word string) bool {
	return t.word && !t.quoted && strings.EqualFold(t.text, word)
}

func (t viewToken) symbol(symbol string) bool {
	return t.punctuation && t.text == symbol
}

// View SQL has already been accepted by the server. Only relation positions are
// needed here, not a second validation of its expression grammar. In particular,
// an older client's parser must not reject new WINDOW, VALUES, or ROLLUP syntax.
func viewDependencies(database, ddl string, mode viewSQLMode) ([]string, error) {
	tokens, err := tokenizeViewSQL(ddl, mode)
	if err != nil {
		return nil, err
	}
	if len(tokens) == 0 || !tokens[0].keyword("create") {
		return nil, fmt.Errorf("expected CREATE VIEW")
	}
	view, source := false, -1
	for i := 1; i < len(tokens); i++ {
		if tokens[i].symbol("(") {
			i = tokens[i].close
		} else if tokens[i].keyword("view") {
			view = true
		} else if view && tokens[i].keyword("as") {
			source = i + 1
			break
		}
	}
	if source < 0 {
		return nil, fmt.Errorf("expected CREATE VIEW AS query")
	}
	collector := viewReferenceCollector{
		database: mode.name(database), mode: mode, tokens: tokens,
		seen: make(map[string]bool),
	}
	if err := collector.query(source, len(tokens), nil); err != nil {
		return nil, err
	}
	return collector.names, nil
}

// Each binding retains its declaration environment, not the use-site scope.
// Visiting marks bound recursion; done prevents rescanning repeated references.
type viewCTE struct {
	start, end int
	scope      *viewCTEScope
	visiting   bool
	done       bool
}

type viewCTEScope struct {
	parent  *viewCTEScope
	name    string
	binding *viewCTE
}

func (s *viewCTEScope) lookup(name string) *viewCTE {
	for ; s != nil; s = s.parent {
		if s.name == name {
			return s.binding
		}
	}
	return nil
}

type viewReferenceCollector struct {
	database string
	mode     viewSQLMode
	tokens   []viewToken
	seen     map[string]bool
	names    []string
}

func (c *viewReferenceCollector) query(start, end int, scope *viewCTEScope) error {
	for start < end && c.tokens[start].symbol("(") && c.tokens[start].close == end-1 {
		start++
		end--
	}
	if start >= end {
		return fmt.Errorf("empty view query")
	}
	if c.tokens[start].keyword("with") {
		start++
		recursive := start < end && c.tokens[start].keyword("recursive")
		if recursive {
			start++
		}
		for {
			if start >= end || !c.tokens[start].word {
				return fmt.Errorf("expected CTE name")
			}
			// CTE declarations use ident.Compare(), independently of the
			// catalog-name mode. References still use GetDbOrTblName above.
			name := strings.ToLower(c.tokens[start].text)
			start++
			if start < end && c.tokens[start].symbol("(") {
				start = c.tokens[start].close + 1 // Optional CTE column names.
			}
			if start+1 >= end || !c.tokens[start].keyword("as") || !c.tokens[start+1].symbol("(") {
				return fmt.Errorf("expected CTE AS query")
			}
			body := start + 1
			binding := &viewCTE{start: body + 1, end: c.tokens[body].close, scope: scope}
			scope = &viewCTEScope{parent: scope, name: name, binding: binding}
			if recursive {
				binding.scope = scope
			}
			start = binding.end + 1
			if start >= end || !c.tokens[start].symbol(",") {
				break
			}
			start++
		}
	}
	if start >= end {
		return fmt.Errorf("empty view query after WITH")
	}
	return c.body(start, end, scope, false)
}

func (c *viewReferenceCollector) body(start, end int, scope *viewCTEScope, inFrom bool) error {
	queryStart := !inFrom
	for i := start; i < end; i++ {
		token := c.tokens[i]
		if token.keyword("as") && i+1 < end && c.tokens[i+1].word {
			i++ // An explicit alias is not a clause or a relation.
			continue
		}
		if token.symbol("(") {
			if err := c.nested(i+1, token.close, scope); err != nil {
				return err
			}
			i = token.close
			continue
		}
		if token.keyword("from") || token.keyword("join") || token.keyword("straight_join") || token.keyword("apply") || (inFrom && token.symbol(",")) || (queryStart && token.keyword("table")) {
			next, err := c.relation(i+1, end, scope)
			if err != nil {
				return err
			}
			i = next - 1
			inFrom, queryStart = true, false
			continue
		}
		if token.word && !token.quoted {
			switch strings.ToLower(token.text) {
			case "union", "intersect", "except", "minus":
				inFrom, queryStart = false, true
			case "select", "values":
				inFrom, queryStart = false, false
			case "where", "group", "having", "window", "order", "limit", "offset", "qualify", "into", "for", "lock":
				inFrom = false
			}
		}
	}
	return nil
}

// Parentheses in expressions may contain subqueries, but FROM in an ordinary
// function (EXTRACT/TRIM/SUBSTRING) is not a table reference.
func (c *viewReferenceCollector) nested(start, end int, scope *viewCTEScope) error {
	if start < end && (c.tokens[start].keyword("select") || c.tokens[start].keyword("with") || c.tokens[start].keyword("values") || c.tokens[start].keyword("table")) {
		return c.query(start, end, scope)
	}
	for i := start; i < end; i++ {
		if c.tokens[i].symbol("(") {
			if err := c.nested(i+1, c.tokens[i].close, scope); err != nil {
				return err
			}
			i = c.tokens[i].close
		}
	}
	return nil
}

func (c *viewReferenceCollector) relation(start, end int, scope *viewCTEScope) (int, error) {
	if start < end && c.tokens[start].keyword("lateral") && start+1 < end && c.tokens[start+1].symbol("(") {
		start++
	}
	if start >= end {
		return start, fmt.Errorf("expected relation after FROM/JOIN")
	}
	if c.tokens[start].symbol("(") {
		close := c.tokens[start].close
		first := start + 1
		if first < close && (c.tokens[first].keyword("select") || c.tokens[first].keyword("with") || c.tokens[first].keyword("values") || c.tokens[first].keyword("table")) {
			return close + 1, c.query(first, close, scope)
		}
		next, err := c.relation(first, close, scope)
		if err != nil {
			return close + 1, err
		}
		return close + 1, c.body(next, close, scope, true)
	}
	if !c.tokens[start].word {
		return start, fmt.Errorf("expected relation name")
	}
	name := c.tokens[start].text
	pos := start + 1
	schema := ""
	qualified := pos < end && c.tokens[pos].symbol(".")
	if qualified {
		if pos+1 >= end || !c.tokens[pos+1].word {
			return pos, fmt.Errorf("expected qualified relation name")
		}
		schema, name = name, c.tokens[pos+1].text
		pos += 2
	}
	if pos < end && c.tokens[pos].symbol("(") { // Table-valued function, not a VIEW.
		return c.tokens[pos].close + 1, c.nested(pos+1, c.tokens[pos].close, scope)
	}
	// A historical relation is resolved in its snapshot, not against the
	// current objects being restored. In particular it may share this VIEW's
	// own name without forming a cycle in the current restore graph.
	if pos < end && c.tokens[pos].symbol("{") {
		return pos, nil
	}
	name = c.mode.name(name)
	if qualified && c.mode.name(schema) != c.database {
		return pos, nil
	}
	if !qualified {
		if binding := scope.lookup(name); binding != nil {
			if !binding.visiting && !binding.done {
				binding.visiting = true
				err := c.query(binding.start, binding.end, binding.scope)
				binding.visiting = false
				if err != nil {
					return pos, err
				}
				binding.done = true
			}
			return pos, nil
		}
	}
	if !c.seen[name] {
		c.seen[name] = true
		c.names = append(c.names, name)
	}
	return pos, nil
}

func tokenizeViewSQL(ddl string, mode viewSQLMode) ([]viewToken, error) {
	text, err := maskViewLiterals(ddl, mode)
	if err != nil {
		return nil, err
	}
	scanner := mysql.NewScanner(dialect.MYSQL, text)
	defer mysql.PutScanner(scanner)
	var tokens []viewToken
	var opens []int
	terminated := false
	for {
		kind, value := scanner.Scan()
		if kind == 0 {
			break
		}
		if kind == mysql.LEX_ERROR {
			return nil, fmt.Errorf("cannot tokenize view definition")
		}
		if terminated {
			return nil, fmt.Errorf("expected one CREATE VIEW statement")
		}
		if kind == ';' {
			terminated = true
			continue
		}
		if kind < 256 {
			value = string(rune(kind))
		}
		raw := strings.TrimSpace(text[scanner.PrePos:scanner.Pos])
		word := len(raw) > 0 && (raw[0] >= 'A' && raw[0] <= 'Z' || raw[0] >= 'a' && raw[0] <= 'z' || raw[0] == '_' || raw[0] == '$' || raw[0] >= 128)
		token := viewToken{text: value, word: word, quoted: strings.HasPrefix(raw, "`"), punctuation: kind < 256, close: -1}
		if token.quoted {
			token.word = true
		}
		switch kind {
		case '(':
			opens = append(opens, len(tokens))
		case ')':
			if len(opens) == 0 {
				return nil, fmt.Errorf("unbalanced view query parentheses")
			}
			tokens[opens[len(opens)-1]].close = len(tokens)
			opens = opens[:len(opens)-1]
		}
		tokens = append(tokens, token)
	}
	if len(opens) != 0 {
		return nil, fmt.Errorf("unbalanced view query parentheses")
	}
	return tokens, nil
}

// Discard literal contents before lexing, and normalize identifier quotes, so
// the older lexer's fixed string-escape rules cannot override the SQL mode.
// Ordinary comment lexing remains owned by that lexer; executable comment
// bodies receive the same literal protection as the surrounding query.
func maskViewLiterals(sql string, mode viewSQLMode) (string, error) {
	var out strings.Builder
	for i := 0; i < len(sql); {
		ch := sql[i]
		lineComment := i+1 < len(sql) && (sql[i:i+2] == "//" || sql[i:i+2] == "--" && (i+2 == len(sql) || strings.ContainsRune(" \n\t\r", rune(sql[i+2]))))
		if ch == '#' || lineComment {
			end := strings.IndexByte(sql[i:], '\n')
			if end < 0 {
				out.WriteString(sql[i:])
				break
			}
			out.WriteString(sql[i : i+end+1])
			i += end + 1
			continue
		}
		if i+1 < len(sql) && sql[i:i+2] == "/*" {
			end := strings.Index(sql[i+2:], "*/")
			if end < 0 {
				return "", fmt.Errorf("unterminated view comment")
			}
			end += i + 4
			if i+2 < len(sql) && sql[i+2] == '!' {
				body := i + 3
				for body < end-2 && sql[body] >= '0' && sql[body] <= '9' {
					body++
				}
				masked, err := maskViewLiterals(sql[body:end-2], mode)
				if err != nil {
					return "", err
				}
				out.WriteByte(' ')
				out.WriteString(masked)
				out.WriteByte(' ')
			} else {
				out.WriteString(sql[i:end])
			}
			i = end
			continue
		}
		if ch == '$' {
			tagEnd := i + 1
			for tagEnd < len(sql) && (sql[tagEnd] >= 'a' && sql[tagEnd] <= 'z' || sql[tagEnd] >= 'A' && sql[tagEnd] <= 'Z' || sql[tagEnd] >= '0' && sql[tagEnd] <= '9' || sql[tagEnd] == '_') {
				tagEnd++
			}
			if tagEnd < len(sql) && sql[tagEnd] == '$' {
				tag := sql[i : tagEnd+1]
				close := strings.Index(sql[tagEnd+1:], tag)
				if close < 0 {
					return "", fmt.Errorf("unterminated view dollar string")
				}
				out.WriteString("''")
				i = tagEnd + 1 + close + len(tag)
				continue
			}
		}
		if ch != '\'' && ch != '"' && ch != '`' {
			out.WriteByte(ch)
			i++
			continue
		}
		identifier := ch == '`' || ch == '"' && mode.ansiQuotes
		var name strings.Builder
		closed := false
		for i++; i < len(sql); i++ {
			if sql[i] == ch {
				if i+1 < len(sql) && sql[i+1] == ch {
					name.WriteByte(ch)
					i++
					continue
				}
				i++
				closed = true
				break
			}
			if !identifier && !mode.noBackslashEscapes && sql[i] == '\\' && i+1 < len(sql) {
				i++
			}
			name.WriteByte(sql[i])
		}
		if !closed {
			return "", fmt.Errorf("unterminated view quoted value")
		}
		if identifier {
			out.WriteByte('`')
			out.WriteString(strings.ReplaceAll(name.String(), "`", "``"))
			out.WriteByte('`')
		} else {
			out.WriteString("''")
		}
	}
	return out.String(), nil
}
