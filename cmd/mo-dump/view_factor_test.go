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
	"testing"

	"github.com/stretchr/testify/require"
)

func TestViewTableFactorRoles(t *testing.T) {
	for _, tc := range []struct {
		name, query string
		refs        []string
	}{
		{"implicit keyword alias", "select offset.x from t offset, z_root", []string{"t", "z_root"}},
		{"explicit keyword alias", "select offset.x from t as offset, z_root", []string{"t", "z_root"}},
		{"nonreserved alias", "select * from t account, z_root", []string{"t", "z_root"}},
		{"quoted alias", "select * from t `offset`, z_root", []string{"t", "z_root"}},
		{"string alias", "select * from t 'offset', z_root", []string{"t", "z_root"}},
		{"derived alias columns", "select * from (select x from t) offset(x), z_root", []string{"t", "z_root"}},
		{"table group alias", "select * from (t join z_root using(x)) offset", []string{"t", "z_root"}},
		{"actual offset clause", "select * from t offset 1", []string{"t"}},
		{"alias before actual offset", "select * from t offset offset 1", []string{"t"}},
		{"actual named window", "select sum(x) over w from t window w as (order by x)", []string{"t"}},
		{"SELECT join modifier", "select straight_join v from t", []string{"t"}},
		{"nested SELECT modifier", "select (select straight_join v from t) from z_root", []string{"t", "z_root"}},
		{"actual straight join", "select * from t straight_join z_root using(x)", []string{"t", "z_root"}},
		{"use index JOIN scope", "select x from t use index for join(v)", []string{"t"}},
		{"force key ORDER scope", "select x from t force key for order by(v), z_root", []string{"t", "z_root"}},
		{"ignore index GROUP scope", "select x from t ignore index for group by(v), z_root", []string{"t", "z_root"}},
		{"hint followed by join", "select * from t use index for join(v) join z_root using(x)", []string{"t", "z_root"}},
		{"alias before multiple hints", "select * from t offset use index(v) ignore key for join(v), z_root", []string{"t", "z_root"}},
		{"empty hint", "select * from t use index() join z_root using(x)", []string{"t", "z_root"}},
		{"numeric prefix", "select x from 1t", []string{"1t"}},
		{"quoted numeric prefix", "select x from `1t`", []string{"1t"}},
		{"numeric prefix alias", "select * from t 1alias, z_root", []string{"t", "z_root"}},
		{"dollar inside identifier", "select x from a$b$c", []string{"a$b$c"}},
		{"quoted dollar identifier", "select x from `a$b$c`", []string{"a$b$c"}},
		{"double dollar inside identifier", "select x from a$$c", []string{"a$$c"}},
		{"dollar at identifier end", "select x from a$b$", []string{"a$b$"}},
		{"dollar string control", "select $b$from t$b$", nil},
		{"combined CTE alias and hint", "with used as (select x from t offset use index for join(v)) select * from used offset, z_root", []string{"t", "z_root"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			refs, err := viewDependencies("d", "create view v as "+tc.query, viewSQLMode{lowerCaseTableNames: 1})
			require.NoError(t, err)
			require.Equal(t, tc.refs, refs)
		})
	}
}

func TestViewTableFactorErrors(t *testing.T) {
	for _, query := range []string{
		"select * from t as", "select * from t as 1",
		"select * from t use index", "select * from t use index for",
		"select * from t use index for where(v)",
		"select * from t force key for order(v)",
		"select * from t ignore index for join",
		"select * from t{snapshot='s')",
	} {
		t.Run(query, func(t *testing.T) {
			_, err := viewDependencies("d", "create view v as "+query, viewSQLMode{})
			require.Error(t, err)
		})
	}
}

func TestViewImplicitAliasRestoreOrder(t *testing.T) {
	ddl := []string{"create view a_dep as select offset.x from t offset, z_root", "create view z_root as select 1 as x"}
	tables := Tables{{Name: "a_dep"}, {Name: "z_root"}}
	require.NoError(t, adjustViewOrder(context.Background(), "d", ddl, tables, 0, viewSQLMode{lowerCaseTableNames: 1}))
	require.Equal(t, Tables{{Name: "z_root"}, {Name: "a_dep"}}, tables)
}

func TestViewNumericIdentifierFollowsLexer(t *testing.T) {
	// MatrixOne's numeric scanner itself folds an unquoted digit-prefixed ID;
	// quoted IDs preserve their spelling. Neither identity may be overwritten.
	for _, tc := range []struct{ sql, name string }{{"1T", "1t"}, {"`1T`", "1T"}} {
		refs, err := viewDependencies("d", "create view v as select * from "+tc.sql, viewSQLMode{})
		require.NoError(t, err)
		require.Equal(t, []string{tc.name}, refs)
	}
}
