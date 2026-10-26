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

// Package catalog contains only the stable mo_catalog SQL wire values consumed
// by this client. Importing the server's catalog implementation for these values
// would unnecessarily link the storage engine and its native libraries.
package catalog

const (
	SystemOrdinaryRel        = "r"
	SystemExternalRel        = "e"
	SystemViewRel            = "v"
	SystemDBTypeSubscription = "subscription"

	// MO_CATALOG is the durable name of the system catalog database.
	MO_CATALOG = "mo_catalog"
	// MOAutoIncrTable is the auto-increment helper table of mo_catalog.
	MOAutoIncrTable = "mo_increment_columns"
)
