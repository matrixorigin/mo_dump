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
	"strings"

	"github.com/matrixorigin/matrixone/pkg/pb/api"
	catalog "github.com/matrixorigin/mo_dump/internal/catalog"
)

// legacyTemporarySessionIDLength is the hex-character length of the session id
// embedded in v4.0/v4.1 temporary backing-table physical names.
const legacyTemporarySessionIDLength = 32

// isLegacyTemporaryTable reports whether a mo_catalog.mo_tables row is the
// backing row of a session temporary table on MO v4.0/v4.1 releases (review
// rounds 6-7). The predicate mirrors the server's own
// pkg/sql/plan/function.builtInIsLegacyTemporaryTable classification
// statement for statement instead of its
// physical name, while a permanent table whose user picked a physical-looking
// name has that name explicit in its own CREATE statement, and that durable
// evidence must win over a matching temporary alias elsewhere in the same
// request.
//
// rel_createsql prefix matching cannot implement this: a request may batch
// `CREATE TEMPORARY ...; CREATE TABLE persistent ...` and mark the permanent
// table's row with a temporary-looking request, while comments, leading
// whitespace or a non-leading position made real temporary rows bypass every
// prefix rule. The classification therefore tokenizes the statement text
// (statementFor below) instead of pattern-matching its start, which keeps the
// behavior identical to the server parser on the shapes that matter:
//   - statements are split on unquoted semicolons, so position never matters;
//   - comments and quoted tokens are skipped, so decoration never matters;
//   - the TEMPORARY flag and the (optionally qualified) name are read from the
//     statement's own keyword sequence, so a physical name in a permanent
//     statement always wins (fail-open) and a temporary alias in any position
//     classifies its backing row as legacy temporary.
func isLegacyTemporaryTable(relKind, relName, database, createSQL, extraInfo string) bool {
	if relKind != catalog.SystemOrdinaryRel || createSQL == "" {
		return false
	}
	logicalName, ok := legacyTemporaryTableLogicalName(relName, database)
	if !ok {
		return false
	}

	// Temporary tables could not be renamed, while rename preserves OldName
	// in SchemaExtra. Treat malformed metadata as permanent as well:
	// compatibility classification must fail open rather than hide a user
	// table.
	if extraInfo != "" {
		extra := &api.SchemaExtra{}
		if err := extra.Unmarshal([]byte(extraInfo)); err != nil || extra.OldName != "" {
			return false
		}
	}

	temporaryStatementMatches := false
	for _, statement := range splitSQLStatements(createSQL) {
		tableName, temporary, matches := matchCreateTableStatement(statement, database)
		if !matches {
			continue
		}
		if !temporary && strings.EqualFold(tableName, relName) {
			return false
		}
		if temporary && strings.EqualFold(tableName, logicalName) {
			temporaryStatementMatches = true
		}
	}
	return temporaryStatementMatches
}

// matchCreateTableStatement classifies one CREATE TABLE statement. It returns
// the (unqualified) table name, the TEMPORARY flag, and whether the statement
// is a CREATE TABLE at all. Keyword sequence per MySQL grammar (cased freely):
//
//	CREATE [TEMPORARY] TABLE [IF NOT EXISTS] [schema.]table ...
//
// A schema qualifier, if present, must equal database — the same relation the
// server applies in createTableNamesRelation. Decorations (comments, extra
// whitespace) may appear anywhere between keywords and never match.
func matchCreateTableStatement(statement, database string) (name string, temporary bool, matches bool) {
	const (
		kwCreate = iota
		kwAfterCreate
		kwAfterTable
		kwAfterIf
		kwAfterNot
	)
	state := kwCreate
	pos := 0
	for {
		token, isWord, next := scanWord(statement, pos)
		pos = next
		switch state {
		case kwCreate:
			if !isWord || !strings.EqualFold(token, "create") {
				return "", false, false
			}
			state = kwAfterCreate
		case kwAfterCreate:
			if !isWord {
				return "", false, false
			}
			switch {
			case strings.EqualFold(token, "temporary"):
				temporary = true
			case strings.EqualFold(token, "table"):
				state = kwAfterTable
			default: // CREATE VIEW / CREATE INDEX / CREATE DATABASE / ...: not our grammar
				return "", false, false
			}
		case kwAfterTable:
			if isWord && strings.EqualFold(token, "if") {
				state = kwAfterIf
				continue
			}
			name := resolveQualifiedIdentifier(statement, token, next, database)
			return name, temporary, name != ""
		case kwAfterIf:
			if isWord && strings.EqualFold(token, "not") {
				state = kwAfterNot
				continue
			}
			return "", false, false
		case kwAfterNot:
			if isWord && strings.EqualFold(token, "exists") {
				word, isWord2, after := scanWord(statement, next)
				if !isWord2 {
					return "", false, false
				}
				name := resolveQualifiedIdentifier(statement, word, after, database)
				return name, temporary, name != ""
			}
			return "", false, false
		}
	}
}

// resolveQualifiedIdentifier interprets token (the already-scanned identifier
// word of the CREATE TABLE grammar, ending at offset next) as an optionally
// schema-qualified table name, mirroring the server's createTableNamesRelation:
// a schema qualifier other than database yields an empty name which can never
// equal a real table name.
func resolveQualifiedIdentifier(statement, token string, next int, database string) string {
	j := next
	for j < len(statement) && isSpace(statement[j]) {
		j++
	}
	if j+1 >= len(statement) || statement[j] != '.' {
		return token
	}
	second, isWord2, _ := scanWord(statement, j+1)
	if !isWord2 {
		return ""
	}
	if !strings.EqualFold(token, database) {
		return ""
	}
	return second
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

// splitSQLStatements splits a COM_QUERY payload into statements the way the
// server interprets it: semicolons split statements only outside string
// literals, quoted identifiers and comments.
func splitSQLStatements(sql string) []string {
	var statements []string
	start := 0
	i := 0
	for i < len(sql) {
		switch sql[i] {
		case '\'', '"', '`':
			end := skipQuoted(sql, i)
			if end < 0 {
				return append(statements, sql[start:])
			}
			i = end
		case '-':
			if i+1 < len(sql) && sql[i+1] == '-' {
				i = skipLine(sql, i)
			} else {
				i++
			}
		case '#':
			i = skipLine(sql, i)
		case '/':
			if i+1 < len(sql) && sql[i+1] == '*' {
				i = skipBlockComment(sql, i)
			} else {
				i++
			}
		case ';':
			statements = append(statements, sql[start:i])
			start = i + 1
			i++
		default:
			i++
		}
	}
	return append(statements, sql[start:])
}

func skipQuoted(sql string, start int) int {
	quote := sql[start]
	for i := start + 1; i < len(sql); i++ {
		if sql[i] == '\\' && (quote == '\'' || quote == '"') {
			i++
			continue
		}
		if sql[i] == quote {
			if i+1 < len(sql) && sql[i+1] == quote {
				i++
				continue
			}
			return i + 1
		}
	}
	return -1
}

func skipLine(sql string, start int) int {
	for i := start; i < len(sql); i++ {
		if sql[i] == '\n' {
			return i + 1
		}
	}
	return len(sql)
}

func skipBlockComment(sql string, start int) int {
	for i := start + 2; i+1 < len(sql); i++ {
		if sql[i] == '*' && sql[i+1] == '/' {
			return i + 2
		}
	}
	return len(sql)
}

// scanWord returns the next bare keyword/identifier word (letters, digits,
// underscore, dollar) starting at or after pos with its end offset; other
// characters (punctuation, whitespace...) are skipped as decoration, except
// that quoted identifiers are returned whole as one word so identifiers such
// as `weird;name` never break statement splitting.
func scanWord(statement string, pos int) (word string, isWord bool, next int) {
	i := pos
	for i < len(statement) {
		c := statement[i]
		switch {
		case c == '\'' || c == '"' || c == '`':
			end := skipQuoted(statement, i)
			if end < 0 {
				return "", false, len(statement)
			}
			return unquote(statement[i:end]), true, end
		case c == '#':
			i = skipLine(statement, i)
		case c == '-' && i+1 < len(statement) && statement[i+1] == '-':
			i = skipLine(statement, i)
		case c == '/' && i+1 < len(statement) && statement[i+1] == '*':
			i = skipBlockComment(statement, i)
		case isWordChar(c):
			end := i
			for end < len(statement) && isWordChar(statement[end]) {
				end++
			}
			return statement[i:end], true, end
		default:
			i++
		}
	}
	return "", false, len(statement)
}

func isWordChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '$'
}

func unquote(token string) string {
	if len(token) < 2 {
		return token
	}
	quote := token[0]
	if quote != '\'' && quote != '"' && quote != '`' || token[len(token)-1] != quote {
		return token
	}
	inner := token[1 : len(token)-1]
	return strings.ReplaceAll(inner, string(quote)+string(quote), string(quote))
}

// legacyTemporaryTableLogicalName reverses the v4.0/v4.1 physical naming
// __mo_tmp_<session32hex>_<database>_<alias> into its logical alias. It is the
// same structural check the server applies before consulting rel_createsql;
// only rows shaped this way ever pay the classification cost.
func legacyTemporaryTableLogicalName(relName, database string) (string, bool) {
	if database == "" || !strings.HasPrefix(relName, "__mo_tmp_") {
		return "", false
	}
	remainder := strings.TrimPrefix(relName, "__mo_tmp_")
	sessionID, physicalName, found := strings.Cut(remainder, "_")
	if !found || len(sessionID) != legacyTemporarySessionIDLength || !isLowerHex(sessionID) {
		return "", false
	}
	databasePrefix := database + "_"
	if len(physicalName) <= len(databasePrefix) ||
		!strings.EqualFold(physicalName[:len(databasePrefix)], databasePrefix) {
		return "", false
	}
	return physicalName[len(databasePrefix):], true
}

func isLowerHex(value string) bool {
	for _, ch := range value {
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			return false
		}
	}
	return true
}
