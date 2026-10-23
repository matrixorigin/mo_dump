// Copyright 2026 Matrix Origin
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bytes"
	"context"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/matrixorigin/matrixone/pkg/common/moerr"
	"github.com/matrixorigin/mo_dump/internal/catalog"
	"github.com/stretchr/testify/require"
)

// TestGetCreateSequence verifies how a sequence's live backing row becomes
// restore SQL (matrixorigin/matrixone#30577): the definition reflects the
// current ALTER state, the allocation row replays last_seq_num/is_called,
// and anything that would corrupt the script fails the dump.
func TestGetCreateSequence(t *testing.T) {
	ctx := context.Background()

	typeQuery := func(db, seq string, isSys bool) string {
		query := "select mo_show_visible_bin(atttyp, 2) from mo_catalog.mo_columns where att_database = " +
			quoteString(db) + " and att_relname = " + quoteString(seq) + " and attname = 'last_seq_num'"
		if isSys {
			query += " and account_id = 0"
		}
		return query
	}
	stateCols := []string{
		"last_seq_num", "min_value", "max_value",
		"start_value", "increment_value", "cycle", "is_called",
	}
	stateQuery := func(db, seq string) string {
		return sequenceStateQuery + quoteIdentifier(db) + "." + quoteIdentifier(seq)
	}
	emptyState := func() *sqlmock.Rows { return sqlmock.NewRows(stateCols) }
	oneState := func(values ...string) *sqlmock.Rows {
		return emptyState().AddRow(
			values[0], values[1], values[2], values[3], values[4], values[5], values[6])
	}

	for _, tc := range []struct {
		name       string
		seq        string
		columnType string
		state      []string
		wantCreate string
		wantState  string
	}{
		{
			name:       "called ascending",
			seq:        "sq",
			columnType: "bigint",
			state:      []string{"15", "10", "100", "10", "5", "0", "1"},
			wantCreate: "CREATE SEQUENCE `sq` AS bigint INCREMENT BY 5 MINVALUE 10 MAXVALUE 100 START WITH 10 NO CYCLE",
			// called=true is replayed with the setval argument negated: the
			// next NEXTVAL after restore returns last+increment.
			wantState: "SELECT setval('sq', 15, false)",
		},
		{
			name:       "never called",
			seq:        "sq",
			columnType: "int",
			state:      []string{"10", "10", "100", "10", "5", "false", "false"},
			wantCreate: "CREATE SEQUENCE `sq` AS int INCREMENT BY 5 MINVALUE 10 MAXVALUE 100 START WITH 10 NO CYCLE",
			// called=false is replayed as setval(last, true): the next NEXTVAL
			// after restore returns last itself.
			wantState: "SELECT setval('sq', 10, true)",
		},
		{
			name:       "cycle and unsigned max",
			seq:        "sq",
			columnType: "bigint unsigned",
			// last_seq_num may exceed int64 for unsigned sequences.
			state:      []string{"18446744073709551615", "1", "18446744073709551615", "1", "-2", "true", "true"},
			wantCreate: "CREATE SEQUENCE `sq` AS bigint unsigned INCREMENT BY -2 MINVALUE 1 MAXVALUE 18446744073709551615 START WITH 1 CYCLE",
			wantState:  "SELECT setval('sq', 18446744073709551615, false)",
		},
		{
			name:       "backtick in name",
			seq:        "s`q",
			columnType: "smallint",
			state:      []string{"21", "1", "3000", "10", "1", "0", "1"},
			wantCreate: "CREATE SEQUENCE `s``q` AS smallint INCREMENT BY 1 MINVALUE 1 MAXVALUE 3000 START WITH 10 NO CYCLE",
			wantState:  "SELECT setval('s`q', 21, false)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, mock := exportTestDB(t)
			mock.ExpectQuery(typeQuery("db", tc.seq, false)).
				WillReturnRows(sqlmock.NewRows([]string{"atttyp"}).AddRow(tc.columnType))
			mock.ExpectQuery(stateQuery("db", tc.seq)).WillReturnRows(oneState(tc.state...))

			createSQL, stateSQL, err := getCreateSequence(ctx, "db", tc.seq, false, true)
			require.NoError(t, err)
			require.Equal(t, tc.wantCreate, createSQL)
			require.Equal(t, tc.wantState, stateSQL)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}

	for _, tc := range []struct {
		name       string
		columnType string
		hasTypeRow bool
		typeRows   *sqlmock.Rows
		hasState   bool
		state      *sqlmock.Rows
		wantErr    string
	}{
		{
			name:       "missing type row",
			hasTypeRow: false,
			hasState:   false,
		},
		{
			name:       "empty type text",
			hasTypeRow: true,
			columnType: "",
			hasState:   false,
		},
		{
			// A non-isolated mo_catalog view can answer the type query with the
			// same-name sequence of another tenant; that must fail the dump with
			// an explicit ambiguity error instead of exporting whichever row the
			// driver happened to return first (matrixorigin/matrixone#30577).
			name:       "ambiguous type rows",
			hasTypeRow: true,
			typeRows: sqlmock.NewRows([]string{"atttyp"}).
				AddRow("bigint").AddRow("smallint"),
			hasState: false,
			wantErr:  "ambiguous column metadata",
		},
		{
			name:       "no state row",
			hasTypeRow: true,
			columnType: "bigint",
			hasState:   true,
			state:      emptyState(),
			wantErr:    "has no state row",
		},
		{
			name:       "multiple state rows",
			hasTypeRow: true,
			columnType: "bigint",
			hasState:   true,
			state: sqlmock.NewRows(stateCols).
				AddRow("1", "1", "100", "1", "1", "0", "0").
				AddRow("2", "1", "100", "1", "1", "0", "0"),
			wantErr: "more than one state row",
		},
		{
			// ALTER SEQUENCE pre-advances last_seq_num in the session that
			// called NEXTVAL, which can leave last outside [min, max]. SETVAL
			// rejects the replay on restore, so a dump of that state must
			// fail at export time (matrixorigin/matrixone#30577).
			name:       "last allocation exceeds max_value",
			hasTypeRow: true,
			columnType: "bigint",
			hasState:   true,
			state:      oneState("11", "1", "10", "10", "1", "0", "0"),
			wantErr:    "outside its bounds",
		},
		{
			name:       "last allocation below min_value",
			hasTypeRow: true,
			columnType: "bigint",
			hasState:   true,
			state:      oneState("0", "1", "10", "1", "1", "0", "0"),
			wantErr:    "outside its bounds",
		},
		{
			name:       "start value out of bounds",
			hasTypeRow: true,
			columnType: "bigint",
			hasState:   true,
			state:      oneState("5", "1", "10", "99", "1", "0", "1"),
			wantErr:    "start_value 99 is outside",
		},
		{
			name:       "non-numeric last value",
			hasTypeRow: true,
			columnType: "bigint",
			hasState:   true,
			state:      oneState("x", "1", "100", "10", "5", "0", "1"),
			wantErr:    "non-numeric value",
		},
		{
			name:       "invalid cycle",
			hasTypeRow: true,
			columnType: "bigint",
			hasState:   true,
			state:      oneState("1", "1", "100", "10", "5", "maybe", "1"),
			wantErr:    "invalid boolean value",
		},
		{
			name:       "invalid is_called",
			hasTypeRow: true,
			columnType: "bigint",
			hasState:   true,
			state:      oneState("1", "1", "100", "10", "5", "1", "maybe"),
			wantErr:    "invalid boolean value",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, mock := exportTestDB(t)
			typeRows := tc.typeRows
			if typeRows == nil {
				typeRows = sqlmock.NewRows([]string{"atttyp"})
				if tc.hasTypeRow {
					typeRows.AddRow(tc.columnType)
				}
			}
			mock.ExpectQuery(typeQuery("db", "sq", false)).WillReturnRows(typeRows)
			if tc.hasState {
				mock.ExpectQuery(stateQuery("db", "sq")).WillReturnRows(tc.state)
			}
			_, _, err := getCreateSequence(ctx, "db", "sq", false, true)
			require.Error(t, err)
			switch tc.name {
			case "missing type row", "empty type text":
				require.True(t, moerr.IsMoErrCode(err, moerr.ErrNoSuchTable), "want ErrNoSuchTable, got %v", err)
			default:
				require.Contains(t, err.Error(), tc.wantErr)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}

	// The -sys mode scopes metadata to account 0, where mo_catalog is not
	// tenant-isolated: only the sys-owned row may answer the type query
	// (matrixorigin/matrixone#30577).
	t.Run("sys account scopes the metadata query", func(t *testing.T) {
		_, mock := exportTestDB(t)
		mock.ExpectQuery(typeQuery("db", "sq", true)).
			WillReturnRows(sqlmock.NewRows([]string{"atttyp"}).AddRow("smallint"))
		mock.ExpectQuery(stateQuery("db", "sq")).WillReturnRows(
			emptyState().AddRow("5", "1", "3000", "5", "3", "0", "1"))

		createSQL, stateSQL, err := getCreateSequence(ctx, "db", "sq", true, true)
		require.NoError(t, err)
		require.Equal(t, "CREATE SEQUENCE `sq` AS smallint INCREMENT BY 3 MINVALUE 1 MAXVALUE 3000 START WITH 5 NO CYCLE", createSQL)
		require.Equal(t, "SELECT setval('sq', 5, false)", stateSQL)
		require.NoError(t, mock.ExpectationsWereMet())
	})

	// A definition-only (-no-data) dump never replays the allocation state,
	// so an unreplayable state must keep the — still legal — definition
	// exportable, while a broken definition is rejected in both modes
	// (matrixorigin/matrixone#30577).
	t.Run("definition only tolerates an unreplayable allocation", func(t *testing.T) {
		_, mock := exportTestDB(t)
		mock.ExpectQuery(typeQuery("db", "sq", false)).
			WillReturnRows(sqlmock.NewRows([]string{"atttyp"}).AddRow("bigint"))
		// last=11 outside [1,10]: ALTER SEQUENCE on a called session leaves
		// exactly this state.
		mock.ExpectQuery(stateQuery("db", "sq")).WillReturnRows(
			emptyState().AddRow("11", "1", "10", "10", "1", "0", "0"))

		createSQL, stateSQL, err := getCreateSequence(ctx, "db", "sq", false, false)
		require.NoError(t, err)
		require.Equal(t, "", stateSQL)
		require.Equal(t, "CREATE SEQUENCE `sq` AS bigint INCREMENT BY 1 MINVALUE 1 MAXVALUE 10 START WITH 10 NO CYCLE", createSQL)
		require.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("definition only still rejects a broken definition", func(t *testing.T) {
		_, mock := exportTestDB(t)
		mock.ExpectQuery(typeQuery("db", "sq", false)).
			WillReturnRows(sqlmock.NewRows([]string{"atttyp"}).AddRow("bigint"))
		mock.ExpectQuery(stateQuery("db", "sq")).WillReturnRows(
			emptyState().AddRow("5", "1", "10", "99", "1", "0", "1"))

		_, _, err := getCreateSequence(ctx, "db", "sq", false, false)
		require.Error(t, err)
		require.Contains(t, err.Error(), "start_value 99 is outside")
		require.NoError(t, mock.ExpectationsWereMet())
	})
}

// TestReorderDumpObjects checks the stable partition that puts sequences
// before the tables depending on them while every other entry — tables and
// the collected view markers alike — keeps its catalog order.
func TestReorderDumpObjects(t *testing.T) {
	t.Run("mixed kinds", func(t *testing.T) {
		create := []string{"C:t1", "C:sq1", "C:v1", "C:e1", "C:sq2"}
		state := []string{"", "U:sq1", "", "", "U:sq2"}
		tables := Tables{
			{Name: "t1", Kind: catalog.SystemOrdinaryRel},
			{Name: "sq1", Kind: catalog.SystemSequenceRel},
			{Name: "v1", Kind: catalog.SystemViewRel},
			{Name: "e1", Kind: catalog.SystemExternalRel},
			{Name: "sq2", Kind: catalog.SystemSequenceRel},
		}
		reorderDumpObjects(create, tables, state)
		require.Equal(t, []string{"C:sq1", "C:sq2", "C:t1", "C:v1", "C:e1"}, create)
		require.Equal(t, []string{"U:sq1", "U:sq2", "", "", ""}, state)
		require.Equal(t, Tables{
			{Name: "sq1", Kind: catalog.SystemSequenceRel},
			{Name: "sq2", Kind: catalog.SystemSequenceRel},
			{Name: "t1", Kind: catalog.SystemOrdinaryRel},
			{Name: "v1", Kind: catalog.SystemViewRel},
			{Name: "e1", Kind: catalog.SystemExternalRel},
		}, tables)
	})

	t.Run("sequence leads even when the catalog returns it last", func(t *testing.T) {
		create := []string{"C:v1", "C:t1", "C:sq1"}
		state := []string{"", "", "U"}
		tables := Tables{
			{Name: "v1", Kind: catalog.SystemViewRel},
			{Name: "t1", Kind: catalog.SystemOrdinaryRel},
			{Name: "sq1", Kind: catalog.SystemSequenceRel},
		}
		reorderDumpObjects(create, tables, state)
		require.Equal(t, []string{"C:sq1", "C:v1", "C:t1"}, create)
		require.Equal(t, []string{"U", "", ""}, state)
		require.Equal(t, Tables{
			{Name: "sq1", Kind: catalog.SystemSequenceRel},
			{Name: "v1", Kind: catalog.SystemViewRel},
			{Name: "t1", Kind: catalog.SystemOrdinaryRel},
		}, tables)
	})
}

// TestFilterUserDatabases pins -db all scope: the seven platform databases
// are skipped and nothing else, in the order SHOW DATABASES returned.
func TestFilterUserDatabases(t *testing.T) {
	schema := []string{
		"information_schema", "user_a", "mo_catalog", "mo_debug", "mo_task",
		"mysql", "system", "system_metrics", "user_b", "mo_catalog_copy", "xmysql",
	}
	got := filterUserDatabases(schema)
	require.Equal(t, []string{"user_a", "user_b", "mo_catalog_copy", "xmysql"}, got)
	require.Empty(t, filterUserDatabases([]string{"system", "mo_catalog"}))
	require.Empty(t, filterUserDatabases(nil))
}

// TestDumpDataExternalTableScript pins the unchanged external-table emit
// now that ordering also handles sequences.
func TestDumpDataExternalTableScript(t *testing.T) {
	ctx := context.Background()
	_, mock := exportTestDB(t)
	mock.ExpectQuery("select datname, dat_type from mo_catalog.mo_database where datname = ?").WithArgs("db").
		WillReturnRows(sqlmock.NewRows([]string{"datname", "dat_type"}).AddRow("db", ""))
	mock.ExpectQuery("show create database `db`").
		WillReturnRows(sqlmock.NewRows([]string{"Database", "Create Database"}).AddRow("db", "CREATE DATABASE db"))
	mock.ExpectQuery("select relname,relkind from mo_catalog.mo_tables where reldatabase = ?").WithArgs("db").
		WillReturnRows(sqlmock.NewRows([]string{"relname", "relkind"}).
			AddRow("ext", catalog.SystemExternalRel))
	mock.ExpectQuery("show create table `db`.`ext`").
		WillReturnRows(sqlmock.NewRows([]string{"Table", "Create Table"}).
			AddRow("ext", "CREATE EXTERNAL TABLE ext (id int)"))

	var buf bytes.Buffer
	sink := sinkOver(&buf)
	opt := Options{dbs: []string{"db"}, emptyTables: true, netBufferLength: 16384}
	require.NoError(t, opt.dumpData(ctx, sink))
	expected := "SET foreign_key_checks = 0;\n\n" +
		"DROP DATABASE IF EXISTS `db`;\n" +
		"CREATE DATABASE db ;\n" +
		"USE `db`;\n\n\n" +
		"DROP TABLE IF EXISTS `ext`;\n" +
		"CREATE EXTERNAL TABLE ext (id int);\n\n\n" +
		"SET foreign_key_checks = 1;\n"
	require.Equal(t, expected, buf.String())
	require.NoError(t, mock.ExpectationsWereMet())
}
