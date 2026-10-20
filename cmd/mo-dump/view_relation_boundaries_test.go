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

func TestViewRelationBoundaries(t *testing.T) {
	for _, tt := range []struct {
		name, query string
		refs        []viewName
	}{
		{"unicode", "SELECT x FROM 数据", []viewName{{"db", "数据"}}},
		{"unicode_mixed", "SELECT x FROM alpha数据123", []viewName{{"db", "alpha数据123"}}},
		{"unicode_qualified", "SELECT x FROM 库.数据", []viewName{{"库", "数据"}}},
		{"unicode_cte", "WITH 临时 AS (SELECT * FROM 数据) SELECT * FROM 临时", []viewName{{"db", "数据"}}},
		{"unicode_quoted_control", "SELECT x FROM `数据`", []viewName{{"db", "数据"}}},
		{"numeric_id", "SELECT x FROM 123source", []viewName{{"db", "123source"}}},
		{"numeric_cte", "WITH 123c AS (SELECT * FROM z_source) SELECT * FROM 123c", []viewName{{"db", "z_source"}}},
		{"numeric_literal_control", "SELECT 123, 1e3, 0xCAFE FROM t", []viewName{{"db", "t"}}},
		{"set_exists", "SELECT * FROM t WHERE EXISTS ((SELECT 1 FROM t) UNION ALL SELECT 1 FROM z_source)", []viewName{{"db", "t"}, {"db", "z_source"}}},
		{"set_scalar", "SELECT ((SELECT x FROM t WHERE false) UNION ALL SELECT x FROM z_source LIMIT 1) AS x", []viewName{{"db", "t"}, {"db", "z_source"}}},
		{"set_nested", "SELECT * FROM t WHERE EXISTS (((SELECT 1 FROM t)) UNION ALL (SELECT 1 FROM z_source))", []viewName{{"db", "t"}, {"db", "z_source"}}},
		{"trim_subquery_control", "SELECT TRIM((SELECT x FROM t) FROM marker) FROM base", []viewName{{"db", "t"}, {"db", "base"}}},
		{"index_join", "SELECT * FROM t USE INDEX FOR JOIN (a_consumer)", []viewName{{"db", "t"}}},
		{"index_order_comma", "SELECT * FROM t USE INDEX FOR ORDER BY (idx), z_source", []viewName{{"db", "t"}, {"db", "z_source"}}},
		{"index_group_comma", "SELECT * FROM t FORCE KEY FOR GROUP BY (idx), z_source", []viewName{{"db", "t"}, {"db", "z_source"}}},
		{"index_multiple", "SELECT * FROM t AS qualify USE INDEX FOR JOIN (a_consumer) IGNORE INDEX FOR ORDER BY (idx), z_source", []viewName{{"db", "t"}, {"db", "z_source"}}},
		{"index_empty", "SELECT * FROM t USE INDEX (), z_source", []viewName{{"db", "t"}, {"db", "z_source"}}},
		{"index_numeric_unicode", "SELECT * FROM t USE INDEX FOR JOIN (123index, 中文索引), z_source", []viewName{{"db", "t"}, {"db", "z_source"}}},
		{"index_key_primary", "SELECT * FROM t IGNORE KEY FOR ORDER BY (PRIMARY) JOIN z_source ON true", []viewName{{"db", "t"}, {"db", "z_source"}}},
		{"implicit_alias", "SELECT * FROM t qualify, z_source", []viewName{{"db", "t"}, {"db", "z_source"}}},
		{"explicit_alias_control", "SELECT * FROM t AS qualify, z_source", []viewName{{"db", "t"}, {"db", "z_source"}}},
		{"implicit_alias_hint", "SELECT * FROM t qualify USE INDEX FOR ORDER BY (idx), z_source", []viewName{{"db", "t"}, {"db", "z_source"}}},
		{"string_alias", "SELECT * FROM t 'qualify', z_source", []viewName{{"db", "t"}, {"db", "z_source"}}},
		{"straight_select_option", "SELECT STRAIGHT_JOIN 1 AS x FROM t", []viewName{{"db", "t"}}},
		{"straight_select_star", "SELECT STRAIGHT_JOIN * FROM t", []viewName{{"db", "t"}}},
		{"straight_join_control", "SELECT * FROM t STRAIGHT_JOIN z_source ON true", []viewName{{"db", "t"}, {"db", "z_source"}}},
		{"straight_both", "SELECT STRAIGHT_JOIN * FROM t STRAIGHT_JOIN z_source ON true", []viewName{{"db", "t"}, {"db", "z_source"}}},
		{"snapshot_self", "SELECT * FROM v {snapshot='saved'}", nil},
		{"snapshot_qualified", "SELECT * FROM other.v {snapshot=saved} JOIN z_source ON true", []viewName{{"db", "z_source"}}},
		{"timestamp", "SELECT * FROM v {timestamp='2026-01-01 00:00:00'}", nil},
		{"mo_timestamp", "SELECT * FROM v {mo_ts=123}", nil},
		{"as_of_timestamp", "SELECT * FROM v {AS OF TIMESTAMP '2026-01-01 00:00:00'}", nil},
		{"historical_and_current", "SELECT * FROM v {snapshot='saved'} UNION ALL SELECT * FROM v", []viewName{{"db", "v"}}},
		{"historical_expression", "SELECT * FROM v {timestamp=(SELECT x FROM clock_view)}", []viewName{{"db", "clock_view"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			refs, err := viewReferences(context.Background(), dumpView{viewName{"db", "a_consumer"}, "CREATE VIEW a_consumer AS " + tt.query}, viewSQLMode{lowerCaseTableNames: 1})
			require.NoError(t, err)
			require.Len(t, refs, len(tt.refs))
			for _, ref := range tt.refs {
				require.Contains(t, refs, ref)
			}
		})
	}
}

func TestHistoricalViewIdentity(t *testing.T) {
	mode := viewSQLMode{lowerCaseTableNames: 1}
	views := []dumpView{{viewName{"db", "v"}, "CREATE VIEW v AS SELECT * FROM db.v {snapshot='saved'}"}}
	ordered, err := orderDumpViews(context.Background(), views, mode)
	require.NoError(t, err)
	require.Equal(t, views, ordered)
	views[0].create += " UNION ALL SELECT * FROM db.v"
	_, err = orderDumpViews(context.Background(), views, mode)
	require.ErrorContains(t, err, "cyclic view dependencies")
}

func TestViewIdentifierSpelling(t *testing.T) {
	refs, err := viewReferences(context.Background(), dumpView{viewName{"DB", "v"}, "CREATE VIEW v AS SELECT * FROM 123Source JOIN Other.数据表 ON true JOIN 123数据ABC n ON true"}, viewSQLMode{})
	require.NoError(t, err)
	require.Equal(t, map[viewName]struct{}{{"DB", "123source"}: {}, {"Other", "数据表"}: {}, {"DB", "123数据abc"}: {}}, refs)
}
