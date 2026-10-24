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
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMergedViewReplaySource(t *testing.T) {
	for _, tc := range []struct{ name, ddl, want string }{
		{"create unchanged", "CREATE VIEW v AS SELECT 'ALTER VIEW' AS x", "CREATE VIEW v AS SELECT 'ALTER VIEW' AS x"},
		{"alter", "ALTER VIEW v AS SELECT 'ALTER VIEW' AS x", "CREATE VIEW v AS SELECT 'ALTER VIEW' AS x"},
		{"leading comment", "/* leading */ ALTER VIEW `v``x` AS SELECT 'a\\' AS x", "/* leading */ CREATE VIEW `v``x` AS SELECT 'a\\' AS x"},
		{"executable verb", "/*! ALTER */ VIEW v AS SELECT 1 AS x", "/*! CREATE */ VIEW v AS SELECT 1 AS x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, err := analyzeViewDependencies("db", tc.ddl, viewSQLMode{sqlMode: "NO_BACKSLASH_ESCAPES"})
			require.NoError(t, err)
			require.Equal(t, tc.want, a.create)
			again, err := analyzeViewDependencies("db", a.create, viewSQLMode{sqlMode: "NO_BACKSLASH_ESCAPES"})
			require.NoError(t, err)
			require.Equal(t, a, again)
		})
	}
}

func TestMergedEmptyDefaultTargetsOwner(t *testing.T) {
	_, output := mockDumpOutput(t)
	mode := ""
	view := dumpView{name: viewName{"owner", "v"}, create: "CREATE VIEW v AS SELECT 7 AS x", definition: &viewDefinition{SQLMode: &mode}}
	require.NoError(t, showDumpView(sinkOver(os.Stdout), view, viewSQLMode{}))
	text, err := os.ReadFile(output.Name())
	require.NoError(t, err)
	require.Contains(t, string(text), "USE `owner`;\n\nDROP VIEW IF EXISTS `owner`.`v`;\nCREATE VIEW v AS SELECT 7 AS x;")
}

func TestMergedAdvisoryFailureKeepsRequiredGraph(t *testing.T) {
	source := dumpView{name: viewName{"b", "root"}, create: "CREATE VIEW root AS SELECT 7 AS x"}
	child := dumpView{name: viewName{"a", "child"}, create: "CREATE VIEW child AS WITH unused AS(SELECT * FROM unused) SELECT x FROM b.root"}
	analysis, err := analyzeDumpView(context.Background(), child, viewSQLMode{})
	require.NoError(t, err, "invalid unused binding is not required by the lazy server")
	require.Equal(t, map[viewName]struct{}{{"b", "root"}: {}}, analysis.references)
	require.Empty(t, analysis.advisory)
	ordered, err := orderDumpViews(context.Background(), []dumpView{child, source}, viewSQLMode{})
	require.NoError(t, err)
	require.Equal(t, []dumpView{source, child}, ordered)
}
