// Copyright 2026 Matrix Origin
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
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

type viewName struct{ database, name string }
type dumpView struct {
	name   viewName
	create string
}

func normalizeViewIdentifier(name string, lowerCase int64) string {
	if lowerCase != 0 {
		return strings.ToLower(name)
	}
	return name
}

func normalizedViewName(database, name string, lowerCase int64) viewName {
	return viewName{normalizeViewIdentifier(database, lowerCase), normalizeViewIdentifier(name, lowerCase)}
}

// orderDumpViews sorts only dependencies within this export. Base tables have
// already been emitted; references outside the selection remain the caller's
// responsibility, just as for a single-database or selected-table dump.
func orderDumpViews(ctx context.Context, views []dumpView, lowerCase int64) ([]dumpView, error) {
	positions := make(map[viewName]int, len(views))
	for i, view := range views {
		key := normalizedViewName(view.name.database, view.name.name, lowerCase)
		if _, exists := positions[key]; exists {
			return nil, fmt.Errorf("duplicate view %s.%s", view.name.database, view.name.name)
		}
		positions[key] = i
	}
	pending := make([]int, len(views))
	dependents := make([][]int, len(views))
	for i, view := range views {
		refs, err := viewReferences(ctx, view, lowerCase)
		if err != nil {
			return nil, err
		}
		for ref := range refs {
			if dependency, ok := positions[ref]; ok {
				pending[i]++
				dependents[dependency] = append(dependents[dependency], i)
			}
		}
	}
	ready := make([]int, 0, len(views))
	for i, n := range pending {
		if n == 0 {
			ready = append(ready, i)
		}
	}
	ordered := make([]dumpView, 0, len(views))
	for next := 0; next < len(ready); next++ {
		i := ready[next]
		ordered = append(ordered, views[i])
		for _, dependent := range dependents[i] {
			pending[dependent]--
			if pending[dependent] == 0 {
				ready = append(ready, dependent)
			}
		}
	}
	if len(ordered) != len(views) {
		for i, n := range pending {
			if n > 0 {
				return nil, fmt.Errorf("cyclic view dependencies involving %s.%s", views[i].name.database, views[i].name.name)
			}
		}
	}
	return ordered, nil
}

func viewReferences(ctx context.Context, view dumpView, lowerCase int64) (map[viewName]struct{}, error) {
	// Preserve spelling while parsing; apply the server's name comparison rules
	// to relation identifiers and CTE scope keys, never to SQL string literals.
	statements, err := mysql.Parse(ctx, view.create, 0)
	if err != nil {
		return nil, fmt.Errorf("parse view %s.%s: %w", view.name.database, view.name.name, err)
	}
	defer func() {
		for _, statement := range statements {
			statement.Free()
		}
	}()
	if len(statements) != 1 {
		return nil, fmt.Errorf("expected one CREATE VIEW for %s.%s", view.name.database, view.name.name)
	}
	create, ok := statements[0].(*tree.CreateView)
	if !ok {
		return nil, fmt.Errorf("expected CREATE VIEW for %s.%s", view.name.database, view.name.name)
	}
	collector := viewReferenceCollector{database: view.name.database, lowerCase: lowerCase, refs: make(map[viewName]struct{})}
	collector.collect(reflect.ValueOf(create.AsSource), nil)
	return collector.refs, nil
}

type viewReferenceCollector struct {
	database  string
	lowerCase int64
	refs      map[viewName]struct{}
}

// The pinned parser has no statement walker and several Expr.Accept methods
// panic (including Subquery.Accept). Traverse its exported syntax fields instead
// so subqueries in expressions, joins, unions and window clauses are not missed.
// Only TableName nodes denote relations; strings, identifiers and aliases do not.
// Select is special because WITH introduces a lexical scope, not a relation.
func (c *viewReferenceCollector) collect(value reflect.Value, ctes map[string]bool) {
	if !value.IsValid() {
		return
	}
	if value.Kind() == reflect.Interface {
		if !value.IsNil() {
			c.collect(value.Elem(), ctes)
		}
		return
	}
	if value.Kind() == reflect.Ptr {
		if value.IsNil() {
			return
		}
		switch node := value.Interface().(type) {
		case *tree.TableName:
			c.collect(reflect.ValueOf(node.AtTsExpr), ctes)
			name := normalizeViewIdentifier(string(node.ObjectName), c.lowerCase)
			// The parser represents SELECT without FROM by an empty table.
			if name == "" {
				return
			}
			if !node.ExplicitSchema && ctes[name] {
				return
			}
			schema := c.database
			if node.ExplicitSchema {
				schema = string(node.SchemaName)
			}
			c.refs[normalizedViewName(schema, name, c.lowerCase)] = struct{}{}
			return
		case *tree.Select:
			if node.With != nil {
				local := make(map[string]bool, len(ctes)+len(node.With.CTEs))
				for name := range ctes {
					local[name] = true
				}
				for _, cte := range node.With.CTEs {
					name := normalizeViewIdentifier(string(cte.Name.Alias), c.lowerCase)
					if node.With.IsRecursive {
						local[name] = true
					}
					c.collect(reflect.ValueOf(cte.Stmt), local)
					local[name] = true
				}
				ctes = local
			}
			// WITH bodies were walked above using their individual visibility scopes.
			fields := value.Elem()
			typ := fields.Type()
			for i := 0; i < fields.NumField(); i++ {
				if typ.Field(i).PkgPath == "" && typ.Field(i).Name != "With" {
					c.collect(fields.Field(i), ctes)
				}
			}
			return
		}
		c.collect(value.Elem(), ctes)
		return
	}
	switch value.Kind() {
	case reflect.Struct:
		typ := value.Type()
		for i := 0; i < value.NumField(); i++ {
			if typ.Field(i).PkgPath == "" {
				c.collect(value.Field(i), ctes)
			}
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < value.Len(); i++ {
			c.collect(value.Index(i), ctes)
		}
	}
}

func quoteIdentifier(name string) string { return "`" + strings.ReplaceAll(name, "`", "``") + "`" }
