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
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestViewLexicalScenarios(t *testing.T) {
	for _, tt := range []struct {
		name, query string
		refs        []viewName
	}{
		{"extract", "SELECT extract(year FROM ts) FROM t", []viewName{{"db", "t"}}},
		{"trim_subquery", "SELECT trim(both 'x' FROM (SELECT x FROM t))", []viewName{{"db", "t"}}},
		{"joins", "SELECT * FROM ((t a)) STRAIGHT_JOIN other.t b ON true", []viewName{{"db", "t"}, {"other", "t"}}},
		{"parenthesized_join", "SELECT * FROM (t JOIN u USING (x))", []viewName{{"db", "t"}, {"db", "u"}}},
		{"correlated_apply", "SELECT * FROM t CROSS APPLY (SELECT x FROM other.u) s", []viewName{{"db", "t"}, {"other", "u"}}},
		{"table_function", "SELECT * FROM f('FROM fake', (SELECT x FROM real_table))", []viewName{{"db", "real_table"}}},
		{"table_shorthand", "TABLE t UNION ALL TABLE other.u", []viewName{{"db", "t"}, {"other", "u"}}},
		{"cte_columns", "WITH c(x) AS (SELECT x FROM t) SELECT * FROM c", []viewName{{"db", "t"}}},
		{"cte_recursive_external", "WITH RECURSIVE c AS (SELECT x FROM t UNION ALL SELECT x+1 FROM c WHERE x<2) SELECT * FROM c", []viewName{{"db", "t"}}},
		{"future_unused", "WITH x AS (SELECT 99) SELECT * FROM (WITH y AS (SELECT * FROM x), x AS (SELECT 7) SELECT 1) s", nil},
		{"quoted_keywords", "SELECT `from` AS `join` FROM `select`", []viewName{{"db", "select"}}},
		{"quoted_punctuation", "SELECT `(` AS `,` FROM `(` AS `,`, `123` AS `where`", []viewName{{"db", "("}, {"db", "123"}}},
		{"group_boundary", "SELECT x,y FROM t GROUP BY x,y WITH ROLLUP", []viewName{{"db", "t"}}},
		{"named_windows", "SELECT sum(x) OVER w FROM t WINDOW w AS (ORDER BY x), z AS ()", []viewName{{"db", "t"}}},
		{"variables", "SELECT @from, @@join", nil},
		{"double_quotes_literal", "SELECT \"FROM fake\" FROM t", []viewName{{"db", "t"}}},
		{"escaped_string", "SELECT 'it\\'s FROM fake' FROM t", []viewName{{"db", "t"}}},
		{"doubled_quote", "SELECT 'it''s FROM fake' FROM t", []viewName{{"db", "t"}}},
		{"dollar_string", "SELECT $$'FROM fake$$ FROM t", []viewName{{"db", "t"}}},
		{"dollar_tag", "SELECT $tag$'FROM fake$tag$ FROM t", []viewName{{"db", "t"}}},
		{"dollar_identifier", "SELECT dollar$column FROM t", []viewName{{"db", "t"}}},
		{"dollar_identifier_ending", "SELECT 1 AS dollar$column$ FROM t", []viewName{{"db", "t"}}},
		{"unicode_dollar_tag", "SELECT $标记$'FROM fake$标记$ FROM t", []viewName{{"db", "t"}}},
		{"bare_offset_alias", "SELECT * FROM t offset, other.u", []viewName{{"db", "t"}, {"other", "u"}}},
		{"block_comment", "SELECT 1 /* ' FROM fake */ FROM t", []viewName{{"db", "t"}}},
		{"line_comments", "SELECT 1 -- ' FROM fake\n FROM t # ' FROM fake", []viewName{{"db", "t"}}},
		{"slash_comment", "SELECT 1 // ' FROM fake\n FROM t", []viewName{{"db", "t"}}},
		{"executable_comment", "SELECT * /*!50000 FROM t */", []viewName{{"db", "t"}}},
		{"minus_arithmetic", "SELECT 1--1 FROM t", []viewName{{"db", "t"}}},
		{"parenthesized_query", "((SELECT * FROM t))", []viewName{{"db", "t"}}},
		{"terminated", "SELECT * FROM t; /* tail */", []viewName{{"db", "t"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			refs, err := viewReferences(context.Background(), dumpView{name: viewName{"db", "v"}, create: "CREATE VIEW v AS " + tt.query}, viewSQLMode{lowerCaseTableNames: 1})
			require.NoError(t, err)
			require.Len(t, refs, len(tt.refs))
			for _, ref := range tt.refs {
				require.Contains(t, refs, ref)
			}
		})
	}
}

func TestViewLexicalSQLModes(t *testing.T) {
	for _, tt := range []struct {
		name, ddl string
		mode      viewSQLMode
		ref       viewName
	}{
		{"ansi", `CREATE VIEW "v" AS SELECT * FROM "other"."t"`, viewSQLMode{lowerCaseTableNames: 1, ansiQuotes: true}, viewName{"other", "t"}},
		{"ansi_escaped_identifier", `CREATE VIEW "v" AS SELECT * FROM "other"."t""x"`, viewSQLMode{lowerCaseTableNames: 1, ansiQuotes: true}, viewName{"other", "t\"x"}},
		{"no_backslash", `CREATE VIEW v AS SELECT '\' FROM t`, viewSQLMode{lowerCaseTableNames: 1, noBackslashEscapes: true}, viewName{"db", "t"}},
		{"case_zero", `CREATE VIEW v AS SELECT * FROM Other.T`, viewSQLMode{}, viewName{"Other", "T"}},
		{"case_two", `CREATE VIEW v AS SELECT * FROM Other.T`, viewSQLMode{lowerCaseTableNames: 2}, viewName{"Other", "T"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			refs, err := viewReferences(context.Background(), dumpView{name: viewName{"db", "v"}, create: tt.ddl}, tt.mode)
			require.NoError(t, err)
			require.Equal(t, map[viewName]struct{}{tt.ref: {}}, refs)
		})
	}
}

func TestViewLexicalErrors(t *testing.T) {
	for _, ddl := range []string{
		// Independently rejected by the real service; retain as negative oracles.
		"CREATE VIEW v AS SELECT * FROM ((t)) a STRAIGHT_JOIN other.t b ON true",
		"CREATE VIEW v AS SELECT * FROM t JOIN LATERAL (SELECT x FROM other.u) s ON true",
		"CREATE VIEW v AS WITH v AS (SELECT * FROM v) SELECT * FROM v",
		"", "SELECT 1", "CREATE VIEW v SELECT 1", "CREATE VIEW v AS", "CREATE VIEW v AS SELECT * FROM", "CREATE VIEW v AS SELECT * FROM 42", "CREATE VIEW v AS SELECT * FROM db.",
		"CREATE VIEW v AS WITH", "CREATE VIEW v AS WITH c(x) SELECT 1", "CREATE VIEW v AS WITH c AS SELECT 1", "CREATE VIEW v AS SELECT (1", "CREATE VIEW v AS SELECT 1)",
		"CREATE VIEW v AS SELECT 'unterminated", "CREATE VIEW v AS SELECT 1 /* unterminated", "CREATE VIEW v AS SELECT $$unterminated", "CREATE VIEW v AS SELECT 1; SELECT 2",
		"CREATE VIEW v AS SELECT * FROM (t JOIN)", "CREATE VIEW v AS WITH c AS (SELECT * FROM 42) SELECT * FROM c",
		"CREATE VIEW v AS SELECT * FROM t AS", "CREATE VIEW v AS SELECT * FROM t AS 42", "CREATE VIEW v AS SELECT * FROM t USE nope",
		"CREATE VIEW v AS SELECT * FROM t USE INDEX FOR nope (idx)", "CREATE VIEW v AS SELECT * FROM t USE INDEX FOR ORDER (idx)",
		"CREATE VIEW v AS SELECT * FROM t USE INDEX", "CREATE VIEW v AS SELECT * FROM t {unknown='s'}", "CREATE VIEW v AS SELECT * FROM t {}",
		"CREATE VIEW v AS SELECT * FROM t {snapshot='s')", "CREATE VIEW v AS SELECT * FROM t }", "CREATE VIEW v AS SELECT * FROM t {snapshot='s'",
	} {
		t.Run(ddl, func(t *testing.T) {
			_, err := viewReferences(context.Background(), dumpView{name: viewName{"db", "v"}, create: ddl}, viewSQLMode{lowerCaseTableNames: 1})
			require.Error(t, err)
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := viewReferences(ctx, dumpView{name: viewName{"db", "v"}, create: "CREATE VIEW v AS SELECT 1"}, viewSQLMode{})
	require.ErrorIs(t, err, context.Canceled)
}

func TestGetViewCatalogModeMerged(t *testing.T) {
	for _, lower := range []int64{0, 1, 2, -1, 3} {
		t.Run(string(rune('0'+lower)), func(t *testing.T) {
			mock, _ := mockDumpOutput(t)
			mock.ExpectQuery("select @@lower_case_table_names").WillReturnRows(sqlmock.NewRows([]string{"lower_case_table_names"}).AddRow(lower))
			mode, err := getViewCatalogMode(context.Background())
			if lower < 0 || lower > 2 {
				require.ErrorContains(t, err, "unsupported lower_case_table_names")
			} else {
				require.NoError(t, err)
				require.Equal(t, lower, mode.lowerCaseTableNames)
				require.False(t, mode.ansiQuotes)
				require.False(t, mode.noBackslashEscapes)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
