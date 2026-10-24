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
	"errors"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// errLegacyProbe satisfies error for the absent-classifier probe: the
// message mirrors the server's own unknown-function answer so the
// production classifier accepts it as a legitimate downgrade signal.
type errLegacyProbe struct{}

func (errLegacyProbe) Error() string {
	return "Error 20105 (HY000): not supported: function or operator 'mo_is_legacy_temporary_table'"
}

// expectLegacyClassifyProbe arms the one-shot capability probe issued by
// dumpData with the given outcome. Most tests use expectLegacyProbeAbsent so
// their catalog expectations keep the shape fallback predicate.
const legacyProbeSQL = "select mo_is_legacy_temporary_table('r','x','d','', '')"

func expectLegacyProbeAbsent(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(legacyProbeSQL).WillReturnError(errLegacyProbe{})
}

func expectLegacyProbePresent(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(legacyProbeSQL).
		WillReturnRows(sqlmock.NewRows([]string{"mo_is_legacy_temporary_table"}).AddRow(true))
}

// errNoLegacyClassifier is the sentinel returned by the probe on clusters
// without the server-side classifier; every dumpData-based test that arms the
// absent branch keeps its expectations on the shape fallback predicate.

// TestProbeLegacyTemporaryTableFn pins the review round-10 contract of the
// one-shot capability probe:
//   - the server's own unknown-function answer means the classifier is absent
//     and the dump legitimately runs in fallback mode;
//   - any other error is not proof of absence — the probe fails the dump
//     instead of silently downgrading a functioning cluster.
func TestProbeLegacyTemporaryTableFn(t *testing.T) {
	t.Run("function present", func(t *testing.T) {
		_, mock := exportTestDB(t)
		expectLegacyProbePresent(mock)
		ok, err := probeLegacyTemporaryTableFn(context.Background())
		require.NoError(t, err)
		assert.True(t, ok)
		require.NoError(t, mock.ExpectationsWereMet())
	})
	t.Run("unknown function downgrades to fallback", func(t *testing.T) {
		_, mock := exportTestDB(t)
		expectLegacyProbeAbsent(mock)
		ok, err := probeLegacyTemporaryTableFn(context.Background())
		require.NoError(t, err)
		assert.False(t, ok)
		require.NoError(t, mock.ExpectationsWereMet())
	})
	t.Run("injected transient error aborts the dump", func(t *testing.T) {
		_, mock := exportTestDB(t)
		mock.ExpectQuery(legacyProbeSQL).WillReturnError(errors.New("connection reset by peer"))
		_, err := probeLegacyTemporaryTableFn(context.Background())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cannot probe mo_is_legacy_temporary_table")
		require.NoError(t, mock.ExpectationsWereMet())
	})
	t.Run("row scan error aborts the dump", func(t *testing.T) {
		_, mock := exportTestDB(t)
		mock.ExpectQuery(legacyProbeSQL).
			WillReturnRows(sqlmock.NewRows([]string{"mo_is_legacy_temporary_table"}).AddRow(true).RowError(0, errors.New("read failed")))
		_, err := probeLegacyTemporaryTableFn(context.Background())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cannot probe mo_is_legacy_temporary_table")
		require.NoError(t, mock.ExpectationsWereMet())
	})
}

// TestIsUnknownFunctionErr pins the error-classification rule itself.
func TestIsUnknownFunctionErr(t *testing.T) {
	// The resolved form every supported release produces (moerr.NotSupported,
	// verified live: "ERROR 20105 (HY000): not supported: function or operator ...").
	assert.True(t, isUnknownFunctionErr(&mysql.MySQLError{Number: 20105, Message: "not supported: function or operator 'mo_is_legacy_temporary_table'"}))
	assert.True(t, isUnknownFunctionErr(errors.New("not supported: function or operator 'mo_is_legacy_temporary_table'")))
	// Stock-MySQL style answer kept for compatibility.
	assert.True(t, isUnknownFunctionErr(&mysql.MySQLError{Number: 1305, Message: "FUNCTION mo.x does not exist"}))
	// Not proofs of absence: transient faults, other server errors, nil.
	assert.False(t, isUnknownFunctionErr(errors.New("connection reset by peer")))
	assert.False(t, isUnknownFunctionErr(&mysql.MySQLError{Number: 1045, Message: "Access denied"}))
	assert.False(t, isUnknownFunctionErr(&mysql.MySQLError{Number: 20105, Message: "not supported: some other object"}))
	assert.False(t, isUnknownFunctionErr(nil))
}
