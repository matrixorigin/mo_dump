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
	"encoding/json"
	"fmt"
	"strings"
)

// This subset is wire-compatible with legacy and current ViewData. Nil SQLMode
// uses the legacy parser fallback; nil LowerCaseTableNames inherits the caller.
// Explicitly persisted empty SQLMode and zero name mode must remain distinct.
type viewDefinition struct {
	Stmt                string
	SQLMode             *string `json:"sql_mode"`
	LowerCaseTableNames *int64  `json:"lower_case_table_names"`
}

func (definition *viewDefinition) parserMode() string {
	if definition.SQLMode == nil {
		// Match the server's legacy-view parser fallback.
		return "PIPES_AS_CONCAT"
	}
	return *definition.SQLMode
}

// Read the statement and its parser environment together. SHOW's SQL-string
// rewriting can lose escapes from definitions created in a different mode,
// and the pinned dependency's ViewData lacks newer fields.
func getDumpView(ctx context.Context, database, name string, isSys bool) (dumpView, error) {
	query := "select viewdef from mo_catalog.mo_tables where reldatabase = ? and relname = ? and relkind = 'v'"
	if isSys {
		query += " and account_id = 0"
	}
	var encoded string
	if err := conn.QueryRowContext(ctx, query, database, name).Scan(&encoded); err != nil {
		return dumpView{}, fmt.Errorf("read view definition %s.%s: %w", database, name, err)
	}
	var definition *viewDefinition
	if err := json.Unmarshal([]byte(encoded), &definition); err != nil {
		return dumpView{}, fmt.Errorf("decode view definition %s.%s: %w", database, name, err)
	}
	if definition == nil || strings.TrimSpace(definition.Stmt) == "" {
		return dumpView{}, fmt.Errorf("view definition %s.%s has no statement", database, name)
	}
	if lower := definition.LowerCaseTableNames; lower != nil && (*lower < 0 || *lower > 2) {
		return dumpView{}, fmt.Errorf("view definition %s.%s has unsupported lower_case_table_names: %d", database, name, *lower)
	}
	return dumpView{name: viewName{database, name}, create: definition.Stmt, definition: definition}, nil
}

// These are the flags interpreted by the server's ParseSQLModeFlags. The view
// loader uses persisted flags for parsing, while non-parser binding policies
// (for example ENABLE_BOOL_SUMAVG) still come from the caller's session.
const viewParserModeFlags = "ANSI_QUOTES,PIPES_AS_CONCAT,NO_BACKSLASH_ESCAPES,REAL_AS_FLOAT,HIGH_NOT_PRECEDENCE,IGNORE_SPACE"

func viewParserMode(mode string) string {
	enabled := make(map[string]bool)
	for _, option := range strings.Split(mode, ",") {
		option = strings.ToUpper(strings.TrimSpace(option))
		if option == "ANSI" {
			for _, flag := range []string{"ANSI_QUOTES", "PIPES_AS_CONCAT", "REAL_AS_FLOAT", "IGNORE_SPACE"} {
				enabled[flag] = true
			}
		} else {
			enabled[option] = true
		}
	}
	var flags []string
	for _, flag := range strings.Split(viewParserModeFlags, ",") {
		if enabled[flag] {
			flags = append(flags, flag)
		}
	}
	return strings.Join(flags, ",")
}

func showDumpView(view dumpView) {
	if view.definition != nil {
		fmt.Println("SET @modump_saved_sql_mode = @@SESSION.sql_mode;")
		// Remove only parser flags from the target; preserve its non-parser policy.
		// All literals in this expression are ASCII without backslash escapes.
		target := "CONCAT(',', @modump_saved_sql_mode, ',')"
		for _, flag := range strings.Split("ANSI,"+viewParserModeFlags, ",") {
			target = fmt.Sprintf("REPLACE(%s, ',%s,', ',')", target, flag)
		}
		flags := viewParserMode(view.definition.parserMode())
		fmt.Printf("SET SESSION sql_mode = CONCAT_WS(',', NULLIF(TRIM(BOTH ',' FROM %s), ''), NULLIF(X'%x', ''));\n", target, flags)
	}
	fmt.Printf("USE %s;\n\n", quoteIdentifier(view.name.database))
	fmt.Printf("DROP VIEW IF EXISTS %s;\n", quoteIdentifier(view.name.name))
	showCreateTable(view.create, true)
	if view.definition != nil {
		fmt.Println("SET SESSION sql_mode = @modump_saved_sql_mode;")
	}
}
