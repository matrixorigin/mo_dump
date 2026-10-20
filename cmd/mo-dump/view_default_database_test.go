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

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestViewDefaultDatabase(t *testing.T) {
	for _, tt := range []struct{ name, field, want string }{
		{"saved", `,"DefaultDatabase":"source"`, "source"},
		{"empty", `,"DefaultDatabase":""`, ""},
		{"missing", "", ""},
		{"null", `,"DefaultDatabase":null`, ""},
		{"invalid_type", `,"DefaultDatabase":42`, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			mock, output := mockDumpOutput(t)
			encoded := `{"Stmt":"CREATE VIEW owned.v AS SELECT x FROM target","lower_case_table_names":0` + tt.field + `}`
			mock.ExpectQuery("select viewdef from mo_catalog.mo_tables where reldatabase = ? and relname = ? and relkind = 'v'").WithArgs("owned", "v").WillReturnRows(sqlmock.NewRows([]string{"viewdef"}).AddRow(encoded)).RowsWillBeClosed()
			view, err := getDumpView(context.Background(), "owned", "v", false)
			if tt.name == "invalid_type" {
				require.ErrorContains(t, err, "DefaultDatabase")
				require.NoError(t, mock.ExpectationsWereMet())
				return
			}
			require.NoError(t, err)
			refs, err := viewReferences(context.Background(), view, viewSQLMode{lowerCaseTableNames: 1})
			require.NoError(t, err)
			require.Equal(t, map[viewName]struct{}{{tt.want, "target"}: {}}, refs)
			if tt.name == "saved" {
				source := dumpView{name: viewName{"source", "target"}, create: "CREATE VIEW source.target AS SELECT 7 AS x"}
				decoy := dumpView{name: viewName{"owned", "target"}, create: "CREATE VIEW owned.target AS SELECT 9 AS x"}
				ordered, err := orderDumpViews(context.Background(), []dumpView{view, decoy, source}, viewSQLMode{lowerCaseTableNames: 1})
				require.NoError(t, err)
				require.Equal(t, []dumpView{decoy, source, view}, ordered)
			}
			showDumpView(view)
			data, err := os.ReadFile(output.Name())
			require.NoError(t, err)
			text := string(data)
			require.Contains(t, text, "DROP VIEW IF EXISTS `owned`.`v`;")
			require.Contains(t, text, "CREATE VIEW owned.v AS SELECT x FROM target;")
			if tt.want == "" {
				require.NotContains(t, text, "USE ")
			} else {
				require.Contains(t, text, "USE `source`;")
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestViewCTECaseMatchesServer(t *testing.T) {
	for _, mode := range []int64{0, 1, 2} {
		for _, tt := range []struct{ declaration, reference string }{{"X", "X"}, {"X", "x"}, {"x", "X"}, {"x", "x"}} {
			var definition viewDefinition
			require.NoError(t, json.Unmarshal([]byte(`{"DefaultDatabase":"db"}`), &definition))
			query := "CREATE VIEW v AS WITH `" + tt.declaration + "` AS (SELECT 1 AS x) SELECT x FROM `" + tt.reference + "`"
			refs, err := viewReferences(context.Background(), dumpView{name: viewName{"db", "v"}, create: query, definition: &definition}, viewSQLMode{lowerCaseTableNames: mode})
			require.NoError(t, err)
			if mode != 1 && tt.reference == "X" {
				require.Equal(t, map[viewName]struct{}{{"db", "X"}: {}}, refs)
			} else {
				require.Empty(t, refs)
			}
		}
	}
}
