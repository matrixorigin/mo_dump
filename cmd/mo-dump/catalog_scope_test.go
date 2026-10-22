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

	hiddenTablesPredicate = " and relname != 'mo_increment_columns'" +
		" and relname not like '__mo_index_%'" +
		" and relname != '__mo_account_lock'" +
		" and relkind != 'partition'" +
		" and relkind != 'temporary_table'" +
		" and not regexp_like(relname, '^__mo_tmp_[0-9a-f]{32}_')"
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
	// itself, so user tables named __mo_user_data and %!%plain survive.
	defaultSQL := baseSQL("d") + tablesScopePredicate + hiddenTablesPredicate
	sysSQL := baseSQL("d") + " and account_id = 0" + hiddenTablesPredicate

	t.Run("default returns user __mo_ tables instead of dropping them (#30562)", func(t *testing.T) {
		mock := newCatalogMock(t)
		rows := sqlmock.NewRows([]string{"relname", "relkind"}).
			AddRow("__mo_user_data", "r").
			AddRow("plain", "r").
			AddRow("v", "v").
			AddRow("%!%plain", "r")
		mock.ExpectQuery(defaultSQL).WillReturnRows(rows)

		tables, err := getTables(context.Background(), "d", nil, false)
		require.NoError(t, err)
		assert.Equal(t, Tables{
			{Name: "__mo_user_data", Kind: "r"},
			{Name: "plain", Kind: "r"},
			{Name: "v", Kind: "v"},
			{Name: "%!%plain", Kind: "r"},
		}, tables)
		require.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("sys account keeps account_id = 0 and the hidden-table predicates", func(t *testing.T) {
		mock := newCatalogMock(t)
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
