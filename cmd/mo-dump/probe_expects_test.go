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
	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

// errLegacyProbeError satisfies error for the absent-classifier probe.
type errLegacyProbe struct{}

func (errLegacyProbe) Error() string { return "mo_is_legacy_temporary_table: function does not exist" }

// expectLegacyClassifyProbe arms the one-shot capability probe issued by
// dumpData with the given outcome. Most tests use expectLegacyProbeAbsent so
// their catalog expectations keep the shape fallback predicate.
const legacyProbeSQL = "select mo_is_legacy_temporary_table('r','x','d','', '')"

func expectLegacyProbeAbsent(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(legacyProbeSQL).WillReturnError(errNoLegacyClassifier)
}

func expectLegacyProbePresent(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(legacyProbeSQL).
		WillReturnRows(sqlmock.NewRows([]string{"mo_is_legacy_temporary_table"}).AddRow(true))
}

// errNoLegacyClassifier is the sentinel returned by the probe on clusters
// without the server-side classifier; every dumpData-based test that arms the
// absent branch keeps its expectations on the shape fallback predicate.
var errNoLegacyClassifier = errLegacyProbe{}
