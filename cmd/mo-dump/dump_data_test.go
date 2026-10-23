// Copyright 2026 Matrix Origin
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/matrixorigin/mo_dump/internal/catalog"
	"github.com/stretchr/testify/require"
)

// TestDumpDataWholeDatabaseScript pins the emit contract of a whole-database
// dump that contains a sequence besides a table and a view
// (matrixorigin/matrixone#30577): sequences are emitted before the tables
// that can default to their NEXTVAL, views after every selected object, and
// -no-data exports sequence definitions without the allocation replay.
func TestDumpDataWholeDatabaseScript(t *testing.T) {
	ctx := context.Background()

	expectCommonQueries := func(mock sqlmock.Sqlmock) {
		mock.ExpectQuery("select datname, dat_type from mo_catalog.mo_database where datname = ?").WithArgs("db").
			WillReturnRows(sqlmock.NewRows([]string{"datname", "dat_type"}).AddRow("db", ""))
		mock.ExpectQuery("show create database `db`").
			WillReturnRows(sqlmock.NewRows([]string{"Database", "Create Database"}).AddRow("db", "CREATE DATABASE db"))
		// The catalog returns the sequence last and the view in the middle;
		// the script must still emit sequence, table, view.
		mock.ExpectQuery("select relname,relkind from mo_catalog.mo_tables where reldatabase = ?").WithArgs("db").
			WillReturnRows(sqlmock.NewRows([]string{"relname", "relkind"}).
				AddRow("t", catalog.SystemOrdinaryRel).
				AddRow("v", catalog.SystemViewRel).
				AddRow("s", catalog.SystemSequenceRel))
		mock.ExpectQuery("show create table `db`.`t`").
			WillReturnRows(sqlmock.NewRows([]string{"Table", "Create Table"}).
				AddRow("t", "CREATE TABLE t (id bigint)"))
		mock.ExpectQuery("select relname,viewdef from mo_catalog.mo_tables where reldatabase = ? and relkind = 'v' and relname in (?)").WithArgs("db", "v").
			WillReturnRows(sqlmock.NewRows([]string{"relname", "viewdef"}).
				AddRow("v", viewDefinitionJSON(t, "db", "CREATE VIEW v AS SELECT * FROM t", "")))
		mock.ExpectQuery("select mo_show_visible_bin(atttyp, 2) from mo_catalog.mo_columns where att_database = 'db' and att_relname = 's' and attname = 'last_seq_num'").
			WillReturnRows(sqlmock.NewRows([]string{"atttyp"}).AddRow("bigint"))
		mock.ExpectQuery("select last_seq_num, min_value, max_value, start_value, increment_value, cycle, is_called from `db`.`s`").
			WillReturnRows(sqlmock.NewRows([]string{
				"last_seq_num", "min_value", "max_value", "start_value", "increment_value", "cycle", "is_called",
			}).AddRow("15", "5", "100", "10", "5", "0", "1"))
	}

	// orderedContains asserts each snippet exists and that all the snippets
	// appear strictly in the listed order, without pinning the exact whole
	// script text: the view tail now interleaves parser-mode statements
	// whose exact shape belongs to the view subsystem's own tests.
	orderedContains := func(t *testing.T, text string, snippets []string) {
		t.Helper()
		pos := 0
		for _, snippet := range snippets {
			idx := strings.Index(text[pos:], snippet)
			require.GreaterOrEqualf(t, idx, 0, "missing in script after offset %d: %q\nscript:\n%s", pos, snippet, text)
			pos += idx + len(snippet)
		}
	}

	t.Run("with data", func(t *testing.T) {
		_, mock := exportTestDB(t)
		expectCommonQueries(mock)
		mock.ExpectQuery("select * from `db`.`t`").
			WillReturnRows(sqlmock.NewRowsWithColumnDefinition(sqlmock.NewColumn("id").OfType("INT", int64(0))).AddRow(1)).
			RowsWillBeClosed()
		// The catalog-mode query runs once for the collected views after every
		// selected database has been dumped.
		mock.ExpectQuery("select @@lower_case_table_names").
			WillReturnRows(sqlmock.NewRows([]string{"lower_case_table_names"}).AddRow(1))

		var buf bytes.Buffer
		sink := sinkOver(&buf)
		opt := Options{
			dbs:             []string{"db"},
			emptyTables:     true,
			netBufferLength: 16384,
			localInfile:     true,
			csvConf:         csvConfig{fieldDelimiter: ','},
		}
		require.NoError(t, opt.dumpData(ctx, sink))
		require.NoError(t, sink.Err())
		script := buf.String()
		orderedContains(t, script, []string{
			"SET foreign_key_checks = 0;\n\n",
			"DROP DATABASE IF EXISTS `db`;\nCREATE DATABASE db ;\nUSE `db`;\n\n\n",
			"DROP SEQUENCE IF EXISTS `s`;\nCREATE SEQUENCE `s` AS bigint INCREMENT BY 5 MINVALUE 5 MAXVALUE 100 START WITH 10 NO CYCLE;\nSELECT setval('s', 15, false);\n\n\n",
			"DROP TABLE IF EXISTS `t`;\nCREATE TABLE t (id bigint);\nINSERT INTO `t` VALUES (1);\n\n\n\n",
			"SET @modump_saved_sql_mode = @@SESSION.sql_mode;",
			"DROP VIEW IF EXISTS `db`.`v`;\nCREATE VIEW v AS SELECT * FROM t;\n\n\n",
			"SET SESSION sql_mode = @modump_saved_sql_mode;\n",
			"SET foreign_key_checks = 1;\n",
		})
		require.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("no data keeps sequence definition only", func(t *testing.T) {
		_, mock := exportTestDB(t)
		expectCommonQueries(mock)
		mock.ExpectQuery("select @@lower_case_table_names").
			WillReturnRows(sqlmock.NewRows([]string{"lower_case_table_names"}).AddRow(1))

		var buf bytes.Buffer
		sink := sinkOver(&buf)
		opt := Options{
			dbs:             []string{"db"},
			emptyTables:     true,
			noData:          true,
			netBufferLength: 16384,
			localInfile:     true,
			csvConf:         csvConfig{fieldDelimiter: ','},
		}
		require.NoError(t, opt.dumpData(ctx, sink))
		require.NoError(t, sink.Err())
		script := buf.String()
		require.Contains(t, script, "CREATE SEQUENCE `s` AS bigint INCREMENT BY 5 MINVALUE 5 MAXVALUE 100 START WITH 10 NO CYCLE;")
		require.NotContains(t, script, "setval(")
		require.NotContains(t, script, "INSERT INTO")
		require.Contains(t, script, "SET foreign_key_checks = 1;\n")
		require.NoError(t, mock.ExpectationsWereMet())
	})
}
