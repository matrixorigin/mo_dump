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

	"github.com/stretchr/testify/require"
)

func TestViewCompleteSyntax(t *testing.T) {
	cases := []struct {
		name, query string
		refs        []string
	}{
		{"unshadowed outer CTE remains visible", "WITH z_root AS(SELECT 999 AS x) SELECT * FROM(WITH used AS(SELECT x FROM z_root) SELECT * FROM used)s", nil},
		{"later CTE masks outer definition", "WITH z_root AS(SELECT 999 AS x) SELECT * FROM(WITH used AS(SELECT x FROM z_root), z_root AS(SELECT 2 AS x) SELECT * FROM used)s", []string{"z_root"}},
		{"masked outer CTE stays unused", "WITH z_root AS(SELECT x FROM a_dep) SELECT * FROM(WITH used AS(SELECT x FROM z_root), z_root AS(SELECT 2 AS x) SELECT * FROM used)s", []string{"z_root"}},
		{"parenthesized query expression union", "SELECT * FROM ((SELECT 1 AS x) UNION ALL SELECT x FROM z_root) s", []string{"z_root"}},
		{"parenthesized query expression order", "SELECT * FROM ((SELECT 1 AS x) ORDER BY (SELECT x FROM z_root)) s", []string{"z_root"}},
		{"offset column in qualified ON", "SELECT l.x FROM t l JOIN t r ON l.offset=r.offset, z_root", []string{"t", "z_root"}},
		{"quoted offset column control", "SELECT l.x FROM t l JOIN t r ON l.`offset`=r.`offset`, z_root", []string{"t", "z_root"}},
		{"ordinary ON column control", "SELECT l.x FROM t l JOIN t r ON l.x=r.x, z_root", []string{"t", "z_root"}},
		{"unicode relation", "SELECT x FROM 数据", []string{"数据"}},
		{"unicode projection", "SELECT 中文 FROM t", []string{"t"}},
		{"quoted unicode control", "SELECT `中文` FROM `数据`", []string{"数据"}},
		{"ODBC join wrapper", "SELECT a.x FROM {oj t a LEFT JOIN t b ON a.x=b.x}", []string{"t"}},
		{"ODBC view dependency", "SELECT a.x FROM {oj t a LEFT JOIN z_root b ON a.x=b.x}", []string{"t", "z_root"}},
		{"ordinary join control", "SELECT a.x FROM t a LEFT JOIN t b ON a.x=b.x", []string{"t"}},
		{"virtual dual", "SELECT 1 FROM DUAL", nil},
		{"quoted virtual dual", "SELECT 1 FROM `dual`", nil},
		{"qualified dual is physical", "SELECT x FROM d.dual", []string{"dual"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			refs, err := viewDependencies("d", "CREATE VIEW a_dep AS "+tc.query, viewSQLMode{lowerCaseTableNames: 1})
			require.NoError(t, err)
			require.ElementsMatch(t, tc.refs, refs)
		})
	}
}

func TestViewCompleteRestoreOrder(t *testing.T) {
	for _, query := range []string{
		"WITH z_root AS(SELECT 999 AS x) SELECT * FROM(WITH used AS(SELECT x FROM z_root), z_root AS(SELECT 2 AS x) SELECT * FROM used)s",
		"WITH z_root AS(SELECT x FROM a_dep) SELECT * FROM(WITH used AS(SELECT x FROM z_root), z_root AS(SELECT 2 AS x) SELECT * FROM used)s",
		"SELECT * FROM ((SELECT 1 AS x) UNION ALL SELECT x FROM z_root) s",
		"SELECT * FROM ((SELECT 1 AS x) ORDER BY (SELECT x FROM z_root)) s",
		"SELECT l.x FROM t l JOIN t r ON l.offset=r.offset, z_root",
		"SELECT a.x FROM {oj t a LEFT JOIN z_root b ON a.x=b.x}",
	} {
		t.Run(query, func(t *testing.T) {
			ddls := []string{"CREATE VIEW a_dep AS " + query, "CREATE VIEW z_root AS SELECT 1 AS x"}
			tables := Tables{{Name: "a_dep", Kind: "v"}, {Name: "z_root", Kind: "v"}}
			require.NoError(t, adjustViewOrder(context.Background(), "d", ddls, tables, 0, viewSQLMode{lowerCaseTableNames: 1}))
			require.Equal(t, "z_root", tables[0].Name)
			require.Equal(t, "CREATE VIEW z_root AS SELECT 1 AS x", ddls[0])
			require.Equal(t, "a_dep", tables[1].Name)
			require.Equal(t, "CREATE VIEW a_dep AS "+query, ddls[1])
		})
	}
}

func TestViewDualIdentity(t *testing.T) {
	for _, mode := range []int64{0, 1, 2} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			refs, err := viewDependencies("d", "CREATE VIEW v AS SELECT x FROM `DUAL`", viewSQLMode{lowerCaseTableNames: mode})
			require.NoError(t, err)
			// The binder compares the parser's canonical name with exactly
			// "dual". Uppercase physical identities survive in modes 0/2.
			want := []string{"DUAL"}
			if mode == 1 {
				want = nil
			}
			require.Equal(t, want, refs)
		})
	}
}
