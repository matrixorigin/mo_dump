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
	"fmt"
	"testing"

	"github.com/matrixorigin/matrixone/pkg/pb/api"
	"github.com/stretchr/testify/assert"
)

// TestIsLegacyTemporaryTable ports the server's own classification test cases
// (pkg/sql/plan/function/func_mo_is_legacy_temporary_table_test.go) plus the
// review round-7 shapes: a batched request, comments and decoration around
// the keywords, and the permanent-physical-name-wins rule.
func TestIsLegacyTemporaryTable(t *testing.T) {
	const session = "123e4567e89b12d3a456426614174000"
	const db = "test_db"
	physical := func(alias string) string { return fmt.Sprintf("__mo_tmp_%s_%s_%s", session, db, alias) }

	renamedExtra := string(api.MustMarshalTblExtra(&api.SchemaExtra{OldName: "permanent_t"}))

	tests := []struct {
		name       string
		relKind    string
		relName    string
		createSQL  string
		extraInfo  string
		wantLegacy bool
	}{
		{
			// Review round 11 (R11): a batched COM_QUERY marks the permanent
			// table's row with the whole request text; the permanent statement
			// names the exact physical row, so the row must stay dumpable.
			name:      "batched request: permanent physical name wins over temporary prefix",
			relKind:   "r",
			relName:   physical("t"),
			createSQL: "CREATE TEMPORARY TABLE t(id int); CREATE TABLE __mo_tmp_" + session + "_test_db_t(id int)",
		},
		{
			name:      "batched request: qualified permanent physical name wins",
			relKind:   "r",
			relName:   physical("t"),
			createSQL: "CREATE TEMPORARY TABLE t(id int); CREATE TABLE test_db.__mo_tmp_" + session + "_test_db_t(id int)",
		},
		{
			name:      "renamed permanent physical row stays visible",
			relKind:   "r",
			relName:   physical("t"),
			createSQL: "CREATE TEMPORARY TABLE t(id int); CREATE TABLE permanent_t(id int)",
			extraInfo: renamedExtra,
		},
		{
			name:      "malformed rename metadata fails open",
			relKind:   "r",
			relName:   physical("t"),
			createSQL: "CREATE TEMPORARY TABLE t(id int)",
			extraInfo: "not protobuf",
		},
		{
			name:       "temporary alias elsewhere in the request classifies the row as legacy",
			relKind:    "r",
			relName:    physical("temp_t"),
			createSQL:  "CREATE TEMPORARY TABLE temp_t(id int); CREATE TABLE permanent_t(id int)",
			wantLegacy: true,
		},
		{
			name:      "permanent row of the same request stays visible",
			relKind:   "r",
			relName:   physical("permanent_t"),
			createSQL: "CREATE TEMPORARY TABLE temp_t(id int); CREATE TABLE __mo_tmp_" + session + "_test_db_permanent_t(id int)",
		},
		{
			// R12: comments and whitespace decoration bypass naive prefix
			// matching; the tokenizer must see through them.
			name:       "decorated temporary statement still classifies as legacy",
			relKind:    "r",
			relName:    physical("t"),
			createSQL:  "/* setup */ CREATE  /*batch*/ TEMPORARY TABLE `t`(id int)",
			wantLegacy: true,
		},
		{
			name:       "lowercase temporary keyword classifies as legacy",
			relKind:    "r",
			relName:    physical("t"),
			createSQL:  "create temporary table if not exists t(id int)",
			wantLegacy: true,
		},
		{
			name:      "temporary statement for another alias does not classify",
			relKind:   "r",
			relName:   physical("t"),
			createSQL: "CREATE TEMPORARY TABLE other(id int)",
		},
		{
			name:      "temporary statement qualified into another database does not classify",
			relKind:   "r",
			relName:   physical("t"),
			createSQL: "CREATE TEMPORARY TABLE other_db.t(id int)",
		},
		{
			name:      "empty createsql classifies as permanent",
			relKind:   "r",
			relName:   physical("t"),
			createSQL: "",
		},
		{
			// The lightweight tokenizer does not parse-verify the tail of the
			// request (the server's full parser fails open on invalid SQL, but
			// rows whose root statement failed never exist in practice). A
			// syntactically sound trailing statement still classifies by the
			// statements that actually match.
			name:       "trailing unrelated statement keeps the classification",
			relKind:    "r",
			relName:    physical("t"),
			createSQL:  "CREATE TEMPORARY TABLE t(id int); SELECT 1 FROM unrelated",
			wantLegacy: true,
		},
		{
			name:      "persistent row named with the shape without any temporary statement",
			relKind:   "r",
			relName:   physical("usercreated"),
			createSQL: "CREATE TABLE __mo_tmp_" + session + "_test_db_usercreated(id int)",
		},
		{
			name:      "non ordinary relkind never classifies",
			relKind:   "i",
			relName:   physical("t"),
			createSQL: "CREATE TEMPORARY TABLE t(id int)",
		},
		{
			// R9: a persistent user table (relkind r) named with the shape but
			// whose own statement is a plain CREATE TABLE stays dumpable.
			name:       "semicolons inside string literals do not split the statement",
			relKind:    "r",
			relName:    physical("t"),
			createSQL:  "CREATE TABLE weird(v text); -- trailing ; comment\n",
			wantLegacy: false,
		},
		{
			name:       "temporary alias inside quotes in a later statement",
			relKind:    "r",
			relName:    physical("t"),
			createSQL:  "INSERT INTO logs VALUES ('CREATE TEMPORARY TABLE t;'); CREATE TEMPORARY TABLE t(id int)",
			wantLegacy: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isLegacyTemporaryTable(tt.relKind, tt.relName, db, tt.createSQL, tt.extraInfo)
			assert.Equal(t, tt.wantLegacy, got)
		})
	}
}

// TestLegacyTemporaryLogicalName checks the structural reverse of the
// physical naming scheme: only exact __mo_tmp_<32hex>_<db>_<alias> shapes
// decode, so normal tables never pay classification costs.
func TestLegacyTemporaryLogicalName(t *testing.T) {
	cases := []struct {
		relName  string
		database string
		want     string
		wantOK   bool
	}{
		{"__mo_tmp_" + "123e4567e89b12d3a456426614174000" + "_d_t", "d", "t", true},
		{"__mo_tmp_" + "123e4567e89b12d3a456426614174000" + "_otherdb_t", "d", "", false},
		{"__mo_tmp_user_t", "d", "", false},                                         // session id not 32 hex
		{"__mo_tmp_" + "123E4567E89B12D3A456426614174000" + "_d_t", "d", "", false}, // uppercase hex
		{"plain", "d", "", false},
		{"__mo_tmp_" + "123e4567e89b12d3a456426614174000_d", "d", "", false}, // no alias part
		{"", "d", "", false},
		{"__mo_tmp_" + "123e4567e89b12d3a456426614174000" + "__x_t", "d", "", false}, // db prefix mismatch
	}
	for _, c := range cases {
		got, ok := legacyTemporaryTableLogicalName(c.relName, c.database)
		assert.Equal(t, c.want, got, c.relName)
		assert.Equal(t, c.wantOK, ok, c.relName)
	}
	// empty database never classifies
	_, ok := legacyTemporaryTableLogicalName("__mo_tmp_123e4567e89b12d3a456426614174000_d_t", "")
	assert.False(t, ok)
}
