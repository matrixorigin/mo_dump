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
	"strings"
)

type viewName struct{ database, name string }
type dumpView struct {
	name       viewName
	create     string
	definition *viewDefinition
}

func (view dumpView) defaultDatabase() string {
	if view.definition != nil {
		return view.definition.DefaultDatabase
	}
	return view.name.database
}

type viewEdge struct {
	child    int
	required bool
}

// orderDumpViews includes only selected views. Base tables/data precede this
// phase; omitted objects and historical snapshots remain restore prerequisites.
// Catalog identities are never folded under the exporting session's mode.
func orderDumpViews(ctx context.Context, views []dumpView, mode viewSQLMode) ([]dumpView, error) {
	positions := make(map[viewName]int, len(views))
	for i, view := range views {
		if _, exists := positions[view.name]; exists {
			return nil, fmt.Errorf("duplicate view %s.%s", view.name.database, view.name.name)
		}
		positions[view.name] = i
	}
	dependents := make([][]viewEdge, len(views))
	for i, view := range views {
		analysis, err := analyzeDumpView(ctx, view, mode)
		if err != nil {
			return nil, err
		}
		for ref := range analysis.references {
			if parent, ok := positions[ref]; ok {
				dependents[parent] = append(dependents[parent], viewEdge{i, true})
			}
		}
		for ref := range analysis.advisory {
			if _, required := analysis.references[ref]; required {
				continue
			}
			if parent, ok := positions[ref]; ok {
				dependents[parent] = append(dependents[parent], viewEdge{i, false})
			}
		}
	}
	sort := func(requiredOnly bool) ([]int, []int, error) {
		pending := make([]int, len(views))
		for _, edges := range dependents {
			for _, edge := range edges {
				if !requiredOnly || edge.required {
					pending[edge.child]++
				}
			}
		}
		ready := make([]int, 0, len(views))
		for i, n := range pending {
			if n == 0 {
				ready = append(ready, i)
			}
		}
		for next := 0; next < len(ready); next++ {
			if err := ctx.Err(); err != nil {
				return nil, nil, err
			}
			for _, edge := range dependents[ready[next]] {
				if requiredOnly && !edge.required {
					continue
				}
				pending[edge.child]--
				if pending[edge.child] == 0 {
					ready = append(ready, edge.child)
				}
			}
		}
		return ready, pending, nil
	}
	// Prefer optional prerequisites when jointly sortable, never reject or lose
	// a required edge because of an unused-CTE cycle. At most two linear passes.
	order, pending, err := sort(false)
	if err != nil {
		return nil, err
	}
	if len(order) != len(views) {
		order, pending, err = sort(true)
	}
	if err != nil {
		return nil, err
	}
	if len(order) != len(views) {
		var blocked []string
		for i, n := range pending {
			if n > 0 {
				blocked = append(blocked, views[i].name.database+"."+views[i].name.name)
			}
		}
		return nil, fmt.Errorf("cyclic view dependencies; blocked views: %s", strings.Join(blocked, ", "))
	}
	ordered := make([]dumpView, 0, len(order))
	for _, i := range order {
		ordered = append(ordered, views[i])
	}
	return ordered, nil
}

func analyzeDumpView(ctx context.Context, view dumpView, mode viewSQLMode) (viewAnalysis, error) {
	if err := ctx.Err(); err != nil {
		return viewAnalysis{}, err
	}
	if view.definition != nil {
		var err error
		mode, err = view.definition.parserMode(mode)
		if err != nil {
			return viewAnalysis{}, err
		}
	}
	analysis, err := analyzeView(ctx, view.defaultDatabase(), view.create, mode)
	if err != nil {
		return viewAnalysis{}, fmt.Errorf("analyze view %s.%s: %w", view.name.database, view.name.name, err)
	}
	return analysis, nil
}

// quoteIdentifier quotes one identifier, not a qualified name.
func quoteIdentifier(name string) string { return "`" + strings.ReplaceAll(name, "`", "``") + "`" }

// quoteString uses the same backslash-escape mode as ordinary dump data.
func quoteString(value string) string {
	value = strings.ReplaceAll(value, "\\", "\\\\")
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}
