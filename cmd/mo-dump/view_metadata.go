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
	"fmt"
	"strings"
)

func (definition *viewDefinition) parserSQLMode() string {
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
	definitions, err := getViewDefinitions(ctx, database, Tables{{Name: name}}, isSys)
	if err != nil {
		return dumpView{}, err
	}
	definition := definitions[name]
	return dumpView{name: viewName{database, name}, create: definition.Stmt, definition: &definition}, nil
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

func showDumpView(out *dumpSink, view dumpView, mode viewSQLMode) error {
	analysis, err := analyzeDumpView(context.Background(), view, mode)
	if err != nil {
		return err
	}
	if view.definition != nil {
		out.printf("SET @modump_saved_sql_mode = @@SESSION.sql_mode;\n")
		// Remove only parser flags from the target; preserve its non-parser policy.
		// All literals in this expression are ASCII without backslash escapes.
		target := "CONCAT(',', @modump_saved_sql_mode, ',')"
		for _, flag := range strings.Split("ANSI,"+viewParserModeFlags, ",") {
			target = fmt.Sprintf("REPLACE(%s, ',%s,', ',')", target, flag)
		}
		flags := viewParserMode(view.definition.parserSQLMode())
		out.printf("SET SESSION sql_mode = CONCAT_WS(',', NULLIF(TRIM(BOTH ',' FROM %s), ''), NULLIF(X'%x', ''));\n", target, flags)
	}
	// An unused saved default is not a prerequisite. Select the owner even
	// for legacy empty defaults, so unqualified CREATE targets remain stable.
	database := view.name.database
	if analysis.usesDefaultDatabase {
		database = view.defaultDatabase()
	}
	if database != "" {
		out.printf("USE %s;\n\n", quoteIdentifier(database))
	}
	out.printf("DROP VIEW IF EXISTS %s.%s;\n", quoteIdentifier(view.name.database), quoteIdentifier(view.name.name))
	showCreateTable(out, analysis.create, true)
	if view.definition != nil {
		out.printf("SET SESSION sql_mode = @modump_saved_sql_mode;\n")
	}
	return nil
}
