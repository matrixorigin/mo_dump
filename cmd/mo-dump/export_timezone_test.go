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
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// tzUTCPin is the exact statement the MySQL driver sends as the first command
// of every physical connection opened by openDBConnection: the DSN
// time_zone parameter is replayed as a session SET by the driver on each new
// physical connection (#30564).
const tzUTCPin = "SET time_zone = '+00:00'"

// tzPeerRecord reports the first command observed from one served peer.
type tzPeerRecord struct {
	firstCommand string
	serveErr     error
}

// startTZMySQLServer accepts up to conns peers sequentially. Every peer must
// send the UTC session pin as its first command; the pin is answered with OK,
// or with an error packet when reject is true. The returned channel carries
// one record per served peer once the loop stops.
func startTZMySQLServer(t *testing.T, conns int, reject bool) (net.Addr, <-chan []tzPeerRecord, func()) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	records := make(chan []tzPeerRecord, 1)
	var once sync.Once
	done := make(chan struct{})
	go func() {
		defer close(done)
		served := make([]tzPeerRecord, 0, conns)
		for i := 0; i < conns; i++ {
			peer, err := listener.Accept()
			if err != nil {
				break
			}
			command, serveErr := serveTZMySQL(peer, reject)
			_ = peer.Close()
			served = append(served, tzPeerRecord{command, serveErr})
			if serveErr != nil {
				break
			}
		}
		records <- served
	}()
	stop := func() {
		once.Do(func() {
			_ = listener.Close()
			<-done
		})
	}
	return listener.Addr(), records, stop
}

// serveTZMySQL performs the handshake shared with serveExportMySQL and then
// serves one peer until it closes. The first command must be the UTC session
// pin; COM_PING and the trailing QUIT are answered or accepted so a full
// open/ping/reuse connection cycle completes. reject answers the pin with a
// MySQL error packet: a server without time_zone support must fail the
// connection instead of leaving an unpinned export session. Every command
// restarts the packet sequence, so every response uses sequence 1.
func serveTZMySQL(peer net.Conn, reject bool) (string, error) {
	if err := exportHandshake(peer); err != nil {
		return "", err
	}
	ok := []byte{0, 0, 0, 2, 0, 0, 0}
	var first string
	pinned := false
	for {
		command, err := readExportPacket(peer)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return first, nil
			}
			return first, err
		}
		if len(command) == 0 {
			return first, fmt.Errorf("unexpected empty command packet")
		}
		switch command[0] {
		case 1: // COM_QUIT: clean end of this peer
			remaining, err := io.ReadAll(peer)
			if err != nil {
				return first, err
			}
			if len(remaining) != 0 {
				return first, fmt.Errorf("expected socket close after QUIT, got %q", remaining)
			}
			return first, nil
		case 3: // COM_QUERY
			if pinned {
				return first, fmt.Errorf("unexpected second query %q", string(command[1:]))
			}
			pinned = true
			first = string(command[1:])
			if first != tzUTCPin {
				return first, fmt.Errorf("expected the UTC session pin as first query, got %q", first)
			}
			if reject {
				// Error 1987 (HY000); the driver closes the physical
				// connection when initializing a session variable fails
				// (connector.handleParams).
				errPacket := []byte{0xff, 0xc3, 0x07, '#', 'H', 'Y', '0', '0', '0'}
				errPacket = append(errPacket, "time zone pin rejected"...)
				if err := writeExportPacket(peer, 1, errPacket); err != nil {
					return first, err
				}
				continue
			}
			if err := writeExportPacket(peer, 1, ok); err != nil {
				return first, err
			}
		case 14: // COM_PING from database/sql Ping
			if err := writeExportPacket(peer, 1, ok); err != nil {
				return first, err
			}
		default:
			return first, fmt.Errorf("unexpected command %q", command)
		}
	}
}

func TestOpenDBConnectionPinsUTC(t *testing.T) {
	// Two separate calls must produce two distinct physical connections, both
	// pinned (#30564: the pin is a DSN parameter applied by the driver on
	// every new physical connection, not a one-shot SET on one pooled
	// connection).
	addr, records, stop := startTZMySQLServer(t, 2, false)
	t.Cleanup(stop)
	tcpAddr := addr.(*net.TCPAddr)
	opt := Options{username: "root", password: "", host: tcpAddr.IP.String(), port: tcpAddr.Port}
	for i := 0; i < 2; i++ {
		db, err := opt.openDBConnection(context.Background(), "db")
		require.NoError(t, err, "physical connection %d must accept the UTC pin", i)
		require.NoError(t, db.Close())
	}
	select {
	case served := <-records:
		require.Len(t, served, 2, "both physical connections must be served")
		for i, record := range served {
			require.NoError(t, record.serveErr, "peer %d", i)
			require.Equal(t, tzUTCPin, record.firstCommand, "physical connection %d lost the UTC pin", i)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fake server did not finish serving both connections")
	}
}

func TestOpenDBConnectionUTCPinRejected(t *testing.T) {
	addr, records, stop := startTZMySQLServer(t, 1, true)
	t.Cleanup(stop)
	tcpAddr := addr.(*net.TCPAddr)
	opt := Options{username: "root", password: "", host: tcpAddr.IP.String(), port: tcpAddr.Port}
	db, err := opt.openDBConnection(context.Background(), "db")
	require.Error(t, err, "a server that rejects the time zone pin must not yield an export connection")
	require.ErrorContains(t, err, "time zone pin rejected")
	require.Nil(t, db)
	select {
	case served := <-records:
		require.Len(t, served, 1)
		require.NoError(t, served[0].serveErr)
		require.Equal(t, tzUTCPin, served[0].firstCommand)
	case <-time.After(5 * time.Second):
		t.Fatal("fake server did not finish serving the rejected connection")
	}
}
