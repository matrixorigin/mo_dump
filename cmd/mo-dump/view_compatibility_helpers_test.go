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
	"github.com/matrixorigin/matrixone/pkg/sql/parsers/tree"
	"github.com/matrixorigin/mo_dump/internal/catalog"
)

// Adapt the old single-schema regression oracles to the one global collector.
func collectViewRefs(ctx context.Context, ddl string, lower int64, emit func(*tree.TableName, bool)) error {
	return collectViewRefsMode(ctx, ddl, viewSQLMode{lowerCaseTableNames: lower}, emit)
}
func collectViewRefsMode(ctx context.Context, ddl string, mode viewSQLMode, emit func(*tree.TableName, bool)) error {
	a, err := analyzeView(ctx, "", ddl, mode)
	if err != nil {
		return err
	}
	send := func(ref viewName, required bool) {
		emit(tree.NewTableName(tree.Identifier(ref.name), tree.ObjectNamePrefix{SchemaName: tree.Identifier(ref.database), ExplicitSchema: ref.database != ""}, nil), required)
	}
	for ref := range a.references {
		send(ref, true)
	}
	for ref := range a.advisory {
		if _, ok := a.references[ref]; !ok {
			send(ref, false)
		}
	}
	return nil
}
func adjustCompatibilityViewOrder(ctx context.Context, db string, lower int64, ddl []string, tables Tables, sqlMode ...string) error {
	if lower < 0 || lower > 2 {
		return fmt.Errorf("invalid lower_case_table_names")
	}
	start := 0
	for start < len(tables) && tables[start].Kind != catalog.SystemViewRel {
		start++
	}
	mode := viewSQLMode{lowerCaseTableNames: lower}
	if len(sqlMode) > 0 {
		mode.sqlMode = sqlMode[0]
	}
	return adjustViewOrder(ctx, db, ddl, tables, start, mode, nil)
}
