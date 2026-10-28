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
	"bytes"
	"context"
	"flag"
	"os"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/matrixorigin/mo_dump/internal/catalog"
	"github.com/stretchr/testify/require"
)

// Called by TestDumpExitStatus in its isolated child process, reusing the
// CLI exit/publication fixture rather than starting a database for flag tests.
func runPermanentConfirmationCLI(t *testing.T, mode string) {
	t.Helper()
	const shaped = "__mo_tmp_0123456789abcdef0123456789abcdef_user"
	const other = "__mo_tmp_0123456789abcdef0123456789abcdef_a,b`c"
	mock := newCatalogMock(t)
	args := []string{"mo-dump", "-db", "db", "-confirm-permanent-table", shaped}
	switch mode {
	case "confirm_all":
		args[2] = "all"
	case "confirm_multiple":
		args[2] = "db,other"
	case "confirm_tbl":
		args = append(args, "-tbl", "plain")
	case "confirm_empty":
		args[4] = ""
	case "confirm_missing", "confirm_success":
		mock.ExpectQuery("select datname, dat_type from mo_catalog.mo_database where datname = ?" + dbScopePredicate).WithArgs("db").
			WillReturnRows(sqlmock.NewRows([]string{"datname", "dat_type"}).AddRow("db", ""))
		mock.ExpectQuery("show create database `db`").
			WillReturnRows(sqlmock.NewRows([]string{"Database", "Create Database"}).AddRow("db", "CREATE DATABASE db"))
		rows := sqlmock.NewRows([]string{"relname", "relkind"}).AddRow("plain", "r")
		if mode == "confirm_success" {
			args = append(args, "-confirm-permanent-table", other, "-confirm-permanent-table", shaped)
			rows.AddRow(shaped, "r").AddRow(other, "")
		}
		mock.ExpectQuery("select relname,relkind from mo_catalog.mo_tables where reldatabase = ?"+tablesScopePredicate+hiddenHelpersFor(false)).WithArgs("db", "db").WillReturnRows(rows).RowsWillBeClosed()
		if mode == "confirm_success" {
			for _, name := range []string{"plain", shaped, other} {
				mock.ExpectQuery("show create table `db`." + quoteIdentifier(name)).
					WillReturnRows(sqlmock.NewRows([]string{"Table", "Create Table"}).AddRow(name, "CREATE TABLE "+quoteIdentifier(name)+" (id int)"))
			}
			for _, name := range []string{"plain", shaped, other} {
				mock.ExpectQuery("select * from `db`." + quoteIdentifier(name)).
					WillReturnRows(sqlmock.NewRowsWithColumnDefinition(sqlmock.NewColumn("id").OfType("INT", int64(0))).AddRow(1)).RowsWillBeClosed()
			}
		}
	}
	if mode != "confirm_success" {
		args = append(args, "-csv")
	}
	mock.ExpectClose()
	flag.CommandLine = flag.NewFlagSet("mo-dump", flag.ExitOnError)
	os.Args = args
	main()
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestValidatePermanentTableOptions(t *testing.T) {
	const name = "__mo_tmp_0123456789abcdef0123456789abcdef_t"
	for _, tc := range []struct {
		name      string
		dbs       []string
		tbl       string
		tables    Tables
		confirm   Tables
		wantError string
	}{
		{"unused option", []string{"all"}, "t", Tables{{Name: "t"}}, nil, ""},
		{"single database", []string{"d"}, "", nil, Tables{{Name: name}}, ""},
		{"duplicates and literal punctuation", []string{"d"}, "", nil, Tables{{Name: name}, {Name: name}, {Name: name + ",`x "}}, ""},
		{"no database", nil, "", nil, Tables{{Name: name}}, "one explicit database"},
		{"all databases", []string{"all"}, "", nil, Tables{{Name: name}}, "one explicit database"},
		{"multiple databases", []string{"d", "other"}, "", nil, Tables{{Name: name}}, "one explicit database"},
		{"explicit selection", []string{"d"}, "t", nil, Tables{{Name: name}}, "cannot be combined with -tbl"},
		{"resolved selection", []string{"d"}, "", Tables{{Name: "t"}}, Tables{{Name: name}}, "cannot be combined with -tbl"},
		{"empty confirmation", []string{"d"}, "", nil, Tables{{Name: ""}}, "non-empty table name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opt := Options{dbs: tc.dbs, tbl: tc.tbl, tables: tc.tables, confirmedPermanentTables: tc.confirm}
			err := opt.validatePermanentTableOptions(context.Background())
			if tc.wantError == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.wantError)
			}
		})
	}
}

func TestPermanentTableConfirmationRejectsSubscription(t *testing.T) {
	mock := newCatalogMock(t)
	mock.ExpectQuery("select datname, dat_type from mo_catalog.mo_database where datname = ?" + dbScopePredicate).WithArgs("d").
		WillReturnRows(sqlmock.NewRows([]string{"datname", "dat_type"}).AddRow("d", catalog.SystemDBTypeSubscription)).RowsWillBeClosed()
	opt := Options{dbs: []string{"d"}, confirmedPermanentTables: Tables{{Name: "__mo_tmp_0123456789abcdef0123456789abcdef_t"}}}
	var out bytes.Buffer
	err := opt.dumpData(context.Background(), sinkOver(&out))
	require.ErrorContains(t, err, "cannot validate subscription database")
	require.NotContains(t, out.String(), "CREATE DATABASE")
	require.NoError(t, mock.ExpectationsWereMet())
}
