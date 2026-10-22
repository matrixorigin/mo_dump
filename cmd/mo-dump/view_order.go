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
	"slices"
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

// The export session supplies catalog identity rules only, never view syntax.
func getViewCatalogMode(ctx context.Context) (viewSQLMode, error) {
	var mode viewSQLMode
	err := conn.QueryRowContext(ctx, "select @@lower_case_table_names").Scan(&mode.lowerCaseTableNames)
	if err != nil {
		return mode, err
	}
	if mode.lowerCaseTableNames < 0 || mode.lowerCaseTableNames > 2 {
		return mode, fmt.Errorf("unsupported lower_case_table_names: %d", mode.lowerCaseTableNames)
	}
	return mode, nil
}

// adjustViewOrder orders only the view suffix. Neither slice is changed on error.
// Non-nil definitions are authoritative for every view. Nil selects explicit
// standalone DDL inputs; it is never a fallback after a metadata-read failure.
func adjustViewOrder(ctx context.Context, database string, createTable []string, tables Tables, start int, mode viewSQLMode, definitions map[string]viewDefinition) error {
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
		parseMode, defaultDatabase := mode, database
		if definitions != nil {
			definition, ok := definitions[tables[start+i].Name]
			if !ok {
				return fmt.Errorf("missing persisted definition for view %s.%s", database, tables[start+i].Name)
			}
			var err error
			parseMode, err = definition.parserMode(mode)
			if err != nil {
				return fmt.Errorf("cannot read creation context for view %s.%s: %w", database, tables[start+i].Name, err)
			}
			ddl = definition.Stmt
			if definition.DefaultDatabase != "" {
				defaultDatabase = definition.DefaultDatabase
			}
		}
		refs, err := viewDependenciesInContext(database, defaultDatabase, ddl, parseMode, mode)
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
	return viewDependenciesInContext(database, database, ddl, mode, mode)
}

func viewDependenciesInContext(database, defaultDatabase, ddl string, mode, catalogMode viewSQLMode) ([]string, error) {
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
	var query *tree.Select
	switch view := statements[0].(type) {
	case *tree.CreateView:
		query = view.AsSource
	case *tree.AlterView:
		query = view.AsSource
	default:
		return nil, fmt.Errorf("expected CREATE VIEW or ALTER VIEW")
	}
	collector := viewReferenceCollector{
		database: catalogMode.name(database), defaultDatabase: catalogMode.name(defaultDatabase),
		catalogMode: catalogMode, seen: make(map[string]bool),
	}
	if err := collector.walk(reflect.ValueOf(query), nil); err != nil {
		return nil, err
	}
	return collector.names, nil
}

type viewCTE struct {
	statement tree.Statement
	scope     *viewCTEScope
	name      string
	positions map[string]int
	ordinal   int
	recursive bool
	visiting  bool
	done      bool
}

func (c *viewCTE) masked(name string) bool {
	if c == nil {
		return false
	}
	position, exists := c.positions[name]
	return exists && position > c.ordinal
}

// A query block has both lexical declarations and an active binding state.
// An inner WITH replaces declarations, but cannot shadow its active mask.
type viewCTEScope struct {
	parent   *viewCTEScope
	bindings map[string]*viewCTE
	active   *viewCTE
}

func newViewCTEScope(parent *viewCTEScope) *viewCTEScope {
	scope := &viewCTEScope{parent: parent}
	// Match NewBindContext: CTE binding children inherit the current local
	// map and state. Replacing the active state later does not erase parents.
	if parent != nil && parent.active != nil {
		scope.bindings = parent.bindings
		scope.active = parent.active
	}
	return scope
}

func (s *viewCTEScope) lookup(name string) *viewCTE {
	for ; s != nil; s = s.parent {
		if s.active.masked(name) {
			return nil
		}
		if binding := s.bindings[name]; binding != nil {
			return binding
		}
	}
	return nil
}

type viewReferenceCollector struct {
	database        string
	defaultDatabase string
	catalogMode     viewSQLMode
	seen            map[string]bool
	names           []string
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
			if node.ExplicitSchema && c.catalogMode.name(string(node.SchemaName)) != c.database {
				return nil
			}
			if !node.ExplicitSchema {
				if scope != nil && scope.active != nil && !scope.active.recursive && scope.active.name == name {
					return fmt.Errorf("nonrecursive CTE %s references itself", name)
				}
				if binding := scope.lookup(name); binding != nil {
					if binding.statement == nil {
						return fmt.Errorf("empty CTE definition: %s", name)
					}
					if !binding.visiting && !binding.done {
						binding.visiting = true
						err := c.cteReferences(binding)
						binding.visiting = false
						if err != nil {
							return err
						}
						binding.done = true
					}
					return nil
				}
			}
			if !node.ExplicitSchema && c.defaultDatabase != c.database {
				return nil
			}
			name = c.catalogMode.name(name)
			if name != "" && !c.seen[name] {
				c.seen[name] = true
				// Returned names outlive the parser-owned AST and its Free.
				c.names = append(c.names, strings.Clone(name))
			}
			return nil
		}
	}
	return c.children(value, scope)
}

// The parser builds an acyclic syntax tree. Unexported implementation fields
// are leaves; no AST node is changed and parser pools retain their normal owner.
func (c *viewReferenceCollector) children(value reflect.Value, scope *viewCTEScope, skip ...string) error {
	switch value.Kind() {
	case reflect.Ptr:
		return c.children(value.Elem(), scope, skip...)
	case reflect.Struct:
		for i := 0; i < value.NumField(); i++ {
			field := value.Type().Field(i)
			if field.IsExported() && !slices.Contains(skip, field.Name) {
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

func (c *viewReferenceCollector) cteReferences(binding *viewCTE) error {
	// bindCte replaces the active state on a child of the declaration
	// context, rather than accumulating every mask from its use site.
	scope := newViewCTEScope(binding.scope)
	scope.active = binding
	switch statement := binding.statement.(type) {
	case *tree.Select:
		return c.selectInScope(statement, scope)
	case *tree.ParenSelect:
		return c.selectInScope(statement.Select, scope)
	default:
		return fmt.Errorf("expected query in CTE %s", binding.name)
	}
}

func (c *viewReferenceCollector) selectReferences(query *tree.Select, parent *viewCTEScope) error {
	return c.selectInScope(query, newViewCTEScope(parent))
}

func (c *viewReferenceCollector) selectInScope(query *tree.Select, scope *viewCTEScope) error {
	// bindSelect strips this entire parenthesis chain in one BindContext.
	// All tails therefore see the final block's WITH, not the scope that
	// happened to be active before entering a ParenSelect child.
	var layers []*tree.Select
	for {
		layers = append(layers, query)
		if query.With != nil {
			// A later WITH replaces this block's local declarations; it
			// does not turn the previous local declarations into a parent.
			scope = viewScopeForWith(query, scope)
		}
		paren, ok := query.Select.(*tree.ParenSelect)
		if !ok {
			break
		}
		query = paren.Select
	}
	if err := c.walk(reflect.ValueOf(query.Select), scope); err != nil {
		return err
	}
	for _, layer := range layers {
		if err := c.children(reflect.ValueOf(layer), scope, "With", "Select"); err != nil {
			return err
		}
	}
	return nil
}

func viewScopeForWith(query *tree.Select, scope *viewCTEScope) *viewCTEScope {
	bindings := make(map[string]*viewCTE, len(query.With.CTEs))
	positions := make(map[string]int, len(query.With.CTEs))
	for i, cte := range query.With.CTEs {
		positions[string(cte.Name.Alias)] = i
	}
	scope.bindings = bindings
	for i, cte := range query.With.CTEs {
		// Match newCTEDeclarationContext: retain this block's local map
		// and active state, but detach the declaration from this block.
		declaration := &viewCTEScope{parent: scope.parent, bindings: bindings, active: scope.active}
		name := string(cte.Name.Alias)
		bindings[name] = &viewCTE{
			statement: cte.Stmt, scope: declaration, name: name,
			positions: positions, ordinal: i, recursive: query.With.IsRecursive,
		}
	}
	return scope
}
