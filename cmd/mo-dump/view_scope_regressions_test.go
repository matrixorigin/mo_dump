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
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestViewBoundDefaultDatabase(t *testing.T) {
	for _, tt := range []struct {
		name, query string
		want        viewName
	}{
		{"implicit_identity", "SELECT x FROM target", viewName{"SOURCE_DB", "target"}},
		{"explicit_token", "SELECT x FROM SOURCE_DB.target", viewName{"source_db", "target"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			refs, err := viewDependencies("SOURCE_DB", "CREATE VIEW v AS "+tt.query, viewSQLMode{lowerCaseTableNames: 1})
			require.NoError(t, err)
			require.Equal(t, map[viewName]struct{}{tt.want: {}}, refs)
		})
	}
	var definition viewDefinition
	require.NoError(t, json.Unmarshal([]byte(`{"DefaultDatabase":"DB","lower_case_table_names":1}`), &definition))
	consumer := dumpView{name: viewName{"db", "v"}, create: "CREATE VIEW db.v AS SELECT x FROM v", definition: &definition}
	source := dumpView{name: viewName{"DB", "v"}, create: "CREATE VIEW DB.v AS SELECT 7 AS x"}
	ordered, err := orderDumpViews(context.Background(), []dumpView{consumer, source}, viewSQLMode{lowerCaseTableNames: 1})
	require.NoError(t, err)
	require.Equal(t, []dumpView{source, consumer}, ordered)
}

func TestViewActiveCTEMask(t *testing.T) {
	for _, tt := range []struct {
		name, query string
		catalog     bool
	}{
		{"nested_main_inherits_mask", "WITH x AS (SELECT 99 AS x) SELECT * FROM (WITH a AS (WITH x AS (SELECT 7 AS x) SELECT * FROM x), x AS (SELECT 1 AS x) SELECT * FROM a) q", true},
		{"nested_binding_replaces_mask", "WITH x AS (SELECT 99 AS x) SELECT * FROM (WITH a AS (WITH x AS (SELECT 7 AS x), b AS (SELECT * FROM x) SELECT * FROM b), x AS (SELECT 1 AS x) SELECT * FROM a) q", false},
		{"no_active_mask", "WITH x AS (SELECT 99 AS x) SELECT * FROM (WITH x AS (SELECT 7 AS x) SELECT * FROM x) q", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			refs, err := viewDependencies("db", "CREATE VIEW v AS "+tt.query, viewSQLMode{lowerCaseTableNames: 1})
			require.NoError(t, err)
			if tt.catalog {
				require.Equal(t, map[viewName]struct{}{{"db", "x"}: {}}, refs)
			} else {
				require.Empty(t, refs)
			}
		})
	}
}

func TestViewEOFExecutableComment(t *testing.T) {
	for _, ddl := range []string{
		"CREATE VIEW v AS SELECT x /*! FROM target",
		"CREATE VIEW v AS SELECT x /*!50000 FROM target",
		"CREATE VIEW v AS SELECT x /*!50000 FROM target WHERE x='not FROM fake'",
	} {
		t.Run(ddl, func(t *testing.T) {
			refs, err := viewDependencies("db", ddl, viewSQLMode{lowerCaseTableNames: 1})
			require.NoError(t, err)
			require.Equal(t, map[viewName]struct{}{{"db", "target"}: {}}, refs)
		})
	}
	_, err := viewDependencies("db", "CREATE VIEW v AS SELECT 1 /* ordinary", viewSQLMode{})
	require.Error(t, err)
}

func TestViewDefaultContextUsage(t *testing.T) {
	for _, tt := range []struct {
		query string
		used  bool
	}{
		{"SELECT * FROM t", true},
		{"SELECT * FROM t {snapshot='saved'}", true},
		{"SELECT * FROM source.t", false},
		{"SELECT * FROM source.t {snapshot='saved'}", false},
		{"WITH t AS (SELECT 7) SELECT * FROM t", false},
		{"SELECT 7 FROM dual", false},
	} {
		t.Run(tt.query, func(t *testing.T) {
			analysis, err := analyzeViewDependencies("source", "CREATE VIEW owned.v AS "+tt.query, viewSQLMode{lowerCaseTableNames: 1})
			require.NoError(t, err)
			require.Equal(t, tt.used, analysis.usesDefaultDatabase)
		})
	}
}

func TestViewRenderAnalysisFailure(t *testing.T) {
	_, output := mockDumpOutput(t)
	err := showDumpView(dumpView{name: viewName{"db", "v"}, create: "CREATE VIEW v AS SELECT FROM"}, viewSQLMode{})
	require.Error(t, err)
	data, readErr := os.ReadFile(output.Name())
	require.NoError(t, readErr)
	require.Empty(t, data, "do not emit partial session-state changes after analysis failure")
}

func TestViewUnusedDefaultDatabase(t *testing.T) {
	for _, query := range []string{"SELECT 7 AS x", "SELECT 7 AS x FROM dual", "WITH c AS (SELECT 7 AS x) SELECT * FROM c", "SELECT x FROM external.source"} {
		t.Run(query, func(t *testing.T) {
			_, output := mockDumpOutput(t)
			var definition viewDefinition
			require.NoError(t, json.Unmarshal([]byte(`{"DefaultDatabase":"deleted","lower_case_table_names":1}`), &definition))
			require.NoError(t, showDumpView(dumpView{name: viewName{"owned", "v"}, create: "CREATE VIEW owned.v AS " + query, definition: &definition}, viewSQLMode{lowerCaseTableNames: 1}))
			data, err := os.ReadFile(output.Name())
			require.NoError(t, err)
			require.NotContains(t, string(data), "USE `deleted`;")
			require.Contains(t, string(data), "DROP VIEW IF EXISTS `owned`.`v`;")
		})
	}
}
