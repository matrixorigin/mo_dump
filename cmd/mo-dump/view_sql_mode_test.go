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
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestViewNameModes(t *testing.T) {
	for _, mode := range []int64{0, 1, 2} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			for _, tc := range []struct {
				query  string
				exact  []string
				folded []string
			}{
				{"select * from Va", []string{"Va"}, []string{"va"}},
				{"select * from D.Va", []string{"Va"}, []string{"va"}},
				{"select * from d.Va", nil, []string{"va"}},
				{"with Vb as (select 1 as x) select * from vb", nil, nil},
				{"with Vb as (select 1 as x) select * from Vb", []string{"Vb"}, nil},
				{"with Vb as (select * from Vc) select * from vb", []string{"Vc"}, []string{"vc"}},
				{"with Vb as (select * from Vc) select * from Vb", []string{"Vb"}, []string{"vc"}},
				{"with Vb as (select 1 as x) select * from D.Vb", []string{"Vb"}, []string{"vb"}},
			} {
				t.Run(tc.query, func(t *testing.T) {
					refs, err := viewDependencies("D", "create view v as "+tc.query, viewSQLMode{lowerCaseTableNames: mode})
					require.NoError(t, err)
					want := tc.folded
					if mode != 1 {
						want = tc.exact
					}
					require.Equal(t, want, refs)
				})
			}
		})
	}
}

func TestViewLiteralModes(t *testing.T) {
	for _, tc := range []struct {
		name, ddl string
		mode      viewSQLMode
		refs      []string
	}{
		{"ANSI identifier", `create view v as select * from "Vb"`, viewSQLMode{ansiQuotes: true}, []string{"Vb"}},
		{"ANSI punctuation", `create view v as select * from "("`, viewSQLMode{ansiQuotes: true}, []string{"("}},
		{"identifier escapes", "create view v as select * from `v``b`", viewSQLMode{}, []string{"v`b"}},
		{"combined ANSI and no backslash", `create view v as select 'a\' as marker,x from "Vb"`, viewSQLMode{ansiQuotes: true, noBackslashEscapes: true}, []string{"Vb"}},
		{"raw SQL mode forwarded", `create view v as select 'a\' as marker,x from "Vb"`, viewSQLMode{sqlMode: "ANSI_QUOTES,NO_BACKSLASH_ESCAPES"}, []string{"Vb"}},
		{"no backslash escapes", `create view v as select 'a\' as x from Vb`, viewSQLMode{noBackslashEscapes: true}, []string{"Vb"}},
		{"no backslash executable comment", `create view v as select 'a\' as x /*!50000 from Vb */`, viewSQLMode{noBackslashEscapes: true}, []string{"Vb"}},
		{"doubled string quote", `create view v as select 'it''s from Vb'`, viewSQLMode{}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			refs, err := viewDependencies("d", tc.ddl, tc.mode)
			require.NoError(t, err)
			require.Equal(t, tc.refs, refs)
		})
	}
}

func TestViewDependencyMalformedStructure(t *testing.T) {
	for _, ddl := range []string{
		"", "create table v(x int)", "create view v as ()",
		"create view v as select (1", "create view v as select 1)",
		"create view v as select 'open", "create view v as select 1 /* open",
		"create view v as select $$open", "create view v as select * from @",
		"create view v as with", "create view v as with a select 1",
		"create view v as with a as () select * from a", "create view v as with a as (select 1)",
		"create view v as select * from d.", "create view v as select * from (d.)",
		"create view v as select * from (select * from)",
		"create view v as select ((select * from))",
		"create view v as with a as (select * from) select * from a",
		"create view v as select * from f((select * from))",
	} {
		t.Run(ddl, func(t *testing.T) {
			_, err := viewDependencies("d", ddl, viewSQLMode{})
			require.Error(t, err)
		})
	}
}

func TestGetViewSQLMode(t *testing.T) {
	for _, value := range []int64{-1, 0, 1, 2, 3} {
		t.Run(fmt.Sprint(value), func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			old := conn
			conn = db
			t.Cleanup(func() { conn = old })
			mock.ExpectQuery("select @@lower_case_table_names, @@sql_mode").WillReturnRows(sqlmock.NewRows([]string{"lower_case_table_names", "sql_mode"}).AddRow(value, " ANSI_QUOTES,NO_BACKSLASH_ESCAPES "))
			mode, err := getViewSQLMode(context.Background())
			if value < 0 || value > 2 {
				require.ErrorContains(t, err, "unsupported lower_case_table_names")
			} else {
				require.NoError(t, err)
				require.Equal(t, value, mode.lowerCaseTableNames)
				require.True(t, mode.ansiQuotes)
				require.True(t, mode.noBackslashEscapes)
			}
			require.NoError(t, mock.ExpectationsWereMet())
			mock.ExpectClose()
		})
	}
}

func TestViewOrderCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ddls := []string{"create view a as select * from b", "create view b as select 1"}
	tables := Tables{{Name: "a"}, {Name: "b"}}
	err := adjustViewOrder(ctx, "d", ddls, tables, 0, viewSQLMode{})
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, []string{"create view a as select * from b", "create view b as select 1"}, ddls)
	require.Equal(t, Tables{{Name: "a"}, {Name: "b"}}, tables)
}
