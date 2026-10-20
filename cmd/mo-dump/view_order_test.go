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
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/matrixorigin/matrixone/pkg/catalog"
	"github.com/matrixorigin/matrixone/pkg/sql/parsers/tree"
	"github.com/stretchr/testify/require"
)

func TestCollectViewRelations(t *testing.T) {
	for _, tc := range []struct {
		name, query string
		lower       int64
		want        []string
	}{
		{"escaped", "SELECT * FROM `d.b`.`z````父`", 0, []string{"d.b.z``父"}},
		{"quoted and unquoted", "SELECT * FROM parent p JOIN `parent` q ON p.id=q.id", 0, []string{"parent", "parent"}},
		{"non relations", "SELECT 'beta', \"alpha\", t.alpha AS beta FROM base t /* beta */ WHERE t.beta = 'alpha'", 0, []string{"base"}},
		{"derived union", "SELECT * FROM (SELECT * FROM parent UNION ALL SELECT * FROM other) q", 0, []string{"parent", "other"}},
		{"expression subqueries", "SELECT CASE WHEN EXISTS(SELECT * FROM p) THEN (SELECT max(id) FROM q) ELSE 0 END FROM r WHERE id IN (SELECT id FROM s)", 0, []string{"p", "q", "r", "s"}},
		{"function subquery", "SELECT coalesce((SELECT max(id) FROM p),0) FROM q", 0, []string{"p", "q"}},
		{"window", "SELECT sum((SELECT max(id) FROM p)) OVER (PARTITION BY (SELECT max(id) FROM q)) FROM r", 0, []string{"p", "q", "r"}},
		{"sample expression", "SELECT sample((SELECT max(id) FROM p), 1 ROWS) FROM q", 0, []string{"p", "q"}},
		{"order limit", "SELECT id FROM p ORDER BY (SELECT max(id) FROM q) LIMIT 1", 0, []string{"p", "q"}},
		{"cte shadow", "WITH p AS (SELECT * FROM base) SELECT * FROM p", 0, []string{"base"}},
		{"cte order", "WITH p AS (SELECT * FROM q), q AS (SELECT * FROM base) SELECT * FROM p JOIN q", 0, []string{"q", "base"}},
		{"recursive cte", "WITH RECURSIVE p AS (SELECT * FROM base UNION ALL SELECT * FROM p) SELECT * FROM p", 0, []string{"base"}},
		{"qualified bypasses cte", "WITH p AS (SELECT * FROM base) SELECT * FROM d.p", 0, []string{"?base", "d.p"}},
		{"nested scope", "SELECT * FROM (WITH p AS (SELECT * FROM base) SELECT * FROM p) x JOIN p", 0, []string{"base", "p"}},
		{"cte lower zero", "WITH X AS (SELECT * FROM base) SELECT * FROM X JOIN x", 0, []string{"base", "X"}},
		{"cte lower one", "WITH X AS (SELECT * FROM base) SELECT * FROM X JOIN x", 1, []string{"base"}},
		{"cte lower two", "WITH X AS (SELECT * FROM base) SELECT * FROM X JOIN x", 2, []string{"base", "X"}},
		{"unused self", "WITH unused AS (SELECT * FROM child) SELECT 1", 0, []string{"?child"}},
		{"unused missing", "WITH unused AS (SELECT * FROM missing) SELECT 1", 0, []string{"?missing"}},
		{"used chain", "WITH x AS (SELECT * FROM parent), y AS (SELECT * FROM x) SELECT * FROM y JOIN y again", 0, []string{"parent"}},
		{"late required", "WITH p AS (SELECT * FROM parent) SELECT (WITH unused AS (SELECT * FROM p) SELECT 1), (SELECT count(*) FROM p)", 0, []string{"parent"}},
		{"definition scope", "WITH p AS (SELECT * FROM parent), q AS (SELECT * FROM p) SELECT * FROM (WITH p AS (SELECT * FROM other) SELECT * FROM q) x", 0, []string{"parent", "?other"}},
		{"forward masks outer", "WITH p AS (SELECT 42 AS id) SELECT * FROM (WITH x AS (SELECT * FROM p), p AS (SELECT 7 AS id) SELECT * FROM x) q", 0, []string{"p"}},
		{"no forward mask", "WITH p AS (SELECT * FROM parent) SELECT * FROM (WITH x AS (SELECT * FROM p) SELECT * FROM x) q", 0, []string{"parent"}},
		{"earlier local shadows outer", "WITH p AS (SELECT * FROM parent) SELECT * FROM (WITH p AS (SELECT * FROM local_parent), x AS (SELECT * FROM p) SELECT * FROM x) q", 0, []string{"local_parent", "?parent"}},
		{"earlier captured binding bypasses mask", "WITH p AS (SELECT * FROM parent), q AS (SELECT * FROM p) SELECT * FROM (WITH x AS (SELECT * FROM q), p AS (SELECT 7 AS id) SELECT * FROM x) r", 0, []string{"parent"}},
		{"forward mask advisory", "WITH p AS (SELECT 42 AS id) SELECT * FROM (WITH x AS (SELECT * FROM p), p AS (SELECT 7 AS id) SELECT 1) q", 0, []string{"?p"}},
		{"recursive forward masks outer", "WITH p AS (SELECT 42 AS id) SELECT * FROM (WITH RECURSIVE x AS (SELECT * FROM p), p AS (SELECT 7 AS id) SELECT * FROM x) q", 0, []string{"p"}},
		{"forward mask nested scope", "WITH p AS (SELECT 42 AS id) SELECT * FROM (WITH x AS (SELECT * FROM (SELECT * FROM p) r), p AS (SELECT 7 AS id) SELECT * FROM x) q", 0, []string{"p"}},
		{"inherited mask precedes inner binding", "WITH p AS (SELECT 42 AS id) SELECT * FROM (WITH x AS (WITH p AS (SELECT * FROM parent) SELECT * FROM p), p AS (SELECT 7 AS id) SELECT * FROM x) q", 0, []string{"p", "?parent"}},
		{"forward mask folded name", "WITH P AS (SELECT 42 AS id) SELECT * FROM (WITH x AS (SELECT * FROM P), P AS (SELECT 7 AS id) SELECT * FROM x) q", 1, []string{"P"}},
		{"qualified bypasses forward mask", "WITH p AS (SELECT 42 AS id) SELECT * FROM (WITH x AS (SELECT * FROM d.p), p AS (SELECT 7 AS id) SELECT * FROM x) q", 0, []string{"d.p"}},
		{"table function", "SELECT * FROM generate_series((SELECT max(id) FROM p), 10) q", 0, []string{"p"}},
		{"dual", "SELECT 1 FROM dual", 0, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			err := collectViewRefs(context.Background(), "CREATE VIEW child AS "+tc.query, tc.lower, func(ref *tree.TableName, required bool) {
				name := string(ref.Name())
				if ref.ExplicitSchema {
					name = string(ref.Schema()) + "." + name
				}
				if !required {
					name = "?" + name
				}
				got = append(got, name)
			})
			require.NoError(t, err)
			require.ElementsMatch(t, tc.want, got)
		})
	}
}

func TestViewOrderDependencies(t *testing.T) {
	for _, tc := range []struct {
		name, db, parent, query string
		lower                   int64
		dependent               bool
	}{
		{"escaped", "d", "z`parent", "SELECT * FROM `z``parent`", 0, true},
		{"dotted unicode", "d.b", "z.父", "SELECT * FROM `d.b`.`z.父`", 0, true},
		{"repeated reference", "d", "parent", "SELECT * FROM parent p JOIN parent q ON p.id=q.id", 0, true},
		{"qualified", "d", "parent", "SELECT * FROM d.parent", 0, true},
		{"other database", "d", "parent", "SELECT * FROM other.parent", 0, false},
		{"prefix", "d", "parent", "SELECT * FROM parent_suffix", 0, false},
		{"literal", "d", "parent", "SELECT 'parent' AS parent", 0, false},
		{"cte", "d", "parent", "WITH parent AS (SELECT 1) SELECT * FROM parent", 0, false},
		{"nested forward shadow", "d", "p", "WITH p AS (SELECT 42 AS id) SELECT * FROM (WITH x AS (SELECT * FROM p), p AS (SELECT 7 AS id) SELECT * FROM x) q", 0, true},
		{"case zero", "D", "Parent", "SELECT * FROM d.parent", 0, false},
		{"case one", "D", "Parent", "SELECT * FROM d.parent", 1, true},
		{"case two", "D", "Parent", "SELECT * FROM d.parent", 2, false},
		{"system database", "MYSQL", "Parent", "SELECT * FROM mysql.parent", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ddl := []string{"CREATE TABLE base(id INT)", "CREATE VIEW child AS " + tc.query, "CREATE VIEW " + quoteIdentifier(tc.parent) + " AS SELECT 1"}
			tables := Tables{{Name: "base", Kind: catalog.SystemOrdinaryRel}, {Name: "child", Kind: catalog.SystemViewRel}, {Name: tc.parent, Kind: catalog.SystemViewRel}}
			originalDDL := append([]string(nil), ddl...)
			originalTables := append(Tables(nil), tables...)
			require.NoError(t, adjustViewOrder(context.Background(), tc.db, tc.lower, ddl, tables))
			order := []int{0, 1, 2}
			if tc.dependent {
				order = []int{0, 2, 1}
			}
			for i, j := range order {
				require.Equal(t, originalDDL[j], ddl[i], "DDL must be unchanged")
				require.Equal(t, originalTables[j], tables[i], "DDL and catalog entry must stay paired")
			}
		})
	}
}

func TestViewOrderFailureIsAtomic(t *testing.T) {
	for _, tc := range []struct {
		name  string
		ddl   []string
		names []string
		lower int64
		want  string
	}{
		{"self", []string{"CREATE VIEW a AS SELECT * FROM a"}, []string{"a"}, 0, "cyclic view dependencies block `a`"},
		{"cycle and descendant", []string{"CREATE VIEW a AS SELECT * FROM b", "CREATE VIEW b AS SELECT * FROM a", "CREATE VIEW c AS SELECT * FROM a", "CREATE VIEW independent AS SELECT 1"}, []string{"a", "b", "c", "independent"}, 0, "block `a`, `b`, `c`"},
		{"used CTE self", []string{"CREATE VIEW a AS WITH x AS (SELECT * FROM a) SELECT * FROM x"}, []string{"a"}, 0, "block `a`"},
		{"required and advisory cycles", []string{"CREATE VIEW a AS SELECT * FROM b", "CREATE VIEW b AS SELECT * FROM a", "CREATE VIEW c AS WITH unused AS (SELECT * FROM c) SELECT 1"}, []string{"a", "b", "c"}, 0, "block `a`, `b`"},
		{"invalid sql", []string{"CREATE VIEW a AS SELECT * FROM"}, []string{"a"}, 0, "database d, view a:"},
		{"not a view", []string{"CREATE TABLE a(id INT)"}, []string{"a"}, 0, "expected CREATE VIEW"},
		{"invalid mode", []string{"CREATE VIEW a AS SELECT 1"}, []string{"a"}, 3, "invalid lower_case_table_names"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tables := make(Tables, len(tc.names))
			for i, name := range tc.names {
				tables[i] = Table{Name: name, Kind: catalog.SystemViewRel}
			}
			original := append([]string(nil), tc.ddl...)
			catalogBefore := append(Tables(nil), tables...)
			err := adjustViewOrder(context.Background(), "d", tc.lower, tc.ddl, tables)
			require.ErrorContains(t, err, tc.want)
			require.Equal(t, original, tc.ddl)
			require.Equal(t, catalogBefore, tables)
		})
	}
	require.NoError(t, adjustViewOrder(context.Background(), "d", 0, nil, nil))
	require.NoError(t, adjustViewOrder(context.Background(), "d", 0, []string{"CREATE TABLE a(id INT)"}, Tables{{Name: "a", Kind: catalog.SystemOrdinaryRel}}))
}

func TestDumpViewDependencies(t *testing.T) {
	for _, mode := range []string{"ordered", "nested forward shadow", "mode error", "parse error", "cycle"} {
		t.Run(mode, func(t *testing.T) {
			mock := newIdentifierMock(t)
			parentName := "z`parent"
			child := "CREATE VIEW a_child AS SELECT * FROM `z``parent`"
			if mode == "nested forward shadow" {
				parentName = "p"
				child = "CREATE VIEW a_child AS WITH p AS (SELECT 42 AS id) SELECT * FROM (WITH x AS (SELECT * FROM p), p AS (SELECT 7 AS id) SELECT * FROM x) q"
			}
			opt := Options{dbs: []string{"d"}, tables: Tables{{Name: "a_child"}, {Name: parentName}}, noData: true}
			mock.ExpectQuery("select datname, dat_type from mo_catalog.mo_database where datname = ?").WithArgs("d").WillReturnRows(sqlmock.NewRows([]string{"datname", "dat_type"}).AddRow("d", ""))
			mock.ExpectQuery("select relname,relkind from mo_catalog.mo_tables where reldatabase = ? and relname in (?,?)").WithArgs("d", "a_child", parentName).WillReturnRows(sqlmock.NewRows([]string{"relname", "relkind"}).AddRow("a_child", catalog.SystemViewRel).AddRow(parentName, catalog.SystemViewRel))
			parent := "CREATE VIEW " + quoteIdentifier(parentName) + " AS SELECT 1"
			if mode == "parse error" {
				child = "CREATE VIEW a_child AS SELECT * FROM"
			}
			if mode == "cycle" {
				parent = "CREATE VIEW `z``parent` AS SELECT * FROM a_child"
			}
			for _, v := range []struct{ name, ddl string }{{"a_child", child}, {parentName, parent}} {
				mock.ExpectQuery("show create table `d`." + quoteIdentifier(v.name)).WillReturnRows(sqlmock.NewRows([]string{"View", "Create", "charset", "collation"}).AddRow(v.name, v.ddl, "utf8mb4", "utf8mb4_bin"))
			}
			expected := mock.ExpectQuery("select @@lower_case_table_names")
			injected := errors.New("mode unavailable")
			if mode == "mode error" {
				expected.WillReturnError(injected)
			} else {
				expected.WillReturnRows(sqlmock.NewRows([]string{"lower_case_table_names"}).AddRow(1))
			}
			var err error
			output := captureIdentifierOutput(t, func() { err = opt.dumpData(context.Background()) })
			if mode == "ordered" || mode == "nested forward shadow" {
				require.NoError(t, err)
				require.Equal(t, "SET foreign_key_checks = 0;\n\nDROP VIEW IF EXISTS "+quoteIdentifier(parentName)+";\n"+parent+";\n\n\nDROP VIEW IF EXISTS `a_child`;\n"+child+";\n\n\nSET foreign_key_checks = 1;\n", output)
			} else {
				require.Error(t, err)
				if mode == "mode error" {
					require.ErrorIs(t, err, injected)
				}
				require.Equal(t, "SET foreign_key_checks = 0;\n\n", output, "failure must not emit relation DDL")
			}
		})
	}
}

func TestViewOrderCTECompatibility(t *testing.T) {
	for _, tc := range []struct {
		name        string
		ddl         []string
		names, want []string
	}{
		{"unused self", []string{"CREATE VIEW a AS WITH unused AS (SELECT * FROM a) SELECT 1"}, []string{"a"}, []string{"a"}},
		{"unused parent", []string{"CREATE VIEW a AS WITH unused AS (SELECT * FROM b) SELECT 1", "CREATE VIEW b AS SELECT 1"}, []string{"a", "b"}, []string{"b", "a"}},
		{"used chain", []string{"CREATE VIEW a AS WITH x AS (SELECT * FROM b), y AS (SELECT * FROM x) SELECT * FROM y", "CREATE VIEW b AS SELECT 1"}, []string{"a", "b"}, []string{"b", "a"}},
		{"mixed cycle", []string{"CREATE VIEW a AS SELECT * FROM b", "CREATE VIEW b AS WITH unused AS (SELECT * FROM a) SELECT 1"}, []string{"a", "b"}, []string{"b", "a"}},
		{"independent literals", []string{"CREATE VIEW alpha AS SELECT 'beta' AS v", "CREATE VIEW beta AS SELECT 'alpha' AS v"}, []string{"alpha", "beta"}, []string{"alpha", "beta"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tables := make(Tables, len(tc.names))
			original := make(map[string]string, len(tc.names))
			for i, name := range tc.names {
				tables[i] = Table{Name: name, Kind: catalog.SystemViewRel}
				original[name] = tc.ddl[i]
			}
			require.NoError(t, adjustViewOrder(context.Background(), "d", 1, tc.ddl, tables))
			for i, name := range tc.want {
				require.Equal(t, name, tables[i].Name)
				require.Equal(t, original[name], tc.ddl[i])
			}
		})
	}
}
