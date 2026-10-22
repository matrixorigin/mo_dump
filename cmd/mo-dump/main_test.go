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
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

func TestConvertValueBit(t *testing.T) {
	for _, tc := range []struct {
		name string
		val  sql.RawBytes
		want string
	}{
		{"null", nil, "NULL"},
		{"empty", sql.RawBytes{}, "''"},
		{"zero", sql.RawBytes{0}, "0x00"},
		{"one", sql.RawBytes{1}, "0x01"},
		{"quote", sql.RawBytes{0x27}, "0x27"},
		{"backslash", sql.RawBytes{0x5c}, "0x5C"},
		{"ordinary", sql.RawBytes{0x41}, "0x41"},
		{"high_byte", sql.RawBytes{0xff}, "0xFF"},
		{"leading_zero", sql.RawBytes{0, 1}, "0x0001"},
		{"special_bytes", sql.RawBytes{0x27, 0x5c}, "0x275C"},
		{"max_width", sql.RawBytes{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, "0xFFFFFFFFFFFFFFFF"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, typ := range []string{"bit", "BIT"} {
				require.Equal(t, tc.want, convertValue(&tc.val, typ))
			}
		})
	}

	t.Run("all_bytes", func(t *testing.T) {
		for i := 0; i < 256; i++ {
			raw := sql.RawBytes{byte(i)}
			literal := convertValue(&raw, "BIT")
			require.True(t, strings.HasPrefix(literal, "0x"), "byte %d: %q", i, literal)
			decoded, err := hex.DecodeString(literal[2:])
			require.NoError(t, err, "byte %d", i)
			require.Equal(t, []byte(raw), decoded, "byte %d", i)
		}
	})
}

func TestShowInsertBit(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, db.Close())
		require.NoError(t, mock.ExpectationsWereMet())
	})
	mock.ExpectQuery("SELECT").WillReturnRows(sqlmock.NewRows([]string{"id", "b"}).
		AddRow(1, []byte{0x27}).
		AddRow(2, []byte{0x5c}).
		AddRow(3, []byte{0}).
		AddRow(4, nil).
		AddRow(5, []byte{0, 1}).
		AddRow(6, []byte{0x41}))
	mock.ExpectClose()
	rows, err := db.Query("SELECT id, b FROM t")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rows.Close()) })

	output, err := os.CreateTemp(t.TempDir(), "insert-*.sql")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, output.Close()) })
	oldStdout := os.Stdout
	os.Stdout = output
	t.Cleanup(func() { os.Stdout = oldStdout })

	var id, bit sql.RawBytes
	pool := &sync.Pool{New: func() any { return new(bytes.Buffer) }}
	err = showInsert(rows, []any{&id, &bit}, []*Column{{Name: "id", Type: "INT"}, {Name: "b", Type: "BIT"}}, "t", pool, defaultNetBufferLength)
	require.NoError(t, err)
	require.NoError(t, rows.Err())
	got, err := os.ReadFile(output.Name())
	require.NoError(t, err)
	require.Equal(t, "INSERT INTO `t` VALUES (1,0x27),(2,0x5C),(3,0x00),(4,NULL),(5,0x0001),(6,0x41);\n\n\n\n", string(got))
}

func makeValue(val string) interface{} {
	tmp := sql.RawBytes(val)
	return &tmp
}

func makeNullValue() interface{} {
	var tmp sql.RawBytes
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
	text := func(s string) []any { return []any{makeValue(s)} }
	textNull := func() []any { return []any{makeNullValue()} }
	varcharCol := []*Column{{Name: "col1", Type: "varchar"}}

	kases := []struct {
		name         string
		args         []any
		cols         []*Column
		enableEscape bool
		wantLine     string
		wantNull     bool
	}{
		// legacy: every backslash is escaped exactly once
		{"backslashes", text("\\10\\36\\86\\"), varcharCol, false, "\\\\10\\\\36\\\\86\\\\", false},
		// SQL NULL must not collide with the NULL marker \N
		{"sql null", textNull(), varcharCol, false, "", true},
		// a literal text \N is escaped so LOAD DATA reads it back as text
		{"text marker N", text("\\N"), varcharCol, false, "\\\\N", false},
		// a text \N must survive -enable-escape as well
		{"text marker N escape", text("\\N"), varcharCol, true, "\\\\N", false},
		{"text backslashes N", text("\\\\N"), varcharCol, false, "\\\\\\\\N", false},
		{"text null word", text("NULL"), varcharCol, false, "NULL", false},
		// -enable-escape escapes ',' for string columns exactly once
		{"comma escape", text("a,b"), varcharCol, true, "a\\,b", false},
		{"comma no escape", text("a,b"), varcharCol, false, "a,b", false},
	}
	for _, kase := range kases {
		t.Run(kase.name, func(t *testing.T) {
			line := make([]string, len(kase.args))
			nulls := make([]bool, len(kase.args))
			toCsvFields(kase.args, kase.cols, line, nulls, kase.enableEscape)
			assert.Equal(t, kase.wantLine, line[0])
			assert.Equal(t, kase.wantNull, nulls[0])
		})
	}
}

func Test_toCsvLine(t *testing.T) {
	text := func(s string) []any { return []any{makeValue(s)} }
	varcharCol := []*Column{{Name: "col1", Type: "varchar"}}

	kases := []struct {
		name         string
		args         []any
		cols         []*Column
		enableEscape bool
		delim        rune
		want         string
	}{
		// legacy content, no quoting needed
		{"backslashes", text("\\10\\36\\86\\"), varcharCol, false, '\t', "\\\\10\\\\36\\\\86\\\\\n"},
		// SQL NULL is written as the unquoted \N marker
		{"sql null", []any{makeNullValue()}, varcharCol, false, ',', "\\N\n"},
		// text \N
		{"text marker N", text("\\N"), varcharCol, false, ',', "\\\\N\n"},
		// a literal text NULL must be quoted, or LOAD DATA reads it as SQL NULL
		{"text null word", text("NULL"), varcharCol, false, ',', "\"NULL\"\n"},
		{"text null word lower", text("null"), varcharCol, false, ',', "\"null\"\n"},
		// framing quotes
		{"comma quoted", text("a,b"), varcharCol, false, ',', "\"a,b\"\n"},
		{"double quote", text("a\"b"), varcharCol, false, ',', "\"a\"\"b\"\n"},
		{"line feed", text("a\nb"), varcharCol, false, ',', "\"a\nb\"\n"},
		{"empty string", text(""), varcharCol, false, ',', "\n"},
		// -enable-escape: ',' escaped once, tab delimiter, no quotes
		{"comma escaped no quote", text("a,b"), varcharCol, true, '\t', "a\\,b\n"},
		// alternate field delimiter from -csv-field-delimiter
		{"pipe delimiter", text("a|b"), varcharCol, false, '|', "\"a|b\"\n"},
		{"text null with pipe delimiter", text("NULL"), varcharCol, false, '|', "\"NULL\"\n"},
		// binary (non-UTF-8) values must survive byte-identical
		{"binary", []any{makeValue("\xff\xfe")}, []*Column{{Name: "col1", Type: "blob"}}, false, ',', "\xff\xfe\n"},
		{"binary with quote", []any{makeValue("\xff\"\xfe")}, []*Column{{Name: "col1", Type: "blob"}}, false, ',', "\"\xff\"\"\xfe\"\n"},
		// whitespace-leading values must be quoted: unquoted whitespace-only
		// records are dropped by older loaders
		{"leading space", text(" a"), varcharCol, false, ',', "\" a\"\n"},
		{"space only", text(" "), varcharCol, false, ',', "\" \"\n"},
		{"tabs only", text("\t\t"), varcharCol, false, ',', "\"\t\t\"\n"},
		{"nbsp only", text("  "), varcharCol, false, ',', "\"  \"\n"},
	}
	for _, kase := range kases {
		t.Run(kase.name, func(t *testing.T) {
			line := make([]string, len(kase.args))
			nulls := make([]bool, len(kase.args))
			bb := bytes.Buffer{}
			err := toCsvLine(&bb, kase.delim, kase.args, kase.cols, line, nulls, kase.enableEscape)
			assert.NoError(t, err)
			assert.Equal(t, kase.want, bb.String())
		})
	}
}

// Test_toCsvLine_whitespaceOnlyRows: every whitespace-only record must be
// written quoted, so an older loader does not skip it as an empty line.
func Test_toCsvLine_whitespaceOnlyRows(t *testing.T) {
	cols := []*Column{{Name: "col1", Type: "varchar"}}
	bb := bytes.Buffer{}
	line := make([]string, 1)
	nulls := make([]bool, 1)
	rows := []string{" ", "\t", " ", "  \t "}
	for _, row := range rows {
		err := toCsvLine(&bb, ',', []any{makeValue(row)}, cols, line, nulls, false)
		assert.NoError(t, err)
	}
	got := bb.String()
	assert.Equal(t, len(rows), strings.Count(got, "\n"))
	assert.Equal(t, "\" \"\n\"\t\"\n\" \"\n\"  \t \"\n", got)
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
			name: "double quote",
			args: args{ctx: nil, s: "\""},
			want: defaultFieldDelimiter,
			wantErr: func(t assert.TestingT, err error, i ...interface{}) bool {
				assert.Error(t, err)
				return false
			},
		},
		{
			name: "line feed",
			args: args{ctx: nil, s: "\n"},
			want: defaultFieldDelimiter,
			wantErr: func(t assert.TestingT, err error, i ...interface{}) bool {
				assert.Error(t, err)
				return false
			},
		},
		{
			name: "carriage return",
			args: args{ctx: nil, s: "\r"},
			want: defaultFieldDelimiter,
			wantErr: func(t assert.TestingT, err error, i ...interface{}) bool {
				assert.Error(t, err)
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
