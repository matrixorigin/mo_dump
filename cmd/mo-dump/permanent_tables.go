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

	"github.com/matrixorigin/matrixone/pkg/common/moerr"
)

// validatePermanentTableOptions bounds the owner's confirmation to one
// explicit database. It must run before expanding -db all or opening a connection.
func (opt Options) validatePermanentTableOptions(ctx context.Context) error {
	if len(opt.confirmedPermanentTables) == 0 {
		return nil
	}
	if len(opt.dbs) != 1 || opt.dbs[0] == "all" {
		return moerr.NewInvalidInput(ctx, "-confirm-permanent-table requires one explicit database; -db all and multiple databases are not supported")
	}
	if opt.tbl != "" || len(opt.tables) != 0 {
		return moerr.NewInvalidInput(ctx, "-confirm-permanent-table is for whole-database dumps and cannot be combined with -tbl")
	}
	for _, table := range opt.confirmedPermanentTables {
		if table.Name == "" {
			return moerr.NewInvalidInput(ctx, "-confirm-permanent-table requires a non-empty table name")
		}
	}
	return nil
}
