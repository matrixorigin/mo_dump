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
	"fmt"
	"strings"

	"github.com/matrixorigin/matrixone/pkg/sql/parsers/dialect"
	"github.com/matrixorigin/matrixone/pkg/sql/parsers/dialect/mysql"
	"github.com/matrixorigin/matrixone/pkg/sql/parsers/tree"
)

// parseViewSource returns the owning statement and its borrowed query source.
// The caller frees only the owner after dependency collection.
func parseViewSource(ctx context.Context, ddl string) (tree.Statement, tree.Statement, error) {
	text, sourceStart, scanErr := scanViewSQL(ddl)
	if scanErr != nil {
		return nil, nil, scanErr
	}
	stmt, err := mysql.ParseOne(ctx, text, 0)
	if err == nil {
		if view, ok := stmt.(*tree.CreateView); ok {
			return stmt, view.AsSource, nil
		}
		stmt.Free()
		return nil, nil, fmt.Errorf("expected CREATE VIEW, got %T", stmt)
	}

	// The pinned grammar supports standalone VALUES but not VALUES as a view
	// source. Validate both parts with the same parser, using a split between
	// complete tokens rather than one that can truncate an executable comment.
	if sourceStart < 0 {
		return nil, nil, err
	}
	header, headerErr := mysql.ParseOne(ctx, text[:sourceStart]+" SELECT 1", 0)
	if headerErr != nil {
		return nil, nil, err
	}
	_, isView := header.(*tree.CreateView)
	header.Free()
	if !isView {
		return nil, nil, err
	}
	query, queryErr := mysql.ParseOne(ctx, text[sourceStart:], 0)
	if queryErr != nil {
		return nil, nil, err
	}
	if _, ok := query.(*tree.ValuesStatement); ok {
		return query, query, nil
	}
	query.Free()
	return nil, nil, err
}

// scanViewSQL preserves raw token spelling, including quoted payloads, while
// removing comment wrappers only in the private dependency-parsing copy. This
// also prevents the parser from pooling an unfinished executable-comment state
// when it rejects a newer grammar before reaching the closing comment marker.
// dumpData continues to emit the original, unmodified DDL.
func scanViewSQL(ddl string) (string, int, error) {
	scanner := mysql.NewScanner(dialect.MYSQL, ddl)
	// The pinned NewScanner does not reset this field on pool acquisition.
	scanner.CommentFlag = false
	defer func() {
		scanner.CommentFlag = false
		mysql.PutScanner(scanner)
	}()
	var text strings.Builder
	depth, sourceStart := 0, -1
	for {
		token, value := scanner.Scan()
		switch token {
		case 0:
			if scanner.CommentFlag {
				return "", -1, fmt.Errorf("unterminated executable comment in view SQL")
			}
			return text.String(), sourceStart, nil
		case mysql.LEX_ERROR:
			return "", -1, fmt.Errorf("invalid token in view SQL: %s", value)
		}
		// Scan skips comments recursively and leaves PrePos at the token, not
		// at the preceding comment. Keep quotes/escapes from the original SQL.
		text.WriteString(strings.TrimSpace(ddl[scanner.PrePos:scanner.Pos]))
		switch token {
		case '(':
			depth++
		case ')':
			depth--
		case mysql.AS:
			if depth == 0 && sourceStart < 0 {
				sourceStart = text.Len()
			}
		}
		text.WriteByte(' ')
	}
}
