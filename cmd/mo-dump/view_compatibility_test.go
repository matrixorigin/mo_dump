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
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/matrixorigin/matrixone/pkg/sql/parsers/dialect"
	"github.com/matrixorigin/matrixone/pkg/sql/parsers/dialect/mysql"
	"github.com/matrixorigin/matrixone/pkg/sql/parsers/tree"
	"github.com/matrixorigin/mo_dump/internal/catalog"
	"github.com/stretchr/testify/require"
)

const nestedLocalCTEQuery = "WITH p AS (SELECT 42 AS id) SELECT * FROM (WITH x AS (WITH p AS (SELECT * FROM q), z AS (SELECT * FROM p) SELECT * FROM z), p AS (SELECT 7 AS id) SELECT * FROM x) t"

func TestCollectViewRelations(t *testing.T) {
	for _, tc := range []struct {
		name, query string
		lower       int64
		want        []string
	}{
		{"escaped", "SELECT * FROM `d.b`.`z````父`", 0, []string{"d.b.z``父"}},
		{"quoted and unquoted", "SELECT * FROM parent p JOIN `parent` q ON p.id=q.id", 0, []string{"parent"}},
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
		{"local binding before ancestor mask", nestedLocalCTEQuery, 1, []string{"q"}},
		{"forward masks outer", "WITH p AS (SELECT 42 AS id) SELECT * FROM (WITH x AS (SELECT * FROM p), p AS (SELECT 7 AS id) SELECT * FROM x) q", 0, []string{"p"}},
		{"no forward mask", "WITH p AS (SELECT * FROM parent) SELECT * FROM (WITH x AS (SELECT * FROM p) SELECT * FROM x) q", 0, []string{"parent"}},
		{"earlier local shadows outer", "WITH p AS (SELECT * FROM parent) SELECT * FROM (WITH p AS (SELECT * FROM local_parent), x AS (SELECT * FROM p) SELECT * FROM x) q", 0, []string{"local_parent", "?parent"}},
		{"earlier captured binding bypasses mask", "WITH p AS (SELECT * FROM parent), q AS (SELECT * FROM p) SELECT * FROM (WITH x AS (SELECT * FROM q), p AS (SELECT 7 AS id) SELECT * FROM x) r", 0, []string{"parent"}},
		{"forward mask advisory", "WITH p AS (SELECT 42 AS id) SELECT * FROM (WITH x AS (SELECT * FROM p), p AS (SELECT 7 AS id) SELECT 1) q", 0, []string{"?p"}},
		{"recursive forward masks outer", "WITH p AS (SELECT 42 AS id) SELECT * FROM (WITH RECURSIVE x AS (SELECT * FROM p), p AS (SELECT 7 AS id) SELECT * FROM x) q", 0, []string{"p"}},
		{"forward mask nested scope", "WITH p AS (SELECT 42 AS id) SELECT * FROM (WITH x AS (SELECT * FROM (SELECT * FROM p) r), p AS (SELECT 7 AS id) SELECT * FROM x) q", 0, []string{"p"}},
		{"inherited mask precedes inner binding", "WITH p AS (SELECT 42 AS id) SELECT * FROM (WITH x AS (WITH p AS (SELECT * FROM parent) SELECT * FROM p), p AS (SELECT 7 AS id) SELECT * FROM x) q", 0, []string{"p", "?parent"}},
		{"forward mask folded name", "WITH P AS (SELECT 42 AS id) SELECT * FROM (WITH x AS (SELECT * FROM P), P AS (SELECT 7 AS id) SELECT * FROM x) q", 1, []string{"p"}},
		{"qualified bypasses forward mask", "WITH p AS (SELECT 42 AS id) SELECT * FROM (WITH x AS (SELECT * FROM d.p), p AS (SELECT 7 AS id) SELECT * FROM x) q", 0, []string{"d.p"}},
		{"table function", "SELECT * FROM generate_series((SELECT max(id) FROM p), 10) q", 0, []string{"p"}},
		{"dual", "SELECT 1 FROM dual", 0, nil},
		{"values constant", "VALUES ROW(1)", 0, nil},
		{"values subquery", "VALUES ROW((SELECT max(id) FROM q))", 0, []string{"q"}},
		{"values multiple rows", "VALUES ROW((SELECT id FROM p)), ROW((SELECT id FROM q))", 0, []string{"p", "q"}},
		{"values ordering", "VALUES ROW(2), ROW(1) ORDER BY column_0 LIMIT 1", 0, nil},
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

func TestCollectValuesViewHeaders(t *testing.T) {
	for _, ddl := range []string{
		"CREATE VIEW `AS` (`id`) AS VALUES ROW((SELECT id FROM q));",
		"CREATE /* AS VALUES */ OR REPLACE VIEW v AS /* AS */ VALUES ROW((SELECT id FROM q))",
		"CREATE VIEW `a``AS` AS VALUES ROW((SELECT id FROM q)), ROW(2) ORDER BY column_0 LIMIT 1",
	} {
		t.Run(ddl, func(t *testing.T) {
			var got []string
			require.NoError(t, collectViewRefs(context.Background(), ddl, 1, func(ref *tree.TableName, required bool) {
				require.True(t, required)
				got = append(got, string(ref.Name()))
			}))
			require.Equal(t, []string{"q"}, got)
		})
	}
}

func TestScanViewSQL(t *testing.T) {
	for _, tc := range []struct{ ddl, want string }{
		{"CREATE VIEW `AS``名` /*! AS */ VALUES ROW(' AS ) (')", "CREATE VIEW `AS``名` AS VALUES ROW ( ' AS ) (' ) "},
		{"CREATE VIEW v /*!50000 AS */ VALUES ROW('/*! AS */')", "CREATE VIEW v AS VALUES ROW ( '/*! AS */' ) "},
		{"CREATE VIEW v AS SELECT q.id>=1 AS ok FROM `q` -- AS\n", "CREATE VIEW v AS SELECT q . id >= 1 AS ok FROM `q` "},
	} {
		t.Run(tc.ddl, func(t *testing.T) {
			analysis, err := analyzeViewDependencies("d", tc.ddl, viewSQLMode{})
			require.NoError(t, err)
			require.Equal(t, tc.ddl, analysis.create, "the complete parser needs no private token rewrite")
		})
	}
}

func TestScanViewTokenEquivalence(t *testing.T) {
	type token struct {
		id    int
		value string
	}
	read := func(sql string) []token {
		scanner := mysql.NewScanner(dialect.MYSQL, sql)
		scanner.CommentFlag = false
		defer func() {
			scanner.CommentFlag = false
			mysql.PutScanner(scanner)
		}()
		var tokens []token
		for {
			id, value := scanner.Scan()
			if id == 0 {
				return tokens
			}
			tokens = append(tokens, token{id, value})
			if id == mysql.LEX_ERROR {
				return tokens
			}
		}
	}
	for _, body := range []string{
		"/*! SELECT 0*34 AS val */",
		"/*! SELECT 2*id FROM base */",
		"/*! SELECT 2 * 34 AS val */",
		"SELECT 2*34 AS val",
		"/*! SELECT 7%34, 2+34, 9-34, 8/34 */",
		"/*! SELECT a<=3, a!=4, a<=>5 FROM q */",
		"/*! SELECT 1 AS val",
		"/*! VALUES ROW(1)",
		"/*!50000 SELECT 1 AS val",
		"/*! SELECT 'word', '-word', 1 AS val */",
		"SELECT/* AS */1 AS `x``名`",
		"SELECT begin/* x */work FROM q",
		"SELECT begin/* x */transaction FROM q",
		"SELECT begin work FROM q",
		"VALUES ROW(' AS ) ('), ROW('/*! AS */')",
		"SELECT 'a\\\\b', 'a\\'b', 1e-2, 0x12 AS v",
	} {
		t.Run(body, func(t *testing.T) {
			ddl := "CREATE VIEW v AS " + body
			want := read(ddl)
			analysis, err := analyzeViewDependencies("d", ddl, viewSQLMode{})
			if strings.Contains(body, "begin/*") {
				// The current service rejects these two old lexer-only inputs.
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Equal(t, ddl, analysis.create)
			}
			require.Equal(t, want, read(ddl), "parser ownership must preserve tokens across success/error calls")
		})
	}
}

func TestCollectViewRelationsPersistedAnsi(t *testing.T) {
	for _, tc := range []struct{ name, query, want string }{
		{"double quoted relation", `SELECT * FROM "parent"`, "parent"},
		{"doubled quote in name", `SELECT * FROM "par""ent"`, "par\"ent"},
		{"qualified relation", `SELECT * FROM d."parent"`, "d.parent"},
		{"missing stays harmless", `SELECT * FROM "missing"`, "missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			err := collectViewRefsMode(context.Background(), "CREATE VIEW child AS "+tc.query, viewSQLMode{sqlMode: "ANSI_QUOTES"}, func(ref *tree.TableName, required bool) {
				name := string(ref.Name())
				if ref.ExplicitSchema {
					name = string(ref.Schema()) + "." + name
				}
				got = append(got, name)
			})
			require.NoError(t, err)
			require.Equal(t, []string{tc.want}, got)
		})
	}

	// Default-mode strings remain literals. The stored creation context is
	// authoritative; no guessed ANSI_QUOTES retry is used.
	var got []string
	err := collectViewRefs(context.Background(), `CREATE VIEW v AS SELECT 1 AS value, "parent" AS label`, 0,
		func(ref *tree.TableName, _ bool) { got = append(got, string(ref.Name())) })
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestViewSourceScannerReuse(t *testing.T) {
	for _, tc := range []struct {
		name, trigger string
		wantErr       bool
	}{
		{"executable AS", "CREATE VIEW a_trigger /*! AS */ VALUES ROW(1)", false},
		{"executable source", "CREATE VIEW a_trigger /*! AS VALUES ROW(1) */", false},
		{"versioned source", "CREATE VIEW a_trigger /*!50000 AS VALUES ROW(1) */", false},
		{"ordinary comment", "CREATE VIEW a_trigger AS /* AS */ VALUES ROW(1)", false},
		{"server accepted executable EOF", "CREATE VIEW a_trigger /*! AS VALUES ROW(1)", false},
		{"server accepted SELECT EOF", "CREATE VIEW a_trigger AS /*! SELECT 1 AS val", false},
		{"adjacent multiply", "CREATE VIEW a_trigger AS /*! SELECT 0*34 AS val */", false},
		{"invalid expression", "CREATE VIEW a_trigger /*! AS VALUES ROW(+) */", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			collect := func(ddl string) error {
				return collectViewRefs(context.Background(), ddl, 1, func(*tree.TableName, bool) {
					t.Fatal("constant view must not emit a relation")
				})
			}
			const victim = "CREATE VIEW b_victim AS VALUES ROW(' AS ) (')"
			require.NoError(t, collect(victim))
			if tc.wantErr {
				require.Error(t, collect(tc.trigger))
			} else {
				require.NoError(t, collect(tc.trigger))
			}
			require.NoError(t, collect(victim), "the next VALUES parse must be independent")
			require.NoError(t, collect("CREATE VIEW c_victim AS SELECT '/*! AS ) ( */' AS v"), "the next SELECT parse must be independent")
		})
	}
}

func TestViewOrderScannerReuse(t *testing.T) {
	ddl := []string{"CREATE VIEW a_trigger /*! AS */ VALUES ROW(1)", "CREATE VIEW b_victim AS VALUES ROW(' AS ) (')"}
	tables := Tables{{Name: "a_trigger", Kind: catalog.SystemViewRel}, {Name: "b_victim", Kind: catalog.SystemViewRel}}
	before := append([]string(nil), ddl...)
	require.NoError(t, adjustCompatibilityViewOrder(context.Background(), "d", 1, ddl, tables))
	require.Equal(t, before, ddl, "DDL including executable comments must be emitted unchanged")
	require.Equal(t, "a_trigger", tables[0].Name)
	require.Equal(t, "b_victim", tables[1].Name)
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
		{"ansi quoted relation", "d", "parent", "SELECT * FROM \"parent\"", 0, true},
		{"case zero", "D", "Parent", "SELECT * FROM d.parent", 0, false},
		{"case one", "d", "parent", "SELECT * FROM D.PARENT", 1, true},
		{"case two", "D", "Parent", "SELECT * FROM d.parent", 2, false},
		{"system database", "mysql", "parent", "SELECT * FROM mysql.parent", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ddl := []string{"CREATE TABLE base(id INT)", "CREATE VIEW child AS " + tc.query, "CREATE VIEW " + quoteIdentifier(tc.parent) + " AS SELECT 1"}
			tables := Tables{{Name: "base", Kind: catalog.SystemOrdinaryRel}, {Name: "child", Kind: catalog.SystemViewRel}, {Name: tc.parent, Kind: catalog.SystemViewRel}}
			originalDDL := append([]string(nil), ddl...)
			originalTables := append(Tables(nil), tables...)
			require.NoError(t, adjustCompatibilityViewOrder(context.Background(), tc.db, tc.lower, ddl, tables, "ANSI_QUOTES"))
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
		{"self", []string{"CREATE VIEW a AS SELECT * FROM a"}, []string{"a"}, 0, "cyclic view dependencies; blocked views: d.a"},
		{"cycle and descendant", []string{"CREATE VIEW a AS SELECT * FROM b", "CREATE VIEW b AS SELECT * FROM a", "CREATE VIEW c AS SELECT * FROM a", "CREATE VIEW independent AS SELECT 1"}, []string{"a", "b", "c", "independent"}, 0, "blocked views: d.a, d.b, d.c"},
		{"used CTE self", []string{"CREATE VIEW a AS WITH x AS (SELECT * FROM a) SELECT * FROM x"}, []string{"a"}, 0, "blocked views: d.a"},
		{"required and advisory cycles", []string{"CREATE VIEW a AS SELECT * FROM b", "CREATE VIEW b AS SELECT * FROM a", "CREATE VIEW c AS WITH unused AS (SELECT * FROM c) SELECT 1"}, []string{"a", "b", "c"}, 0, "blocked views: d.a, d.b"},
		{"invalid sql", []string{"CREATE VIEW a AS SELECT * FROM"}, []string{"a"}, 0, "analyze view d.a:"},
		{"not a view", []string{"CREATE TABLE a(id INT)"}, []string{"a"}, 0, "expected CREATE VIEW"},
		{"standalone values", []string{"VALUES ROW(1)"}, []string{"a"}, 0, "expected CREATE VIEW"},
		{"invalid values", []string{"CREATE VIEW a AS VALUES ROW("}, []string{"a"}, 0, "analyze view d.a:"},
		{"invalid values header", []string{"CREATE VIEW () AS VALUES ROW(1)"}, []string{"a"}, 0, "analyze view d.a:"},
		{"non view values header", []string{"CREATE TABLE a AS VALUES ROW(1)"}, []string{"a"}, 0, "analyze view d.a:"},
		{"invalid source type", []string{"CREATE VIEW a AS SHOW DATABASES"}, []string{"a"}, 0, "analyze view d.a:"},
		{"multiple statements", []string{"CREATE VIEW a AS VALUES ROW(1); DROP TABLE t"}, []string{"a"}, 0, "analyze view d.a:"},
		{"missing as", []string{"CREATE VIEW a VALUES ROW(1)"}, []string{"a"}, 0, "analyze view d.a:"},
		{"lexer error", []string{"CREATE VIEW `unterminated AS VALUES ROW(1)"}, []string{"a"}, 0, "analyze view d.a:"},
		{"invalid mode", []string{"CREATE VIEW a AS SELECT 1"}, []string{"a"}, 3, "invalid lower_case_table_names"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tables := make(Tables, len(tc.names))
			for i, name := range tc.names {
				tables[i] = Table{Name: name, Kind: catalog.SystemViewRel}
			}
			original := append([]string(nil), tc.ddl...)
			catalogBefore := append(Tables(nil), tables...)
			err := adjustCompatibilityViewOrder(context.Background(), "d", tc.lower, tc.ddl, tables)
			require.ErrorContains(t, err, tc.want)
			require.Equal(t, original, tc.ddl)
			require.Equal(t, catalogBefore, tables)
		})
	}
	require.NoError(t, adjustCompatibilityViewOrder(context.Background(), "d", 0, nil, nil))
	require.NoError(t, adjustCompatibilityViewOrder(context.Background(), "d", 0, []string{"CREATE TABLE a(id INT)"}, Tables{{Name: "a", Kind: catalog.SystemOrdinaryRel}}))
}

func TestDumpViewDependencies(t *testing.T) {
	for _, mode := range []string{"ordered", "nested forward shadow", "ansi quoted", "mode error", "parse error", "cycle"} {
		t.Run(mode, func(t *testing.T) {
			mock := newIdentifierMock(t)
			parentName := "z`parent"
			child := "CREATE VIEW a_child AS SELECT * FROM `z``parent`"
			if mode == "nested forward shadow" {
				parentName = "p"
				child = "CREATE VIEW a_child AS WITH p AS (SELECT 42 AS id) SELECT * FROM (WITH x AS (SELECT * FROM p), p AS (SELECT 7 AS id) SELECT * FROM x) q"
			}
			if mode == "ansi quoted" {
				child = "create view a_child as select * from \"parent\""
				parentName = "parent"
			}
			opt := Options{dbs: []string{"d"}, tables: Tables{{Name: "a_child"}, {Name: parentName}}, noData: true}
			mock.ExpectQuery("select datname, dat_type from mo_catalog.mo_database where datname = ?" + dbScopePredicate).WithArgs("d").WillReturnRows(sqlmock.NewRows([]string{"datname", "dat_type"}).AddRow("d", ""))
			mock.ExpectQuery("select relname,relkind from mo_catalog.mo_tables where reldatabase = ?"+tablesScopePredicate+hiddenHelpersFor(false)+" and relname in (?,?)").WithArgs("d", "d", "a_child", parentName).WillReturnRows(sqlmock.NewRows([]string{"relname", "relkind"}).AddRow("a_child", catalog.SystemViewRel).AddRow(parentName, catalog.SystemViewRel))
			parent := "CREATE VIEW " + quoteIdentifier(parentName) + " AS SELECT 1"
			if mode == "parse error" {
				child = "CREATE VIEW a_child AS SELECT * FROM"
			}
			if mode == "cycle" {
				parent = "CREATE VIEW `z``parent` AS SELECT * FROM a_child"
			}
			for _, v := range []struct{ name, ddl string }{{"a_child", child}, {parentName, parent}} {
				stored := ""
				if mode == "ansi quoted" {
					stored = "ANSI_QUOTES"
				}
				mock.ExpectQuery("select relname,viewdef from mo_catalog.mo_tables where reldatabase = ? and relkind = 'v' and account_id = current_account_id() and relname in (?)").WithArgs("d", v.name).WillReturnRows(sqlmock.NewRows([]string{"relname", "viewdef"}).AddRow(v.name, viewDefinitionJSON(t, "d", v.ddl, stored)))
			}
			expected := mock.ExpectQuery("select @@lower_case_table_names")
			injected := errors.New("mode unavailable")
			if mode == "mode error" {
				expected.WillReturnError(injected)
			} else {
				expected.WillReturnRows(sqlmock.NewRows([]string{"lower_case_table_names"}).AddRow(1))
			}
			var err error
			output := captureIdentifierOutput(t, func() { err = runDumpDataOverStdout(t, &opt, context.Background()) })
			if mode == "ordered" || mode == "nested forward shadow" || mode == "ansi quoted" {
				require.NoError(t, err)
				require.Contains(t, output, "DROP VIEW IF EXISTS `d`."+quoteIdentifier(parentName)+";\n"+parent+";")
				require.Contains(t, output, "DROP VIEW IF EXISTS `d`.`a_child`;\n"+child+";")
				require.Less(t, strings.Index(output, parent), strings.Index(output, child))
				require.Equal(t, 2, strings.Count(output, "SET SESSION sql_mode = @modump_saved_sql_mode;"))
				require.True(t, strings.HasSuffix(output, "SET foreign_key_checks = 1;\n"))
			} else {
				require.Error(t, err)
				if mode == "mode error" {
					require.ErrorIs(t, err, injected)
				}
				// The dump preamble (fk preset + sql_mode pin for
				// matrixorigin/matrixone#30587) is emitted before any
				// relation DDL even when the dump later fails.
				require.Equal(t, "SET foreign_key_checks = 0;\n\nSET @MODUMP_SCRIPT_SAVED_SQL_MODE=@@SESSION.sql_mode;\nSET SESSION sql_mode=(SELECT TRIM(BOTH ',' FROM REPLACE(CONCAT(',',@@session.sql_mode,','),',NO_BACKSLASH_ESCAPES,',',')));\n\n\n", output, "failure must not emit relation DDL")
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
		{"values prerequisite", []string{"CREATE VIEW a_child AS VALUES ROW((SELECT id FROM q))", "CREATE VIEW q AS SELECT 2 AS id"}, []string{"a_child", "q"}, []string{"q", "a_child"}},
		{"local binding avoids false cycle", []string{"CREATE VIEW a_child AS " + nestedLocalCTEQuery, "CREATE VIEW q AS SELECT 2 AS id", "CREATE VIEW p AS SELECT * FROM a_child"}, []string{"a_child", "q", "p"}, []string{"q", "a_child", "p"}},
		{"local binding survives advisory fallback", []string{"CREATE VIEW a_child AS " + nestedLocalCTEQuery, "CREATE VIEW unused_cycle AS WITH unused AS (SELECT * FROM unused_cycle) SELECT 8 AS id", "CREATE VIEW q AS SELECT 2 AS id"}, []string{"a_child", "unused_cycle", "q"}, []string{"unused_cycle", "q", "a_child"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tables := make(Tables, len(tc.names))
			original := make(map[string]string, len(tc.names))
			for i, name := range tc.names {
				tables[i] = Table{Name: name, Kind: catalog.SystemViewRel}
				original[name] = tc.ddl[i]
			}
			require.NoError(t, adjustCompatibilityViewOrder(context.Background(), "d", 1, tc.ddl, tables))
			for i, name := range tc.want {
				require.Equal(t, name, tables[i].Name)
				require.Equal(t, original[name], tc.ddl[i])
			}
		})
	}
}
