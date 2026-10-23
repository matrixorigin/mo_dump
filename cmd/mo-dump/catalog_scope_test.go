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

	// Hidden helpers are excluded by a compound predicate: the literal
	// __mo_index_ name prefix (underscores escaped, '_' is a single-character
	// LIKE wildcard that also swallowed demo_index_data) combined with a
	// dumpable-relkind gate (r/v/e). A bare kind predicate cannot cover all
	// hidden kinds: unique/secondary are 'i' since MO v3.0 and empty before,
	// fulltext/vector helpers carry per-role kinds (fulltext, metadata,
	// centroids, entries, ...). User objects legally named __mo_index_* with a
	// dumpable relkind stay dump payloads.
	hiddenHelpersPredicate = " and not (relname like '\\_\\_mo\\_index\\_%' and relkind not in ('r','v','e'))" +
		" and relkind <> 'partition'" +
		" and relkind <> 'temporary_table'" +
		" and not regexp_like(relname, '^__mo_tmp_[0-9a-f]{32}_')"

	catalogHelpersPredicate = " and relname != 'mo_increment_columns'" +
		" and relname != '__mo_account_lock'"
)

func TestGetDatabaseTypeScopedQuery(t *testing.T) {
	const (
		defaultSQL = "select datname, dat_type from mo_catalog.mo_database where datname = 'd'" + dbScopePredicate
		sysSQL     = "select datname, dat_type from mo_catalog.mo_database where datname = 'd' and account_id = 0"
	)

	t.Run("default scopes the query to the connected account", func(t *testing.T) {
		mock := newCatalogMock(t)
		mock.ExpectQuery(defaultSQL).
			WillReturnRows(sqlmock.NewRows([]string{"datname", "dat_type"}).AddRow("d", ""))
		got, err := getDatabaseType(context.Background(), "d", false)
		require.NoError(t, err)
		assert.Equal(t, Db{Name: "d", DBType: ""}, got)
		require.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("default keeps mo_catalog visible for every account", func(t *testing.T) {
		mock := newCatalogMock(t)
		mock.ExpectQuery("select datname, dat_type from mo_catalog.mo_database where datname = 'mo_catalog'" + dbScopePredicate).
			WillReturnRows(sqlmock.NewRows([]string{"datname", "dat_type"}).AddRow("mo_catalog", ""))
		got, err := getDatabaseType(context.Background(), "mo_catalog", false)
		require.NoError(t, err)
		assert.Equal(t, Db{Name: "mo_catalog"}, got)
		require.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("no row reports database not exists", func(t *testing.T) {
		mock := newCatalogMock(t)
		mock.ExpectQuery(defaultSQL).WillReturnRows(sqlmock.NewRows([]string{"datname", "dat_type"}))
		_, err := getDatabaseType(context.Background(), "d", false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "database d not exists")
		require.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("sys account dumps only the sys account", func(t *testing.T) {
		mock := newCatalogMock(t)
		mock.ExpectQuery(sysSQL).
			WillReturnRows(sqlmock.NewRows([]string{"datname", "dat_type"}).AddRow("d", ""))
		got, err := getDatabaseType(context.Background(), "d", true)
		require.NoError(t, err)
		assert.Equal(t, Db{Name: "d"}, got)
		require.NoError(t, mock.ExpectationsWereMet())
	})
}

func TestGetTablesScopedQuery(t *testing.T) {
	baseSQL := func(db string) string {
		return "select relname,relkind from mo_catalog.mo_tables where reldatabase = '" + db + "'"
	}

	// The mocked row set models what the server returns after applying the
	// emitted predicates: hidden helpers are filtered away by the query
	// itself, so user tables named __mo_user_data and %!%plain survive, and
	// same-named helper objects in ordinary databases survive too.
	defaultSQL := baseSQL("d") + tablesScopePredicate + hiddenHelpersPredicate
	sysSQL := baseSQL("d") + " and account_id = 0" + hiddenHelpersPredicate

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
			// A user table legally named with the literal index-table prefix is
			// identified as ordinary by its relkind, not by its name.
			AddRow("__mo_index_data", "r")
		mock.ExpectQuery(defaultSQL).WillReturnRows(rows)

		tables, err := getTables(context.Background(), "d", nil, false)
		require.NoError(t, err)
		assert.Equal(t, Tables{
			{Name: "__mo_user_data", Kind: "r"},
			{Name: "plain", Kind: "r"},
			{Name: "v", Kind: "v"},
			{Name: "%!%plain", Kind: "r"},
			{Name: "demo_index_data", Kind: "r"},
			{Name: "__mo_index_data", Kind: "r"},
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
		mock.ExpectQuery(sysSQL).WillReturnRows(rows)

		tables, err := getTables(context.Background(), "d", nil, true)
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
		mock.ExpectQuery(defaultSQL).WillReturnRows(rows) // db "d": no helper-name predicates

		tables, err := getTables(context.Background(), "d", nil, false)
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
		catalogSQL := baseSQL("mo_catalog") + tablesScopePredicate + hiddenHelpersPredicate + catalogHelpersPredicate
		// The pre-filtered result the server returns for mo_catalog itself.
		rows := sqlmock.NewRows([]string{"relname", "relkind"}).
			AddRow("mo_database", "r").
			AddRow("mo_tables", "r").
			AddRow("mo_columns", "r")
		mock.ExpectQuery(catalogSQL).WillReturnRows(rows)

		tables, err := getTables(context.Background(), "mo_catalog", nil, false)
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
		sqlWithTbl := defaultSQL + " and relname in ('__mo_user_data','plain')"
		rows := sqlmock.NewRows([]string{"relname", "relkind"}).
			AddRow("__mo_user_data", "r").
			AddRow("plain", "r")
		mock.ExpectQuery(sqlWithTbl).WillReturnRows(rows)

		tables, err := getTables(context.Background(), "d", Tables{{Name: "__mo_user_data"}, {Name: "plain"}}, false)
		require.NoError(t, err)
		assert.Equal(t, Tables{
			{Name: "__mo_user_data", Kind: "r"},
			{Name: "plain", Kind: "r"},
		}, tables)
		require.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("explicit -tbl missing table reports not exists", func(t *testing.T) {
		mock := newCatalogMock(t)
		sqlWithTbl := defaultSQL + " and relname in ('__mo_user_data')"
		// The hidden-table predicates filter nothing a user could request
		// explicitly; an empty result means the object does not exist.
		mock.ExpectQuery(sqlWithTbl).WillReturnRows(sqlmock.NewRows([]string{"relname", "relkind"}))

		_, err := getTables(context.Background(), "d", Tables{{Name: "__mo_user_data"}}, false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "table __mo_user_data not exists")
		require.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("query error is propagated", func(t *testing.T) {
		mock := newCatalogMock(t)
		mock.ExpectQuery(defaultSQL).WillReturnError(sql.ErrConnDone)
		_, err := getTables(context.Background(), "d", nil, false)
		require.ErrorIs(t, err, sql.ErrConnDone)
		require.NoError(t, mock.ExpectationsWereMet())
	})
}
