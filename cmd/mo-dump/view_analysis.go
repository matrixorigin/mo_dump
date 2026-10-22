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

	"github.com/matrixorigin/matrixone/pkg/sql/parsers/dialect"
	"github.com/matrixorigin/matrixone/pkg/sql/parsers/dialect/mysql"
	"github.com/matrixorigin/matrixone/pkg/sql/parsers/tree"
)

type viewAnalysis struct {
	references          map[viewName]struct{}
	advisory            map[viewName]struct{}
	usesDefaultDatabase bool
	create              string
}

func analyzeView(ctx context.Context, database, ddl string, mode viewSQLMode) (viewAnalysis, error) {
	sqlMode := mode.sqlMode
	if mode.ansiQuotes {
		sqlMode += ",ANSI_QUOTES"
	}
	if mode.noBackslashEscapes {
		sqlMode += ",NO_BACKSLASH_ESCAPES"
	}
	statements, err := mysql.ParseWithSQLMode(ctx, ddl, mode.lowerCaseTableNames, sqlMode)
	if err != nil {
		return viewAnalysis{}, err
	}
	defer func() {
		for _, statement := range statements {
			statement.Free()
		}
	}()
	if len(statements) != 1 {
		return viewAnalysis{}, fmt.Errorf("expected one CREATE VIEW statement")
	}
	var query *tree.Select
	switch view := statements[0].(type) {
	case *tree.CreateView:
		query = view.AsSource
	case *tree.AlterView:
		query = view.AsSource
		// A persisted ALTER source needs CREATE on an empty target. Replace only
		// the validated statement verb, never reformat literals, names or the query.
		scanner := mysql.NewScanner(dialect.MYSQL, ddl)
		token, _ := scanner.Scan()
		start, end := scanner.PrePos, scanner.Pos
		mysql.PutScanner(scanner)
		if token != mysql.ALTER {
			return viewAnalysis{}, fmt.Errorf("expected ALTER VIEW verb")
		}
		raw := ddl[start:end]
		leading := len(raw) - len(strings.TrimLeft(raw, " \t\r\n"))
		ddl = ddl[:start+leading] + "CREATE" + ddl[end:]
	default:
		return viewAnalysis{}, fmt.Errorf("expected CREATE VIEW or ALTER VIEW")
	}
	collector := viewReferenceCollector{defaultDatabase: database, refs: make(map[viewName]struct{})}
	if err := collector.walk(reflect.ValueOf(query), nil); err != nil {
		return viewAnalysis{}, err
	}
	analysis := viewAnalysis{references: collector.refs, usesDefaultDatabase: collector.usesDefaultDatabase, create: ddl}
	// Required expansion finishes first. Optional prerequisites can improve
	// portability to older eager binders, but never invalidate a lazy view.
	collector.refs = make(map[viewName]struct{})
	for i := 0; i < len(collector.pending); i++ {
		binding := collector.pending[i]
		if binding.done {
			continue
		}
		binding.visiting = true
		err := collector.cteReferences(binding)
		binding.visiting = false
		binding.done = true
		if err != nil {
			collector.refs = nil
			break
		}
	}
	analysis.advisory = collector.refs
	return analysis, nil
}
