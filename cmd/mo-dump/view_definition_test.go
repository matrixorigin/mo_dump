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
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func viewMetadataMock(t *testing.T) sqlmock.Sqlmock {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	previous := conn
	t.Cleanup(func() {
		conn = previous
		mock.ExpectClose()
		require.NoError(t, db.Close())
		require.NoError(t, mock.ExpectationsWereMet())
	})
	conn = db
	return mock
}

func TestViewPersistedParserContext(t *testing.T) {
	empty, ansi, nbe := "", "ANSI_QUOTES", "NO_BACKSLASH_ESCAPES"
	zero := int64(0)
	for _, tc := range []struct {
		name, stmt, database string
		sqlMode              *string
		lower                *int64
	}{
		{"session ANSI creation", `CREATE VIEW a_dep AS SELECT x FROM "z_root"`, "d", &ansi, nil},
		{"explicit empty creation mode", `CREATE VIEW a_dep AS SELECT "literal" AS marker,x FROM z_root`, "d", &empty, nil},
		{"legacy creation mode", `CREATE VIEW a_dep AS SELECT 'a'||'b' AS marker,x FROM z_root`, "d", nil, nil},
		{"no backslash creation", `CREATE VIEW a_dep AS SELECT 'a\' AS marker,x FROM z_root`, "d", &nbe, nil},
		{"persisted ALTER", `ALTER VIEW a_dep AS SELECT x FROM "z_root"`, "d", &ansi, nil},
		{"definition default schema", `CREATE VIEW a_dep AS SELECT x FROM d.z_root`, "other", &empty, &zero},
	} {
		t.Run(tc.name, func(t *testing.T) {
			definitions := map[string]viewDefinition{
				"a_dep":  {Stmt: tc.stmt, DefaultDatabase: tc.database, SQLMode: tc.sqlMode, LowerCaseTableNames: tc.lower},
				"z_root": {Stmt: "CREATE VIEW z_root AS SELECT 1 AS x", DefaultDatabase: "d", SQLMode: &empty},
			}
			// The export session deliberately contradicts the saved context.
			mode := viewSQLMode{lowerCaseTableNames: 1, ansiQuotes: true, noBackslashEscapes: true, sqlMode: "ANSI_QUOTES,NO_BACKSLASH_ESCAPES"}
			if tc.sqlMode == &ansi {
				mode = viewSQLMode{lowerCaseTableNames: 1}
			}
			ddls := []string{"server DDL for a_dep must remain unchanged", "server DDL for z_root must remain unchanged"}
			tables := Tables{{Name: "a_dep"}, {Name: "z_root"}}
			require.NoError(t, adjustViewOrder(context.Background(), "d", ddls, tables, 0, mode, definitions))
			require.Equal(t, "z_root", tables[0].Name)
			require.Equal(t, []string{"server DDL for z_root must remain unchanged", "server DDL for a_dep must remain unchanged"}, ddls)
		})
	}
	t.Run("unqualified relation belongs to definition database", func(t *testing.T) {
		refs, err := viewDependenciesInContext("d", "other", "CREATE VIEW v AS SELECT * FROM z_root", viewSQLMode{}, viewSQLMode{})
		require.NoError(t, err)
		require.Empty(t, refs)
	})
	t.Run("CTE lookup precedes default schema filtering", func(t *testing.T) {
		refs, err := viewDependenciesInContext("d", "other", "CREATE VIEW v AS WITH q AS(SELECT x FROM d.z_root) SELECT * FROM q", viewSQLMode{}, viewSQLMode{})
		require.NoError(t, err)
		require.Equal(t, []string{"z_root"}, refs)
	})
	t.Run("saved case rules are distinct from catalog identity", func(t *testing.T) {
		refs, err := viewDependenciesInContext("D", "D", "CREATE VIEW v AS WITH Vb AS(SELECT 999 AS x) SELECT * FROM Vb", viewSQLMode{}, viewSQLMode{lowerCaseTableNames: 1})
		require.NoError(t, err)
		// Mode-0 Vb does not resolve the CTE declaration normalized to vb.
		// The exporting session cannot fold this already-bound identity.
		require.Equal(t, []string{"Vb"}, refs)
	})
	for _, saved := range []*string{nil, &empty} {
		mode, err := (viewDefinition{SQLMode: saved}).parserMode(viewSQLMode{ansiQuotes: true, noBackslashEscapes: true})
		require.NoError(t, err)
		require.False(t, mode.ansiQuotes)
		require.False(t, mode.noBackslashEscapes)
		if saved == nil {
			require.Equal(t, "PIPES_AS_CONCAT", mode.sqlMode)
		} else {
			require.Empty(t, mode.sqlMode)
		}
	}
}

func TestViewDefinitionFailuresAreAtomic(t *testing.T) {
	badLower := int64(3)
	for _, definitions := range []map[string]viewDefinition{
		{},
		{"v": {Stmt: "CREATE VIEW v AS SELECT 1", LowerCaseTableNames: &badLower}},
		{"v": {Stmt: "CREATE TABLE v(x int)"}},
	} {
		ddls := []string{"unchanged"}
		tables := Tables{{Name: "v"}}
		require.Error(t, adjustViewOrder(context.Background(), "d", ddls, tables, 0, viewSQLMode{}, definitions))
		require.Equal(t, []string{"unchanged"}, ddls)
		require.Equal(t, Tables{{Name: "v"}}, tables)
	}
}

func TestViewDefinitionReader(t *testing.T) {
	for _, tc := range []struct {
		name, payload string
		err           string
	}{
		{"legacy", `{"Stmt":"CREATE VIEW v AS SELECT 1","DefaultDatabase":"d"}`, ""},
		{"explicit empty", `{"Stmt":"CREATE VIEW v AS SELECT 1","sql_mode":"","lower_case_table_names":0}`, ""},
		{"malformed JSON", `{`, "invalid persisted definition"},
		{"wrong field type", `{"Stmt":"CREATE VIEW v AS SELECT 1","sql_mode":1}`, "invalid persisted definition"},
		{"missing statement", `{"sql_mode":"ANSI_QUOTES"}`, "empty persisted statement"},
		{"null envelope", `null`, "empty persisted statement"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock := viewMetadataMock(t)
			mock.ExpectQuery("select relname,viewdef").WithArgs("d", "v").WillReturnRows(sqlmock.NewRows([]string{"relname", "viewdef"}).AddRow("v", tc.payload)).RowsWillBeClosed()
			got, err := getViewDefinitions(context.Background(), "d", Tables{{Name: "v"}}, false)
			if tc.err != "" {
				require.ErrorContains(t, err, tc.err)
				require.Nil(t, got)
				return
			}
			require.NoError(t, err)
			var want viewDefinition
			require.NoError(t, json.Unmarshal([]byte(tc.payload), &want))
			require.Equal(t, want, got["v"])
		})
	}
	for _, failure := range []string{"query", "scan", "rows", "close", "missing", "duplicate"} {
		t.Run(failure, func(t *testing.T) {
			mock := viewMetadataMock(t)
			query := mock.ExpectQuery("select relname,viewdef").WithArgs("d", "v")
			rows := sqlmock.NewRows([]string{"relname", "viewdef"})
			switch failure {
			case "query":
				query.WillReturnError(errors.New("query failed"))
			case "scan":
				query.WillReturnRows(rows.AddRow("v", nil)).RowsWillBeClosed()
			case "rows":
				query.WillReturnRows(rows.AddRow("v", "{}").RowError(0, errors.New("row failed"))).RowsWillBeClosed()
			case "close":
				query.WillReturnRows(rows.CloseError(errors.New("close failed"))).RowsWillBeClosed()
			case "missing":
				query.WillReturnRows(rows).RowsWillBeClosed()
			case "duplicate":
				payload := `{"Stmt":"CREATE VIEW v AS SELECT 1"}`
				query.WillReturnRows(rows.AddRow("v", payload).AddRow("v", payload)).RowsWillBeClosed()
			}
			got, err := getViewDefinitions(context.Background(), "d", Tables{{Name: "v"}}, false)
			require.Error(t, err)
			require.Nil(t, got)
		})
	}
}

func TestViewDefinitionReaderBatches(t *testing.T) {
	mock := viewMetadataMock(t)
	tables := make(Tables, viewDefinitionBatchSize+1)
	for i := range tables {
		tables[i].Name = fmt.Sprintf("v%d", i)
	}
	for start := 0; start < len(tables); start += viewDefinitionBatchSize {
		rows := sqlmock.NewRows([]string{"relname", "viewdef"})
		for _, table := range tables[start:min(start+viewDefinitionBatchSize, len(tables))] {
			rows.AddRow(table.Name, `{"Stmt":"CREATE VIEW v AS SELECT 1"}`)
		}
		mock.ExpectQuery("select relname,viewdef.*account_id = 0.*relname in").WillReturnRows(rows).RowsWillBeClosed()
	}
	definitions, err := getViewDefinitions(context.Background(), "d", tables, true)
	require.NoError(t, err)
	require.Len(t, definitions, len(tables))
}

func TestViewDefinitionReaderEmpty(t *testing.T) {
	definitions, err := getViewDefinitions(context.Background(), "d", nil, false)
	require.NoError(t, err)
	require.Empty(t, definitions)
}

func TestViewDefinitionReaderCancelled(t *testing.T) {
	viewMetadataMock(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	definitions, err := getViewDefinitions(ctx, "d", Tables{{Name: "v"}}, false)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, definitions)
}
