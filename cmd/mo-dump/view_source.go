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

	"github.com/matrixorigin/matrixone/pkg/sql/parsers/dialect"
	"github.com/matrixorigin/matrixone/pkg/sql/parsers/dialect/mysql"
	"github.com/matrixorigin/matrixone/pkg/sql/parsers/tree"
)

// parseViewSource returns the owning statement and its borrowed query source.
// The caller frees only the owner after dependency collection.
func parseViewSource(ctx context.Context, ddl string) (tree.Statement, tree.Statement, error) {
	stmt, err := mysql.ParseOne(ctx, ddl, 0)
	if err == nil {
		if view, ok := stmt.(*tree.CreateView); ok {
			return stmt, view.AsSource, nil
		}
		stmt.Free()
		return nil, nil, fmt.Errorf("expected CREATE VIEW, got %T", stmt)
	}

	// The pinned grammar supports standalone VALUES but not VALUES as a view
	// source. Use its own scanner to find AS, then the same parser to validate
	// both the view header and the complete VALUES query. Never infer relation
	// names from tokens or alter the DDL that dumpData emits.
	scanner := mysql.NewScanner(dialect.MYSQL, ddl)
	defer mysql.PutScanner(scanner)
	depth := 0
	for {
		token, _ := scanner.Scan()
		switch token {
		case 0, mysql.LEX_ERROR:
			return nil, nil, err
		case '(':
			depth++
		case ')':
			depth--
		case mysql.AS:
			if depth != 0 {
				continue
			}
			end := scanner.Pos
			header, headerErr := mysql.ParseOne(ctx, ddl[:end]+" SELECT 1", 0)
			if headerErr != nil {
				return nil, nil, err
			}
			_, isView := header.(*tree.CreateView)
			header.Free()
			if !isView {
				return nil, nil, err
			}
			query, queryErr := mysql.ParseOne(ctx, ddl[end:], 0)
			if queryErr != nil {
				return nil, nil, err
			}
			if _, ok := query.(*tree.ValuesStatement); ok {
				return query, query, nil
			}
			query.Free()
			return nil, nil, err
		}
	}
}
