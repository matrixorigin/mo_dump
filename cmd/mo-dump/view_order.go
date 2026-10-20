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

	"github.com/matrixorigin/matrixone/pkg/catalog"
	"github.com/matrixorigin/matrixone/pkg/sql/parsers/dialect/mysql"
	"github.com/matrixorigin/matrixone/pkg/sql/parsers/tree"
)

// adjustViewOrder orders the view suffix after dumpData has partitioned tables
// and views. DDL is only inspected: every statement is emitted unchanged.
func adjustViewOrder(ctx context.Context, db string, lower int64, ddl []string, tables Tables) error {
	start := 0
	for start < len(tables) && tables[start].Kind != catalog.SystemViewRel {
		start++
	}
	count := len(tables) - start
	if count == 0 {
		return nil
	}
	if lower < 0 || lower > 2 {
		return fmt.Errorf("database %s: invalid lower_case_table_names %d", db, lower)
	}
	currentDB, _ := viewIdentifier(db, "", lower)
	positions := make(map[string]int, count)
	for i := range tables[start:] {
		_, name := viewIdentifier(db, tables[start+i].Name, lower)
		positions[name] = i
	}
	dependents := make([][]viewDependency, count)
	for i := range tables[start:] {
		err := collectViewRefs(ctx, ddl[start+i], lower, func(ref *tree.TableName, required bool) {
			refDB := db
			if ref.ExplicitSchema {
				refDB = string(ref.Schema())
			}
			refDB, name := viewIdentifier(refDB, string(ref.Name()), lower)
			if refDB != currentDB {
				return
			}
			if parent, ok := positions[name]; ok {
				dependents[parent] = append(dependents[parent], viewDependency{child: i, required: required})
			}
		})
		if err != nil {
			return fmt.Errorf("database %s, view %s: %w", db, tables[start+i].Name, err)
		}
	}
	// Prefer all syntactic prerequisites for older eager CTE binders. Newer
	// servers allow unused CTEs to reference missing/self objects: if those
	// advisory edges cycle, enforce only references reachable from the query.
	indegree := make([]int, count)
	order := make([]int, 0, count)
	sort := func(requiredOnly bool) {
		clear(indegree)
		order = order[:0]
		for _, children := range dependents {
			for _, edge := range children {
				if !requiredOnly || edge.required {
					indegree[edge.child]++
				}
			}
		}
		for i, degree := range indegree {
			if degree == 0 {
				order = append(order, i)
			}
		}
		// Paired indegree/adjacency entries also handle repeated references.
		for next := 0; next < len(order); next++ {
			for _, edge := range dependents[order[next]] {
				if requiredOnly && !edge.required {
					continue
				}
				indegree[edge.child]--
				if indegree[edge.child] == 0 {
					order = append(order, edge.child)
				}
			}
		}
	}
	sort(false)
	if len(order) != count {
		sort(true)
	}
	if len(order) != count {
		blocked := make([]string, 0, count-len(order))
		for i, degree := range indegree {
			if degree != 0 {
				blocked = append(blocked, quoteIdentifier(tables[start+i].Name))
			}
		}
		return fmt.Errorf("database %s: cyclic view dependencies block %s", db, strings.Join(blocked, ", "))
	}
	orderedDDL := make([]string, count)
	orderedTables := make(Tables, count)
	for i, original := range order {
		orderedDDL[i] = ddl[start+original]
		orderedTables[i] = tables[start+original]
	}
	copy(ddl[start:], orderedDDL)
	copy(tables[start:], orderedTables)
	return nil
}

type viewDependency struct {
	child    int
	required bool
}

// Match the pinned MatrixOne binder: mode 1 folds table names; modes 0 and
// 2 preserve spelling. System database names are always case insensitive.
func viewIdentifier(db, name string, lower int64) (string, string) {
	if lower == 1 || strings.EqualFold(db, "information_schema") || strings.EqualFold(db, "mysql") {
		return strings.ToLower(db), strings.ToLower(name)
	}
	return db, name
}

func collectViewRefs(ctx context.Context, ddl string, lower int64, emit func(*tree.TableName, bool)) error {
	stmt, err := mysql.ParseOne(ctx, ddl, 0)
	if err != nil {
		return err
	}
	defer stmt.Free()
	view, ok := stmt.(*tree.CreateView)
	if !ok {
		return fmt.Errorf("expected CREATE VIEW, got %T", stmt)
	}
	walker := viewRefWalker{lower: lower, emit: emit}
	walker.walk(reflect.ValueOf(view.AsSource), nil, true)
	// Finish required expansion globally before touching unused declarations.
	// Nested SELECTs can append further declarations while this queue drains.
	for i := 0; i < len(walker.pending); i++ {
		walker.expand(walker.pending[i], false)
	}
	return nil
}

// The pinned tree package has no complete query visitor (Subquery.Accept
// panics). Discover children structurally, but keep binding semantics explicit
// for SELECT/CTE scope and relation names. Only exported tree fields are walked;
// literal payloads and objects from other packages are leaves. Reaudit this
// boundary when upgrading the parser, especially private expression children.
type viewRefWalker struct {
	lower   int64
	emit    func(*tree.TableName, bool)
	pending []*viewCTEBinding
}

type viewCTEBinding struct {
	name     string
	stmt     tree.Statement
	visible  *viewCTEScope
	expanded bool
}

// A declaration sees earlier bindings, but later names mask outer scopes.
// Both slices share the WITH's immutable declaration list.
type viewCTEScope struct {
	parent   *viewCTEScope
	bindings []*viewCTEBinding
	masked   []*viewCTEBinding
}

// Masks remain active inside nested queries of a CTE definition, including
// nested WITH declarations. Like the binder, check them before visible names.
func (scope *viewCTEScope) lookup(name string) *viewCTEBinding {
	for current := scope; current != nil; current = current.parent {
		for _, binding := range current.masked {
			if name == binding.name {
				return nil
			}
		}
	}
	for current := scope; current != nil; current = current.parent {
		for i := len(current.bindings) - 1; i >= 0; i-- {
			if name == current.bindings[i].name {
				return current.bindings[i]
			}
		}
	}
	return nil
}

func (w *viewRefWalker) expand(binding *viewCTEBinding, required bool) {
	if !binding.expanded {
		binding.expanded = true // Stop recursive CTE references before descending.
		w.walk(reflect.ValueOf(binding.stmt), binding.visible, required)
	}
}

var viewTreePackage = reflect.TypeOf(tree.Select{}).PkgPath()

func (w *viewRefWalker) walk(value reflect.Value, ctes *viewCTEScope, required bool) {
	if !value.IsValid() {
		return
	}
	if value.Kind() == reflect.Interface {
		if !value.IsNil() {
			w.walk(value.Elem(), ctes, required)
		}
		return
	}
	if value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return
		}
	}
	if value.CanInterface() {
		switch node := value.Interface().(type) {
		case *tree.Select:
			if node.With != nil {
				bindings := make([]*viewCTEBinding, len(node.With.CTEs))
				for i, cte := range node.With.CTEs {
					bindings[i] = &viewCTEBinding{name: string(cte.Name.Alias), stmt: cte.Stmt}
				}
				for i, binding := range bindings {
					end := i
					if node.With.IsRecursive {
						end++
					}
					binding.visible = &viewCTEScope{parent: ctes, bindings: bindings[:end], masked: bindings[i+1:]}
					w.pending = append(w.pending, binding)
				}
				ctes = &viewCTEScope{parent: ctes, bindings: bindings}
			}
			w.walkFields(value.Elem(), ctes, required, "With")
			return
		case *tree.TableName:
			name := string(node.Name())
			if w.lower == 1 {
				name = strings.ToLower(name)
			}
			if !node.ExplicitSchema {
				if name == "" || name == "dual" {
					return
				}
				if binding := ctes.lookup(name); binding != nil {
					w.expand(binding, required)
					return
				}
			}
			w.emit(node, required)
			w.walk(reflect.ValueOf(node.AtTsExpr), ctes, required)
			return
		case *tree.SampleExpr:
			columns, _ := node.GetColumns()
			w.walk(reflect.ValueOf(columns), ctes, required)
			return
		case tree.SampleExpr:
			columns, _ := node.GetColumns()
			w.walk(reflect.ValueOf(columns), ctes, required)
			return
		}
	}
	switch value.Kind() {
	case reflect.Pointer:
		w.walk(value.Elem(), ctes, required)
	case reflect.Struct:
		w.walkFields(value, ctes, required, "")
	case reflect.Slice, reflect.Array:
		for i := 0; i < value.Len(); i++ {
			w.walk(value.Index(i), ctes, required)
		}
	}
}

func (w *viewRefWalker) walkFields(value reflect.Value, ctes *viewCTEScope, required bool, skip string) {
	if value.Type().PkgPath() != viewTreePackage {
		return
	}
	for i := 0; i < value.NumField(); i++ {
		field := value.Type().Field(i)
		if field.IsExported() && field.Name != skip {
			w.walk(value.Field(i), ctes, required)
		}
	}
}
