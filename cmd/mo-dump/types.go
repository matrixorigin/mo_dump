// Copyright 2023 Matrix Origin
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
	"database/sql"
	"time"

	"github.com/matrixorigin/matrixone/pkg/common/mpool"
)

const (
	defaultUsername        = "dump"
	defaultPassword        = "111"
	defaultHost            = "127.0.0.1"
	defaultPort            = 6001
	defaultNetBufferLength = mpool.MB
	minNetBufferLength     = mpool.KB * 16
	maxNetBufferLength     = mpool.MB * 16
	defaultCsv             = false
	defaultLocalInfile     = true
	defaultNoData          = false
	timeout                = 10 * time.Second
	//default Field delimiter (set to ',')
	defaultFieldDelimiter rune = ','
	blobPrefix                 = "0x"
	// zeroByte                   = '\x00'
	// lineByte                   = '\n'
	// carriageByte               = '\r'
	// tabularByte                = '\t'
	defaultEnableEscape = false
)

const (
	quoteFmt   = "%q"
	defaultFmt = "%s"
	jsonFmt    = "\"%s\""

	// emptyRelKind is the relkind the server records for CREATE TABLE names
	// that collide with internal naming (e.g. a user table named
	// __mo_index_renamed, or restored from such a dump). The object itself
	// stays a fully usable ordinary table (SHOW CREATE TABLE and SELECT are
	// verified to work); only the kind column is blank. Treat it as an
	// ordinary table so the dump/restore cycle converges instead of making
	// the whole database undumpable.
	emptyRelKind = ""
)

var (
	conn      *sql.DB
	nullBytes = []byte("\\N")
)

type Column struct {
	Name string
	Type string
}

type Table struct {
	Name string
	Kind string
}

type Tables []Table

// csvConfig is the configuration for csv output
type csvConfig struct {
	enable         bool
	fieldDelimiter rune
	enableEscape   bool
}

type Db struct {
	Name   string
	DBType string
}
