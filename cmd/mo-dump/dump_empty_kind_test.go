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
	"os"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDumpDataEmptyRelKind covers the review round-4 regression: the server
// records a CREATE TABLE whose name collides with internal naming (e.g. the
// restored __mo_index_renamed) with an empty relkind. dumpData must export the
// object as an ordinary table instead of aborting or silently skipping it, so
// the dump/restore cycle converges and no data is lost behind MODUMP SUCCESS.
func TestDumpDataEmptyRelKind(t *testing.T) {
	_, mock := exportTestDB(t)
	out := exportTestStdout(t)

	opt := Options{
		dbs:             []string{"d"},
		tables:          Tables{{Name: "__mo_index_renamed", Kind: ""}},
		netBufferLength: 1024 * 1024,
	}

	// getDatabaseType
	mock.ExpectQuery("select datname, dat_type from mo_catalog.mo_database where datname = ?" + dbScopePredicate).WithArgs("d").
		WillReturnRows(sqlmock.NewRows([]string{"datname", "dat_type"}).AddRow("d", ""))
	// getTables
	mock.ExpectQuery("select relname,relkind from mo_catalog.mo_tables where reldatabase = ?"+
		tablesScopePredicate+hiddenHelpersFor(false)+" and relname in (?)").WithArgs("d", "d", "__mo_index_renamed").
		WillReturnRows(sqlmock.NewRows([]string{"relname", "relkind"}).AddRow("__mo_index_renamed", ""))
	// getCreateTable on the empty-kind object
	mock.ExpectQuery("show create table `d`.`__mo_index_renamed`").
		WillReturnRows(sqlmock.NewRows([]string{"Table", "Create Table"}).
			AddRow("__mo_index_renamed", "CREATE TABLE `__mo_index_renamed` (\n  `id` int NOT NULL\n)"))
	// genOutput data read
	mock.ExpectQuery("select * from `d`.`__mo_index_renamed`").
		WillReturnRows(sqlmock.NewRowsWithColumnDefinition(
			sqlmock.NewColumn("id").OfType("INT", int64(0))).AddRow(int64(1)))

	require.NoError(t, opt.dumpData(context.Background()))
	require.NoError(t, mock.ExpectationsWereMet())

	data, err := os.ReadFile(out.Name())
	require.NoError(t, err)
	got := string(data)
	assert.Contains(t, got, "DROP TABLE IF EXISTS `__mo_index_renamed`;")
	assert.Contains(t, got, "CREATE TABLE `__mo_index_renamed`")
	assert.Contains(t, got, "INSERT INTO `__mo_index_renamed` VALUES (1);")
	assert.Contains(t, got, "SET foreign_key_checks = 1;")
}

// TestDumpDataEmptyRelKindNotSilent asserts a genuinely unsupported relkind
// still fails loudly instead of disappearing behind a success marker.
func TestDumpDataEmptyRelKindNotSilent(t *testing.T) {
	_, mock := exportTestDB(t)
	exportTestStdout(t)

	opt := Options{
		dbs:             []string{"d"},
		tables:          Tables{{Name: "whatever", Kind: "somefuturekind"}},
		netBufferLength: 1024 * 1024,
	}

	mock.ExpectQuery("select datname, dat_type from mo_catalog.mo_database where datname = ?" + dbScopePredicate).WithArgs("d").
		WillReturnRows(sqlmock.NewRows([]string{"datname", "dat_type"}).AddRow("d", ""))
	mock.ExpectQuery("select relname,relkind from mo_catalog.mo_tables where reldatabase = ?"+
		tablesScopePredicate+hiddenHelpersFor(false)+" and relname in (?)").WithArgs("d", "d", "whatever").
		WillReturnRows(sqlmock.NewRows([]string{"relname", "relkind"}).AddRow("whatever", "somefuturekind"))

	err := opt.dumpData(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not supported")
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestGetCreateTableRequiresHead covers the review round-5 guard: server
// versions whose SHOW CREATE TABLE output lacks the statement head for
// empty-relkind tables must produce a loud error, not a "successful" dump
// that cannot be restored. The CREATE EXTERNAL TABLE shape stays valid.
func TestGetCreateTableRequiresHead(t *testing.T) {
	cases := []struct {
		name    string
		tbl     string
		sql     string
		output  string
		wantErr string
	}{
		{
			name:   "ordinary table keeps passing",
			tbl:    "t",
			sql:    "show create table `d`.`t`",
			output: "CREATE TABLE `t` (\n  `id` int NOT NULL\n)",
		},
		{
			name:   "external table shape keeps passing",
			tbl:    "e",
			sql:    "show create table `d`.`e`",
			output: "CREATE EXTERNAL TABLE `e` (...)",
		},
		{
			name:    "fragment without head fails loudly (review round 5)",
			tbl:     "__mo_index_renamed",
			sql:     "show create table `d`.`__mo_index_renamed`",
			output:  "(`id` int NOT NULL)",
			wantErr: "missing CREATE statement head",
		},
		{
			name:    "empty output fails loudly",
			tbl:     "x",
			sql:     "show create table `d`.`x`",
			output:  "",
			wantErr: "missing CREATE statement head",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, mock := exportTestDB(t)
			mock.ExpectQuery(tc.sql).WillReturnRows(
				sqlmock.NewRows([]string{"Table", "Create Table"}).AddRow("x", tc.output))
			got, err := getCreateTable("d", tc.tbl)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				assert.Empty(t, got)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tc.output, got)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
