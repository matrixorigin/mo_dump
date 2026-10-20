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
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestViewStoredNameMode(t *testing.T) {
	for _, tt := range []struct {
		name, metadata string
		current        int64
		want           viewName
	}{
		{"saved_zero", `{"lower_case_table_names":0}`, 1, viewName{"DB", "A_CONSUMER"}},
		{"saved_one", `{"lower_case_table_names":1}`, 0, viewName{"db", "a_consumer"}},
		{"saved_two", `{"lower_case_table_names":2}`, 1, viewName{"DB", "A_CONSUMER"}},
		{"legacy_missing", `{}`, 1, viewName{"db", "a_consumer"}},
		{"legacy_null", `{"lower_case_table_names":null}`, 1, viewName{"db", "a_consumer"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var definition viewDefinition
			require.NoError(t, json.Unmarshal([]byte(tt.metadata), &definition))
			before := viewSQLMode{lowerCaseTableNames: tt.current}
			refs, err := viewReferences(context.Background(), dumpView{name: viewName{"DB", "v"}, create: "CREATE VIEW v AS SELECT x FROM DB.A_CONSUMER", definition: &definition}, before)
			require.NoError(t, err)
			require.Equal(t, map[viewName]struct{}{tt.want: {}}, refs)
			require.Equal(t, tt.current, before.lowerCaseTableNames)
		})
	}
}

func TestOrderViewsStoredNameIdentity(t *testing.T) {
	var sensitive, folded viewDefinition
	require.NoError(t, json.Unmarshal([]byte(`{"lower_case_table_names":0}`), &sensitive))
	require.NoError(t, json.Unmarshal([]byte(`{"lower_case_table_names":1}`), &folded))
	consumer := dumpView{name: viewName{"db", "a_consumer"}, create: "CREATE VIEW a_consumer AS SELECT x FROM A_CONSUMER", definition: &sensitive}
	source := dumpView{name: viewName{"db", "A_CONSUMER"}, create: "CREATE VIEW A_CONSUMER AS SELECT 7 AS x", definition: &sensitive}
	for _, current := range []int64{0, 1, 2} {
		t.Run(string(rune('0'+current)), func(t *testing.T) {
			mode := viewSQLMode{lowerCaseTableNames: current}
			ordered, err := orderDumpViews(context.Background(), []dumpView{consumer}, mode)
			require.NoError(t, err)
			require.Equal(t, []dumpView{consumer}, ordered, "uppercase dependency is outside selection")
			ordered, err = orderDumpViews(context.Background(), []dumpView{consumer, source}, mode)
			require.NoError(t, err)
			require.Equal(t, []dumpView{source, consumer}, ordered)
		})
	}
	// A mode-1 reference resolves to lowercase, not a similarly spelled uppercase
	// catalog object. Folding both sides would invent a dependency or cycle.
	upper := dumpView{name: viewName{"db", "UPPER"}, create: "CREATE VIEW UPPER AS SELECT 7 AS x", definition: &sensitive}
	caller := dumpView{name: viewName{"db", "caller"}, create: "CREATE VIEW caller AS SELECT x FROM UPPER", definition: &folded}
	ordered, err := orderDumpViews(context.Background(), []dumpView{caller, upper}, viewSQLMode{lowerCaseTableNames: 1})
	require.NoError(t, err)
	require.Equal(t, []dumpView{caller, upper}, ordered)
	consumer.definition = &folded
	_, err = orderDumpViews(context.Background(), []dumpView{consumer, source}, viewSQLMode{lowerCaseTableNames: 0})
	require.ErrorContains(t, err, "cyclic view dependencies", "saved folding still detects real self references")
}

func TestDumpViewNameModeMetadataErrors(t *testing.T) {
	for _, encoded := range []string{`{"Stmt":"CREATE VIEW v AS SELECT 1","lower_case_table_names":"0"}`, `{"Stmt":"CREATE VIEW v AS SELECT 1","lower_case_table_names":-1}`, `{"Stmt":"CREATE VIEW v AS SELECT 1","lower_case_table_names":3}`} {
		t.Run(encoded, func(t *testing.T) {
			mock, _ := mockDumpOutput(t)
			mock.ExpectQuery("select viewdef from mo_catalog.mo_tables where reldatabase = ? and relname = ? and relkind = 'v'").WithArgs("db", "v").WillReturnRows(sqlmock.NewRows([]string{"viewdef"}).AddRow(encoded)).RowsWillBeClosed()
			_, err := getDumpView(context.Background(), "db", "v", false)
			require.ErrorContains(t, err, "lower_case_table_names")
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
