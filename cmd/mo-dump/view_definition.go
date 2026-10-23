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
	"encoding/json"
	"fmt"
	"strings"
)

// This is the persisted ViewData envelope, not the exporting connection's
// context. Pointers distinguish absent legacy fields from explicit empty/zero.
// Only fields used to interpret the original statement are decoded.
type viewDefinition struct {
	Stmt                string
	DefaultDatabase     string
	SQLMode             *string `json:"sql_mode"`
	LowerCaseTableNames *int64  `json:"lower_case_table_names"`
}

func (d viewDefinition) parserMode(catalogMode viewSQLMode) (viewSQLMode, error) {
	// Match buildView's legacyViewParserSQLMode, not the caller's sql_mode.
	mode := viewSQLMode{lowerCaseTableNames: catalogMode.lowerCaseTableNames, sqlMode: "PIPES_AS_CONCAT"}
	if d.SQLMode != nil {
		mode.sqlMode = *d.SQLMode
	}
	if d.LowerCaseTableNames != nil {
		mode.lowerCaseTableNames = *d.LowerCaseTableNames
	}
	if mode.lowerCaseTableNames < 0 || mode.lowerCaseTableNames > 2 {
		return mode, fmt.Errorf("unsupported persisted lower_case_table_names: %d", mode.lowerCaseTableNames)
	}
	return mode, nil
}

// Bound prepared-statement parameters and avoid fetching unselected view DDLs.
const viewDefinitionBatchSize = 512

func getViewDefinitions(ctx context.Context, database string, tables Tables, systemAccount bool) (map[string]viewDefinition, error) {
	definitions := make(map[string]viewDefinition, len(tables))
	for start := 0; start < len(tables); start += viewDefinitionBatchSize {
		end := min(start+viewDefinitionBatchSize, len(tables))
		if err := readViewDefinitionBatch(ctx, database, tables[start:end], systemAccount, definitions); err != nil {
			return nil, fmt.Errorf("cannot read persisted view definitions in database %s: %w", database, err)
		}
	}
	for _, table := range tables {
		if _, ok := definitions[table.Name]; !ok {
			return nil, fmt.Errorf("missing persisted definition for view %s.%s", database, table.Name)
		}
	}
	return definitions, nil
}

func readViewDefinitionBatch(ctx context.Context, database string, tables Tables, systemAccount bool, definitions map[string]viewDefinition) (err error) {
	query := "select relname,viewdef from mo_catalog.mo_tables where reldatabase = ? and relkind = 'v'"
	if systemAccount {
		query += " and account_id = 0"
	}
	query += " and relname in (" + strings.TrimSuffix(strings.Repeat("?,", len(tables)), ",") + ")"
	args := make([]any, 0, len(tables)+1)
	args = append(args, database)
	for _, table := range tables {
		args = append(args, table.Name)
	}
	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := rows.Close(); err == nil {
			err = closeErr
		}
	}()
	for rows.Next() {
		var name, payload string
		if err := rows.Scan(&name, &payload); err != nil {
			return err
		}
		var definition viewDefinition
		if err := json.Unmarshal([]byte(payload), &definition); err != nil {
			return fmt.Errorf("invalid persisted definition for view %s.%s: %w", database, name, err)
		}
		if strings.TrimSpace(definition.Stmt) == "" {
			return fmt.Errorf("empty persisted statement for view %s.%s", database, name)
		}
		if _, err := definition.parserMode(viewSQLMode{}); err != nil {
			return fmt.Errorf("invalid persisted definition for view %s.%s: %w", database, name, err)
		}
		if _, exists := definitions[name]; exists {
			return fmt.Errorf("duplicate persisted definition for view %s.%s", database, name)
		}
		definitions[name] = definition
	}
	return rows.Err()
}
