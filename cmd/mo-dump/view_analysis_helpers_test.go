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
	"slices"
)

// These adapters preserve the existing single-schema/suffix regression oracles.
// Production uses only the complete analyzer and global graph, never this API.
func adjustViewOrder(ctx context.Context, database string, createTable []string, tables Tables, start int, mode viewSQLMode, definitions map[string]viewDefinition) error {
	views := make([]dumpView, 0, len(tables)-start)
	positions := make(map[viewName]int)
	for i, table := range tables[start:] {
		view := dumpView{name: viewName{database, table.Name}, create: createTable[start+i]}
		if definitions != nil {
			definition, ok := definitions[table.Name]
			if !ok {
				return fmt.Errorf("missing persisted definition for view %s.%s", database, table.Name)
			}
			view.definition = &definition
			view.create = definition.Stmt
		}
		positions[view.name] = start + i
		views = append(views, view)
	}
	ordered, err := orderDumpViews(ctx, views, mode)
	if err != nil {
		return err
	}
	ddl := make([]string, len(ordered))
	orderedTables := make(Tables, len(ordered))
	for i, view := range ordered {
		old := positions[view.name]
		ddl[i], orderedTables[i] = createTable[old], tables[old]
	}
	copy(createTable[start:], ddl)
	copy(tables[start:], orderedTables)
	return nil
}

func analyzeViewDependencies(database, ddl string, mode viewSQLMode) (viewAnalysis, error) {
	return analyzeView(context.Background(), database, ddl, mode)
}
func viewReferences(ctx context.Context, view dumpView, mode viewSQLMode) (map[viewName]struct{}, error) {
	analysis, err := analyzeDumpView(ctx, view, mode)
	return analysis.references, err
}
func viewDependencies(database, ddl string, mode viewSQLMode) ([]string, error) {
	return viewDependenciesInContext(database, database, ddl, mode, mode)
}
func viewDependenciesInContext(database, defaultDatabase, ddl string, mode, _ viewSQLMode) ([]string, error) {
	analysis, err := analyzeViewDependencies(defaultDatabase, ddl, mode)
	if err != nil {
		return nil, err
	}
	var names []string
	for ref := range analysis.references {
		if ref.database == database {
			names = append(names, ref.name)
		}
	}
	slices.Sort(names)
	return names, nil
}
