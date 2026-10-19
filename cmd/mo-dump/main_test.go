// Copyright 2023 Matrix Origin
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
	"database/sql"
	"encoding/csv"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func exportTestDB(t *testing.T) (*sql.DB, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	oldConn := conn
	conn = db
	t.Cleanup(func() { conn = oldConn })
	return db, mock
}

func exportTestStdout(t *testing.T) *os.File {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stdout")
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })
	oldStdout := os.Stdout
	os.Stdout = f
	t.Cleanup(func() { os.Stdout = oldStdout })
	return f
}

func exportTestRows(n int, rowErr error, errorRow int) *sqlmock.Rows {
	rows := sqlmock.NewRowsWithColumnDefinition(sqlmock.NewColumn("id").OfType("INT", int64(0)))
	for i := 0; i < n; i++ {
		rows.AddRow(i + 1)
	}
	if rowErr != nil {
		rows.RowError(errorRow, rowErr)
	}
	return rows
}

func TestShowInsertRows(t *testing.T) {
	for _, tc := range []struct {
		name       string
		rows       int
		bufferSize int
		err        error
		errorRow   int
		want       string
	}{
		{name: "empty", bufferSize: 1024, want: "\n\n\n"},
		{name: "single", rows: 1, bufferSize: 1024, want: "INSERT INTO `t` VALUES (1);\n\n\n\n"},
		{name: "one_chunk", rows: 3, bufferSize: 1024, want: "INSERT INTO `t` VALUES (1),(2),(3);\n\n\n\n"},
		{name: "multiple_chunks", rows: 3, bufferSize: 29, want: "INSERT INTO `t` VALUES (1);\nINSERT INTO `t` VALUES (2);\nINSERT INTO `t` VALUES (3);\n\n\n\n"},
		{name: "cancel_before_first_row", rows: 1, bufferSize: 1024, err: context.Canceled},
		{name: "cancel_with_buffered_row", rows: 2, bufferSize: 1024, err: context.Canceled, errorRow: 1},
		{name: "cancel_after_chunk", rows: 3, bufferSize: 29, err: context.Canceled, errorRow: 2, want: "INSERT INTO `t` VALUES (1);\n"},
		{name: "read_error", rows: 2, bufferSize: 1024, err: io.ErrUnexpectedEOF, errorRow: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock := exportTestDB(t)
			mock.ExpectQuery("select id").WillReturnRows(exportTestRows(tc.rows, tc.err, tc.errorRow)).RowsWillBeClosed()
			rows, err := db.Query("select id")
			require.NoError(t, err)
			defer rows.Close()
			out := exportTestStdout(t)
			var buffers []*bytes.Buffer
			pool := &sync.Pool{New: func() any {
				buf := &bytes.Buffer{}
				buffers = append(buffers, buf)
				return buf
			}}
			var id sql.RawBytes
			err = showInsert(rows, []any{&id}, []*Column{{Name: "id", Type: "INT"}}, "t", pool, tc.bufferSize)
			require.ErrorIs(t, err, tc.err)
			for _, buf := range buffers {
				require.Zero(t, buf.Len(), "pooled buffers must be reset on every return path")
			}
			data, err := os.ReadFile(out.Name())
			require.NoError(t, err)
			require.Equal(t, tc.want, string(data))
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestShowInsertScanError(t *testing.T) {
	db, mock := exportTestDB(t)
	mock.ExpectQuery("select id").WillReturnRows(exportTestRows(1, nil, 0)).RowsWillBeClosed()
	rows, err := db.Query("select id")
	require.NoError(t, err)
	defer rows.Close()
	var buffers []*bytes.Buffer
	pool := &sync.Pool{New: func() any {
		buf := &bytes.Buffer{}
		buffers = append(buffers, buf)
		return buf
	}}
	err = showInsert(rows, nil, nil, "t", pool, 1024)
	require.ErrorContains(t, err, "expected 1 destination arguments")
	for _, buf := range buffers {
		require.Zero(t, buf.Len())
	}
	require.NoError(t, rows.Close())
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestToCsvRows(t *testing.T) {
	for _, tc := range []struct {
		name     string
		rows     int
		err      error
		errorRow int
		want     string
	}{
		{name: "empty"},
		{name: "success", rows: 2, want: "1\n2\n"},
		{name: "cancel_before_first_row", rows: 1, err: context.Canceled},
		{name: "cancel_after_row", rows: 2, err: context.Canceled, errorRow: 1, want: "1\n"},
		{name: "read_error", rows: 2, err: io.ErrUnexpectedEOF, errorRow: 1, want: "1\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock := exportTestDB(t)
			mock.ExpectQuery("select id").WillReturnRows(exportTestRows(tc.rows, tc.err, tc.errorRow)).RowsWillBeClosed()
			rows, err := db.Query("select id")
			require.NoError(t, err)
			defer rows.Close()
			var out bytes.Buffer
			var id sql.RawBytes
			err = toCsv(rows, &out, []any{&id}, []*Column{{Name: "id", Type: "INT"}}, &csvConfig{fieldDelimiter: ','})
			require.ErrorIs(t, err, tc.err)
			require.Equal(t, tc.want, out.String())
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestGenOutputClosesRows(t *testing.T) {
	for _, mode := range []string{"sql_cancel", "sql_write_error", "csv_create_error"} {
		t.Run(mode, func(t *testing.T) {
			_, mock := exportTestDB(t)
			out := exportTestStdout(t)
			rows := exportTestRows(3, nil, 0)
			conf := csvConfig{fieldDelimiter: ','}
			database := "db"
			switch mode {
			case "sql_cancel":
				rows.RowError(2, context.Canceled)
			case "sql_write_error":
				require.NoError(t, out.Close())
			case "csv_create_error":
				conf.enable = true
				database = filepath.Join(t.TempDir(), "missing", "db")
			}
			mock.ExpectQuery("select * from `" + database + "`.`t`").WillReturnRows(rows).RowsWillBeClosed()
			pool := &sync.Pool{New: func() any { return &bytes.Buffer{} }}
			err := genOutput(database, "t", pool, 29, true, &conf, "")
			switch mode {
			case "sql_cancel":
				require.ErrorIs(t, err, context.Canceled)
			case "sql_write_error":
				require.ErrorIs(t, err, os.ErrClosed)
			case "csv_create_error":
				require.ErrorIs(t, err, os.ErrNotExist)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestDumpExitStatus(t *testing.T) {
	if mode := os.Getenv("MO_DUMP_TEST_EXIT_STATUS"); mode != "" {
		_, mock := exportTestDB(t)
		mock.ExpectQuery("select datname, dat_type from mo_catalog.mo_database where datname = 'db'").
			WillReturnRows(sqlmock.NewRows([]string{"datname", "dat_type"}).AddRow("db", ""))
		mock.ExpectQuery("select relname,relkind from mo_catalog.mo_tables where reldatabase = 'db' and relname in ('t')").
			WillReturnRows(sqlmock.NewRows([]string{"relname", "relkind"}).AddRow("t", "r"))
		mock.ExpectQuery("show create table `db`.`t`").
			WillReturnRows(sqlmock.NewRows([]string{"Table", "Create Table"}).AddRow("t", "CREATE TABLE t (payload TEXT)"))
		payload := strings.Repeat("x", minNetBufferLength/2)
		rows := sqlmock.NewRowsWithColumnDefinition(sqlmock.NewColumn("payload").OfType("TEXT", "")).
			AddRow(payload).AddRow(payload).AddRow(payload)
		if mode == "cancel" {
			rows.RowError(2, context.Canceled)
		}
		mock.ExpectQuery("select * from `db`.`t`").WillReturnRows(rows).RowsWillBeClosed()
		mock.ExpectClose()
		flag.CommandLine = flag.NewFlagSet("mo-dump", flag.ExitOnError)
		os.Args = []string{"mo-dump", "-db", "db", "-tbl", "t", "-net-buffer-length", "16384"}
		main()
		require.NoError(t, mock.ExpectationsWereMet())
		return
	}

	for _, mode := range []string{"success", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestDumpExitStatus$")
			cmd.Env = append(os.Environ(), "MO_DUMP_TEST_EXIT_STATUS="+mode)
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err := cmd.Run()
			require.NoError(t, ctx.Err(), "child process did not terminate")
			require.Contains(t, stdout.String(), "INSERT INTO `t` VALUES", "must reach streaming output before termination")
			if mode == "cancel" {
				var exitErr *exec.ExitError
				require.ErrorAs(t, err, &exitErr)
				require.Equal(t, 1, exitErr.ExitCode())
				require.Contains(t, stderr.String(), "modump error: context canceled")
				require.NotContains(t, stdout.String(), "MODUMP SUCCESS")
				require.NotContains(t, stdout.String(), "SET foreign_key_checks = 1;")
			} else {
				require.NoError(t, err, stderr.String())
				require.Empty(t, stderr.String())
				require.Contains(t, stdout.String(), "MODUMP SUCCESS")
				require.Contains(t, stdout.String(), "SET foreign_key_checks = 1;")
			}
		})
	}
}

func TestConvertValue(t *testing.T) {
	kase := []struct {
		val string
		typ string
	}{
		{"1", "int"},
		{"1", "tinyint"},
		{"1", "smallint"},
		{"1", "bigint"},
		{"1", "unsigned bigint"},
		{"1", "unsigned int"},
		{"1", "unsigned tinyint"},
		{"1", "unsigned smallint"},
		{"1.1", "float"},
		{"1.1", "double"},
		{"1.1", "decimal"},
		{"asa", "varchar"},
		{"asa", "char"},
		{"asa", "text"},
		{"asa", "blob"},
		{"asa", "uuid"},
		{"asa", "json"},
		{"2021-01-01", "date"},
		{"2021-01-01 00:00:00", "datetime"},
		{"2021-01-01 00:00:00", "timestamp"},
		{"[1,2,3]", "vecf32"},
		{"[4,5,6]", "vecf64"},
	}
	for _, v := range kase {
		s := convertValue(makeValue(v.val), v.typ)
		switch v.typ {
		case "int", "tinyint", "smallint", "bigint", "unsigned bigint", "unsigned int", "unsigned tinyint", "unsigned smallint", "float", "double", "vecf32", "vecf64":
			require.Equal(t, v.val, s)
		default:

			require.Equal(t, fmt.Sprintf("'%v'", v.val), s)
		}
	}
}

func makeValue(val string) interface{} {
	tmp := sql.RawBytes(val)
	return &tmp
}

func TestConvertValue2(t *testing.T) {
	v := makeValue("\\10\\11\\12")
	v2, f := convertValue2(v, "string")
	assert.Equal(t, *(v.(*sql.RawBytes)), v2)
	assert.Equal(t, f, defaultFmt)
}

func TestShowCreateTable(t *testing.T) {
	kases := []struct {
		sql          string
		withNextLine bool
		res          string
	}{
		{
			sql:          "create table t1 (a int, b int)",
			withNextLine: false,
			res:          "create table t1 (a int, b int);\n",
		},
		{
			sql:          "create table t1 (a int, b int)",
			withNextLine: true,
			res:          "create table t1 (a int, b int);\n\n\n",
		},
		{
			sql:          "create table t1 (a int, b int);",
			withNextLine: false,
			res:          "create table t1 (a int, b int);\n",
		},
		{
			sql:          "create table t1 (a int, b int);",
			withNextLine: true,
			res:          "create table t1 (a int, b int);\n\n\n",
		},
	}
	old := os.Stdout
	for _, v := range kases {
		r, w, _ := os.Pipe()
		os.Stdout = w
		showCreateTable(v.sql, v.withNextLine)

		e := w.Close()
		require.Nil(t, e)
		var buf bytes.Buffer
		_, e = buf.ReadFrom(r)
		require.Nil(t, e)
		require.Equal(t, v.res, buf.String())
	}
	os.Stdout = old
}

func TestViewOrder(t *testing.T) {
	start := 1
	createTable := []string{
		"create table t1(a int);",
		"create view t4 as select * from t2, t3;",
		"create view t2 as select * from t1;",
		"create view t3 as select * from t2;",
		"create view t6 as select * from t4, t5;",
		"create view t5 as select * from t4;",
	}
	tables := []Table{
		{Name: "t1"},
		{Name: "t4"},
		{Name: "t2"},
		{Name: "t3"},
		{Name: "t6"},
		{Name: "t5"},
	}
	createTarget := []string{
		"create table t1(a int);",
		"create view t2 as select * from t1;",
		"create view t3 as select * from t2;",
		"create view t4 as select * from t2, t3;",
		"create view t5 as select * from t4;",
		"create view t6 as select * from t4, t5;",
	}
	tableTarget := []Table{
		{Name: "t1"},
		{Name: "t2"},
		{Name: "t3"},
		{Name: "t4"},
		{Name: "t5"},
		{Name: "t6"},
	}
	adjustViewOrder(createTable, tables, start)
	for i := 0; i < len(createTable); i++ {
		require.Equal(t, createTable[i], createTarget[i])
		require.Equal(t, tables[i], tableTarget[i])
	}
}

func Test_toCsvFields(t *testing.T) {
	bys1 := []byte{0x5C, 0x31, 0x30, 0x5C, 0x33, 0x36, 0x5C, 0x38, 0x36, 0x5c}
	args1 := []any{makeValue(string(bys1))}
	cols1 := []*Column{
		{
			Name: "col1",
			Type: "varchar",
		},
	}
	line := make([]string, 1)
	toCsvFields(args1, cols1, line, false)
	want := "\\10\\36\\86\\"
	assert.Equal(t, want, line[0])
}

func Test_toCsvLine(t *testing.T) {
	bys1 := []byte{0x5C, 0x31, 0x30, 0x5C, 0x33, 0x36, 0x5C, 0x38, 0x36, 0x5c}
	args1 := []any{makeValue(string(bys1))}
	cols1 := []*Column{
		{
			Name: "col1",
			Type: "varchar",
		},
	}
	line := make([]string, 1)
	bb := bytes.Buffer{}
	cw1 := csv.NewWriter(&bb)
	cw1.Comma = '\t'
	err := toCsvLine(cw1, args1, cols1, line, false)
	assert.NoError(t, err)
	want := "\\10\\36\\86\\"
	assert.Equal(t, want, line[0])
	assert.Equal(t, bb.Bytes()[:len(bys1)], bys1)
}

func Test_checkFieldDelimiter(t *testing.T) {
	type args struct {
		ctx context.Context
		s   string
	}
	tests := []struct {
		name    string
		args    args
		want    rune
		wantErr assert.ErrorAssertionFunc
	}{
		{
			name: "t1",
			args: args{
				ctx: nil,
				s:   "",
			},
			want: defaultFieldDelimiter,
			wantErr: func(t assert.TestingT, err error, i ...interface{}) bool {
				assert.Error(t, err)
				assert.True(t, strings.Contains(err.Error(), "csv field delimiter is invalid utf8 character"))
				return false
			},
		},
		{
			name: "t2",
			args: args{
				ctx: nil,
				s:   "fdaf",
			},
			want: defaultFieldDelimiter,
			wantErr: func(t assert.TestingT, err error, i ...interface{}) bool {
				assert.Error(t, err)
				assert.True(t, strings.Contains(err.Error(), "there are multiple utf8 characters for csv field delimiter."))
				return false
			},
		},
		{
			name: "t3",
			args: args{
				ctx: nil,
				s:   string([]rune{utf8.RuneError}),
			},
			want: defaultFieldDelimiter,
			wantErr: func(t assert.TestingT, err error, i ...interface{}) bool {
				assert.Error(t, err)
				assert.True(t, strings.Contains(err.Error(), "csv field delimiter is invalid utf8 character"))
				return false
			},
		},
		{
			name: "t4",
			args: args{
				ctx: nil,
				s:   " ",
			},
			want: defaultFieldDelimiter,
			wantErr: func(t assert.TestingT, err error, i ...interface{}) bool {
				assert.NoError(t, err)
				return false
			},
		},
		{
			name: "t5",
			args: args{
				ctx: nil,
				s:   "中文",
			},
			want: defaultFieldDelimiter,
			wantErr: func(t assert.TestingT, err error, i ...interface{}) bool {
				assert.Error(t, err)
				assert.True(t, strings.Contains(err.Error(), "there are multiple utf8 characters for csv field delimiter."))
				return false
			},
		},
		{
			name: "t6",
			args: args{
				ctx: nil,
				s:   "中",
			},
			want: defaultFieldDelimiter,
			wantErr: func(t assert.TestingT, err error, i ...interface{}) bool {
				assert.NoError(t, err)
				return false
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := checkFieldDelimiter(tt.args.ctx, tt.args.s)
			if !tt.wantErr(t, err, fmt.Sprintf("checkFieldDelimiter(%v, %v)", tt.args.ctx, tt.args.s)) {
				return
			}
			assert.Equalf(t, tt.want, got, "checkFieldDelimiter(%v, %v)", tt.args.ctx, tt.args.s)
		})
	}
}

func TestGetDatabases(t *testing.T) {
	// create mock database
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("Failed to create mock database: %v", err)
	}
	defer db.Close()

	ctx := context.Background()

	rows := sqlmock.NewRows([]string{"Database"}).
		AddRow("db1").
		AddRow("db2").
		AddRow("db3")

	mock.ExpectQuery("show databases").WillReturnRows(rows)

	conn = db
	databases, err := getDatabases(ctx)

	// check the results
	assert.NoError(t, err)
	expected := []string{"db1", "db2", "db3"}
	if len(databases) != len(expected) {
		t.Errorf("Unexpected number of databases. Expected: %d, Got: %d", len(expected), len(databases))
	}

	for i, db := range databases {
		if db != expected[i] {
			t.Errorf("Unexpected database name at index %d. Expected: %s, Got: %s", i, expected[i], db)
		}
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("Unfulfilled expectations: %s", err)
	}
}

func TestGetCreateDB(t *testing.T) {
	// create mock database
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("Failed to create mock database: %v", err)
	}
	defer db.Close()

	rows := sqlmock.NewRows([]string{"Database", "Create"}).
		AddRow("db1", "CREATE DATABASE db1").
		AddRow("db2", "CREATE DATABASE db2").
		AddRow("db3", "CREATE DATABASE db3")

	mock.ExpectQuery("show create database").WillReturnRows(rows)
	conn = db

	// check the results
	createDB, err := getCreateDB("db1")
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	expectedCreateDB := "CREATE DATABASE db1"
	if createDB != expectedCreateDB {
		t.Errorf("Unexpected create database statement. Expected: %s, Got: %s", expectedCreateDB, createDB)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("Unfulfilled expectations: %s", err)
	}
}

func TestGetCreateTable(t *testing.T) {
	// create mock database
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("Failed to create mock database: %v", err)
	}
	defer db.Close()

	rows := sqlmock.NewRows([]string{"Table", "Create"}).
		AddRow("table1", "CREATE TABLE table1 (id INT, name VARCHAR(255))").
		AddRow("table2", "CREATE TABLE table2 (id INT, age INT)")

	mock.ExpectQuery("show create table").WillReturnRows(rows)
	conn = db

	// check the results
	createTable, err := getCreateTable("db1", "table1")
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	expectedCreateTable := "CREATE TABLE table1 (id INT, name VARCHAR(255))"
	if createTable != expectedCreateTable {
		t.Errorf("Unexpected create table statement. Expected: %s, Got: %s", expectedCreateTable, createTable)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("Unfulfilled expectations: %s", err)
	}
}

func TestGetCreateView(t *testing.T) {
	// create mock database
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("Failed to create mock database: %v", err)
	}
	defer db.Close()

	rows := sqlmock.NewRows([]string{"Table", "Create"}).
		AddRow("table1_view", "CREATE VIEW table1_view as SELECT * FROM table1")

	mock.ExpectQuery("show create table").WillReturnRows(rows)
	conn = db

	// check the results
	createTable, err := getCreateTable("db1", "table1_view")
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	expectedCreateTable := "CREATE VIEW table1_view as SELECT * FROM table1"
	if createTable != expectedCreateTable {
		t.Errorf("Unexpected create table statement. Expected: %s, Got: %s", expectedCreateTable, createTable)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("Unfulfilled expectations: %s", err)
	}
}
