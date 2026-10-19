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
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/matrixorigin/matrixone/pkg/catalog"
	"github.com/stretchr/testify/require"
)

var identifierCases = []struct {
	name   string
	quoted string
}{
	{"plain", "`plain`"},
	{"a'b", "`a'b`"},
	{"a`b", "`a``b`"},
	{"a``b", "`a````b`"},
	{"a'`\\b", "`a'``\\b`"},
	{"中文", "`中文`"},
	{"a.b", "`a.b`"},
	{"a b", "`a b`"},
}

func newIdentifierMock(t *testing.T) sqlmock.Sqlmock {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	require.NoError(t, err)
	oldConn := conn
	conn = db
	t.Cleanup(func() {
		conn = oldConn
		expectErr := mock.ExpectationsWereMet()
		mock.ExpectClose()
		closeErr := db.Close()
		require.NoError(t, expectErr)
		require.NoError(t, closeErr)
	})
	return mock
}

func captureIdentifierOutput(t *testing.T, fn func()) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stdout")
	require.NoError(t, err)
	old := os.Stdout
	t.Cleanup(func() {
		os.Stdout = old
		require.NoError(t, f.Close())
	})
	os.Stdout = f
	fn()
	os.Stdout = old
	_, err = f.Seek(0, io.SeekStart)
	require.NoError(t, err)
	data, err := io.ReadAll(f)
	require.NoError(t, err)
	return string(data)
}

func TestQuoteIdentifier(t *testing.T) {
	for _, tc := range identifierCases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.quoted, quoteIdentifier(tc.name))
		})
	}
}

func TestQuoteString(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"", "''"},
		{"plain", "'plain'"},
		{"a'b", "'a''b'"},
		{"a`b", "'a`b'"},
		{"a\\'b", "'a\\\\''b'"},
	} {
		t.Run(tc.input, func(t *testing.T) {
			require.Equal(t, tc.want, quoteString(tc.input))
		})
	}
}

func TestCatalogIdentifiers(t *testing.T) {
	for _, sys := range []bool{false, true} {
		for _, selected := range []bool{false, true} {
			t.Run(fmt.Sprintf("sys=%t/selected=%t", sys, selected), func(t *testing.T) {
				mock := newIdentifierMock(t)
				const dbName = "d'`\\b"
				dbQuery := "select datname, dat_type from mo_catalog.mo_database where datname = ?"
				tblQuery := "select relname,relkind from mo_catalog.mo_tables where reldatabase = ?"
				if sys {
					dbQuery += " and account_id = 0"
					tblQuery += " and account_id = 0"
				}
				mock.ExpectQuery(dbQuery).WithArgs(dbName).
					WillReturnRows(sqlmock.NewRows([]string{"datname", "dat_type"}).AddRow(dbName, "")).RowsWillBeClosed()
				gotDB, err := getDatabaseType(context.Background(), dbName, sys)
				require.NoError(t, err)
				require.Equal(t, Db{Name: dbName}, gotDB)

				names := []string{"plain", "a'b", "a`b", "x') OR 1=1 --", "a\\'b"}
				want := make(Tables, 0, len(names))
				args := []driver.Value{dbName}
				rows := sqlmock.NewRows([]string{"relname", "relkind"})
				for _, name := range names {
					want = append(want, Table{Name: name, Kind: catalog.SystemOrdinaryRel})
					rows.AddRow(name, catalog.SystemOrdinaryRel)
					if selected {
						args = append(args, name)
					}
				}
				var tables Tables
				if selected {
					tblQuery += " and relname in (?,?,?,?,?)"
					tables = append(Tables(nil), want...)
				} else {
					rows.AddRow("__mo_hidden", catalog.SystemOrdinaryRel).AddRow("%!%hidden", catalog.SystemOrdinaryRel)
				}
				mock.ExpectQuery(tblQuery).WithArgs(args...).WillReturnRows(rows).RowsWillBeClosed()
				got, err := getTables(context.Background(), dbName, tables, sys)
				require.NoError(t, err)
				require.Equal(t, want, got)
			})
		}
	}
}

func TestCatalogIdentifierErrors(t *testing.T) {
	query := "select relname,relkind from mo_catalog.mo_tables where reldatabase = ? and relname in (?)"
	injected := errors.New("catalog unavailable")
	for _, scenario := range []string{"query", "missing", "scan", "iteration"} {
		t.Run(scenario, func(t *testing.T) {
			mock := newIdentifierMock(t)
			expect := mock.ExpectQuery(query).WithArgs("d'b", "a'b")
			switch scenario {
			case "query":
				expect.WillReturnError(injected)
			case "missing":
				expect.WillReturnRows(sqlmock.NewRows([]string{"relname", "relkind"})).RowsWillBeClosed()
			case "scan":
				expect.WillReturnRows(sqlmock.NewRows([]string{"relname", "relkind"}).AddRow(nil, "r")).RowsWillBeClosed()
			case "iteration":
				expect.WillReturnRows(sqlmock.NewRows([]string{"relname", "relkind"}).AddRow("a'b", "r").RowError(0, injected)).RowsWillBeClosed()
			}
			_, err := getTables(context.Background(), "d'b", Tables{{Name: "a'b"}}, false)
			switch scenario {
			case "query", "iteration":
				require.ErrorIs(t, err, injected)
			case "missing":
				require.ErrorContains(t, err, "table a'b not exists")
			default:
				require.ErrorContains(t, err, "converting NULL to string")
			}
		})
	}
}

func TestDatabaseIdentifierErrors(t *testing.T) {
	injected := errors.New("catalog unavailable")
	for _, scenario := range []string{"query", "missing", "scan", "iteration"} {
		t.Run(scenario, func(t *testing.T) {
			mock := newIdentifierMock(t)
			expect := mock.ExpectQuery("select datname, dat_type from mo_catalog.mo_database where datname = ?").WithArgs("d'b")
			switch scenario {
			case "query":
				expect.WillReturnError(injected)
			case "missing":
				expect.WillReturnRows(sqlmock.NewRows([]string{"datname", "dat_type"})).RowsWillBeClosed()
			case "scan":
				expect.WillReturnRows(sqlmock.NewRows([]string{"datname", "dat_type"}).AddRow(nil, "")).RowsWillBeClosed()
			case "iteration":
				expect.WillReturnRows(sqlmock.NewRows([]string{"datname", "dat_type"}).AddRow("d'b", "").RowError(0, injected)).RowsWillBeClosed()
			}
			_, err := getDatabaseType(context.Background(), "d'b", false)
			switch scenario {
			case "query", "iteration":
				require.ErrorIs(t, err, injected)
			case "missing":
				require.ErrorContains(t, err, "database d'b not exists")
			default:
				require.ErrorContains(t, err, "converting NULL to string")
			}
		})
	}
}

func TestIdentifierQueryErrors(t *testing.T) {
	injected := errors.New("query unavailable")
	for _, query := range []struct {
		name string
		sql  string
		run  func() error
	}{
		{"database", "show create database `d'``b`", func() error { _, err := getCreateDB("d'`b"); return err }},
		{"table", "show create table `d'``b`.`a'``b`", func() error { _, err := getCreateTable("d'`b", "a'`b"); return err }},
		{"view", "show create table `d'``b`.`a'``b`", func() error { _, err := getCreateView("d'`b", "a'`b"); return err }},
		{"select", "select * from `d'``b`.`a'``b`", func() error { return genOutput("d'`b", "a'`b", nil, 1024, false, &csvConfig{}, "") }},
	} {
		t.Run(query.name, func(t *testing.T) {
			mock := newIdentifierMock(t)
			mock.ExpectQuery(query.sql).WillReturnError(injected)
			require.ErrorIs(t, query.run(), injected)
		})
	}
}

func TestDumpIdentifiers(t *testing.T) {
	for _, tc := range identifierCases {
		for _, mode := range []string{"database", "table", "schema"} {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				mock := newIdentifierMock(t)
				const dbName = "d'`b"
				const quotedDB = "`d'``b`"
				opt := Options{dbs: []string{dbName}, emptyTables: mode != "table", noData: mode == "schema", netBufferLength: 1024}
				mock.ExpectQuery("select datname, dat_type from mo_catalog.mo_database where datname = ?").WithArgs(dbName).
					WillReturnRows(sqlmock.NewRows([]string{"datname", "dat_type"}).AddRow(dbName, "")).RowsWillBeClosed()
				want := "SET foreign_key_checks = 0;\n\n"
				query := "select relname,relkind from mo_catalog.mo_tables where reldatabase = ?"
				args := []driver.Value{dbName}
				if opt.emptyTables {
					mock.ExpectQuery("show create database " + quotedDB).
						WillReturnRows(sqlmock.NewRows([]string{"Database", "Create"}).AddRow(dbName, "CREATE DATABASE "+quotedDB)).RowsWillBeClosed()
					want += "DROP DATABASE IF EXISTS " + quotedDB + ";\nCREATE DATABASE " + quotedDB + " ;\nUSE " + quotedDB + ";\n\n\n"
				} else {
					opt.tables = Tables{{Name: tc.name}}
					opt.where = "id = 1"
					query += " and relname in (?)"
					args = append(args, tc.name)
				}
				mock.ExpectQuery(query).WithArgs(args...).
					WillReturnRows(sqlmock.NewRows([]string{"relname", "relkind"}).AddRow(tc.name, catalog.SystemOrdinaryRel)).RowsWillBeClosed()
				ddl := "CREATE TABLE " + tc.quoted + " (id INT, v VARCHAR(20))"
				mock.ExpectQuery("show create table " + quotedDB + "." + tc.quoted).
					WillReturnRows(sqlmock.NewRows([]string{"Table", "Create"}).AddRow(tc.name, ddl)).RowsWillBeClosed()
				want += "DROP TABLE IF EXISTS " + tc.quoted + ";\n" + ddl + ";\n"
				if !opt.noData {
					query = "select * from " + quotedDB + "." + tc.quoted
					if opt.where != "" {
						query += " where id = 1"
					}
					mock.ExpectQuery(query).WillReturnRows(sqlmock.NewRowsWithColumnDefinition(
						sqlmock.NewColumn("id").OfType("INT", int64(1)), sqlmock.NewColumn("v").OfType("VARCHAR", "row"),
					).AddRow(1, "row")).RowsWillBeClosed()
					want += "INSERT INTO " + tc.quoted + " VALUES (1,'row');\n\n\n\n"
				}
				want += "SET foreign_key_checks = 1;\n"
				got := captureIdentifierOutput(t, func() { require.NoError(t, opt.dumpData(context.Background())) })
				require.Equal(t, want, got)
			})
		}
	}
}

func TestDumpOtherRelationIdentifiers(t *testing.T) {
	for _, kind := range []string{catalog.SystemExternalRel, catalog.SystemViewRel} {
		t.Run(kind, func(t *testing.T) {
			mock := newIdentifierMock(t)
			opt := Options{dbs: []string{"d"}, tables: Tables{{Name: "a`b"}}, noData: true}
			mock.ExpectQuery("select datname, dat_type from mo_catalog.mo_database where datname = ?").WithArgs("d").
				WillReturnRows(sqlmock.NewRows([]string{"datname", "dat_type"}).AddRow("d", "")).RowsWillBeClosed()
			mock.ExpectQuery("select relname,relkind from mo_catalog.mo_tables where reldatabase = ? and relname in (?)").WithArgs("d", "a`b").
				WillReturnRows(sqlmock.NewRows([]string{"relname", "relkind"}).AddRow("a`b", kind)).RowsWillBeClosed()
			var ddl, drop string
			var rows *sqlmock.Rows
			if kind == catalog.SystemViewRel {
				ddl = "CREATE VIEW `a``b` AS SELECT 1"
				drop = "DROP VIEW IF EXISTS `a``b`;\n"
				rows = sqlmock.NewRows([]string{"View", "Create", "charset", "collation"}).AddRow("a`b", ddl, "utf8mb4", "utf8mb4_bin")
			} else {
				ddl = "CREATE EXTERNAL TABLE `a``b` (id INT)"
				drop = "/*!EXTERNAL TABLE `a``b`*/\nDROP TABLE IF EXISTS `a``b`;\n"
				rows = sqlmock.NewRows([]string{"Table", "Create"}).AddRow("a`b", ddl)
			}
			mock.ExpectQuery("show create table `d`.`a``b`").WillReturnRows(rows).RowsWillBeClosed()
			if kind == catalog.SystemViewRel {
				mock.ExpectQuery("select @@lower_case_table_names").WillReturnRows(sqlmock.NewRows([]string{"lower_case_table_names"}).AddRow(1))
			}
			got := captureIdentifierOutput(t, func() { require.NoError(t, opt.dumpData(context.Background())) })
			require.Equal(t, "SET foreign_key_checks = 0;\n\n"+drop+ddl+";\n\n\nSET foreign_key_checks = 1;\n", got)
		})
	}
}

func TestLoadIdentifiers(t *testing.T) {
	for _, local := range []bool{false, true} {
		t.Run(fmt.Sprintf("local=%t", local), func(t *testing.T) {
			mock := newIdentifierMock(t)
			oldDir, err := os.Getwd()
			require.NoError(t, err)
			root := t.TempDir()
			dir := filepath.Join(root, "work'`\\dir")
			require.NoError(t, os.Mkdir(dir, 0700))
			t.Cleanup(func() { require.NoError(t, os.Chdir(oldDir)) })
			require.NoError(t, os.Chdir(dir))
			t.Setenv("PWD", dir)
			mock.ExpectQuery("select * from `d`.`a'``\\b`").WillReturnRows(sqlmock.NewRowsWithColumnDefinition(
				sqlmock.NewColumn("id").OfType("INT", int64(1)),
			).AddRow(1)).RowsWillBeClosed()
			got := captureIdentifierOutput(t, func() {
				require.NoError(t, genOutput("d", "a'`\\b", nil, 1024, local, &csvConfig{enable: true, fieldDelimiter: ','}, ""))
			})
			prefix := "LOAD DATA"
			if local {
				prefix += " LOCAL"
			}
			require.Equal(t, prefix+" INFILE '"+root+"/work''`\\\\dir/d_a''`\\\\b.csv' INTO TABLE `a'``\\b` FIELDS TERMINATED BY ',' ENCLOSED BY '\"' LINES TERMINATED BY '\\n' PARALLEL 'FALSE';\n", got)
			data, err := os.ReadFile(filepath.Join(dir, "d_a'`\\b.csv"))
			require.NoError(t, err)
			require.Equal(t, "1\n", string(data))
		})
	}
}
