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
	"database/sql"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newCatalogMock installs a sqlmock handle over the package-level conn with
// exact-query matching, so every test proves both the emitted SQL contract
// and the row-result behavior.
func newCatalogMock(t *testing.T) sqlmock.Sqlmock {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	require.NoError(t, err)
	oldConn := conn
	conn = db
	t.Cleanup(func() {
		conn = oldConn
		_ = db.Close()
	})
	return mock
}

// scope predicates shared by the catalog queries, mirroring the server's
// SHOW DATABASES / SHOW TABLES visibility (issues #30562 / #30579).
const (
	dbScopePredicate = " and (account_id = current_account_id() or (account_id = 0 and datname = 'mo_catalog'))"

	tablesScopePredicate = " and (account_id = current_account_id() or (account_id = 0 and " +
		"(relname in ('mo_database','mo_tables','mo_columns') or relkind = 'cluster')))"

	catalogHelpersPredicate = " and relname != 'mo_increment_columns'" +
		" and relname != '__mo_account_lock'"
)

// Hidden index helpers are identified by their per-database registration
// in mo_catalog.mo_indexes.index_table_name (the column exists under this
// exact name from MO v1.2 on), joined through mo_database with the same
// account scoping mo-dump uses for getDatabaseType, so helpers of other
// databases can never match a same-named user object here. Temporary
// tables are excluded by relkind only — a former '__mo_tmp_<32hex>_'
// name regexp was removed because it silently swallowed persistent user
// tables legally named with that shape.
// hiddenHelpersFor renders the temporary-table classification tail of the
// getTables predicate for the connected authority: the server-side classifier
// function (mo_is_legacy_temporary_table) when available, otherwise the
// __mo_tmp_<32hex>_ shape rule that matches the fallback servers' own SHOW
// TABLES semantics.
func hiddenHelpersFor(legacyClassify bool) string {
	base := " and relname not in (select i.index_table_name from mo_catalog.mo_indexes i" +
		" join mo_catalog.mo_database d on i.database_id = d.dat_id" +
		" where d.datname = ?" +
		" and (d.account_id = current_account_id()" +
		" or (d.account_id = 0 and d.datname = 'mo_catalog'))" +
		" and i.index_table_name is not null and i.index_table_name <> '')" +
		" and relkind <> 'partition'" +
		" and relkind <> 'temporary_table'"
	if legacyClassify {
		return base + " and not mo_is_legacy_temporary_table(relkind, relname, reldatabase, coalesce(rel_createsql, ''), coalesce(extra_info, ''))"
	}
	// No classifier on v4.1.x and older clusters: the __mo_tmp_<32hex>_ rows
	// are not filtered by the query — getTables inspects them and aborts the
	// dump unless the user selected them explicitly with -tbl.
	return base
}

func TestGetDatabaseTypeScopedQuery(t *testing.T) {
	const (
		defaultSQL = "select datname, dat_type from mo_catalog.mo_database where datname = ?" + dbScopePredicate
		sysSQL     = "select datname, dat_type from mo_catalog.mo_database where datname = ? and account_id = 0"
	)

	t.Run("default scopes the query to the connected account", func(t *testing.T) {
		mock := newCatalogMock(t)
		mock.ExpectQuery(defaultSQL).WithArgs("d").
			WillReturnRows(sqlmock.NewRows([]string{"datname", "dat_type"}).AddRow("d", ""))
		got, err := getDatabaseType(context.Background(), "d", false)
		require.NoError(t, err)
		assert.Equal(t, Db{Name: "d", DBType: ""}, got)
		require.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("default keeps mo_catalog visible for every account", func(t *testing.T) {
		mock := newCatalogMock(t)
		mock.ExpectQuery("select datname, dat_type from mo_catalog.mo_database where datname = ?" + dbScopePredicate).WithArgs("mo_catalog").
			WillReturnRows(sqlmock.NewRows([]string{"datname", "dat_type"}).AddRow("mo_catalog", ""))
		got, err := getDatabaseType(context.Background(), "mo_catalog", false)
		require.NoError(t, err)
		assert.Equal(t, Db{Name: "mo_catalog"}, got)
		require.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("no row reports database not exists", func(t *testing.T) {
		mock := newCatalogMock(t)
		mock.ExpectQuery(defaultSQL).WithArgs("d").WillReturnRows(sqlmock.NewRows([]string{"datname", "dat_type"}))
		_, err := getDatabaseType(context.Background(), "d", false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "database d not exists")
		require.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("sys account dumps only the sys account", func(t *testing.T) {
		mock := newCatalogMock(t)
		mock.ExpectQuery(sysSQL).WithArgs("d").
			WillReturnRows(sqlmock.NewRows([]string{"datname", "dat_type"}).AddRow("d", ""))
		got, err := getDatabaseType(context.Background(), "d", true)
		require.NoError(t, err)
		assert.Equal(t, Db{Name: "d"}, got)
		require.NoError(t, mock.ExpectationsWereMet())
	})
}

func TestGetTablesScopedQuery(t *testing.T) {
	baseSQL := "select relname,relkind from mo_catalog.mo_tables where reldatabase = ?"

	// The mocked row set models what the server returns after applying the
	// emitted predicates: hidden helpers are filtered away by the query
	// itself, so user tables named __mo_user_data and %!%plain survive, and
	// same-named helper objects in ordinary databases survive too.
	defaultSQL := baseSQL + tablesScopePredicate + hiddenHelpersFor(false)
	sysSQL := baseSQL + " and account_id = 0" + hiddenHelpersFor(false)

	t.Run("default returns user __mo_ tables instead of dropping them (#30562)", func(t *testing.T) {
		mock := newCatalogMock(t)
		rows := sqlmock.NewRows([]string{"relname", "relkind"}).
			AddRow("__mo_user_data", "r").
			AddRow("plain", "r").
			AddRow("v", "v").
			AddRow("%!%plain", "r").
			// Review regression #R1: '_' is a LIKE wildcard; demo_index_data is
			// an ordinary table that a not-like '__mo_index_%' pattern matched.
			AddRow("demo_index_data", "r").
			// Review regression #R5: a user table legally named with the
			// literal index-table prefix is not registered in mo_indexes, so it
			// is dumped whatever kind its row carries ('r' on first creation,
			// '' once the server blanks the kind after rename/restore).
			AddRow("__mo_index_renamed", "")
		mock.ExpectQuery(defaultSQL).WithArgs("d", "d").WillReturnRows(rows)

		tables, err := getTables(context.Background(), "d", nil, Options{sysAccount: false})
		require.NoError(t, err)
		assert.Equal(t, Tables{
			{Name: "__mo_user_data", Kind: "r"},
			{Name: "plain", Kind: "r"},
			{Name: "v", Kind: "v"},
			{Name: "%!%plain", Kind: "r"},
			{Name: "demo_index_data", Kind: "r"},
			{Name: "__mo_index_renamed", Kind: ""},
		}, tables)
		require.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("sys account keeps account_id = 0 and the hidden-helper predicates", func(t *testing.T) {
		mock := newCatalogMock(t)
		// Rows as returned by the server after the emitted predicates: no
		// relkind='i' index-table row survives the query itself.
		rows := sqlmock.NewRows([]string{"relname", "relkind"}).
			AddRow("mo_user", "r").
			AddRow("__mo_user_data", "r")
		mock.ExpectQuery(sysSQL).WithArgs("d", "d").WillReturnRows(rows)

		tables, err := getTables(context.Background(), "d", nil, Options{sysAccount: true})
		require.NoError(t, err)
		assert.Equal(t, Tables{
			{Name: "mo_user", Kind: "r"},
			{Name: "__mo_user_data", Kind: "r"},
		}, tables)
		require.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("ordinary databases keep same-named helper objects (review R2)", func(t *testing.T) {
		mock := newCatalogMock(t)
		// Same-named user table and view in an ordinary database: the
		// mo_catalog-only helper exclusion must not apply to them.
		rows := sqlmock.NewRows([]string{"relname", "relkind"}).
			AddRow("mo_increment_columns", "r").
			AddRow("mo_account_lock", "v").
			AddRow("__mo_account_lock", "v")
		mock.ExpectQuery(defaultSQL).WithArgs("d", "d").WillReturnRows(rows) // db "d": no helper-name predicates

		tables, err := getTables(context.Background(), "d", nil, Options{sysAccount: false})
		require.NoError(t, err)
		assert.Equal(t, Tables{
			{Name: "mo_increment_columns", Kind: "r"},
			{Name: "mo_account_lock", Kind: "v"},
			{Name: "__mo_account_lock", Kind: "v"},
		}, tables)
		require.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("mo_catalog dump still excludes its own helper tables", func(t *testing.T) {
		mock := newCatalogMock(t)
		catalogSQL := baseSQL + tablesScopePredicate + hiddenHelpersFor(false) + catalogHelpersPredicate
		// The pre-filtered result the server returns for mo_catalog itself.
		rows := sqlmock.NewRows([]string{"relname", "relkind"}).
			AddRow("mo_database", "r").
			AddRow("mo_tables", "r").
			AddRow("mo_columns", "r")
		mock.ExpectQuery(catalogSQL).WithArgs("mo_catalog", "mo_catalog").WillReturnRows(rows)

		tables, err := getTables(context.Background(), "mo_catalog", nil, Options{sysAccount: false})
		require.NoError(t, err)
		assert.Equal(t, Tables{
			{Name: "mo_database", Kind: "r"},
			{Name: "mo_tables", Kind: "r"},
			{Name: "mo_columns", Kind: "r"},
		}, tables)
		require.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("explicit -tbl accepts a user __mo_ table (#30562)", func(t *testing.T) {
		mock := newCatalogMock(t)
		sqlWithTbl := defaultSQL + " and relname in (?,?)"
		rows := sqlmock.NewRows([]string{"relname", "relkind"}).
			AddRow("__mo_user_data", "r").
			AddRow("plain", "r")
		mock.ExpectQuery(sqlWithTbl).WithArgs("d", "d", "__mo_user_data", "plain").WillReturnRows(rows)

		tables, err := getTables(context.Background(), "d", Tables{{Name: "__mo_user_data"}, {Name: "plain"}}, Options{})
		require.NoError(t, err)
		assert.Equal(t, Tables{
			{Name: "__mo_user_data", Kind: "r"},
			{Name: "plain", Kind: "r"},
		}, tables)
		require.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("explicit -tbl missing table reports not exists", func(t *testing.T) {
		mock := newCatalogMock(t)
		sqlWithTbl := defaultSQL + " and relname in (?)"
		// The hidden-table predicates filter nothing a user could request
		// explicitly; an empty result means the object does not exist.
		mock.ExpectQuery(sqlWithTbl).WithArgs("d", "d", "__mo_user_data").WillReturnRows(sqlmock.NewRows([]string{"relname", "relkind"}))

		_, err := getTables(context.Background(), "d", Tables{{Name: "__mo_user_data"}}, Options{sysAccount: false})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "table __mo_user_data not exists")
		require.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("fallback aborts on ambiguous tmp-shaped rows; explicit -tbl exports them (round 10)", func(t *testing.T) {
		const shaped = "__mo_tmp_0123456789abcdef0123456789abcdef_user"
		t.Run("default dump aborts", func(t *testing.T) {
			mock := newCatalogMock(t)
			rows := sqlmock.NewRows([]string{"relname", "relkind"}).
				AddRow("plain", "r").
				AddRow(shaped, "r")
			mock.ExpectQuery(defaultSQL).WithArgs("d", "d").WillReturnRows(rows)
			_, err := getTables(context.Background(), "d", nil, Options{})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "has the legacy temporary name shape")
			require.NoError(t, mock.ExpectationsWereMet())
		})
		t.Run("explicit -tbl selection exports the row", func(t *testing.T) {
			mock := newCatalogMock(t)
			rows := sqlmock.NewRows([]string{"relname", "relkind"}).
				AddRow(shaped, "r")
			mock.ExpectQuery(defaultSQL+" and relname in (?)").WithArgs("d", "d", shaped).WillReturnRows(rows)
			got, err := getTables(context.Background(), "d", Tables{{Name: shaped}}, Options{})
			require.NoError(t, err)
			assert.Equal(t, Tables{{Name: shaped, Kind: "r"}}, got)
			require.NoError(t, mock.ExpectationsWereMet())
		})
		t.Run("empty-kind shape row without explicit selection aborts too", func(t *testing.T) {
			mock := newCatalogMock(t)
			rows := sqlmock.NewRows([]string{"relname", "relkind"}).
				AddRow("__mo_tmp_0123456789abcdef0123456789abcdef_d_idx", "")
			mock.ExpectQuery(sysSQL).WithArgs("d", "d").WillReturnRows(rows)
			_, err := getTables(context.Background(), "d", nil, Options{sysAccount: true})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "has the legacy temporary name shape")
			require.NoError(t, mock.ExpectationsWereMet())
		})
	})

	const shaped = "__mo_tmp_0123456789abcdef0123456789abcdef_user"
	const other = "__mo_tmp_0123456789abcdef0123456789abcdef_a,b`c"
	for _, tc := range []struct {
		name      string
		confirmed Tables
		rows      Tables
		isSys     bool
		wantError string
	}{
		{"confirmation keeps ordinary tables", Tables{{Name: shaped}}, Tables{{Name: "plain", Kind: "r"}, {Name: shaped, Kind: "r"}}, false, ""},
		{"empty kind and repeated exact names", Tables{{Name: shaped}, {Name: shaped}, {Name: other}}, Tables{{Name: shaped}, {Name: other, Kind: "r"}}, false, ""},
		{"sys scope unchanged", Tables{{Name: shaped}}, Tables{{Name: shaped, Kind: "r"}}, true, ""},
		{"unconfirmed ambiguous row", Tables{{Name: shaped}}, Tables{{Name: shaped, Kind: "r"}, {Name: other, Kind: "r"}}, false, "has the legacy temporary name shape"},
		{"missing or filtered target", Tables{{Name: shaped}}, Tables{{Name: "plain", Kind: "r"}}, false, "confirmed permanent table"},
		{"ordinary name needs no confirmation", Tables{{Name: "plain"}}, Tables{{Name: "plain", Kind: "r"}}, false, "confirmed permanent table"},
		{"view is not an ambiguous table", Tables{{Name: shaped}}, Tables{{Name: shaped, Kind: "v"}}, false, "confirmed permanent table"},
		{"temporary kind cannot be confirmed", Tables{{Name: shaped}}, Tables{{Name: shaped, Kind: "temporary_table"}}, false, "confirmed permanent table"},
		{"confirmation is not a glob", Tables{{Name: shaped + "*"}}, Tables{{Name: shaped, Kind: "r"}}, false, "has the legacy temporary name shape"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock := newCatalogMock(t)
			query := defaultSQL
			if tc.isSys {
				query = sysSQL
			}
			rows := sqlmock.NewRows([]string{"relname", "relkind"})
			for _, table := range tc.rows {
				rows.AddRow(table.Name, table.Kind)
			}
			mock.ExpectQuery(query).WithArgs("d", "d").WillReturnRows(rows).RowsWillBeClosed()
			got, err := getTables(context.Background(), "d", nil, Options{confirmedPermanentTables: tc.confirmed, sysAccount: tc.isSys})
			if tc.wantError != "" {
				require.ErrorContains(t, err, tc.wantError)
				require.Nil(t, got)
			} else {
				require.NoError(t, err)
				require.Equal(t, tc.rows, got)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}

	for _, failure := range []string{"query", "scan", "iteration"} {
		t.Run("confirmation propagates "+failure+" error", func(t *testing.T) {
			mock := newCatalogMock(t)
			expect := mock.ExpectQuery(defaultSQL).WithArgs("d", "d")
			switch failure {
			case "query":
				expect.WillReturnError(context.Canceled)
			case "scan":
				expect.WillReturnRows(sqlmock.NewRows([]string{"relname", "relkind"}).AddRow(shaped, nil)).RowsWillBeClosed()
			case "iteration":
				expect.WillReturnRows(sqlmock.NewRows([]string{"relname", "relkind"}).
					AddRow(shaped, "r").AddRow("plain", "r").RowError(1, context.Canceled)).RowsWillBeClosed()
			}
			got, err := getTables(context.Background(), "d", nil, Options{confirmedPermanentTables: Tables{{Name: shaped}, {Name: other}}})
			require.Error(t, err)
			require.Nil(t, got)
			if failure != "scan" {
				require.ErrorIs(t, err, context.Canceled, "read errors must precede unmatched-confirmation errors")
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}

	t.Run("confirmation consumption is local to each read", func(t *testing.T) {
		mock := newCatalogMock(t)
		opt := Options{confirmedPermanentTables: Tables{{Name: shaped}}}
		mock.ExpectQuery(defaultSQL).WithArgs("d", "d").
			WillReturnRows(sqlmock.NewRows([]string{"relname", "relkind"}).AddRow(shaped, "r"))
		_, err := getTables(context.Background(), "d", nil, opt)
		require.NoError(t, err)
		mock.ExpectQuery(defaultSQL).WithArgs("d", "d").
			WillReturnRows(sqlmock.NewRows([]string{"relname", "relkind"}))
		_, err = getTables(context.Background(), "d", nil, opt)
		require.ErrorContains(t, err, "confirmed permanent table")
		require.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("query error is propagated", func(t *testing.T) {
		mock := newCatalogMock(t)
		mock.ExpectQuery(defaultSQL).WithArgs("d", "d").WillReturnError(sql.ErrConnDone)
		_, err := getTables(context.Background(), "d", nil, Options{sysAccount: false})
		require.ErrorIs(t, err, sql.ErrConnDone)
		require.NoError(t, mock.ExpectationsWereMet())
	})
}
