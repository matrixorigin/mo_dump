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

	"github.com/stretchr/testify/require"
)

func TestViewReferences(t *testing.T) {
	tests := []struct {
		name, query string
		refs        []viewName
	}{
		{"constant", "SELECT 'other.v FROM fake' AS v /* FROM another.v */", nil},
		{"unqualified", "SELECT * FROM v", []viewName{{"db", "v"}}},
		{"qualified_same_name", "SELECT * FROM other.v JOIN v AS other ON true", []viewName{{"other", "v"}, {"db", "v"}}},
		{"quoted", "SELECT * FROM `other.db`.`v``x`", []viewName{{"other.db", "v`x"}}},
		{"duplicate", "SELECT a.x FROM other.v a JOIN other.v b ON a.x=b.x", []viewName{{"other", "v"}}},
		{"union", "SELECT * FROM a UNION ALL SELECT * FROM other.b", []viewName{{"db", "a"}, {"other", "b"}}},
		{"derived", "SELECT * FROM (SELECT * FROM other.v) AS x", []viewName{{"other", "v"}}},
		{"scalar", "SELECT (SELECT x FROM other.v) AS x", []viewName{{"other", "v"}}},
		{"predicate", "SELECT * FROM a WHERE EXISTS (SELECT 1 FROM other.v)", []viewName{{"db", "a"}, {"other", "v"}}},
		{"join_condition", "SELECT * FROM a JOIN b ON a.x=(SELECT x FROM other.v)", []viewName{{"db", "a"}, {"db", "b"}, {"other", "v"}}},
		{"having_order", "SELECT count(*) FROM a HAVING count(*) > (SELECT x FROM b) ORDER BY (SELECT x FROM c)", []viewName{{"db", "a"}, {"db", "b"}, {"db", "c"}}},
		{"cte", "WITH v AS (SELECT * FROM other.v) SELECT * FROM v", []viewName{{"other", "v"}}},
		{"cte_definition_sees_table", "WITH v AS (SELECT * FROM v) SELECT * FROM v", []viewName{{"db", "v"}}},
		{"cte_sequential", "WITH a AS (SELECT * FROM t), b AS (SELECT * FROM a) SELECT * FROM b", []viewName{{"db", "t"}}},
		{"cte_recursive", "WITH RECURSIVE v AS (SELECT 1 AS x UNION ALL SELECT x+1 FROM v WHERE x<2) SELECT * FROM v", nil},
		{"cte_qualified", "WITH v AS (SELECT 1) SELECT * FROM db.v", []viewName{{"db", "v"}}},
		{"cte_scope_does_not_escape", "SELECT * FROM (WITH v AS (SELECT 1) SELECT * FROM v) x JOIN v ON true", []viewName{{"db", "v"}}},
		{"cte_nested_inherits", "WITH v AS (SELECT * FROM t) SELECT * FROM (SELECT * FROM v) x", []viewName{{"db", "t"}}},
		{"case", "SELECT * FROM OTHER.V", []viewName{{"other", "v"}}},
		{"time_travel", "SELECT * FROM a {timestamp = (SELECT x FROM b)}", []viewName{{"db", "b"}}},
		{"window", "SELECT sum(x) OVER (PARTITION BY (SELECT x FROM b) ORDER BY (SELECT x FROM c)) FROM a", []viewName{{"db", "a"}, {"db", "b"}, {"db", "c"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			refs, err := viewReferences(context.Background(), dumpView{name: viewName{"db", "result"}, create: "CREATE VIEW result AS " + tt.query}, viewSQLMode{lowerCaseTableNames: 1})
			require.NoError(t, err)
			require.Len(t, refs, len(tt.refs))
			for _, ref := range tt.refs {
				require.Contains(t, refs, ref)
			}
		})
	}
}

func TestOrderDumpViews(t *testing.T) {
	v := func(db, name, query string) dumpView {
		return dumpView{name: viewName{db, name}, create: "CREATE VIEW `" + name + "` AS " + query}
	}
	tests := []struct {
		name  string
		views []dumpView
		order []viewName
		err   string
	}{
		{name: "empty"},
		{name: "single", views: []dumpView{v("db", "v", "SELECT 1")}, order: []viewName{{"db", "v"}}},
		{name: "cross_database_chain", views: []dumpView{v("a", "v", "SELECT * FROM b.v"), v("b", "v", "SELECT * FROM c.v"), v("c", "v", "SELECT * FROM data.t")}, order: []viewName{{"c", "v"}, {"b", "v"}, {"a", "v"}}},
		{name: "database_cycle_not_object_cycle", views: []dumpView{v("a", "v", "SELECT * FROM b.t"), v("b", "v", "SELECT * FROM a.t")}, order: []viewName{{"a", "v"}, {"b", "v"}}},
		{name: "qualified_and_local", views: []dumpView{v("a", "v", "SELECT * FROM b.v JOIN v2 ON true"), v("a", "v2", "SELECT 1"), v("b", "v", "SELECT 2")}, order: []viewName{{"a", "v2"}, {"b", "v"}, {"a", "v"}}},
		{name: "independent_literals", views: []dumpView{v("db", "va", "SELECT 'vb' AS x"), v("db", "vb", "SELECT 'va' AS x")}, order: []viewName{{"db", "va"}, {"db", "vb"}}},
		{name: "external_dependency", views: []dumpView{v("a", "v", "SELECT * FROM b.v")}, order: []viewName{{"a", "v"}}},
		{name: "self_cycle", views: []dumpView{v("db", "v", "SELECT * FROM v")}, err: "cyclic view dependencies"},
		{name: "unused_cte_not_cycle", views: []dumpView{v("db", "v", "WITH unused AS (SELECT * FROM v) SELECT 1 AS x")}, order: []viewName{{"db", "v"}}},
		{name: "dual_not_cycle", views: []dumpView{v("db", "dual", "SELECT 1 AS x FROM dual")}, order: []viewName{{"db", "dual"}}},
		{name: "qualified_dual_self_cycle", views: []dumpView{v("db", "dual", "SELECT * FROM db.dual")}, err: "cyclic view dependencies"},
		{name: "masked_outer_cte_orders_catalog_view", views: []dumpView{v("db", "a_shadow", "WITH x AS (SELECT 99 AS x) SELECT * FROM (WITH y AS (SELECT * FROM x), x AS (SELECT 7 AS x) SELECT * FROM y) q"), v("db", "x", "SELECT 5 AS x")}, order: []viewName{{"db", "x"}, {"db", "a_shadow"}}},
		{name: "cross_database_cycle", views: []dumpView{v("a", "v", "SELECT * FROM b.v"), v("b", "v", "SELECT * FROM a.v")}, err: "cyclic view dependencies"},
		{name: "partial_cycle", views: []dumpView{v("db", "ok", "SELECT 1"), v("db", "v", "SELECT * FROM v")}, err: "cyclic view dependencies"},
		{name: "invalid", views: []dumpView{{name: viewName{"db", "v"}, create: "CREATE VIEW v AS SELECT FROM"}}, err: "analyze view db.v"},
		{name: "not_a_view", views: []dumpView{{name: viewName{"db", "v"}, create: "SELECT 1"}}, err: "expected CREATE VIEW"},
		{name: "duplicate", views: []dumpView{v("db", "v", "SELECT 1"), v("db", "v", "SELECT 2")}, err: "duplicate view"},
		{name: "multiple_statements", views: []dumpView{{name: viewName{"db", "v"}, create: "CREATE VIEW v AS SELECT 1; CREATE VIEW other AS SELECT 2"}}, err: "expected one CREATE VIEW"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := append([]dumpView(nil), tt.views...)
			ordered, err := orderDumpViews(context.Background(), tt.views, viewSQLMode{lowerCaseTableNames: 1})
			require.Equal(t, before, tt.views, "sorting must not mutate the input, including on errors")
			if tt.err != "" {
				require.ErrorContains(t, err, tt.err)
				require.Nil(t, ordered)
				return
			}
			require.NoError(t, err)
			require.Len(t, ordered, len(tt.order))
			for i, name := range tt.order {
				require.Equal(t, name, ordered[i].name)
			}
		})
	}
}

func TestQuoteIdentifier(t *testing.T) {
	require.Equal(t, "`a``b`", quoteIdentifier("a`b"))
}

func TestCaseSensitiveViews(t *testing.T) {
	views := []dumpView{
		{name: viewName{"db", "v"}, create: "CREATE VIEW v AS SELECT * FROM db.V"},
		{name: viewName{"db", "V"}, create: "CREATE VIEW V AS SELECT 1"},
	}
	for _, mode := range []int64{0, 2} {
		ordered, err := orderDumpViews(context.Background(), views, viewSQLMode{lowerCaseTableNames: mode})
		require.NoError(t, err)
		require.Equal(t, []dumpView{views[1], views[0]}, ordered)
	}
	_, err := orderDumpViews(context.Background(), views, viewSQLMode{lowerCaseTableNames: 1})
	require.ErrorContains(t, err, "cyclic view dependencies")
	refs, err := viewReferences(context.Background(), dumpView{name: viewName{"db", "result"}, create: "CREATE VIEW result AS WITH v AS (SELECT 1) SELECT * FROM V"}, viewSQLMode{})
	require.NoError(t, err)
	require.Equal(t, map[viewName]struct{}{{"db", "V"}: {}}, refs)
	refs, err = viewReferences(context.Background(), dumpView{name: viewName{"DB", "result"}, create: "CREATE VIEW result AS WITH V AS (SELECT 1) SELECT * FROM v JOIN OTHER.T ON true"}, viewSQLMode{lowerCaseTableNames: 2})
	require.NoError(t, err)
	require.Equal(t, map[viewName]struct{}{{"OTHER", "T"}: {}}, refs)
}
