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

// adjustViewOrder orders only the view suffix. Neither slice is changed on error.
func adjustViewOrder(ctx context.Context, database string, createTable []string, tables Tables, start int) error {
	n := len(tables) - start
	positions := make(map[string]int, n)
	for i, table := range tables[start:] {
		positions[strings.ToLower(table.Name)] = i
	}
	counts := make([]int, n)
	dependents := make([][]int, n)
	for i, ddl := range createTable[start:] {
		refs, err := viewDependencies(ctx, database, ddl)
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

func viewDependencies(ctx context.Context, database, ddl string) ([]string, error) {
	statements, err := mysql.Parse(ctx, ddl, 1)
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
		return nil, fmt.Errorf("expected CREATE VIEW, got %T", statements[0])
	}
	collector := viewReferenceCollector{
		database: database,
		ctes:     make(map[string]int),
		seen:     make(map[string]bool),
	}
	if err := collector.walk(reflect.ValueOf(view.AsSource)); err != nil {
		return nil, err
	}
	return collector.names, nil
}

// The pinned parser's Expr.Accept panics for subqueries and omits window/order
// expressions. Walk exported AST fields instead, handling relation names and
// SELECT scopes explicitly. This visits subqueries in every expression position
// without interpreting string literals, column names, aliases, or function names.
// The parser produces an acyclic AST; unexported implementation fields are leaves.
type viewReferenceCollector struct {
	database string
	ctes     map[string]int
	seen     map[string]bool
	names    []string
}

func (c *viewReferenceCollector) walk(value reflect.Value) error {
	if !value.IsValid() {
		return nil
	}
	if value.Kind() == reflect.Interface {
		if value.IsNil() {
			return nil
		}
		return c.walk(value.Elem())
	}
	if value.Kind() == reflect.Ptr && value.IsNil() {
		return nil
	}
	if value.CanInterface() {
		switch node := value.Interface().(type) {
		case *tree.Select:
			return c.selectReferences(node)
		case *tree.TableName:
			name := strings.ToLower(string(node.ObjectName))
			local := !node.ExplicitSchema || strings.EqualFold(string(node.SchemaName), c.database)
			shadowed := !node.ExplicitSchema && c.ctes[name] != 0
			if local && !shadowed && !c.seen[name] && name != "" {
				c.seen[name] = true
				c.names = append(c.names, name)
			}
			return c.walk(reflect.ValueOf(node.AtTsExpr))
		case *tree.SelectClause, *tree.UnionClause, *tree.ParenSelect, *tree.ValuesClause:
			// Supported query bodies: descend into their exported children below.
		case tree.Statement:
			return fmt.Errorf("unsupported view query %T", node)
		}
	}
	switch value.Kind() {
	case reflect.Ptr:
		return c.walk(value.Elem())
	case reflect.Struct:
		for i := 0; i < value.NumField(); i++ {
			if value.Type().Field(i).IsExported() {
				if err := c.walk(value.Field(i)); err != nil {
					return err
				}
			}
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < value.Len(); i++ {
			if err := c.walk(value.Index(i)); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *viewReferenceCollector) selectReferences(query *tree.Select) error {
	var introduced []string
	defer func() {
		for _, name := range introduced {
			c.ctes[name]--
		}
	}()
	if query.With != nil {
		for _, cte := range query.With.CTEs {
			name := strings.ToLower(string(cte.Name.Alias))
			// Non-recursive CTEs can see earlier CTEs, but not themselves.
			if query.With.IsRecursive {
				c.ctes[name]++
				introduced = append(introduced, name)
			}
			if err := c.walk(reflect.ValueOf(cte.Stmt)); err != nil {
				return err
			}
			if !query.With.IsRecursive {
				c.ctes[name]++
				introduced = append(introduced, name)
			}
		}
	}
	for _, child := range []any{query.Select, query.OrderBy, query.Limit, query.TimeWindow} {
		if err := c.walk(reflect.ValueOf(child)); err != nil {
			return err
		}
	}
	return nil
}
