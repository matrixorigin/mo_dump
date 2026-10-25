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
	"os"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/matrixorigin/mo_dump/internal/catalog"
	"github.com/stretchr/testify/require"
)

func TestViewQualifierBindingOrder(t *testing.T) {
	for _, tt := range []struct {
		name, query string
		refs        []viewName
	}{
		{"cte_named_snapshot_ignored", "WITH c AS (SELECT * FROM z_source) SELECT * FROM c {snapshot='missing'}", []viewName{{"db", "z_source"}}},
		{"cte_timestamp_expression_ignored", "WITH c AS (SELECT * FROM z_source) SELECT * FROM c {timestamp=(SELECT x FROM a_consumer)}", []viewName{{"db", "z_source"}}},
		{"cte_qualified_catalog_control", "WITH c AS (SELECT * FROM z_source) SELECT * FROM db.c {timestamp=(SELECT x FROM clock_view)}", []viewName{{"db", "clock_view"}}},
		{"dual_timestamp_expression_ignored", "SELECT 1 AS x FROM dual {timestamp=(SELECT x FROM a_consumer)}", nil},
		{"dual_named_snapshot_ignored", "SELECT 1 AS x FROM dual {snapshot='missing'}", nil},
		{"dual_wins_over_cte", "WITH dual AS (SELECT * FROM z_source) SELECT 1 AS x FROM dual {timestamp=(SELECT x FROM a_consumer)}", nil},
		{"qualified_dual_catalog_control", "SELECT * FROM db.dual {timestamp=(SELECT x FROM clock_view)}", []viewName{{"db", "clock_view"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			refs, err := viewReferences(context.Background(), dumpView{name: viewName{"db", "a_consumer"}, create: "CREATE VIEW a_consumer AS " + tt.query}, viewSQLMode{lowerCaseTableNames: 1})
			require.NoError(t, err)
			require.Len(t, refs, len(tt.refs))
			for _, ref := range tt.refs {
				require.Contains(t, refs, ref)
			}
		})
	}
}

func TestViewODBCOuterJoins(t *testing.T) {
	for _, query := range []string{
		"SELECT t.x FROM {OJ t LEFT JOIN z_source ON t.x=z_source.x}",
		"SELECT t.x FROM {oj t RIGHT JOIN z_source ON t.x=z_source.x}",
		"SELECT t.x FROM {OJ (t LEFT JOIN z_source ON t.x=z_source.x)}",
		"SELECT t.x FROM {OJ t LEFT JOIN ({OJ u LEFT JOIN z_source ON true}) ON true}",
		"SELECT t.x FROM {OJ t LEFT JOIN z_source ON true},u",
	} {
		t.Run(query, func(t *testing.T) {
			refs, err := viewReferences(context.Background(), dumpView{name: viewName{"db", "a_consumer"}, create: "CREATE VIEW a_consumer AS " + query}, viewSQLMode{lowerCaseTableNames: 1})
			require.NoError(t, err)
			expected := map[viewName]struct{}{{"db", "t"}: {}, {"db", "z_source"}: {}}
			if strings.Contains(query, " u ") || strings.Contains(query, ",u") {
				expected[viewName{"db", "u"}] = struct{}{}
			}
			require.Equal(t, expected, refs)
		})
	}
}

func TestDumpStoredViewSQLMode(t *testing.T) {
	mock, output := mockDumpOutput(t)
	mock.ExpectQuery("select datname, dat_type from mo_catalog.mo_database where datname = ?" + dbScopePredicate).WithArgs("db").WillReturnRows(sqlmock.NewRows([]string{"datname", "dat_type"}).AddRow("db", ""))
	mock.ExpectQuery("show create database `db`").WillReturnRows(sqlmock.NewRows([]string{"Database", "Create Database"}).AddRow("db", "CREATE DATABASE db"))
	mock.ExpectQuery("select relname,relkind from mo_catalog.mo_tables where reldatabase = ?"+tablesScopePredicate+hiddenHelpersFor(false)).WithArgs("db", "db").WillReturnRows(sqlmock.NewRows([]string{"relname", "relkind"}).AddRow("a_consumer", catalog.SystemViewRel).AddRow("z_source", catalog.SystemViewRel))
	createConsumer := `CREATE VIEW a_consumer AS SELECT x FROM "z_source"`
	query := "select relname,viewdef from mo_catalog.mo_tables where reldatabase = ? and relkind = 'v' and account_id = current_account_id() and relname in (?)"
	mock.ExpectQuery(query).WithArgs("db", "a_consumer").WillReturnRows(sqlmock.NewRows([]string{"relname", "viewdef"}).AddRow("a_consumer", viewDefinitionJSON(t, "db", createConsumer, "ANSI_QUOTES")))
	mock.ExpectQuery(query).WithArgs("db", "z_source").WillReturnRows(sqlmock.NewRows([]string{"relname", "viewdef"}).AddRow("z_source", viewDefinitionJSON(t, "db", "CREATE VIEW z_source AS SELECT 7 AS x", "")))
	mock.ExpectQuery("select @@lower_case_table_names").WillReturnRows(sqlmock.NewRows([]string{"lower_case_table_names"}).AddRow(1))
	opt := Options{dbs: []string{"db"}, emptyTables: true, noData: true}
	require.NoError(t, opt.dumpData(context.Background()))
	require.NoError(t, mock.ExpectationsWereMet())
	content, err := os.ReadFile(output.Name())
	require.NoError(t, err)
	text := string(content)
	sourcePos, consumerPos := strings.Index(text, "CREATE VIEW z_source"), strings.Index(text, createConsumer)
	require.Greater(t, sourcePos, -1)
	require.Greater(t, consumerPos, sourcePos)
	require.Equal(t, 2, strings.Count(text, "SET @modump_saved_sql_mode = @@SESSION.sql_mode;"))
	require.Equal(t, 2, strings.Count(text, "SET SESSION sql_mode = @modump_saved_sql_mode;"))
}
