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
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/matrixorigin/mo_dump/internal/catalog"
	"github.com/stretchr/testify/require"
)

func TestViewDependencies(t *testing.T) {
	cases := []struct {
		name, query string
		refs        []string
	}{
		{"literal", "select 'vb' as marker", nil},
		{"comments and aliases", "select 1 as vb /* from vb */ -- join vb\n", nil},
		{"table alias", "select vb.x from base as vb", []string{"base"}},
		{"qualified and quoted", "select * from `D`.`Vb`", []string{"vb"}},
		{"other database", "select * from other.vb", nil},
		{"deduplicate", "select * from vb a join vb b on a.x = b.x", []string{"vb"}},
		{"name prefix", "select * from vbb", []string{"vbb"}},
		{"joins and parentheses", "select * from (vb join vc using (x))", []string{"vb", "vc"}},
		{"derived table", "select * from (select * from vb) as vc", []string{"vb"}},
		{"scalar subquery", "select (select x from vb) as vc", []string{"vb"}},
		{"where subquery", "select 1 where exists (select 1 from vb)", []string{"vb"}},
		{"join condition subquery", "select * from base a join base b on a.x = (select x from vb)", []string{"base", "vb"}},
		{"having subquery", "select count(*) from base having count(*) > (select x from vb)", []string{"base", "vb"}},
		{"order subquery", "select 1 order by (select x from vb)", []string{"vb"}},
		{"function subquery", "select coalesce((select x from vb), 0)", []string{"vb"}},
		{"window subquery", "select sum(x) over (partition by (select x from vb)) from base", []string{"vb", "base"}},
		{"union", "select * from vb union all select * from vc", []string{"vb", "vc"}},
		{"CTE shadows view", "with vb as (select 1 as x) select * from vb", nil},
		{"CTE body dependency", "with vb as (select * from vc) select * from vb", []string{"vc"}},
		{"earlier CTE", "with vb as (select 1 as x), vc as (select * from vb) select * from vc", nil},
		{"recursive CTE", "with recursive vb as (select 1 as x union all select x+1 from vb where x<2) select * from vb", nil},
		{"qualified bypasses CTE", "with vb as (select 1 as x) select * from d.vb", []string{"vb"}},
		{"nested scope restored", "select * from (with vb as (select 1 as x) select * from vb) s join vb using (x)", []string{"vb"}},
		{"outer CTE visible", "with vb as (select 1 as x) select * from (select * from vb) s", nil},
		{"nested CTE restores outer", "with vb as (select 1 as x) select * from (with vb as (select * from vc) select * from vb) s join vb using (x)", []string{"vc"}},
		{"named window", "select sum(x) over w from vb window w as (order by x)", []string{"vb"}},
		{"VALUES ROW", "values row(1)", nil},
		{"VALUES subquery", "values row((select x from vb))", []string{"vb"}},
		{"ROLLUP", "select x, count(*) from vb group by x with rollup", []string{"vb"}},
		{"unused self CTE", "with unused as (select * from va) select 1 as x", nil},
		{"unused external CTE", "with unused as (select * from vb) select * from vc", []string{"vc"}},
		{"only reachable CTE", "with unused as (select * from va), used as (select * from vb) select * from used", []string{"vb"}},
		{"reachable CTE chain", "with a as (select * from vb), b as (select * from a) select * from b join b c using(x)", []string{"vb"}},
		{"unused recursive CTE", "with recursive unused as (select * from va union all select * from unused) select 1", nil},
		{"reachable recursive CTE", "with recursive used as (select * from vb union all select * from used) select * from used", []string{"vb"}},
		{"declaration scope not use site", "with a as (select * from vb) select * from (with vb as (select 1 as x) select * from a) s", []string{"vb"}},
		{"CTE columns", "with vb(x) as (select x from vc) select * from vb", []string{"vc"}},
		{"EXTRACT FROM", "select extract(year from ts) from vb", []string{"vb"}},
		{"TRIM FROM subquery", "select trim(both 'x' from (select x from vb))", []string{"vb"}},
		{"table valued function", "select * from vb('vc')", nil},
		{"table function subquery", "select * from f((select x from vb))", []string{"vb"}},
		{"implicit derived alias", "select * from (select * from vb) s", []string{"vb"}},
		{"nested table groups", "select * from ((vb a)) join vc on a.x=vc.y", []string{"vb", "vc"}},
		{"TABLE shorthand", "table vb", []string{"vb"}},
		{"TABLE union", "table vb union all table vc", []string{"vb", "vc"}},
		{"mixed TABLE union", "select * from vb union table vc", []string{"vb", "vc"}},
		{"quoted keywords", "select `from` as `join` from `select`", []string{"select"}},
		{"quoted punctuation", "select `(` as `,` from `(` as `,`", []string{"("}},
		{"quoted numeric name", "select * from `123`", []string{"123"}},
		{"quoted comma alias", "select * from vb as `,`, vc as `where`", []string{"vb", "vc"}},
		{"double quoted literal", "select \"from vb\" as x", nil},
		{"literal escaping", "select 'it\\'s from vb'", nil},
		{"dollar string", "select $$'from vb$$", nil},
		{"tagged dollar string", "select $tag$'from vb$tag$", nil},
		{"executable comment", "select * /*!50000 from vb */", []string{"vb"}},
		{"comment quote", "select 1 /* ' from vb */", nil},
		{"end line comment", "select 1 -- ' from vb", nil},
		{"hash comment", "select 1 # ' from vb", nil},
		{"double minus arithmetic", "select 1--1 from vb", []string{"vb"}},
		{"group boundary", "select x,y from vb group by x,y", []string{"vb"}},
		{"window boundary", "select sum(x) over w from vb window w as (), z as ()", []string{"vb"}},
		{"parenthesized query", "(select * from vb)", []string{"vb"}},
		{"explicit nonreserved alias", "select * from vb as offset, vc", []string{"vb", "vc"}},
		{"variable names", "select @from, @@join, @`(`", nil},
		{"STRAIGHT_JOIN", "select * from vb straight_join vc on vb.x=vc.x", []string{"vb", "vc"}},
		{"APPLY", "select * from vb cross apply (select * from vc) s", []string{"vb", "vc"}},
		{"typed literals", "select X'66726f6d207662', B'01', date '2020-01-01', _binary'from vb'", nil},
		{"snapshot reference", "select * from va{snapshot='historical'}", nil},
		{"snapshot and current references", "select * from va{snapshot='historical'} a join vb using(x)", []string{"vb"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			refs, err := viewDependencies("d", "create view va as "+tc.query, viewSQLMode{lowerCaseTableNames: 1})
			require.NoError(t, err)
			require.Equal(t, tc.refs, refs)
		})
	}
}

func TestViewOrderBoundaries(t *testing.T) {
	cases := []struct {
		name  string
		names []string
		ddls  []string
		start int
		want  []string
		err   string
		mode  *viewSQLMode
	}{
		{name: "empty"},
		{name: "ordinary table only", names: []string{"t"}, ddls: []string{"create table t(x int)"}, start: 1, want: []string{"t"}},
		{name: "single view", names: []string{"va"}, ddls: []string{"create view va as select 1"}, want: []string{"va"}},
		{name: "mutual literals", names: []string{"va", "vb"}, ddls: []string{"create view va as select 'vb' as marker", "create view vb as select 'va' as marker"}, want: []string{"va", "vb"}},
		{name: "mutual comments aliases and prefixes", names: []string{"va", "vb"}, ddls: []string{"create view va as select vb.x as vb from vbb vb /* from vb */", "create view vb as select 1 as va /* from va */"}, want: []string{"va", "vb"}},
		{name: "reverse edge deduplicated", names: []string{"va", "vb"}, ddls: []string{"create view va as select a.x from vb a join vb b using (x)", "create view vb as select 1 as x"}, want: []string{"vb", "va"}},
		{name: "ordinary prefix preserved", names: []string{"t", "va", "vb"}, ddls: []string{"create table t(x int)", "create view va as select * from d.vb", "create view vb as select * from t"}, start: 1, want: []string{"t", "vb", "va"}},
		{name: "external schema not local cycle", names: []string{"va", "vb"}, ddls: []string{"create view va as select * from other.vb", "create view vb as select * from va"}, want: []string{"va", "vb"}},
		{name: "case folded", names: []string{"Va", "Vb"}, ddls: []string{"create view Va as select * from `D`.`VB`", "create view Vb as select 1"}, want: []string{"Vb", "Va"}},
		{name: "case sensitive identity", names: []string{"va", "Va"}, ddls: []string{"create view va as select * from Va", "create view Va as select 1"}, want: []string{"Va", "va"}, mode: &viewSQLMode{}},
		{name: "mode 2 preserves identity", names: []string{"va", "Va"}, ddls: []string{"create view va as select * from Va", "create view Va as select 1"}, want: []string{"Va", "va"}, mode: &viewSQLMode{lowerCaseTableNames: 2}},
		{name: "CTE declaration normalization", names: []string{"v"}, ddls: []string{"create view v as with V as (select 1 as x) select * from v"}, want: []string{"v"}, mode: &viewSQLMode{}},
		{name: "folded duplicate identity", names: []string{"va", "Va"}, ddls: []string{"create view va as select 1", "create view Va as select 1"}, err: "duplicate view identity"},
		{name: "unused CTE no self cycle", names: []string{"v"}, ddls: []string{"create view v as with unused as (select * from v) select 1 as x"}, want: []string{"v"}},
		{name: "cycle", names: []string{"va", "vb"}, ddls: []string{"create view va as select * from vb", "create view vb as select * from va"}, err: "blocked views: va, vb"},
		{name: "self reference", names: []string{"va"}, ddls: []string{"create view va as select * from va"}, err: "blocked views: va"},
		{name: "partial progress", names: []string{"va", "free", "vb", "downstream"}, ddls: []string{"create view va as select * from vb", "create view free as select 1", "create view vb as select * from va", "create view downstream as select * from va"}, err: "blocked views: va, vb, downstream"},
		{name: "invalid DDL", names: []string{"va"}, ddls: []string{"create view va as select from"}, err: "cannot order view d.va"},
		{name: "not a view", names: []string{"va"}, ddls: []string{"select 1"}, err: "expected CREATE VIEW"},
		{name: "multiple statements", names: []string{"va"}, ddls: []string{"create view va as select 1; select 2"}, err: "expected one CREATE VIEW"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tables := make(Tables, len(tc.names))
			byName := make(map[string]string)
			for i, name := range tc.names {
				tables[i] = Table{Name: name, Kind: catalog.SystemViewRel}
				byName[name] = tc.ddls[i]
			}
			originalDDL := append([]string(nil), tc.ddls...)
			originalTables := append(Tables{}, tables...)
			mode := viewSQLMode{lowerCaseTableNames: 1}
			if tc.mode != nil {
				mode = *tc.mode
			}
			err := adjustViewOrder(context.Background(), "d", tc.ddls, tables, tc.start, mode, nil)
			if tc.err != "" {
				require.ErrorContains(t, err, tc.err)
				require.Equal(t, originalDDL, tc.ddls)
				require.Equal(t, originalTables, tables)
				return
			}
			require.NoError(t, err)
			for i, name := range tc.want {
				require.Equal(t, name, tables[i].Name)
				require.Equal(t, byName[name], tc.ddls[i])
			}
		})
	}
}

func TestDumpDataViewOrder(t *testing.T) {
	cases := []struct {
		name, kind, ddl, second string
		err                     string
		modeError               bool
		metadataError           bool
	}{
		{name: "independent views", kind: catalog.SystemViewRel, ddl: "create view va as select 'vb' as marker", second: "create view vb as select 'va' as marker"},
		{name: "cycle error propagated", kind: catalog.SystemViewRel, ddl: "create view va as select * from vb", second: "create view vb as select * from va", err: "cyclic view dependencies"},
		{name: "parse error propagated", kind: catalog.SystemViewRel, ddl: "create view va as select from", err: "cannot order view d.va"},
		{name: "mode query error", kind: catalog.SystemViewRel, ddl: "create view va as select 1", err: "cannot read view catalog mode", modeError: true},
		{name: "metadata query error", kind: catalog.SystemViewRel, ddl: "create view va as select 1", err: "cannot read persisted view definitions", metadataError: true},
		{name: "one ordinary table", kind: catalog.SystemOrdinaryRel, ddl: "create table va (x int)"},
		{name: "empty database"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			oldConn := conn
			conn = db
			t.Cleanup(func() { conn = oldConn })
			file, err := os.CreateTemp(t.TempDir(), "dump-*.sql")
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, file.Close()) })
			oldStdout := os.Stdout
			os.Stdout = file
			t.Cleanup(func() { os.Stdout = oldStdout })
			mock.ExpectQuery("select datname, dat_type").WillReturnRows(sqlmock.NewRows([]string{"datname", "dat_type"}).AddRow("d", ""))
			mock.ExpectQuery("show create database").WillReturnRows(sqlmock.NewRows([]string{"Database", "Create Database"}).AddRow("d", "create database d"))
			rows := sqlmock.NewRows([]string{"relname", "relkind"})
			if tc.ddl != "" {
				rows.AddRow("va", tc.kind)
			}
			if tc.second != "" {
				rows.AddRow("vb", catalog.SystemViewRel)
			}
			mock.ExpectQuery("select relname,relkind").WillReturnRows(rows)
			if tc.ddl != "" {
				columns := []string{"View", "Create View", "character_set_client", "collation_connection"}
				definition := sqlmock.NewRows(columns).AddRow("va", tc.ddl, "utf8mb4", "utf8mb4_bin")
				if tc.kind != catalog.SystemViewRel {
					definition = sqlmock.NewRows([]string{"Table", "Create Table"}).AddRow("va", tc.ddl)
				}
				mock.ExpectQuery("show create table `d`.`va`").WillReturnRows(definition)
				if tc.second != "" {
					mock.ExpectQuery("show create table `d`.`vb`").WillReturnRows(sqlmock.NewRows(columns).AddRow("vb", tc.second, "utf8mb4", "utf8mb4_bin"))
				}
			}
			if tc.kind == catalog.SystemViewRel {
				query := mock.ExpectQuery("select @@lower_case_table_names")
				if tc.modeError {
					query.WillReturnError(errors.New("mode query failed"))
				} else {
					query.WillReturnRows(sqlmock.NewRows([]string{"lower_case_table_names"}).AddRow(1))
					definitions := sqlmock.NewRows([]string{"relname", "viewdef"})
					payload, err := json.Marshal(viewDefinition{Stmt: tc.ddl, DefaultDatabase: "d"})
					require.NoError(t, err)
					definitions.AddRow("va", string(payload))
					if tc.second != "" {
						payload, err = json.Marshal(viewDefinition{Stmt: tc.second, DefaultDatabase: "d"})
						require.NoError(t, err)
						definitions.AddRow("vb", string(payload))
					}
					if tc.metadataError {
						mock.ExpectQuery("select relname,viewdef").WillReturnError(errors.New("view metadata unavailable"))
					} else {
						mock.ExpectQuery("select relname,viewdef").WillReturnRows(definitions).RowsWillBeClosed()
					}
				}
			}
			opt := Options{dbs: []string{"d"}, emptyTables: true, noData: true}
			err = opt.dumpData(context.Background())
			if tc.err != "" {
				require.ErrorContains(t, err, tc.err)
			} else {
				require.NoError(t, err)
			}
			require.NoError(t, mock.ExpectationsWereMet())
			mock.ExpectClose()
			_, err = file.Seek(0, io.SeekStart)
			require.NoError(t, err)
			output, err := io.ReadAll(file)
			require.NoError(t, err)
			if tc.err != "" {
				require.NotContains(t, string(output), "DROP VIEW")
			} else {
				for _, ddl := range []string{tc.ddl, tc.second} {
					if ddl != "" {
						require.Contains(t, string(output), ddl)
					}
				}
				require.True(t, strings.HasSuffix(string(output), "SET foreign_key_checks = 1;\n"))
			}
		})
	}
}
