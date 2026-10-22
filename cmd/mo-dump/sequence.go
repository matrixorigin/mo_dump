// Copyright 2026 Matrix Origin
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
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
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/matrixorigin/matrixone/pkg/common/moerr"
)

// sequenceStateQuery reads the one backing row every sequence relation
// keeps. The columns describe the definition that is effective at dump time
// (ALTER SEQUENCE rewrites them, so the original CREATE text is not a source
// of truth) plus the allocation state last_seq_num/is_called consumed by
// NEXTVAL (matrixorigin/matrixone#30577).
const sequenceStateQuery = "select last_seq_num, min_value, max_value, start_value, increment_value, cycle, is_called from "

// getCreateSequence builds the restore SQL for one user sequence.
//
// createSQL recreates the sequence with its current definition; stateSQL
// repeats last_seq_num/is_called so a restored sequence does not hand out
// values the source already consumed. -no-data callers may drop stateSQL,
// which makes the restored sequence continue from its START value.
func getCreateSequence(ctx context.Context, db, seq string) (createSQL string, stateSQL string, err error) {
	columnType, err := sequenceColumnType(ctx, db, seq)
	if err != nil {
		return "", "", err
	}

	state, err := readSequenceState(ctx, db, seq)
	if err != nil {
		return "", "", err
	}

	cycle, err := sequenceBool(ctx, db, seq, "cycle", state.cycle)
	if err != nil {
		return "", "", err
	}
	isCalled, err := sequenceBool(ctx, db, seq, "is_called", state.isCalled)
	if err != nil {
		return "", "", err
	}

	name := quoteIdentifier(seq)
	cycleWord := "NO CYCLE"
	if cycle {
		cycleWord = "CYCLE"
	}
	createSQL = fmt.Sprintf(
		"CREATE SEQUENCE %s AS %s INCREMENT BY %s MINVALUE %s MAXVALUE %s START WITH %s %s",
		name, columnType, state.increment, state.minimum, state.maximum, state.start, cycleWord,
	)
	// The statements are not database-qualified, matching the table DDL in
	// the same script: the whole-db form emits a USE before them, and a
	// -tbl restore is replayed inside the operator's chosen database.
	//
	// The server rejects direct UPDATE on the backing row
	// ("Cannot insert/update/delete from sequence"), so SETVAL is the
	// user-reachable state writer. On a freshly created sequence SETVAL
	// stores !thirdArg as the is_called flag, so the script passes the dumped
	// flag negated and the first NEXTVAL after restore continues exactly
	// where the source stopped (matrixorigin/matrixone#30577): a sequence
	// dumped as (last, called=true) restores by setval(last, false) and the
	// next NEXTVAL returns last+increment; (last, called=false) restores by
	// setval(last, true) and the next NEXTVAL returns last.
	stateSQL = fmt.Sprintf(
		"SELECT setval(%s, %s, %t)",
		quoteSQLLiteral(seq), state.lastValue, !isCalled,
	)
	return createSQL, stateSQL, nil
}

// sequenceColumnType returns the numeric type of the sequence, as written by
// SHOW SEQUENCES, by decoding the backing column type of last_seq_num from
// mo_catalog.mo_columns.
func sequenceColumnType(ctx context.Context, db, seq string) (string, error) {
	query := "select mo_show_visible_bin(atttyp, 2) from mo_catalog.mo_columns where att_database = " +
		quoteSQLLiteral(db) + " and att_relname = " + quoteSQLLiteral(seq) + " and attname = 'last_seq_num'"
	var columnType string
	if err := conn.QueryRow(query).Scan(&columnType); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", moerr.NewNoSuchTable(ctx, db, seq)
		}
		return "", err
	}
	columnType = strings.TrimSpace(columnType)
	if columnType == "" {
		return "", moerr.NewNoSuchTable(ctx, db, seq)
	}
	return columnType, nil
}

type sequenceState struct {
	lastValue string
	minimum   string
	maximum   string
	start     string
	increment string
	cycle     string
	isCalled  string
}

// readSequenceState reads the sequence's single backing row. A sequence
// always has exactly one row; zero or multiple rows mean the relation is
// corrupt, and a dump built from it would restore wrong state.
func readSequenceState(ctx context.Context, db, seq string) (sequenceState, error) {
	rows, err := conn.Query(sequenceStateQuery + quoteIdentifier(db) + "." + quoteIdentifier(seq))
	if err != nil {
		return sequenceState{}, err
	}
	defer rows.Close()

	var state sequenceState
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return sequenceState{}, err
		}
		return sequenceState{}, moerr.NewInternalErrorf(
			ctx, "sequence %s.%s has no state row", db, seq)
	}
	if err := rows.Scan(
		&state.lastValue, &state.minimum, &state.maximum,
		&state.start, &state.increment, &state.cycle, &state.isCalled,
	); err != nil {
		return sequenceState{}, err
	}
	if rows.Next() {
		return sequenceState{}, moerr.NewInternalErrorf(
			ctx, "sequence %s.%s has more than one state row", db, seq)
	}
	if err := rows.Err(); err != nil {
		return sequenceState{}, err
	}

	for column, value := range map[string]string{
		"last_seq_num":    state.lastValue,
		"min_value":       state.minimum,
		"max_value":       state.maximum,
		"start_value":     state.start,
		"increment_value": state.increment,
	} {
		if err := checkSequenceNumber(ctx, db, seq, column, value); err != nil {
			return sequenceState{}, err
		}
	}
	return state, nil
}

// checkSequenceNumber accepts the textual form the server sends for an
// integer column: digits with an optional sign, including values beyond
// int64 for unsigned sequences. Anything else would produce a broken or
// injectable literal in the dump script and fails the dump instead.
func checkSequenceNumber(ctx context.Context, db, seq, column, value string) error {
	if value == "" {
		return moerr.NewInternalErrorf(
			ctx, "sequence %s.%s column %s has an empty value", db, seq, column)
	}
	if _, err := strconv.ParseInt(value, 10, 64); err == nil {
		return nil
	}
	if _, err := strconv.ParseUint(value, 10, 64); err == nil {
		return nil
	}
	return moerr.NewInternalErrorf(
		ctx, "sequence %s.%s column %s has non-numeric value %q", db, seq, column, value)
}

// sequenceBool normalizes a boolean wire text to the true/false literal
// used in generated statements; anything else would produce a broken script
// and fails the dump instead.
func sequenceBool(ctx context.Context, db, seq, column, value string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true":
		return true, nil
	case "0", "false":
		return false, nil
	default:
		return false, moerr.NewInternalErrorf(
			ctx, "sequence %s.%s column %s has invalid boolean value %q", db, seq, column, value)
	}
}

// quoteIdentifier backticks an identifier for generated SQL, doubling any
// embedded backtick so a sequence named s`q (matrixorigin/matrixone#28245)
// round-trips through the restore script.
func quoteIdentifier(s string) string {
	return "`" + strings.ReplaceAll(s, "`", "``") + "`"
}

// quoteSQLLiteral quotes a string for a literal in the catalog queries the
// dump itself runs, using the same escaping as the MatrixOne frontend.
func quoteSQLLiteral(s string) string {
	replacer := strings.NewReplacer(
		`\`, `\\`,
		`'`, `''`,
	)
	return "'" + replacer.Replace(s) + "'"
}
