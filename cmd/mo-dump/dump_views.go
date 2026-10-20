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

func normalizedViewName(database, name string, mode viewSQLMode) viewName {
	return viewName{mode.name(database), mode.name(name)}
}

// orderDumpViews sorts only dependencies within this export. Base tables have
// already been emitted; references outside the selection remain the caller's
// responsibility, just as for a single-database or selected-table dump.
func orderDumpViews(ctx context.Context, views []dumpView, mode viewSQLMode) ([]dumpView, error) {
	positions := make(map[viewName]int, len(views))
	for i, view := range views {
		// Catalog names are already object identities. Only references are
		// normalized by their owning view's parser environment.
		key := view.name
		if _, exists := positions[key]; exists {
			return nil, fmt.Errorf("duplicate view %s.%s", view.name.database, view.name.name)
		}
		positions[key] = i
	}
	pending := make([]int, len(views))
	dependents := make([][]int, len(views))
	for i, view := range views {
		refs, err := viewReferences(ctx, view, mode)
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

func viewReferences(ctx context.Context, view dumpView, mode viewSQLMode) (map[viewName]struct{}, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if view.definition != nil {
		mode = mode.withSQLMode(view.definition.parserMode())
		if lower := view.definition.LowerCaseTableNames; lower != nil {
			mode.lowerCaseTableNames = *lower
		}
	}
	refs, err := viewDependencies(view.name.database, view.create, mode)
	if err != nil {
		return nil, fmt.Errorf("analyze view %s.%s: %w", view.name.database, view.name.name, err)
	}
	return refs, nil
}

func quoteIdentifier(name string) string { return "`" + strings.ReplaceAll(name, "`", "``") + "`" }
