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
	"reflect"
	"strings"

	"github.com/matrixorigin/matrixone/pkg/sql/parsers/dialect/mysql"
	"github.com/matrixorigin/matrixone/pkg/sql/parsers/tree"
)

type viewSQLMode struct {
	lowerCaseTableNames int64
	ansiQuotes          bool
	noBackslashEscapes  bool
	sqlMode             string
}

func (m viewSQLMode) name(name string) string {
	// Match MatrixOne's GetDbOrTblName, including its mode-2 behavior.
	if m.lowerCaseTableNames != 1 {
		return name
	}
	return strings.ToLower(name)
}

func getViewSQLMode(ctx context.Context) (viewSQLMode, error) {
	var mode viewSQLMode
	err := conn.QueryRowContext(ctx, "select @@lower_case_table_names, @@sql_mode").Scan(&mode.lowerCaseTableNames, &mode.sqlMode)
	if err != nil {
		return mode, err
	}
	if mode.lowerCaseTableNames < 0 || mode.lowerCaseTableNames > 2 {
		return mode, fmt.Errorf("unsupported lower_case_table_names: %d", mode.lowerCaseTableNames)
	}
	for _, option := range strings.Split(mode.sqlMode, ",") {
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

func viewDependencies(database, ddl string, mode viewSQLMode) ([]string, error) {
	sqlMode := mode.sqlMode
	if mode.ansiQuotes {
		sqlMode += ",ANSI_QUOTES"
	}
	if mode.noBackslashEscapes {
		sqlMode += ",NO_BACKSLASH_ESCAPES"
	}
	statements, err := mysql.ParseWithSQLMode(context.Background(), ddl, mode.lowerCaseTableNames, sqlMode)
	if err != nil {
		return nil, err
	}
	defer func() {
		for _, statement := range statements {
			statement.Free()
		}
	}()
	if len(statements) != 1 {
		return nil, fmt.Errorf("expected one CREATE VIEW statement")
	}
	view, ok := statements[0].(*tree.CreateView)
	if !ok {
		return nil, fmt.Errorf("expected CREATE VIEW")
	}
	collector := viewReferenceCollector{database: mode.name(database), seen: make(map[string]bool)}
	if err := collector.walk(reflect.ValueOf(view.AsSource), nil); err != nil {
		return nil, err
	}
	return collector.names, nil
}

type viewCTE struct {
	statement tree.Statement
	scope     *viewCTEScope
	visiting  bool
	done      bool
}

type viewCTEScope struct {
	parent  *viewCTEScope
	name    string
	binding *viewCTE
}

func (s *viewCTEScope) lookup(name string) *viewCTE {
	for ; s != nil; s = s.parent {
		if s.name == name {
			// A nil binding masks outer declarations, like findCTE's
			// masked-name barrier; the reference is then physical.
			return s.binding
		}
	}
	return nil
}

type viewReferenceCollector struct {
	database string
	seen     map[string]bool
	names    []string
}

// Walk the complete AST, not token spellings. Only TableName is a relation;
// ON/projection identifiers and index-hint names cannot introduce dependencies.
// Query-expression branches, ORDER BY, ODBC joins and nested expressions all
// remain visible without maintaining a second SQL grammar in this client.
func (c *viewReferenceCollector) walk(value reflect.Value, scope *viewCTEScope) error {
	if !value.IsValid() {
		return nil
	}
	if value.Kind() == reflect.Interface {
		if value.IsNil() {
			return nil
		}
		return c.walk(value.Elem(), scope)
	}
	if value.Kind() == reflect.Ptr && value.IsNil() {
		return nil
	}
	if value.CanInterface() {
		switch node := value.Interface().(type) {
		case *tree.Select:
			return c.selectReferences(node, scope)
		case *tree.TableName:
			if node.AtTsExpr != nil {
				// Snapshot objects are not current restore-graph vertices.
				return c.walk(reflect.ValueOf(node.AtTsExpr), scope)
			}
			name := string(node.ObjectName)
			// Like the server binder, unqualified DUAL is a virtual row
			// source even when a same-named persistent relation exists.
			if !node.ExplicitSchema && name == "dual" {
				return nil
			}
			if node.ExplicitSchema && string(node.SchemaName) != c.database {
				return nil
			}
			if !node.ExplicitSchema {
				if binding := scope.lookup(name); binding != nil {
					if binding.statement == nil {
						return fmt.Errorf("empty CTE definition: %s", name)
					}
					if !binding.visiting && !binding.done {
						binding.visiting = true
						err := c.walk(reflect.ValueOf(binding.statement), binding.scope)
						binding.visiting = false
						if err != nil {
							return err
						}
						binding.done = true
					}
					return nil
				}
			}
			if name != "" && !c.seen[name] {
				c.seen[name] = true
				// Returned names outlive the parser-owned AST and its Free.
				c.names = append(c.names, strings.Clone(name))
			}
			return nil
		}
	}
	return c.children(value, scope, "")
}

// The parser builds an acyclic syntax tree. Unexported implementation fields
// are leaves; no AST node is changed and parser pools retain their normal owner.
func (c *viewReferenceCollector) children(value reflect.Value, scope *viewCTEScope, skip string) error {
	switch value.Kind() {
	case reflect.Ptr:
		return c.children(value.Elem(), scope, skip)
	case reflect.Struct:
		for i := 0; i < value.NumField(); i++ {
			field := value.Type().Field(i)
			if field.IsExported() && field.Name != skip {
				if err := c.walk(value.Field(i), scope); err != nil {
					return err
				}
			}
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < value.Len(); i++ {
			if err := c.walk(value.Index(i), scope); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *viewReferenceCollector) selectReferences(query *tree.Select, scope *viewCTEScope) error {
	if query.With != nil {
		// Own/later declarations are not yet visible while a CTE body
		// binds, and must not accidentally expose a same-named outer CTE.
		for _, cte := range query.With.CTEs {
			scope = &viewCTEScope{parent: scope, name: string(cte.Name.Alias)}
		}
		for _, cte := range query.With.CTEs {
			binding := &viewCTE{statement: cte.Stmt, scope: scope}
			// Both names and references have already passed the server's
			// own catalog/CTE name rules in the complete parser.
			scope = &viewCTEScope{parent: scope, name: string(cte.Name.Alias), binding: binding}
			if query.With.IsRecursive {
				binding.scope = scope
			}
		}
	}
	// Register definitions, but expand a body only when a TableName uses it.
	return c.children(reflect.ValueOf(query), scope, "With")
}
