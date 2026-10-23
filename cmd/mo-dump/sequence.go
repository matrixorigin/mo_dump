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
	"fmt"
	"math/big"
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
// values the source already consumed. A state that cannot be replayed
// exactly — for example a last_seq_num outside [min_value, max_value], which
// ALTER SEQUENCE can produce — fails the dump instead of writing a script
// whose restore breaks after dropping the old objects
// (matrixorigin/matrixone#30577). -no-data callers may drop stateSQL, which
// makes the restored sequence continue from its START value.
func getCreateSequence(ctx context.Context, db, seq string, isSys bool) (createSQL string, stateSQL string, err error) {
	columnType, err := sequenceColumnType(ctx, db, seq, isSys)
	if err != nil {
		return "", "", err
	}

	state, err := readSequenceState(ctx, db, seq)
	if err != nil {
		return "", "", err
	}
	if err := checkSequenceRange(ctx, db, seq, state); err != nil {
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
// mo_catalog.mo_columns. Even a filtered query must find exactly one row;
// anything else is an ambiguous sequence and fails the dump.
func sequenceColumnType(ctx context.Context, db, seq string, isSys bool) (string, error) {
	query := "select mo_show_visible_bin(atttyp, 2) from mo_catalog.mo_columns where att_database = " +
		quoteSQLLiteral(db) + " and att_relname = " + quoteSQLLiteral(seq) + " and attname = 'last_seq_num'"
	if isSys {
		// A sys account's mo_catalog view is not tenant-isolated: without this
		// filter the same-name sequence of another tenant can answer the query
		// and poison the dump with a wrong type (matrixorigin/matrixone#30577).
		query += " and account_id = 0"
	}
	rows, err := conn.Query(query)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	var columnType string
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return "", err
		}
		return "", moerr.NewNoSuchTable(ctx, db, seq)
	}
	if err := rows.Scan(&columnType); err != nil {
		return "", err
	}
	if rows.Next() {
		return "", moerr.NewInternalErrorf(
			ctx, "sequence %s.%s has ambiguous column metadata in mo_catalog.mo_columns", db, seq)
	}
	if err := rows.Err(); err != nil {
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

// checkSequenceRange verifies the allocation state against the sequence's own
// bounds. SETVAL — the only user-reachable state writer on restore — rejects a
// value outside [min_value, max_value], and ALTER SEQUENCE can leave a
// sequence in exactly such a state (it pre-advances last_seq_num in the
// session that called NEXTVAL). A dump that would later fail on restore
// because of this must fail now, at export time, before any old objects are
// dropped by the script (matrixorigin/matrixone#30577).
func checkSequenceRange(ctx context.Context, db, seq string, state sequenceState) error {
	minimum, ok := new(big.Int).SetString(state.minimum, 10)
	maximum, okMax := new(big.Int).SetString(state.maximum, 10)
	if !ok || !okMax {
		return moerr.NewInternalErrorf(
			ctx, "sequence %s.%s has non-numeric min/max bounds %q, %q", db, seq, state.minimum, state.maximum)
	}
	lastValue, ok := new(big.Int).SetString(state.lastValue, 10)
	if !ok {
		return moerr.NewInternalErrorf(
			ctx, "sequence %s.%s has non-numeric last_seq_num %q", db, seq, state.lastValue)
	}
	if lastValue.Cmp(minimum) < 0 || lastValue.Cmp(maximum) > 0 {
		return moerr.NewInternalErrorf(
			ctx, "sequence %s.%s allocation state last_seq_num %s is outside its bounds [%s, %s]: sequence bounds are not enforced on ALTER SEQUENCE, and SETVAL cannot restore such a state",
			db, seq, state.lastValue, state.minimum, state.maximum)
	}
	startValue, ok := new(big.Int).SetString(state.start, 10)
	if !ok {
		return moerr.NewInternalErrorf(
			ctx, "sequence %s.%s has non-numeric start_value %q", db, seq, state.start)
	}
	if startValue.Cmp(minimum) < 0 || startValue.Cmp(maximum) > 0 {
		return moerr.NewInternalErrorf(
			ctx, "sequence %s.%s start_value %s is outside its bounds [%s, %s]",
			db, seq, state.start, state.minimum, state.maximum)
	}
	return nil
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
