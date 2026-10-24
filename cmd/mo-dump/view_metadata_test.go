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
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestDumpViewMetadata(t *testing.T) {
	for _, tt := range []struct {
		name, encoded, wantMode string
		sys                     bool
		wantError               string
	}{
		{"ansi", `{"Stmt":"CREATE VIEW v AS SELECT 1","sql_mode":"ANSI_QUOTES"}`, "ANSI_QUOTES", false, ""},
		{"explicit_empty", `{"Stmt":"CREATE VIEW v AS SELECT 1","sql_mode":""}`, "", false, ""},
		{"legacy_missing", `{"Stmt":"CREATE VIEW v AS SELECT 1"}`, "PIPES_AS_CONCAT", false, ""},
		{"legacy_null", `{"Stmt":"CREATE VIEW v AS SELECT 1","sql_mode":null}`, "PIPES_AS_CONCAT", false, ""},
		{"sys", `{"Stmt":"CREATE VIEW v AS SELECT 1","sql_mode":"NO_BACKSLASH_ESCAPES"}`, "NO_BACKSLASH_ESCAPES", true, ""},
		{"invalid_json", `{`, "", false, "invalid persisted definition"},
		{"invalid_mode_type", `{"Stmt":"CREATE VIEW v AS SELECT 1","sql_mode":42}`, "", false, "invalid persisted definition"},
		{"invalid_stmt_type", `{"Stmt":42}`, "", false, "invalid persisted definition"},
		{"empty_object", `{}`, "", false, "empty persisted statement"},
		{"null_object", `null`, "", false, "empty persisted statement"},
		{"empty_stmt", `{"Stmt":"  "}`, "", false, "empty persisted statement"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			mock, _ := mockDumpOutput(t)
			query := "select relname,viewdef from mo_catalog.mo_tables where reldatabase = ? and relkind = 'v'"
			if tt.sys {
				query += " and account_id = 0"
			} else {
				query += " and account_id = current_account_id()"
			}
			query += " and relname in (?)"
			mock.ExpectQuery(query).WithArgs("db'雪", "v").WillReturnRows(sqlmock.NewRows([]string{"relname", "viewdef"}).AddRow("v", tt.encoded)).RowsWillBeClosed()
			view, err := getDumpView(context.Background(), "db'雪", "v", tt.sys)
			if tt.wantError != "" {
				require.ErrorContains(t, err, tt.wantError)
			} else {
				require.NoError(t, err)
				require.Equal(t, viewName{"db'雪", "v"}, view.name)
				require.Equal(t, "CREATE VIEW v AS SELECT 1", view.create)
				require.NotNil(t, view.definition)
				require.Equal(t, tt.wantMode, view.definition.parserSQLMode())
				if tt.name == "legacy_missing" || tt.name == "legacy_null" {
					require.Nil(t, view.definition.SQLMode)
				} else {
					require.NotNil(t, view.definition.SQLMode)
				}
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestDumpViewMetadataErrors(t *testing.T) {
	for _, failure := range []string{"query", "no_rows", "null_column", "cancel"} {
		t.Run(failure, func(t *testing.T) {
			mock, _ := mockDumpOutput(t)
			injected := errors.New("metadata read failed")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if failure == "cancel" {
				cancel()
			} else {
				expect := mock.ExpectQuery("select relname,viewdef from mo_catalog.mo_tables where reldatabase = ? and relkind = 'v' and account_id = current_account_id() and relname in (?)").WithArgs("db", "v")
				switch failure {
				case "query":
					expect.WillReturnError(injected)
				case "no_rows":
					expect.WillReturnRows(sqlmock.NewRows([]string{"relname", "viewdef"}))
				case "null_column":
					expect.WillReturnRows(sqlmock.NewRows([]string{"relname", "viewdef"}).AddRow("v", nil))
				}
			}
			_, err := getDumpView(ctx, "db", "v", false)
			switch failure {
			case "query":
				require.ErrorIs(t, err, injected)
			case "no_rows":
				require.ErrorContains(t, err, "missing persisted definition")
			case "cancel":
				require.ErrorIs(t, err, context.Canceled)
			default:
				require.ErrorContains(t, err, "cannot read persisted view definitions")
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestStoredModeOverridesConnection(t *testing.T) {
	for _, tt := range []struct {
		name, query, stored string
		connection          viewSQLMode
		refs                []viewName
	}{
		{"saved_ansi", `SELECT x FROM "z_source"`, "ANSI_QUOTES", viewSQLMode{lowerCaseTableNames: 1}, []viewName{{"db", "z_source"}}},
		{"saved_ansi_alias", `SELECT x FROM "z_source"`, "ANSI", viewSQLMode{lowerCaseTableNames: 1}, []viewName{{"db", "z_source"}}},
		{"saved_empty", `SELECT "z_source" AS marker`, "", viewSQLMode{lowerCaseTableNames: 1, ansiQuotes: true, noBackslashEscapes: true}, nil},
		{"saved_backslash", `SELECT '\' AS marker FROM z_source`, "NO_BACKSLASH_ESCAPES", viewSQLMode{lowerCaseTableNames: 1}, []viewName{{"db", "z_source"}}},
		{"saved_legacy", `SELECT "z_source" AS marker`, "PIPES_AS_CONCAT", viewSQLMode{lowerCaseTableNames: 1, ansiQuotes: true}, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			before := tt.connection
			refs, err := viewReferences(context.Background(), dumpView{name: viewName{"db", "a_consumer"}, create: "CREATE VIEW a_consumer AS " + tt.query, definition: &viewDefinition{DefaultDatabase: "db", SQLMode: &tt.stored}}, tt.connection)
			require.NoError(t, err)
			require.Equal(t, before, tt.connection)
			require.Len(t, refs, len(tt.refs))
			for _, ref := range tt.refs {
				require.Contains(t, refs, ref)
			}
		})
	}
}

func TestDumpViewModeOutput(t *testing.T) {
	for _, tt := range []struct {
		name, mode, flags string
		legacy            bool
	}{
		{"empty", "", "", false},
		{"ansi_quotes", "ANSI_QUOTES", "ANSI_QUOTES", false},
		{"no_backslash", "NO_BACKSLASH_ESCAPES", "NO_BACKSLASH_ESCAPES", false},
		{"ansi_alias", "ANSI", "ANSI_QUOTES,PIPES_AS_CONCAT,REAL_AS_FLOAT,IGNORE_SPACE", false},
		{"all_parser_flags", "IGNORE_SPACE,HIGH_NOT_PRECEDENCE,REAL_AS_FLOAT,NO_BACKSLASH_ESCAPES,PIPES_AS_CONCAT,ANSI_QUOTES", "ANSI_QUOTES,PIPES_AS_CONCAT,NO_BACKSLASH_ESCAPES,REAL_AS_FLOAT,HIGH_NOT_PRECEDENCE,IGNORE_SPACE", false},
		{"non_parser_policy", "ENABLE_BOOL_SUMAVG,STRICT_TRANS_TABLES", "", false},
		{"literal_boundary", "quote'and\\slash", "", false},
		{"legacy", "", "PIPES_AS_CONCAT", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, output := mockDumpOutput(t)
			definition := &viewDefinition{}
			if !tt.legacy {
				definition.SQLMode = &tt.mode
			}
			require.NoError(t, showDumpView(dumpView{name: viewName{"db", "v"}, create: "CREATE VIEW v AS SELECT 1", definition: definition}, viewSQLMode{lowerCaseTableNames: 1}))
			contents, err := os.ReadFile(output.Name())
			require.NoError(t, err)
			text := string(contents)
			save := strings.Index(text, "SET @modump_saved_sql_mode = @@SESSION.sql_mode;")
			apply := strings.Index(text, "SET SESSION sql_mode = CONCAT_WS(")
			create := strings.Index(text, "CREATE VIEW v AS SELECT 1;")
			restore := strings.Index(text, "SET SESSION sql_mode = @modump_saved_sql_mode;")
			require.Zero(t, save)
			require.Greater(t, apply, save)
			require.Greater(t, create, apply)
			require.Greater(t, restore, create)
			require.Contains(t, text, fmt.Sprintf("NULLIF(X'%x', '')", tt.flags))
			require.Contains(t, text, "CONCAT(',', @modump_saved_sql_mode, ',')")
			require.Contains(t, text, "',ANSI_QUOTES,'")
			require.Contains(t, text, "',NO_BACKSLASH_ESCAPES,'")
		})
	}
}

func TestLegacyViewModeDoesNotInheritConnection(t *testing.T) {
	refs, err := viewReferences(context.Background(), dumpView{name: viewName{"db", "v"}, create: `CREATE VIEW v AS SELECT "z_source" AS marker`, definition: &viewDefinition{}}, viewSQLMode{lowerCaseTableNames: 1, ansiQuotes: true, noBackslashEscapes: true})
	require.NoError(t, err)
	require.Empty(t, refs)
}
