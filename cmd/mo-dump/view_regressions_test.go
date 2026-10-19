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

func TestViewReviewRegressions(t *testing.T) {
	for _, tt := range []struct {
		name, query string
		refs        []viewName
	}{
		{"values", "VALUES ROW(1), ROW(2)", nil},
		{"values_subquery", "VALUES ROW((SELECT x FROM other.x)),ROW(2)", []viewName{{"other", "x"}}},
		{"fulltext", "SELECT id FROM docs WHERE MATCH(body) AGAINST('hello' IN BOOLEAN MODE)", []viewName{{"db", "docs"}}},
		{"future_cte_masks_outer", "WITH x AS (SELECT 99 AS x) SELECT * FROM (WITH y AS (SELECT * FROM x), x AS (SELECT 7 AS x) SELECT * FROM y) q", []viewName{{"db", "x"}}},
		{"prior_cte_shadows_outer", "WITH x AS (SELECT 99 AS x) SELECT * FROM (WITH x AS (SELECT 7 AS x), y AS (SELECT * FROM x) SELECT * FROM y) q", nil},
		{"cte_declaration_not_use_scope", "WITH y AS (SELECT * FROM x) SELECT * FROM (WITH x AS (SELECT 7 AS x) SELECT * FROM y) q", []viewName{{"db", "x"}}},
		{"unused_self", "WITH unused AS (SELECT * FROM v) SELECT 1 AS x", nil},
		{"used_self", "WITH used AS (SELECT * FROM v) SELECT * FROM used", []viewName{{"db", "v"}}},
		{"unused_chain", "WITH a AS (SELECT * FROM v), b AS (SELECT * FROM a) SELECT 1 AS x", nil},
		{"used_chain", "WITH a AS (SELECT * FROM x), b AS (SELECT * FROM a) SELECT * FROM b JOIN b c ON true", []viewName{{"db", "x"}}},
		{"dual", "SELECT 1 AS x FROM dual", nil},
		{"quoted_dual", "SELECT 1 AS x FROM `dual`", nil},
		{"qualified_dual", "SELECT x FROM db.dual", []viewName{{"db", "dual"}}},
		{"other_database_dual", "SELECT x FROM other.dual", []viewName{{"other", "dual"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			refs, err := viewReferences(context.Background(), dumpView{viewName{"db", "v"}, "CREATE VIEW v AS " + tt.query}, viewSQLMode{lowerCaseTableNames: 1})
			require.NoError(t, err)
			require.Len(t, refs, len(tt.refs))
			for _, ref := range tt.refs {
				require.Contains(t, refs, ref)
			}
		})
	}
}
