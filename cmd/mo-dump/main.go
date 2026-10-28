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
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	_ "github.com/go-sql-driver/mysql"
	"github.com/matrixorigin/matrixone/pkg/common/moerr"
	"github.com/matrixorigin/mo_dump/internal/catalog"
)

type Options struct {
	username                 string
	password                 string
	host                     string
	database                 string
	tbl                      string
	dbs                      []string
	tables                   Tables
	confirmedPermanentTables Tables
	port                     int
	netBufferLength          int
	toCsv                    bool
	localInfile              bool
	noData                   bool
	emptyTables              bool
	csvConf                  csvConfig
	csvFieldDelimiterStr     string
	enableEscape             bool
	where                    string
	sysAccount               bool // used in getDatabaseType, getTables
}

func (t *Tables) String() string {
	return fmt.Sprint(*t)
}

func (t *Tables) Set(value string) error {
	*t = append(*t, Table{value, ""})
	return nil
}

var usage = func() {
	fmt.Fprintf(os.Stderr, "Usage: %s -u <username> -p <password> -h <host> -P <port> -db <database> [--local-infile=true] [--enable-escape=false] [-csv] [-tbl <table>...] [-confirm-permanent-table <table>]... [-no-data] -net-buffer-length <net-buffer-length>\n", os.Args[0])
	flag.PrintDefaults()
}

// parseDatabases splits the -db option value by comma, trims spaces and drops
// empty items. It returns an invalid-input error when no valid database name
// remains (e.g. -db ',,'), so callers never dump with an empty database list.
func parseDatabases(ctx context.Context, raw string) ([]string, error) {
	if len(raw) == 0 {
		return nil, moerr.NewInvalidInput(ctx, "database must be specified")
	}
	var dbs []string
	for _, db := range strings.Split(raw, ",") {
		db = strings.TrimSpace(db)
		if len(db) != 0 {
			dbs = append(dbs, db)
		}
	}
	if len(dbs) == 0 {
		return nil, moerr.NewInvalidInput(ctx, fmt.Sprintf("database '%s' contains no valid database name", raw))
	}
	return dbs, nil
}

func main() {
	var (
		err     error
		opt     Options
		sink    *dumpSink
		sinkErr error
	)
	dumpStart := time.Now()
	defer func() {
		if err != nil {
			fmt.Fprintf(os.Stderr, "modump error: %v\n", err)
			os.Exit(1)
		}
		if conn != nil {
			err := conn.Close()
			if err != nil {
				fmt.Fprintf(os.Stderr, "modump error while close connection: %v\n", err)
				os.Exit(1)
			}
		}
		if err == nil && flag.NFlag() != 0 {
			// The success notes are the last piece of output their reader can
			// rely on; ignoring their write failure would report a completed
			// backup from a dying pipe as a silent exit 0
			// (matrixorigin/matrixone#30577).
			if noteErr := writeSuccessFootnotes(os.Stdout, time.Since(dumpStart), opt.toCsv); noteErr != nil {
				// os.Exit below skips the remaining defers, and publish already
				// flagged the sink fully published — revoke takes the csv files
				// back explicitly so a tail-note failure still leaves nothing
				// of this run behind (matrixorigin/matrixone#30577).
				if sink != nil {
					sink.revoke()
				}
				fmt.Fprintf(os.Stderr, "modump error: %v\n", noteErr)
				os.Exit(1)
			}
		}
	}()

	ctx := context.Background()
	// A reader closing stdout early (a broken pipe) must surface as a write
	// error instead of the default SIGPIPE death: dying at fd 1 would skip
	// the deferred cleanup and leave the staged spool and csv files behind.
	// Go escalates SIGPIPE on fd 1/2 to a kill only while the signal is
	// unhandled, so ignoring it turns the write into EPIPE
	// (matrixorigin/matrixone#30577).
	signal.Ignore(syscall.SIGPIPE)
	// The published csv mode follows the process umask; probing mutates the
	// process umask, so it must happen before any other file creation.
	probeFileMode()
	flag.StringVar(&opt.username, "u", defaultUsername, "username")
	flag.StringVar(&opt.password, "p", defaultPassword, "password")
	flag.StringVar(&opt.host, "h", defaultHost, "hostname")
	flag.IntVar(&opt.port, "P", defaultPort, "portNumber")
	flag.IntVar(&opt.netBufferLength, "net-buffer-length", defaultNetBufferLength, "net-buffer-length used to set the buffer size for network connections")
	flag.StringVar(&opt.database, "db", "", "databaseName, must be specified")
	flag.StringVar(&opt.tbl, "tbl", "", "tableNameList (default all)")
	flag.Var(&opt.confirmedPermanentTables, "confirm-permanent-table", "confirm one legacy-temporary-shaped name as a permanent table for a single-database whole dump (repeatable); incorrect confirmation may export legacy temporary data")
	flag.BoolVar(&opt.toCsv, "csv", defaultCsv, "set export format to csv (default false)")
	flag.StringVar(&opt.csvFieldDelimiterStr, "csv-field-delimiter", string(defaultFieldDelimiter), "set csv field delimiter (one utf8 character; no backslash, double quote, CR, LF, or NUL). enabled only when the option 'csv' is set.")
	flag.BoolVar(&opt.localInfile, "local-infile", defaultLocalInfile, "use load data local infile")
	flag.BoolVar(&opt.noData, "no-data", defaultNoData, "dump database and table definitions only without data (default false)")
	flag.BoolVar(&opt.enableEscape, "enable-escape", defaultEnableEscape, "enable escape characters in csv output")
	flag.StringVar(&opt.where, "where", "", "Dump only selected records. Quotes are mandatory.")
	flag.BoolVar(&opt.sysAccount, "sys", false, "Dump system table from 'sys' account, used for check db / table schema.")
	flag.Parse()

	flag.Usage = usage
	if flag.NFlag() == 0 {
		flag.Usage()
		return
	}

	if opt.netBufferLength < minNetBufferLength {
		fmt.Fprintf(os.Stderr, "net_buffer_length must be greater than %d, set to %d\n", minNetBufferLength, minNetBufferLength)
		opt.netBufferLength = minNetBufferLength
	}
	if opt.netBufferLength > maxNetBufferLength {
		fmt.Fprintf(os.Stderr, "net_buffer_length must be less than %d, set to %d\n", maxNetBufferLength, maxNetBufferLength)
		opt.netBufferLength = maxNetBufferLength
	}

	opt.dbs, err = parseDatabases(ctx, opt.database)
	if err != nil {
		return
	}

	if len(opt.tbl) > 0 {
		tbls := strings.Split(opt.tbl, ",")
		for _, t := range tbls {
			t = strings.TrimSpace(t)
			if len(t) != 0 {
				opt.tables = append(opt.tables, Table{t, ""})
			}
		}
	}
	if opt.tables == nil {
		opt.emptyTables = true
	}
	if err = opt.validatePermanentTableOptions(ctx); err != nil {
		return
	}

	//replace : in username to #, because : is used as separator in dsn.
	//password can have ":".
	opt.username = strings.ReplaceAll(opt.username, ":", "#")

	// if host has ":", reports error
	if strings.Count(opt.host, ":") > 0 {
		err = moerr.NewInvalidInput(ctx, "host can not have character ':'")
		return
	}

	if opt.toCsv {
		opt.csvConf.enable = opt.toCsv
		opt.csvConf.fieldDelimiter, err = checkFieldDelimiter(ctx, opt.csvFieldDelimiterStr)
		opt.csvConf.enableEscape = opt.enableEscape
		if err != nil {
			return
		}
	}

	if opt.database == "all" {
		if conn == nil {
			conn, err = opt.openDBConnection(ctx, "")
			if err != nil {
				return
			}
			defer conn.Close()
		}

		var dbs []string
		dbs, err = getDatabases(ctx)
		if err != nil {
			return
		}
		dbs = filterUserDatabases(dbs)
		if len(dbs) == 0 {
			err = moerr.NewInvalidInput(ctx, "-db all found only system databases, nothing to dump")
			return
		}
		opt.dbs = dbs
	}

	// The whole dump is staged before anything reaches stdout: a failed run
	// must not publish a truncated script whose DROP statements then destroy
	// a target database (matrixorigin/matrixone#30577).
	sink, sinkErr = newDumpSink()
	if sinkErr != nil {
		err = sinkErr
		return
	}
	defer sink.discard()

	err = opt.dumpData(ctx, sink)
	if err != nil {
		return
	}
	err = sink.publish()
	if err != nil {
		return
	}
}

// writeSuccessFootnotes writes the closing notes. The csv reminder goes
// first and the success marker is the very last output a reader sees: a
// visible MODUMP SUCCESS must guarantee that everything — script, csv
// files and their reminder — was already delivered, so a failing note
// never leaves a script whose final complete marker costs the operator a
// restore that cannot find its files (matrixorigin/matrixone#30577).
func writeSuccessFootnotes(w io.Writer, cost time.Duration, csvNote bool) error {
	// Restore the script-level mode before the final success marker. A failed
	// write follows the same checked-error and CSV revocation path as the notes.
	if _, err := fmt.Fprint(w, "SET SESSION sql_mode=@MODUMP_SCRIPT_SAVED_SQL_MODE;\n\n\n"); err != nil {
		return err
	}
	if csvNote {
		if _, err := fmt.Fprintf(w, "/* !!!MUST KEEP FILE IN CURRENT DIRECTORY, OR YOU SHOULD CHANGE THE PATH IN LOAD DATA STMT!!! */ \n"); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(w, "/* MODUMP SUCCESS, COST %v */\n", cost)
	return err
}

func (opt *Options) dumpData(ctx context.Context, out *dumpSink) error {
	var (
		createDb    string
		createTable []string
		stateSQL    []string
		views       []dumpView
		err         error
	)

	if conn == nil {
		conn, err = opt.openDBConnection(ctx, opt.dbs[0])
		if err != nil {
			return err
		}
		defer conn.Close()
	}

	// add foreign_key_checks variable to dump file
	out.printf("SET foreign_key_checks = 0;\n\n")

	// String literals in this script were quoted assuming backslash escapes.
	// Pin the restore session's sql_mode correspondingly even when it runs
	// under NO_BACKSLASH_ESCAPES, and restore it at the end of the script
	// (matrixorigin/matrixone#30587). NO_BACKSLASH_ESCAPES is removed from a
	// comma-wrapped copy of the mode list so stripping it can never leave an
	// empty token, whatever its position.
	// Keep this variable distinct from the per-view saved mode: SQL user
	// variable names are case-insensitive.
	out.printf("SET @MODUMP_SCRIPT_SAVED_SQL_MODE=@@SESSION.sql_mode;\nSET SESSION sql_mode=(SELECT TRIM(BOTH ',' FROM REPLACE(CONCAT(',',@@session.sql_mode,','),',NO_BACKSLASH_ESCAPES,',',')));\n\n\n")

	for _, db := range opt.dbs {
		var dbStruct Db
		dbStruct, err = getDatabaseType(ctx, db, opt.sysAccount)
		if err != nil {
			return err
		}

		if dbStruct.DBType == catalog.SystemDBTypeSubscription && len(opt.confirmedPermanentTables) > 0 {
			return moerr.NewInvalidInputf(ctx, "-confirm-permanent-table cannot validate subscription database %q", dbStruct.Name)
		}
		db = dbStruct.Name // Catalog identity, not the requested spelling.
		if opt.emptyTables {
			opt.tables = nil
		}
		if len(opt.tables) == 0 { //dump all tables
			if dbStruct.DBType == catalog.SystemDBTypeSubscription {
				createDb = "CREATE DATABASE IF NOT EXISTS " + quoteIdentifier(db)
			} else {
				createDb, err = getCreateDB(db)
				if err != nil {
					return err
				}
			}
			out.printf("DROP DATABASE IF EXISTS %s;\n", quoteIdentifier(db))
			out.printf("%s ;\n", createDb)
			out.printf("USE %s;\n\n\n", quoteIdentifier(db))
		}

		if dbStruct.DBType == catalog.SystemDBTypeSubscription {
			opt.tables, err = opt.getSubScriptionTables(ctx, db, opt.tables)
			if err != nil {
				return err
			}
		} else {
			opt.tables, err = getTables(ctx, db, opt.tables, *opt)
			if err != nil {
				return err
			}
		}
		createTable = make([]string, len(opt.tables))
		stateSQL = make([]string, len(opt.tables))
		for i, tbl := range opt.tables {
			switch tbl.Kind {
			case catalog.SystemOrdinaryRel, catalog.SystemExternalRel, emptyRelKind:
				createTable[i], err = getCreateTable(db, tbl.Name)
				if err != nil {
					return err
				}
			case catalog.SystemViewRel:
				view, err := getDumpView(ctx, db, tbl.Name, opt.sysAccount)
				if err != nil {
					return err
				}
				views = append(views, view)
				createTable[i] = view.create
			case catalog.SystemSequenceRel:
				// A sequence's current definition and allocation state live in
				// its backing row, not in SHOW CREATE TABLE
				// (matrixorigin/matrixone#30577). isSys scopes the catalog
				// metadata query to the sys account, where mo_catalog is not
				// tenant-isolated; a state-carrying dump exports the allocation
				// replay, while -no-data exports the legal definition only.
				var state string
				createTable[i], state, err = getCreateSequence(ctx, db, tbl.Name, opt.sysAccount, !opt.noData)
				if err != nil {
					return err
				}
				stateSQL[i] = state
			default:
				err = moerr.NewNotSupported(ctx, fmt.Sprintf("table: %s table type: %s", tbl.Name, tbl.Kind))
				return err
			}
		}
		// Sequences must be created before the tables whose DEFAULT nextval
		// references them; views are collected and emitted after everything
		// they can select from.
		reorderDumpObjects(createTable, opt.tables, stateSQL)
		bufPool := &sync.Pool{
			New: func() any {
				return &bytes.Buffer{}
			},
		}
		for i, create := range createTable {
			tbl := opt.tables[i]
			switch tbl.Kind {
			case catalog.SystemSequenceRel:
				out.printf("DROP SEQUENCE IF EXISTS %s;\n", quoteIdentifier(tbl.Name))
				showCreateTable(out, create, false)
				if !opt.noData {
					// Replay the allocation state so a restored sequence does
					// not hand out values the source already consumed.
					out.printf("%s;\n\n\n", stateSQL[i])
				} else {
					// -no-data exports definitions only: the restored sequence
					// starts handing out values from its START value.
					out.printf("\n\n")
				}
			case catalog.SystemOrdinaryRel, emptyRelKind:
				out.printf("DROP TABLE IF EXISTS %s;\n", quoteIdentifier(tbl.Name))
				showCreateTable(out, create, false)
				if !opt.noData {
					err = genOutput(db, tbl.Name, out, bufPool, opt.netBufferLength, opt.localInfile, &opt.csvConf, opt.where)
					if err != nil {
						return err
					}
				}
			case catalog.SystemExternalRel:
				// issue-30559: a regular mysql client executes /*! ... */ content on
				// restore, and a table name containing */ would terminate the
				// block early — the external-table section emits no marker
				// comment at all, and the CREATE TABLE text is the same SQL the
				// server accepted.
				out.printf("DROP TABLE IF EXISTS %s;\n", quoteIdentifier(tbl.Name))
				showCreateTable(out, create, true)
			case catalog.SystemViewRel:
				// The definition and its creation mode were captured together above.
				continue
			default:
				err = moerr.NewNotSupported(ctx, fmt.Sprintf("table: %s table type: %s", tbl.Name, tbl.Kind))
				return err
			}
			if e := out.Err(); e != nil {
				return e
			}
		}
	}
	var mode viewSQLMode
	if len(views) > 0 {
		mode, err = getViewCatalogMode(ctx)
		if err != nil {
			return fmt.Errorf("cannot read view catalog mode: %w", err)
		}
	}
	orderedViews, err := orderDumpViews(ctx, views, mode)
	if err != nil {
		return err
	}
	for _, view := range orderedViews {
		if err := showDumpView(out, view, mode); err != nil {
			return err
		}
		if e := out.Err(); e != nil {
			return e
		}
	}
	// reset foreign_key_checks variable to dump file
	if e := out.Err(); e != nil {
		return e
	}
	out.printf("SET foreign_key_checks = 1;\n")
	return out.Err()
}

// reorderDumpObjects stably partitions one database's objects for restore:
// sequences first — a table DEFAULT nextval requires the sequence to exist at
// create time — while every other entry keeps its registry order; collected
// views produce no per-object output in this loop and are emitted globally
// after all selected databases. The parallel slices are reordered in place.
func reorderDumpObjects(createTable []string, tables Tables, stateSQL []string) {
	n := len(tables)
	counts := [2]int{}
	ranks := make([]int, n)
	for i, tbl := range tables {
		r := 1
		if tbl.Kind == catalog.SystemSequenceRel {
			r = 0
		}
		ranks[i] = r
		counts[r]++
	}
	starts := [2]int{}
	// starts[r] is the write cursor for partition r: sequences write from 0,
	// every other entry from counts[0]. next walks them in one stable pass.
	for r := 1; r < 2; r++ {
		starts[r] = starts[r-1] + counts[r-1]
	}
	next := starts
	newCreate := make([]string, n)
	newState := make([]string, n)
	newTables := make(Tables, n)
	for i := 0; i < n; i++ {
		j := next[ranks[i]]
		next[ranks[i]]++
		newCreate[j] = createTable[i]
		newState[j] = stateSQL[i]
		newTables[j] = tables[i]
	}
	copy(createTable, newCreate)
	copy(stateSQL, newState)
	copy(tables, newTables)
}

func (opt *Options) openDBConnection(ctx context.Context, database string) (*sql.DB, error) {
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%d)/%s", opt.username, opt.password, opt.host, opt.port, database)

	conn, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}

	ch := make(chan error)
	go func() {
		err := conn.Ping()
		ch <- err
	}()

	select {
	case err = <-ch:
	case <-time.After(timeout):
		return nil, moerr.NewInternalError(ctx, fmt.Sprintf("connect to %s timeout", dsn))
	}
	if err != nil {
		return nil, err
	}

	return conn, nil
}

func showCreateTable(out *dumpSink, createSql string, withNextLine bool) {
	var suffix string
	if !strings.HasSuffix(createSql, ";") {
		suffix = ";"
	}
	if withNextLine {
		suffix += "\n\n"
	}
	out.printf("%s%s\n", createSql, suffix)
}

func getDatabaseType(ctx context.Context, db string, isSys bool) (Db, error) {
	sql := "select datname, dat_type from mo_catalog.mo_database where datname = ?"
	if isSys {
		sql += " and account_id = 0"
	} else {
		// issue #30579: rows are owned by the connected account. The sys
		// account sees catalog rows of every tenant, so without this scope a
		// same-named tenant database either fails as "not exists" or leaks that
		// tenant's data. The mo_catalog row stays reachable for every account,
		// matching the server's SHOW DATABASES visibility.
		sql += " and (account_id = current_account_id() or (account_id = 0 and datname = 'mo_catalog'))"
	}
	r, err := conn.QueryContext(ctx, sql, db)
	if err != nil {
		return Db{}, err
	}
	defer r.Close()
	dbs := make([]Db, 0)
	for r.Next() {
		var dbName string
		var dbType string
		err = r.Scan(&dbName, &dbType)
		if err != nil {
			return Db{}, err
		}
		dbs = append(dbs, Db{dbName, dbType})
	}
	if err := r.Err(); err != nil {
		return Db{}, err
	}
	if len(dbs) != 1 {
		return Db{}, moerr.NewInvalidInput(ctx, fmt.Sprintf("database %s not exists", db))
	}
	return dbs[0], nil
}

func getTables(ctx context.Context, db string, tables Tables, opt Options) (Tables, error) {
	isSys := opt.sysAccount
	sql := "select relname,relkind from mo_catalog.mo_tables where reldatabase = ?"
	args := make([]any, 0, len(tables)+2)
	args = append(args, db)
	if isSys {
		sql += " and account_id = 0"
	} else {
		// issue #30579: same account scoping as the server's SHOW TABLES
		// plan: the connected account's own rows plus the always-visible catalog
		// metadata tables and cluster tables owned by the sys account.
		sql += " and (account_id = current_account_id() or (account_id = 0 and " +
			"(relname in ('mo_database','mo_tables','mo_columns') or relkind = 'cluster')))"
	}
	// Hidden index helpers are identified by their per-database registration
	// in mo_catalog.mo_indexes.index_table_name (the column exists under
	// this exact name from MO v1.2 on), joined through mo_database with the
	// same account scoping as getDatabaseType so helpers of other databases
	// can never match a same-named user object here. Name- and kind-based
	// identification were removed across review rounds 2-6: each variant
	// proved able to silently swallow named or renamed user tables behind a
	// MODUMP SUCCESS — the exact omission class this change forbids.
	// Explicit temporary relkind rows are excluded. Ordinary/empty-kind
	// legacy temporary shapes require owner confirmation below; neither a
	// server-version probe nor the historical SQL classifier proves permanence.
	// Anything that still reaches the export switch with an unsupported
	// relkind fails loudly there; the empty relkind is exported as an
	// ordinary table, with getCreateTable rejecting fragments that lack
	// the CREATE statement head.
	// #30562: user tables legally named with any `__mo_`/`%!%` prefix or
	// with the name of a catalog helper are regular dump payloads and must
	// never be skipped silently.
	sql += " and relname not in (select i.index_table_name from mo_catalog.mo_indexes i" +
		" join mo_catalog.mo_database d on i.database_id = d.dat_id" +
		" where d.datname = ?" +
		" and (d.account_id = current_account_id()" +
		" or (d.account_id = 0 and d.datname = 'mo_catalog'))" +
		" and i.index_table_name is not null and i.index_table_name <> '')" +
		" and relkind <> 'partition'" +
		" and relkind <> 'temporary_table'"
	// Rows named with the __mo_tmp_<32hex>_ shape and carrying relkind 'r'
	// or '' are the ambiguous class (review rounds 9-11): they can be
	// v4.0/v4.1 session temporary backing rows surviving an upgrade —
	// including empty-kind index rows — renamed permanent tables whose
	// requests the classifiers cannot reconstruct, or persistent tables
	// named that way legally. No metadata on any supported release, and no
	// classifier output, separates these cases reliably (the server-side
	// mo_is_legacy_temporary_table misclassifies renamed-permanent rows and
	// USE-qualified statements, and its relkind gate never inspects the
	// empty-kind index rows — review round 11). The query therefore flags
	// nothing here: the rows reach the client, and the dump aborts unless
	// the user explicitly selected each such row with -tbl or confirmed it
	// with -confirm-permanent-table. Confirmation does not change these SQL
	// predicates or narrow a whole-database dump to the confirmed names.
	args = append(args, db)
	// mo_increment_columns and __mo_account_lock (the latter has no
	// matching constant in the pinned matrixone dependency) only exist in
	// mo_catalog, and ordinary databases may legally contain same-named
	// tables or views: apply the exclusion only when dumping that catalog.
	if db == catalog.MO_CATALOG {
		sql += " and relname != '" + catalog.MOAutoIncrTable + "'" +
			" and relname != '__mo_account_lock'"
	}
	tableNames := make(map[string]bool, len(tables))
	if len(tables) > 0 {
		sql += " and relname in ("
		for i, tbl := range tables {
			if i != 0 {
				sql += ","
			}
			sql += "?"
			args = append(args, tbl.Name)
			tableNames[tbl.Name] = false
		}
		sql += ")"
	}
	confirmed := make(map[string]bool, len(opt.confirmedPermanentTables))
	for _, table := range opt.confirmedPermanentTables {
		confirmed[table.Name] = false
	}
	r, err := conn.QueryContext(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer r.Close()

	if tables == nil {
		tables = Tables{}
	}
	tables = tables[:0]
	for r.Next() {
		var table string
		var kind string
		err = r.Scan(&table, &kind)
		if err != nil {
			return nil, err
		}
		if isLegacyTemporaryShape(table, kind) {
			_, explicit := tableNames[table]
			_, ownerConfirmed := confirmed[table]
			if !explicit && !ownerConfirmed {
				return nil, moerr.NewInvalidInput(ctx, fmt.Sprintf(
					"table %s has the legacy temporary name shape; its permanent or session-temporary identity is not decidable from catalog metadata or the server classifier, "+
						"so the dump refuses to guess: if it is a permanent table, confirm it with -confirm-permanent-table for a single-database whole dump or select it with -tbl; otherwise clean up expired session temporary backing rows and dump again",
					table))
			}
			if ownerConfirmed {
				confirmed[table] = true
			}
		}
		tables = append(tables, Table{table, kind})
		tableNames[table] = true
	}
	if err := r.Err(); err != nil {
		return nil, err
	}

	for k, v := range tableNames {
		if !v {
			return nil, moerr.NewInvalidInput(ctx, fmt.Sprintf("table %s not exists", k))
		}
	}

	for _, table := range opt.confirmedPermanentTables {
		if !confirmed[table.Name] {
			return nil, moerr.NewInvalidInputf(ctx,
				"confirmed permanent table %q is not a visible legacy-temporary-shaped ordinary table in database %q", table.Name, db)
		}
	}
	return tables, nil
}

// systemDatabases are the databases the MatrixOne platform itself creates
// in every account. -db all is a whole user backup, and these relations are
// engine internals: mo_catalog alone contains every relkind, and sending
// them through the ordinary-table dispatcher fails a clean tenant's export
// (matrixorigin/matrixone#30601). The list mirrors catalog.SystemDatabases
// from the MatrixOne server.
var systemDatabases = map[string]struct{}{
	"information_schema": {},
	"mo_catalog":         {},
	"mo_debug":           {},
	"mo_task":            {},
	"mysql":              {},
	"system":             {},
	"system_metrics":     {},
}

// filterUserDatabases removes built-in system databases from a SHOW
// DATABASES result. Matching is exact: a user database whose name merely
// starts like a system name is kept.
func filterUserDatabases(dbs []string) []string {
	kept := make([]string, 0, len(dbs))
	for _, db := range dbs {
		if _, isSystem := systemDatabases[db]; isSystem {
			continue
		}
		kept = append(kept, db)
	}
	return kept
}

func getCreateDB(db string) (string, error) {
	r := conn.QueryRow("show create database " + quoteIdentifier(db))
	var create string
	err := r.Scan(&db, &create)
	if err != nil {
		return "", err
	}
	// What if it is a subscription database?
	return create, err
}

func getDatabases(ctx context.Context) ([]string, error) {
	r, err := conn.QueryContext(ctx, "show databases")
	if err != nil {
		return nil, err
	}
	if r.Err() != nil {
		return nil, r.Err()
	}
	dbs := make([]string, 0)

	for r.Next() {
		var dbName string
		err := r.Scan(&dbName)
		if err != nil {
			return nil, err
		}
		dbs = append(dbs, dbName)
	}
	defer r.Close()

	return dbs, nil
}

func getCreateTable(db, tbl string) (string, error) {
	r := conn.QueryRow("show create table " + quoteIdentifier(db) + "." + quoteIdentifier(tbl))
	var create string
	err := r.Scan(&tbl, &create)
	if err != nil {
		return "", err
	}
	// Review round 5: on some server versions (verified on MO v1.2.4) SHOW
	// CREATE TABLE returns a fragment without the statement head for tables
	// recorded with an empty relkind (e.g. restored under a colliding
	// __mo_index_* or helper name). Emitting such a fragment would produce a
	// dump that reports success but cannot be restored, so require the head
	// and fail loudly. "CREATE " (not "CREATE TABLE") also accepts the
	// CREATE EXTERNAL TABLE shape used by the external-table branch.
	if !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(create)), "CREATE ") {
		return "", moerr.NewInternalErrorNoCtxf(
			"invalid SHOW CREATE TABLE output for table `%s`.`%s`: missing CREATE statement head, got: %.80q", db, tbl, create)
	}
	return create, nil
}

func showInsert(r *sql.Rows, args []any, cols []*Column, tbl string, bufPool *sync.Pool, netBufferLength int, out *dumpSink) error {
	var err error
	buf := bufPool.Get().(*bytes.Buffer)
	defer func() {
		buf.Reset()
		bufPool.Put(buf)
	}()
	curBuf := bufPool.Get().(*bytes.Buffer)
	defer func() {
		curBuf.Reset()
		bufPool.Put(curBuf)
	}()
	buf.Grow(netBufferLength)
	initInert := "INSERT INTO " + quoteIdentifier(tbl) + " VALUES "
	for {
		buf.WriteString(initInert)
		preLen := buf.Len()
		first := true
		if curBuf.Len() > 0 {
			bts := curBuf.Bytes()
			if bts[0] == ',' {
				bts = bts[1:]
			}
			buf.Write(bts)
			curBuf.Reset()
			first = false
		}
		for r.Next() {
			err = r.Scan(args...)
			if err != nil {
				return err
			}
			if !first {
				curBuf.WriteString(",(")
			} else {
				curBuf.WriteString("(")
				first = false
			}

			for i, v := range args {
				if i > 0 {
					curBuf.WriteString(",")
				}
				if strings.ToLower(cols[i].Type) == "blob" {
					ret := *(v.(*sql.RawBytes))
					if ret == nil {
						curBuf.WriteString("NULL")
					} else {
						if len(ret) == 0 {
							curBuf.WriteString("''")
						} else {
							curBuf.WriteString(blobPrefix)
							curBuf.WriteString(fmt.Sprintf("%X", ret))
						}
					}
				} else {
					curBuf.WriteString(convertValue(v, cols[i].Type))
				}
			}
			curBuf.WriteString(")")
			if buf.Len()+curBuf.Len() >= netBufferLength {
				break
			}
			buf.Write(curBuf.Bytes())
			curBuf.Reset()
		}
		if err := r.Err(); err != nil {
			return err
		}
		if buf.Len() > preLen {
			buf.WriteString(";\n")
			_, err = buf.WriteTo(out)
			if err != nil {
				return err
			}
			continue
		}
		if curBuf.Len() > 0 {
			//matrixone#30638: nothing was emitted above; drop the unflushed
			// header so the carried-over oversized row is not preceded by a
			// duplicated INSERT statement prefix.
			buf.Reset()
			continue
		}
		break
	}
	out.printf("\n\n\n")
	return out.Err()
}

func showLoad(r *sql.Rows, rowResults []any, cols []*Column, db string, tbl string, localInfile bool, csvConf *csvConfig, out *dumpSink) error {
	// The csv data file is staged under a name unique to this run and is
	// renamed to its published name only after the whole dump succeeded, so
	// a failed run cannot replace the files of a previous dump.
	f, publishedName, err := out.startCsv(db, tbl)
	if err != nil {
		return err
	}
	err = toCsv(r, f, rowResults, cols, csvConf)
	if err == nil {
		// A failed final close must fail the dump: a silently truncated csv
		// restores as corrupted data (same class as
		// matrixorigin/matrixone#30628).
		err = f.Close()
	} else {
		_ = f.Close()
	}
	if err != nil {
		out.abortCsv(f)
		return err
	}
	// LOCAL paths follow the importing client's working directory; non-LOCAL
	// paths must be absolute for the server. Always reference the reserved final
	// name, not the staged file, and escape using the script's pinned SQL mode.
	path := publishedName
	if !localInfile {
		path, err = filepath.Abs(path)
		if err != nil {
			return err
		}
	}
	quotedPath := sqlStringLiteral(path)
	quotedDelim := sqlStringLiteral(string(csvConf.fieldDelimiter))
	if localInfile {
		out.printf("LOAD DATA LOCAL INFILE %s INTO TABLE %s FIELDS TERMINATED BY %s ENCLOSED BY '\"' LINES TERMINATED BY '\\n' PARALLEL 'FALSE';\n", quotedPath, quoteIdentifier(tbl), quotedDelim)
	} else {
		out.printf("LOAD DATA INFILE %s INTO TABLE %s FIELDS TERMINATED BY %s ENCLOSED BY '\"' LINES TERMINATED BY '\\n' PARALLEL 'FALSE';\n", quotedPath, quoteIdentifier(tbl), quotedDelim)
	}
	return out.Err()
}

// sqlStringLiteral renders s as a backslash-escaped SQL string literal. The
// dump script pins a backslash-escapes sql_mode at restore time
// (matrixorigin/matrixone#30587), so \' and \\ are always valid there.
func sqlStringLiteral(s string) string {
	var b strings.Builder
	b.WriteByte('\'')
	for i := 0; i < len(s); i++ {
		if s[i] == '\'' || s[i] == '\\' {
			b.WriteByte('\\')
		}
		b.WriteByte(s[i])
	}
	b.WriteByte('\'')
	return b.String()
}

// toCsv converts the result from mo to csv file
func toCsv(r *sql.Rows, output io.Writer, rowResults []any, cols []*Column, csvConf *csvConfig) error {
	var err error
	w := bufio.NewWriter(output)
	line := make([]string, len(rowResults))
	nulls := make([]bool, len(rowResults))

	for r.Next() {
		err = r.Scan(rowResults...)
		if err != nil {
			return err
		}
		err = toCsvLine(w, csvConf.fieldDelimiter, rowResults, cols, line, nulls, csvConf.enableEscape)
		if err != nil {
			return err
		}
	}
	if err := r.Err(); err != nil {
		return err
	}
	return w.Flush()
}

// toCsvFields converts the result from mo to string
func toCsvFields(rowResults []any, cols []*Column, line []string, nulls []bool, enableEscape bool) {
	for i, v := range rowResults {
		// SQL NULL is reported through nulls[i] instead of a magic string, so
		// a literal text value never collides with the NULL marker \N.
		if *(v.(*sql.RawBytes)) == nil {
			nulls[i] = true
			continue
		}
		nulls[i] = false
		dt, format := convertValue2(v, cols[i].Type)
		str := fmt.Sprintf(format, dt)
		line[i] = escapeString(str, enableEscape && isStringType(cols[i].Type))
	}
}

// escapeString escapes a field value under the LOAD DATA escape protocol
// (FIELDS ESCAPED BY '\'). Backslash is always escaped exactly once, so a
// literal text value such as \N cannot be mistaken for the NULL marker on
// load. escapeComma additionally escapes ',' and is enabled by -enable-escape
// for string columns.
func escapeString(s string, escapeComma bool) string {
	var builder strings.Builder
	// byte-level iteration keeps binary (non-UTF-8) values intact
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\\' || (escapeComma && c == ',') {
			builder.WriteByte('\\')
		}
		builder.WriteByte(c)
	}
	return builder.String()
}

// toCsvLine converts the result from mo to csv single line
func toCsvLine(w io.Writer, delim rune, rowResults []any, cols []*Column, line []string, nulls []bool, enableEscape bool) error {
	toCsvFields(rowResults, cols, line, nulls, enableEscape)
	return writeCsvRecord(w, line, nulls, delim)
}

// writeCsvRecord writes one CSV record terminated by '\n' in the framing the
// generated dump.sql restores with:
//
//	FIELDS TERMINATED BY <delim> ENCLOSED BY '"' LINES TERMINATED BY '\n'
//
// encoding/csv cannot express "quote exactly these fields": the SQL NULL
// marker \N must stay unquoted to load as NULL, while a literal text NULL
// must be quoted to stay a string value (both MatrixOne and MySQL read an
// unquoted NULL word as SQL NULL when FIELDS ENCLOSED BY is non-empty).
func writeCsvRecord(w io.Writer, fields []string, nulls []bool, delim rune) error {
	var b strings.Builder
	for i, f := range fields {
		if i > 0 {
			b.WriteRune(delim)
		}
		if nulls[i] {
			b.WriteString(string(nullBytes))
			continue
		}
		if csvFieldNeedsQuote(f, delim) {
			// byte-level quoting keeps binary (non-UTF-8) values intact
			b.WriteByte('"')
			b.WriteString(strings.ReplaceAll(f, "\"", "\"\""))
			b.WriteByte('"')
		} else {
			b.WriteString(f)
		}
	}
	b.WriteByte('\n')
	_, err := io.WriteString(w, b.String())
	return err
}

// csvFieldNeedsQuote reports whether a non-null field must be enclosed in
// quotes: it is empty (so a single-field record is not a skipped blank line),
// it contains the delimiter, a quote or a line break, it starts with
// whitespace (unquoted whitespace-leading and whitespace-only records are
// dropped by older loaders), or its content would otherwise be read back as
// SQL NULL.
func csvFieldNeedsQuote(field string, delim rune) bool {
	if field == "" || strings.EqualFold(field, "NULL") {
		return true
	}
	if r1, _ := utf8.DecodeRuneInString(field); unicode.IsSpace(r1) {
		return true
	}
	return strings.ContainsRune(field, delim) || strings.ContainsAny(field, "\"\r\n")
}

func genOutput(db string, tbl string, out *dumpSink, bufPool *sync.Pool, netBufferLength int, localInfile bool, csvConf *csvConfig, condition string) (err error) {
	querySql := "select * from " + quoteIdentifier(db) + "." + quoteIdentifier(tbl)
	if condition != "" {
		querySql += " where " + condition
	}

	// Match DB.Query's three-attempt budget, but retain exclusive ownership of
	// the successful connection. Never retry once a result set is returned.
	const maxQueryAttempts = 3
	var queryConn *sql.Conn
	var r *sql.Rows
	for attempt := 0; attempt < maxQueryAttempts; attempt++ {
		queryConn, err = conn.Conn(context.Background())
		if err != nil {
			return err
		}
		r, err = queryConn.QueryContext(context.Background(), querySql)
		if err == nil {
			break
		}
		_ = queryConn.Close()
		if !errors.Is(err, driver.ErrBadConn) {
			return err
		}
	}
	if err != nil {
		return err
	}
	defer queryConn.Close()
	defer func() {
		if err != nil {
			// MySQL Rows.Close drains unread results. Abort the physical connection
			// first so a local failure cannot wait for the source to send EOF.
			_ = queryConn.Raw(func(raw any) error {
				_ = raw.(driver.Conn).Close()
				// Returning ErrBadConn here would make sql.Conn wait for the rows
				// we still own. Release them before returning the connection.
				return nil
			})
		}
		_ = r.Close()
	}()
	colTypes, err := r.ColumnTypes()
	if err != nil {
		return err
	}
	cols := make([]*Column, 0, len(colTypes))
	for _, col := range colTypes {
		var c Column
		c.Name = col.Name()
		c.Type = col.DatabaseTypeName()
		cols = append(cols, &c)
	}
	rowResults := make([]any, 0, len(cols))
	for range cols {
		var v sql.RawBytes
		rowResults = append(rowResults, &v)
	}
	if !csvConf.enable {
		return showInsert(r, rowResults, cols, tbl, bufPool, netBufferLength, out)
	}
	return showLoad(r, rowResults, cols, db, tbl, localInfile, csvConf, out)
}

func convertValue(v any, typ string) string {
	ret := *(v.(*sql.RawBytes))
	if ret == nil {
		return "NULL"
	}
	typ = strings.ToLower(typ)

	if typ == "" {
		// why empty string in column type?
		// see https://github.com/matrixorigin/matrixone/issues/8050#issuecomment-1431251524 and https://github.com/matrixorigin/matrixone/issues/21903
		// now the driver cannot identify BOOL and UUID type.
		// so the current way to identify these two types is to observe the traits of values
		val := string(ret)
		if val == "true" || val == "false" {
			return val
		} else {
			str := strings.Replace(val, "\\", "\\\\", -1)
			return "'" + strings.Replace(str, "'", "\\'", -1) + "'"
		}
	}
	// bool can be somehow identified as varchar
	if typ == "varchar" && (string(ret) == "true" || string(ret) == "false") {
		return string(ret)
	}

	switch typ {
	case "float", "double":
		retStr := string(ret)
		if (retStr[0] >= '0' && retStr[0] <= '9') || (retStr[0] == '-' && retStr[1] >= '0' && retStr[1] <= '9') {
			// The wire text of a finite FLOAT that overflows the float32
			// lexical range (e.g. its max finite value) cannot be re-inserted
			// as a bare numeric literal; CAST parses it back losslessly
			// (matrixorigin/matrixone#30598).
			if typ == "float" {
				if f, err := strconv.ParseFloat(retStr, 64); err == nil && math.Abs(f) > math.MaxFloat32 {
					return "cast('" + retStr + "' as float)"
				}
			}
			// A bare -0 loses the negative-zero sign when re-inserted as a
			// numeric literal; CAST preserves it (matrixorigin/matrixone#30598).
			if retStr == "-0" {
				return "cast('-0' as " + typ + ")"
			}
			return retStr
		}
		// IEEE NaN/±Inf have no numeric SQL literal. Quoting them made INSERT
		// fail at the first row when the tokens were bare (the original
		// matrixorigin/matrixone#30596 symptom), and default MySQL-compatible
		// strict sessions additionally reject the quoted form with 1265
		// "Data truncated" since matrixorigin/matrixone#31191, so quoted
		// values cannot round-trip there either (matrixorigin/matrixone#30596).
		// A bit_cast expression rebuilds the canonical IEEE word from raw
		// bytes, so the sql_mode text rule does not apply; restore is
		// verified under the default strict, MATRIXONE_NATIVE and
		// NO_BACKSLASH_ESCAPES sessions. bit_cast itself exists in
		// MatrixOne since matrixorigin/matrixone#11441 (2023-09).
		if expr, ok := ieeeSpecialSQL(retStr, typ); ok {
			return expr
		}
		// Unknown non-numeric wire text keeps the legacy quoted form.
		return "'" + retStr + "'"
	case "int", "tinyint", "smallint", "bigint", "unsigned bigint", "unsigned int", "unsigned tinyint", "unsigned smallint", "bool", "boolean":
		return string(ret)
	case "vecf32", "vecf64":
		return string(ret)
	case "bit":
		if len(ret) == 0 {
			return "''"
		}
		return fmt.Sprintf("0x%X", ret)
	default:
		str := strings.Replace(string(ret), "\\", "\\\\", -1)
		return "'" + strings.Replace(str, "'", "\\'", -1) + "'"
	}
}

// ieeeSpecialSQL maps the FLOAT/DOUBLE wire text of NaN/+Inf/-Inf to a
// bit_cast(unhex(..)) expression that reproduces the canonical IEEE
// NaN/+Inf/-Inf words on restore, using the little-endian hex patterns
// of the server's own special-value fixtures. The text wire carries no
// NaN payload bits, so the canonical quiet-NaN word is emitted — the
// same canonical value the CSV roundtrip restores
// (matrixorigin/matrixone#30596).
func ieeeSpecialSQL(s, typ string) (string, bool) {
	ieeeHex := map[string][2]string{
		"NaN":  {"0000c07f", "000000000000f87f"}, // 0x7FC00000 / 0x7FF8000000000000
		"+Inf": {"0000807f", "000000000000f07f"}, // 0x7F800000 / 0x7FF0000000000000
		"-Inf": {"000080ff", "000000000000f0ff"}, // 0xFF800000 / 0xFFF0000000000000
	}
	pair, ok := ieeeHex[s]
	if !ok {
		return "", false
	}
	if typ == "float" {
		return "bit_cast(unhex('" + pair[0] + "') as float)", true
	}
	return "bit_cast(unhex('" + pair[1] + "') as double)", true
}

func convertValue2(v any, typ string) (sql.RawBytes, string) {
	ret := *(v.(*sql.RawBytes))
	if ret == nil {
		return nullBytes, defaultFmt
	}
	typ = strings.ToLower(typ)
	switch typ {
	case "int", "tinyint", "smallint", "bigint", "unsigned bigint", "unsigned int", "unsigned tinyint", "unsigned smallint", "double", "bool", "boolean", "", "float":
		// why empty string in column type?
		// see https://github.com/matrixorigin/matrixone/issues/8050#issuecomment-1431251524
		return ret, defaultFmt
	case "json":
		return ret, jsonFmt
	case "text":
		return ret, defaultFmt
	case "vecf32", "vecf64":
		return ret, defaultFmt
	default:
		//note: do not use the quoteFmt instead of the standard package csv,
		//it is error-prone.
		return ret, defaultFmt
	}
}

// validFieldDelimiter follows encoding/csv's delimiter rule, additionally
// excluding backslash: LOAD DATA uses it for escapes and the NULL marker \N.
func validFieldDelimiter(r rune) bool {
	return r != 0 && r != '\\' && r != '"' && r != '\r' && r != '\n' && utf8.ValidRune(r) && r != utf8.RuneError
}

// checkFieldDelimiter checks string is valid utf8 character and returns rune
func checkFieldDelimiter(ctx context.Context, s string) (rune, error) {
	if utf8.ValidString(s) {
		if utf8.RuneCountInString(s) > 1 {
			return rune(0), moerr.NewInvalidInput(ctx, "there are multiple utf8 characters for csv field delimiter. only one utf8 character is allowed")
		}
		runCh, _ := utf8.DecodeRuneInString(s)
		if !validFieldDelimiter(runCh) {
			if runCh == '\\' {
				return 0, moerr.NewInvalidInput(ctx, "csv field delimiter cannot be backslash: conflicts with LOAD DATA escapes and NULL markers")
			}
			return rune(0), moerr.NewInvalidInput(ctx, "csv field delimiter is invalid utf8 character")
		}
		return runCh, nil
	} else {
		return rune(0), moerr.NewInvalidInput(ctx, "csv field delimiter is invalid utf8 character")
	}
}

func (opt *Options) getSubScriptionTables(ctx context.Context, db string, tables Tables) (Tables, error) {
	var err error
	var hasTableOpt bool
	conn, err = opt.openDBConnection(ctx, db)
	if err != nil {
		return nil, err
	}

	tableNames := make(map[string]bool, len(tables))
	if len(tables) > 0 {
		for _, tbl := range tables {
			tableNames[tbl.Name] = false
		}
		hasTableOpt = true
	}

	r, err := conn.QueryContext(ctx, "SHOW TABLES")
	if err != nil {
		return nil, err
	}
	defer r.Close()

	if tables == nil {
		tables = Tables{}
	}
	tables = tables[:0]
	for r.Next() {
		var table string
		err = r.Scan(&table)
		if err != nil {
			return nil, err
		}

		if hasTableOpt {
			if _, ok := tableNames[table]; ok {
				tables = append(tables, Table{table, catalog.SystemOrdinaryRel})
				tableNames[table] = true
			}
		} else {
			tables = append(tables, Table{table, catalog.SystemOrdinaryRel})
			tableNames[table] = true
		}

	}
	if err := r.Err(); err != nil {
		return nil, err
	}

	for k, v := range tableNames {
		if !v {
			return nil, moerr.NewInvalidInput(ctx, fmt.Sprintf("table %s not exists", k))
		}
	}

	return tables, nil
}

func isStringType(typ string) bool {
	typ = strings.ToLower(typ)
	return typ == "text" || typ == "char" || typ == "varchar" || typ == "json"
}

// isLegacyTemporaryShape reports whether a catalog row carries the
// v4.0/v4.1 temporary backing-table shape: the physical name encodes a
// session id (__mo_tmp_<32 lowercase hex>_...) and the row's kind is one
// those releases wrote through the ordinary path ('r' or empty).
func isLegacyTemporaryShape(relName, relKind string) bool {
	if relKind != catalog.SystemOrdinaryRel && relKind != "" {
		return false
	}
	rest, ok := strings.CutPrefix(relName, "__mo_tmp_")
	if !ok {
		return false
	}
	session, _, found := strings.Cut(rest, "_")
	if !found || len(session) != 32 {
		return false
	}
	for i := 0; i < len(session); i++ {
		c := session[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
