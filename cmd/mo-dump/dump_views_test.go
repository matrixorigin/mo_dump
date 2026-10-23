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
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/matrixorigin/mo_dump/internal/catalog"
	"github.com/stretchr/testify/require"
)

func TestDumpCrossDatabaseViews(t *testing.T) {
	for _, tt := range []struct {
		name   string
		dbs    []string
		noData bool
	}{
		{"view_first", []string{"view_db", "data_db"}, false},
		{"data_first", []string{"data_db", "view_db"}, false},
		{"schema_view_first", []string{"view_db", "data_db"}, true},
		{"schema_data_first", []string{"data_db", "view_db"}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			mock, output := mockDumpOutput(t)
			dbs := tt.dbs
			for _, name := range dbs {
				mock.ExpectQuery("select datname, dat_type from mo_catalog.mo_database where datname = ?" + dbScopePredicate).WithArgs(name).WillReturnRows(sqlmock.NewRows([]string{"datname", "dat_type"}).AddRow(name, ""))
				mock.ExpectQuery("show create database `" + name + "`").WillReturnRows(sqlmock.NewRows([]string{"Database", "Create Database"}).AddRow(name, "CREATE DATABASE `"+name+"`"))
				rows := sqlmock.NewRows([]string{"relname", "relkind"})
				if name == "view_db" {
					rows.AddRow("v", catalog.SystemViewRel)
				} else {
					rows.AddRow("t", catalog.SystemOrdinaryRel)
				}
				mock.ExpectQuery("select relname,relkind from mo_catalog.mo_tables where reldatabase = ?"+tablesScopePredicate+hiddenHelpersFor(false)).WithArgs(name, name).WillReturnRows(rows)
				if name == "view_db" {
					mock.ExpectQuery("select relname,viewdef from mo_catalog.mo_tables where reldatabase = ? and relkind = 'v' and account_id = current_account_id() and relname in (?)").WithArgs("view_db", "v").WillReturnRows(sqlmock.NewRows([]string{"relname", "viewdef"}).AddRow("v", viewDefinitionJSON(t, "view_db", "CREATE VIEW v AS SELECT id, v FROM data_db.t WHERE v > 1", "")))
				} else {
					mock.ExpectQuery("show create table `data_db`.`t`").WillReturnRows(sqlmock.NewRows([]string{"Table", "Create Table"}).AddRow("t", "CREATE TABLE t (id INT PRIMARY KEY, v DECIMAL(10,2))"))
					if !tt.noData {
						mock.ExpectQuery("select * from `data_db`.`t`").WillReturnRows(sqlmock.NewRowsWithColumnDefinition(sqlmock.NewColumn("id").OfType("INT", 0), sqlmock.NewColumn("v").OfType("DECIMAL", "")).AddRow(1, "1.25").AddRow(2, "2.50")).RowsWillBeClosed()
					}
				}
			}
			mock.ExpectQuery("select @@lower_case_table_names").WillReturnRows(sqlmock.NewRows([]string{"lower_case_table_names"}).AddRow(1))
			opt := Options{dbs: dbs, emptyTables: true, noData: tt.noData, netBufferLength: defaultNetBufferLength}
			require.NoError(t, opt.dumpData(context.Background()))
			require.NoError(t, mock.ExpectationsWereMet())
			content, err := os.ReadFile(output.Name())
			require.NoError(t, err)
			text := string(content)
			table, view := strings.Index(text, "CREATE TABLE t"), strings.Index(text, "CREATE VIEW v")
			require.Greater(t, table, -1)
			require.Greater(t, view, table, "all base tables must precede views:\n%s", text)
			require.Contains(t, text[strings.LastIndex(text[:view], "USE "):view], "USE `view_db`;")
			if tt.noData {
				require.NotContains(t, text, "INSERT INTO")
			} else {
				insert := strings.Index(text, "INSERT INTO `t` VALUES (1,'1.25'),(2,'2.50');")
				require.Greater(t, insert, table)
				require.Less(t, insert, view)
			}
			require.True(t, strings.HasSuffix(text, "SET foreign_key_checks = 1;\n"))
		})
	}
}

func viewDefinitionJSON(t *testing.T, database, create, sqlMode string) string {
	t.Helper()
	encoded, err := json.Marshal(map[string]string{"Stmt": create, "DefaultDatabase": database, "sql_mode": sqlMode})
	require.NoError(t, err)
	return string(encoded)
}

func mockDumpOutput(t *testing.T) (sqlmock.Sqlmock, *os.File) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	require.NoError(t, err)
	oldConn := conn
	t.Cleanup(func() { conn = oldConn; _ = db.Close() })
	conn = db
	output, err := os.CreateTemp(t.TempDir(), "dump.sql")
	require.NoError(t, err)
	oldStdout := os.Stdout
	t.Cleanup(func() { os.Stdout = oldStdout; _ = output.Close() })
	os.Stdout = output
	return mock, output
}

func TestDumpViewsFailure(t *testing.T) {
	for _, failure := range []string{"database", "database_ddl", "tables", "view_ddl", "table_ddl", "data", "case_setting", "invalid_view", "cycle"} {
		t.Run(failure, func(t *testing.T) {
			mock, output := mockDumpOutput(t)
			injected := errors.New("injected " + failure)
			create := "CREATE VIEW v AS SELECT * FROM t"
			if failure == "invalid_view" {
				create = "CREATE VIEW v AS SELECT FROM"
			}
			if failure == "cycle" {
				create = "CREATE VIEW v AS SELECT * FROM v"
			}
			type queryStep struct {
				name, query string
				rows        *sqlmock.Rows
			}
			steps := []queryStep{
				{"database", "select datname, dat_type from mo_catalog.mo_database where datname = ?" + dbScopePredicate, sqlmock.NewRows([]string{"datname", "dat_type"}).AddRow("db", "")},
				{"database_ddl", "show create database `db`", sqlmock.NewRows([]string{"Database", "Create Database"}).AddRow("db", "CREATE DATABASE db")},
				{"tables", "select relname,relkind from mo_catalog.mo_tables where reldatabase = ?" + tablesScopePredicate + hiddenHelpersFor(false), sqlmock.NewRows([]string{"relname", "relkind"}).AddRow("v", catalog.SystemViewRel).AddRow("t", catalog.SystemOrdinaryRel)},
				{"view_ddl", "select relname,viewdef from mo_catalog.mo_tables where reldatabase = ? and relkind = 'v' and account_id = current_account_id() and relname in (?)", sqlmock.NewRows([]string{"relname", "viewdef"}).AddRow("v", viewDefinitionJSON(t, "db", create, ""))},
				{"table_ddl", "show create table `db`.`t`", sqlmock.NewRows([]string{"Table", "Create Table"}).AddRow("t", "CREATE TABLE t (x INT)")},
			}
			if failure == "data" {
				steps = append(steps, queryStep{"data", "select * from `db`.`t`", nil})
			} else {
				steps = append(steps, queryStep{"case_setting", "select @@lower_case_table_names", sqlmock.NewRows([]string{"lower_case_table_names"}).AddRow(1)})
			}
			for _, step := range steps {
				expect := mock.ExpectQuery(step.query)
				if step.name == "database" {
					expect.WithArgs("db")
				}
				if step.name == "tables" {
					expect.WithArgs("db", "db")
				}
				if step.name == "view_ddl" {
					expect.WithArgs("db", "v")
				}
				if failure == step.name {
					expect.WillReturnError(injected)
					break
				}
				expect.WillReturnRows(step.rows)
			}
			opt := Options{dbs: []string{"db"}, emptyTables: true, noData: failure != "data", netBufferLength: defaultNetBufferLength}
			err := opt.dumpData(context.Background())
			switch failure {
			case "invalid_view":
				require.ErrorContains(t, err, "analyze view db.v")
			case "cycle":
				require.ErrorContains(t, err, "cyclic view dependencies")
			default:
				require.ErrorIs(t, err, injected)
			}
			require.NoError(t, mock.ExpectationsWereMet())
			content, readErr := os.ReadFile(output.Name())
			require.NoError(t, readErr)
			require.NotContains(t, string(content), "CREATE VIEW")
			require.NotContains(t, string(content), "SET foreign_key_checks = 1;")
		})
	}
}

func TestDumpWithoutViews(t *testing.T) {
	for _, kind := range []string{"", catalog.SystemOrdinaryRel, catalog.SystemExternalRel} {
		t.Run("kind="+kind, func(t *testing.T) {
			mock, output := mockDumpOutput(t)
			mock.ExpectQuery("select datname, dat_type from mo_catalog.mo_database where datname = ?" + dbScopePredicate).WithArgs("db").WillReturnRows(sqlmock.NewRows([]string{"datname", "dat_type"}).AddRow("db", ""))
			mock.ExpectQuery("show create database `db`").WillReturnRows(sqlmock.NewRows([]string{"Database", "Create Database"}).AddRow("db", "CREATE DATABASE db"))
			tables := sqlmock.NewRows([]string{"relname", "relkind"})
			if kind != "" {
				tables.AddRow("t", kind)
			}
			mock.ExpectQuery("select relname,relkind from mo_catalog.mo_tables where reldatabase = ?"+tablesScopePredicate+hiddenHelpersFor(false)).WithArgs("db", "db").WillReturnRows(tables)
			if kind != "" {
				mock.ExpectQuery("show create table `db`.`t`").WillReturnRows(sqlmock.NewRows([]string{"Table", "Create Table"}).AddRow("t", "CREATE TABLE t (x INT)"))
			}
			opt := Options{dbs: []string{"db"}, emptyTables: true, noData: true}
			require.NoError(t, opt.dumpData(context.Background()))
			require.NoError(t, mock.ExpectationsWereMet())
			content, err := os.ReadFile(output.Name())
			require.NoError(t, err)
			if kind != "" {
				require.Contains(t, string(content), "CREATE TABLE t (x INT)")
			}
			require.NotContains(t, string(content), "CREATE VIEW")
			require.True(t, strings.HasSuffix(string(content), "SET foreign_key_checks = 1;\n"))
		})
	}
}
