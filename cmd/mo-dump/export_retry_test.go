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
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"testing"
)

// This connector permits deterministic physical reconnection after ErrBadConn
// without registering a global driver or depending on network timing.
type exportRetryConnector struct {
	queryErrors []error
	connectErr  error
	rowErr      error
	connects    int
	queryIDs    []int
	queries     []string
	closed      []bool
	rowsClosed  int
}

func (c *exportRetryConnector) Connect(context.Context) (driver.Conn, error) {
	c.connects++
	if c.connectErr != nil {
		return nil, c.connectErr
	}
	id := len(c.closed)
	c.closed = append(c.closed, false)
	return &exportRetryConn{owner: c, id: id}, nil
}

func (c *exportRetryConnector) Driver() driver.Driver { return c }
func (c *exportRetryConnector) Open(string) (driver.Conn, error) {
	return nil, errors.New("use connector.Connect")
}

type exportRetryConn struct {
	owner *exportRetryConnector
	id    int
}

func (c *exportRetryConn) Prepare(string) (driver.Stmt, error) {
	return nil, driver.ErrSkip
}
func (c *exportRetryConn) Begin() (driver.Tx, error) { return nil, driver.ErrSkip }
func (c *exportRetryConn) Close() error {
	c.owner.closed[c.id] = true
	return nil
}
func (c *exportRetryConn) IsValid() bool { return !c.owner.closed[c.id] }
func (c *exportRetryConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	attempt := len(c.owner.queries)
	c.owner.queryIDs = append(c.owner.queryIDs, c.id)
	c.owner.queries = append(c.owner.queries, query)
	if attempt < len(c.owner.queryErrors) && c.owner.queryErrors[attempt] != nil {
		return nil, c.owner.queryErrors[attempt]
	}
	return &exportRetryRows{owner: c.owner}, nil
}

type exportRetryRows struct {
	owner *exportRetryConnector
	index int
}

func (r *exportRetryRows) Columns() []string                     { return []string{"id"} }
func (r *exportRetryRows) ColumnTypeDatabaseTypeName(int) string { return "INT" }
func (r *exportRetryRows) Close() error                          { r.owner.rowsClosed++; return nil }
func (r *exportRetryRows) Next(dest []driver.Value) error {
	if r.index < 2 {
		r.index++
		dest[0] = int64(r.index)
		return nil
	}
	if r.owner.rowErr != nil {
		return r.owner.rowErr
	}
	return io.EOF
}

func TestGenOutputBadConnRetry(t *testing.T) {
	wrappedBad := fmt.Errorf("stale reserved connection: %w", driver.ErrBadConn)
	queryErr := errors.New("query rejected")
	complete := "INSERT INTO `t` VALUES (1);\nINSERT INTO `t` VALUES (2);\n\n\n\n"
	partial := "INSERT INTO `t` VALUES (1);\n"
	for _, tc := range []struct {
		name        string
		beforeRows  []error
		connectErr  error
		rowErr      error
		wantErr     error
		attempts    int
		connections int
		closed      int
		rowsClosed  int
		output      string
	}{
		{name: "healthy", attempts: 1, connections: 1, rowsClosed: 1, output: complete},
		{name: "one_bad_connection", beforeRows: []error{driver.ErrBadConn}, attempts: 2, connections: 2, closed: 1, rowsClosed: 1, output: complete},
		{name: "two_bad_connections", beforeRows: []error{driver.ErrBadConn, driver.ErrBadConn}, attempts: 3, connections: 3, closed: 2, rowsClosed: 1, output: complete},
		{name: "wrapped_bad_connection", beforeRows: []error{wrappedBad}, attempts: 2, connections: 2, closed: 1, rowsClosed: 1, output: complete},
		{name: "retry_budget_exhausted", beforeRows: []error{driver.ErrBadConn, driver.ErrBadConn, wrappedBad}, wantErr: wrappedBad, attempts: 3, connections: 3, closed: 3},
		{name: "query_error_not_retried", beforeRows: []error{queryErr}, wantErr: queryErr, attempts: 1, connections: 1},
		{name: "acquisition_error", connectErr: io.ErrUnexpectedEOF, wantErr: io.ErrUnexpectedEOF, connections: 1},
		{name: "row_error_not_retried", rowErr: io.ErrUnexpectedEOF, wantErr: io.ErrUnexpectedEOF, attempts: 1, connections: 1, closed: 1, rowsClosed: 1, output: partial},
		{name: "row_bad_conn_not_retried", rowErr: driver.ErrBadConn, wantErr: driver.ErrBadConn, attempts: 1, connections: 1, closed: 1, rowsClosed: 1, output: partial},
	} {
		t.Run(tc.name, func(t *testing.T) {
			connector := &exportRetryConnector{queryErrors: tc.beforeRows, connectErr: tc.connectErr, rowErr: tc.rowErr}
			db := sql.OpenDB(connector)
			t.Cleanup(func() { _ = db.Close() })
			oldConn := conn
			conn = db
			t.Cleanup(func() { conn = oldConn })
			out, expectFile := exportTestSink(t)
			pool := &sync.Pool{New: func() any { return &bytes.Buffer{} }}
			err := genOutput("db", "t", out, pool, 29, true, &csvConfig{}, "id > 0")
			if err != tc.wantErr {
				t.Fatalf("error = %v, want original error %v", err, tc.wantErr)
			}
			if len(connector.queries) != tc.attempts || connector.connects != tc.connections {
				t.Fatalf("queries/connections = %d/%d, want %d/%d", len(connector.queries), connector.connects, tc.attempts, tc.connections)
			}
			for attempt, query := range connector.queries {
				if query != "select * from `db`.`t` where id > 0" || connector.queryIDs[attempt] != attempt {
					t.Fatalf("attempt %d reused a failed connection or changed the query: %q, connection %d", attempt, query, connector.queryIDs[attempt])
				}
			}
			for id, closed := range connector.closed {
				if closed != (id < tc.closed) {
					t.Fatalf("connection %d closed = %v, want %v", id, closed, id < tc.closed)
				}
			}
			if stats := db.Stats(); stats.InUse != 0 {
				t.Fatalf("reserved connection leaked: %+v", stats)
			}
			if connector.rowsClosed != tc.rowsClosed {
				t.Fatalf("rows closed %d times, want %d", connector.rowsClosed, tc.rowsClosed)
			}
			data, err := os.ReadFile(expectFile.Name())
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != tc.output {
				t.Fatalf("output = %q, want %q", data, tc.output)
			}
		})
	}
}
