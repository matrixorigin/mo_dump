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
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

// exportMySQLServer exercises the real MySQL driver without an external service.
// Unfinished results deliberately never send EOF: cleanup must close the socket
// rather than wait for the source to finish producing rows.
func exportMySQLServer(t *testing.T, query string, finish bool) (*sql.DB, func(), <-chan error) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan net.Conn, 1)
	done := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		defer close(done)
		peer, err := listener.Accept()
		if err != nil {
			close(accepted)
			result <- err
			return
		}
		accepted <- peer
		close(accepted)
		defer peer.Close()
		result <- serveExportMySQL(peer, query, finish)
	}()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			_ = listener.Close()
			for peer := range accepted {
				_ = peer.Close()
			}
			<-done
		})
	}
	t.Cleanup(stop)
	db, err := sql.Open("mysql", fmt.Sprintf("root@tcp(%s)/?maxAllowedPacket=16777216", listener.Addr()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, stop, result
}

// exportHandshake performs the fixed protocol 4.1, secure native-password,
// no-TLS handshake shared by both fake servers, and leaves the connection
// ready for its first command.
func exportHandshake(peer net.Conn) error {
	capabilities := uint32(1 | 4 | 512 | 8192 | 32768 | 1<<19)
	hello := append([]byte{10}, []byte("8.0.30-test\x00")...)
	hello = binary.LittleEndian.AppendUint32(hello, 1)
	hello = append(hello, []byte("12345678\x00")...)
	hello = binary.LittleEndian.AppendUint16(hello, uint16(capabilities))
	hello = append(hello, 33, 2, 0)
	hello = binary.LittleEndian.AppendUint16(hello, uint16(capabilities>>16))
	hello = append(hello, 21)
	hello = append(hello, make([]byte, 10)...)
	hello = append(hello, []byte("abcdefghijkl\x00mysql_native_password\x00")...)
	if err := writeExportPacket(peer, 0, hello); err != nil {
		return err
	}
	if _, err := readExportPacket(peer); err != nil {
		return err
	}
	return writeExportPacket(peer, 2, []byte{0, 0, 0, 2, 0, 0, 0})
}

func serveExportMySQL(peer net.Conn, query string, finish bool) error {
	if err := exportHandshake(peer); err != nil {
		return err
	}
	ok := []byte{0, 0, 0, 2, 0, 0, 0}
	command, err := readExportPacket(peer)
	if err != nil {
		return err
	}
	if !bytes.Equal(command, append([]byte{3}, query...)) {
		return fmt.Errorf("unexpected query packet: %q", command)
	}
	// One INT column, then two text-protocol rows. EOF is sent only for the
	// successful control, never as a fallback to unblock failure cleanup.
	column := []byte{3, 'd', 'e', 'f', 2, 'd', 'b', 1, 't', 1, 't', 2, 'i', 'd', 2, 'i', 'd',
		12, 33, 0, 11, 0, 0, 0, 3, 0, 0, 0, 0, 0}
	eof := []byte{0xfe, 0, 0, 2, 0}
	packets := [][]byte{{1}, column, eof, {1, '1'}, {1, '2'}}
	if finish {
		packets = append(packets, eof)
	}
	var response bytes.Buffer
	for i, packet := range packets {
		if err := writeExportPacket(&response, byte(i+1), packet); err != nil {
			return err
		}
	}
	if _, err := peer.Write(response.Bytes()); err != nil {
		return err
	}
	if finish {
		command, err = readExportPacket(peer)
		if err != nil {
			return err
		}
		if !bytes.Equal(command, []byte{14}) { // COM_PING on the reused connection.
			return fmt.Errorf("expected connection reuse via PING, got %q", command)
		}
		if err := writeExportPacket(peer, 1, ok); err != nil {
			return err
		}
	}
	// Both failure and success must close the physical connection. The driver
	// may skip COM_QUIT when unread data still occupies its packet buffer.
	remaining, err := io.ReadAll(peer)
	if err != nil {
		return err
	}
	if len(remaining) != 0 && !bytes.Equal(remaining, []byte{1, 0, 0, 0, 1}) {
		return fmt.Errorf("expected socket close with optional QUIT, got %q", remaining)
	}
	return nil
}

func writeExportPacket(w io.Writer, sequence byte, payload []byte) error {
	packet := []byte{byte(len(payload)), byte(len(payload) >> 8), byte(len(payload) >> 16), sequence}
	_, err := w.Write(append(packet, payload...))
	return err
}

func readExportPacket(r io.Reader) ([]byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, err
	}
	length := int(header[0]) | int(header[1])<<8 | int(header[2])<<16
	if length > 4096 {
		return nil, fmt.Errorf("unexpected test packet length: %d", length)
	}
	payload := make([]byte, length)
	_, err := io.ReadFull(r, payload)
	return payload, err
}

func TestGenOutputUnfinishedRows(t *testing.T) {
	for _, mode := range []string{"csv_create_error", "sql_write_error", "complete_and_reuse"} {
		t.Run(mode, func(t *testing.T) {
			out := exportTestStdout(t)
			database := "db"
			conf := csvConfig{fieldDelimiter: ','}
			var wantErr error
			switch mode {
			case "csv_create_error":
				conf.enable = true
				database = filepath.Join(t.TempDir(), "missing", "db")
				wantErr = os.ErrNotExist
			case "sql_write_error":
				if err := out.Close(); err != nil {
					t.Fatal(err)
				}
				wantErr = os.ErrClosed
			}
			db, stop, serverResult := exportMySQLServer(t, "select * from `"+database+"`.`t`", wantErr == nil)
			oldConn := conn
			conn = db
			t.Cleanup(func() { conn = oldConn })
			finished := make(chan struct{})
			var gotErr error
			go func() {
				defer close(finished)
				pool := &sync.Pool{New: func() any { return &bytes.Buffer{} }}
				gotErr = genOutput(database, "t", pool, 29, true, &conf, "")
			}()
			t.Cleanup(func() {
				stop()
				<-finished
			})
			select {
			case <-finished:
			case <-time.After(5 * time.Second):
				stack := make([]byte, 64*1024)
				t.Logf("%s", stack[:runtime.Stack(stack, true)])
				t.Fatal("local failure cleanup waited for source EOF")
			}
			if !errors.Is(gotErr, wantErr) {
				t.Fatalf("genOutput error = %v, want %v", gotErr, wantErr)
			}
			if stats := db.Stats(); stats.InUse != 0 {
				t.Fatalf("query connection still in use: %+v", stats)
			}
			if wantErr == nil {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := db.PingContext(ctx); err != nil {
					t.Fatalf("successful export did not preserve the connection: %v", err)
				}
			} else if stats := db.Stats(); stats.OpenConnections != 0 {
				t.Fatalf("failed export connection was not discarded: %+v", stats)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-serverResult:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("source did not observe client connection close")
			}
		})
	}
}
