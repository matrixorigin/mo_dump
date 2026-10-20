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

	"github.com/matrixorigin/matrixone/pkg/sql/parsers/dialect/mysql"
	"github.com/matrixorigin/matrixone/pkg/sql/parsers/tree"
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
	kind        int
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

// The server has validated the complete DDL. Only relation syntax is interpreted
// here; expression grammar such as WINDOW, ROLLUP, and VALUES stays server-owned.
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
			// CTE declarations use ident.Compare(), not the catalog-name mode.
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
	return c.body(start, end, scope)
}

// SELECT modifiers, projection aliases, and expression keywords are not join
// operators. Only FROM opens a table-reference context (TABLE opens one factor).
func (c *viewReferenceCollector) body(start, end int, scope *viewCTEScope) error {
	queryStart := true
	for i := start; i < end; i++ {
		token := c.tokens[i]
		if token.symbol("(") {
			if err := c.nested(i+1, token.close, scope); err != nil {
				return err
			}
			i = token.close
		} else if token.keyword("from") {
			next, err := c.fromClause(i+1, end, scope)
			if err != nil {
				return err
			}
			i = next - 1
		} else if queryStart && token.keyword("table") {
			next, err := c.relation(i+1, end, scope)
			if err != nil {
				return err
			}
			i, queryStart = next-1, false
		} else if token.keyword("select") || token.keyword("values") {
			queryStart = false
		} else if token.keyword("union") || token.keyword("intersect") || token.keyword("except") || token.keyword("minus") {
			queryStart = true
		}
	}
	return nil
}

func viewJoinOperator(t viewToken) bool {
	return t.keyword("join") || t.keyword("straight_join") || t.keyword("apply")
}

func viewFromEnd(t viewToken) bool {
	if !t.word || t.quoted {
		return false
	}
	switch strings.ToLower(t.text) {
	case "where", "group", "having", "window", "order", "limit", "offset", "qualify", "into", "for", "lock", "union", "intersect", "except", "minus":
		return true
	}
	return false
}

// Each relation consumes a complete factor, including aliases and hints. Thus
// this loop never interprets an alias or FOR JOIN inside an index hint as syntax.
func (c *viewReferenceCollector) fromClause(start, end int, scope *viewCTEScope) (int, error) {
	pos, err := c.relation(start, end, scope)
	if err != nil {
		return pos, err
	}
	for pos < end {
		token := c.tokens[pos]
		if viewFromEnd(token) {
			return pos, nil
		}
		if token.symbol(",") || viewJoinOperator(token) {
			pos, err = c.relation(pos+1, end, scope)
			if err != nil {
				return pos, err
			}
			continue
		}
		if token.keyword("using") && pos+1 < end && c.tokens[pos+1].symbol("(") {
			pos = c.tokens[pos+1].close + 1
			continue
		}
		if token.symbol("(") {
			if err := c.nested(pos+1, token.close, scope); err != nil {
				return pos, err
			}
			pos = token.close + 1
		} else {
			pos++
		}
	}
	return pos, nil
}

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
	if start+1 < end && c.tokens[start].keyword("lateral") && c.tokens[start+1].symbol("(") {
		start++
	}
	if start >= end {
		return start, fmt.Errorf("expected relation after FROM/JOIN")
	}
	pos := start
	if c.tokens[pos].symbol("(") {
		close := c.tokens[pos].close
		first := pos + 1
		if first < close && (c.tokens[first].keyword("select") || c.tokens[first].keyword("with") || c.tokens[first].keyword("values") || c.tokens[first].keyword("table")) {
			if err := c.query(first, close, scope); err != nil {
				return pos, err
			}
		} else if _, err := c.fromClause(first, close, scope); err != nil {
			return pos, err
		}
		pos = close + 1
	} else {
		if !c.tokens[pos].word {
			return pos, fmt.Errorf("expected relation name")
		}
		name, schema := c.tokens[pos].text, ""
		pos++
		qualified := pos < end && c.tokens[pos].symbol(".")
		if qualified {
			if pos+1 >= end || !c.tokens[pos+1].word {
				return pos, fmt.Errorf("expected qualified relation name")
			}
			schema, name = name, c.tokens[pos+1].text
			pos += 2
		}
		if pos < end && c.tokens[pos].symbol("(") {
			if err := c.nested(pos+1, c.tokens[pos].close, scope); err != nil {
				return pos, err
			}
			pos = c.tokens[pos].close + 1
		} else if pos < end && c.tokens[pos].symbol("{") {
			// A historical object is not a current restore dependency.
			pos = c.tokens[pos].close + 1
		} else if err := c.reference(schema, name, qualified, scope); err != nil {
			return pos, err
		}
	}
	return c.factorSuffix(pos, end)
}

func (c *viewReferenceCollector) reference(schema, name string, qualified bool, scope *viewCTEScope) error {
	name = c.mode.name(name)
	if qualified && c.mode.name(schema) != c.database {
		return nil
	}
	if !qualified {
		if binding := scope.lookup(name); binding != nil {
			if !binding.visiting && !binding.done {
				binding.visiting = true
				err := c.query(binding.start, binding.end, binding.scope)
				binding.visiting = false
				if err != nil {
					return err
				}
				binding.done = true
			}
			return nil
		}
	}
	if !c.seen[name] {
		c.seen[name] = true
		c.names = append(c.names, name)
	}
	return nil
}

// Use the dialect's alias production for known keyword tokens, not a hand-kept
// list of nonreserved words. This parses only a fixed table/alias scaffold, never
// the original VIEW or its expression grammar. New lexer IDs need no parsing.
func viewAliasName(t viewToken) bool {
	if t.kind == mysql.ID || t.kind == mysql.QUOTE_ID || t.kind == mysql.STRING {
		return true
	}
	if !t.word {
		return false
	}
	statements, err := mysql.Parse(context.Background(), "select * from __modump_alias__ as "+t.text, 0)
	if err != nil {
		return false
	}
	defer func() {
		for _, statement := range statements {
			statement.Free()
		}
	}()
	if len(statements) != 1 {
		return false
	}
	query, ok := statements[0].(*tree.Select)
	if !ok {
		return false
	}
	selectClause, ok := query.Select.(*tree.SelectClause)
	if !ok || selectClause.From == nil || len(selectClause.From.Tables) != 1 {
		return false
	}
	factorNode := selectClause.From.Tables[0]
	// The pinned grammar wraps even a single factor in a CROSS node.
	if join, ok := factorNode.(*tree.JoinTableExpr); ok && join.Right == nil {
		factorNode = join.Left
	}
	factor, ok := factorNode.(*tree.AliasedTableExpr)
	return ok && factor.As.Alias != ""
}

func (c *viewReferenceCollector) columnAliases(pos, end int) bool {
	if pos >= end || !c.tokens[pos].symbol("(") {
		return false
	}
	close := c.tokens[pos].close
	if close == pos+1 {
		return false
	}
	for i := pos + 1; i < close; i++ {
		if (i-pos)%2 == 1 {
			if !c.tokens[i].word || c.tokens[i].keyword("select") || c.tokens[i].keyword("with") {
				return false
			}
		} else if !c.tokens[i].symbol(",") {
			return false
		}
	}
	return (close-pos)%2 == 0
}

func viewHintStart(tokens []viewToken, pos, end int) bool {
	return pos+1 < end && (tokens[pos].keyword("use") || tokens[pos].keyword("ignore") || tokens[pos].keyword("force")) && (tokens[pos+1].keyword("index") || tokens[pos+1].keyword("key"))
}

func (c *viewReferenceCollector) followsFactor(pos, end int) bool {
	if pos == end {
		return true
	}
	t := c.tokens[pos]
	if t.symbol(",") || viewJoinOperator(t) || viewFromEnd(t) || viewHintStart(c.tokens, pos, end) {
		return true
	}
	for _, word := range []string{"on", "using", "left", "right", "full", "inner", "outer", "cross", "natural", "asof", "dedup"} {
		if t.keyword(word) {
			return true
		}
	}
	return false
}

func (c *viewReferenceCollector) factorSuffix(pos, end int) (int, error) {
	if pos < end && c.tokens[pos].keyword("as") {
		pos++
		if pos >= end || !viewAliasName(c.tokens[pos]) {
			return pos, fmt.Errorf("expected table alias after AS")
		}
		pos++
	} else if pos < end && !viewHintStart(c.tokens, pos, end) && viewAliasName(c.tokens[pos]) {
		next := pos + 1
		if c.columnAliases(next, end) {
			next = c.tokens[next].close + 1
		}
		if c.followsFactor(next, end) {
			pos++
		}
	}
	if c.columnAliases(pos, end) {
		pos = c.tokens[pos].close + 1
	}
	for viewHintStart(c.tokens, pos, end) {
		pos += 2
		if pos < end && c.tokens[pos].keyword("for") {
			pos++
			if pos < end && c.tokens[pos].keyword("join") {
				pos++
			} else if pos+1 < end && (c.tokens[pos].keyword("order") || c.tokens[pos].keyword("group")) && c.tokens[pos+1].keyword("by") {
				pos += 2
			} else {
				return pos, fmt.Errorf("invalid index hint scope")
			}
		}
		if pos >= end || !c.tokens[pos].symbol("(") {
			return pos, fmt.Errorf("expected index hint names")
		}
		pos = c.tokens[pos].close + 1
	}
	return pos, nil
}
