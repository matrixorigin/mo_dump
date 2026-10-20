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
	"bytes"
	"context"
	"database/sql"
	"encoding/csv"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	_ "github.com/go-sql-driver/mysql"
	"github.com/matrixorigin/matrixone/pkg/catalog"
	"github.com/matrixorigin/matrixone/pkg/common/moerr"
)

type Options struct {
	username             string
	password             string
	host                 string
	database             string
	tbl                  string
	dbs                  []string
	tables               Tables
	port                 int
	netBufferLength      int
	toCsv                bool
	localInfile          bool
	noData               bool
	emptyTables          bool
	csvConf              csvConfig
	csvFieldDelimiterStr string
	enableEscape         bool
	where                string
	sysAccount           bool // used in getDatabaseType, getTables
}

func (t *Tables) String() string {
	return fmt.Sprint(*t)
}

func (t *Tables) Set(value string) error {
	*t = append(*t, Table{value, ""})
	return nil
}

var usage = func() {
	fmt.Fprintf(os.Stderr, "Usage: %s -u <username> -p <password> -h <host> -P <port> -db <database> [--local-infile=true] [--enable-escape=false] [-csv] [-tbl <table>...] [-no-data] -net-buffer-length <net-buffer-length>\n", os.Args[0])
	flag.PrintDefaults()
}

func main() {
	var (
		err error
		opt Options
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
			fmt.Fprintf(os.Stdout, "/* MODUMP SUCCESS, COST %v */\n", time.Since(dumpStart))
			if opt.toCsv {
				fmt.Fprintf(os.Stdout, "/* !!!MUST KEEP FILE IN CURRENT DIRECTORY, OR YOU SHOULD CHANGE THE PATH IN LOAD DATA STMT!!! */ \n")
			}
		}
	}()

	ctx := context.Background()
	flag.StringVar(&opt.username, "u", defaultUsername, "username")
	flag.StringVar(&opt.password, "p", defaultPassword, "password")
	flag.StringVar(&opt.host, "h", defaultHost, "hostname")
	flag.IntVar(&opt.port, "P", defaultPort, "portNumber")
	flag.IntVar(&opt.netBufferLength, "net-buffer-length", defaultNetBufferLength, "net-buffer-length used to set the buffer size for network connections")
	flag.StringVar(&opt.database, "db", "", "databaseName, must be specified")
	flag.StringVar(&opt.tbl, "tbl", "", "tableNameList (default all)")
	flag.BoolVar(&opt.toCsv, "csv", defaultCsv, "set export format to csv (default false)")
	flag.StringVar(&opt.csvFieldDelimiterStr, "csv-field-delimiter", string(defaultFieldDelimiter), "set csv field delimiter (only one utf8 character). enabled only when the option 'csv' is set.")
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

	if len(opt.database) == 0 {
		err = moerr.NewInvalidInput(ctx, "database must be specified")
		return
	} else {
		dbs := strings.Split(opt.database, ",")
		for _, db := range dbs {
			db = strings.TrimSpace(db)
			if len(db) != 0 {
				opt.dbs = append(opt.dbs, db)
			}
		}
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
		conn, err = opt.openDBConnection(ctx, "")
		if err != nil {
			return
		}
		defer conn.Close()

		opt.dbs, err = getDatabases(ctx)
		if err != nil {
			return
		}
	}

	err = opt.dumpData(ctx)
	if err != nil {
		return
	}
}

func (opt *Options) dumpData(ctx context.Context) error {
	var (
		createDb    string
		createTable []string
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
	fmt.Printf("SET foreign_key_checks = 0;\n\n")

	for _, db := range opt.dbs {
		var dbStruct Db
		dbStruct, err = getDatabaseType(ctx, db, opt.sysAccount)
		if err != nil {
			return err
		}

		if opt.emptyTables {
			opt.tables = nil
		}
		if len(opt.tables) == 0 { //dump all tables
			if dbStruct.DBType == catalog.SystemDBTypeSubscription {
				createDb = fmt.Sprintf("CREATE DATABASE IF NOT EXISTS `%s`", db)
			} else {
				createDb, err = getCreateDB(db)
				if err != nil {
					return err
				}
			}
			fmt.Printf("DROP DATABASE IF EXISTS `%s`;\n", db)
			fmt.Println(createDb, ";")
			fmt.Printf("USE `%s`;\n\n\n", db)
		}

		if dbStruct.DBType == catalog.SystemDBTypeSubscription {
			opt.tables, err = opt.getSubScriptionTables(ctx, db, opt.tables)
			if err != nil {
				return err
			}
		} else {
			opt.tables, err = getTables(ctx, db, opt.tables, opt.sysAccount)
			if err != nil {
				return err
			}
		}
		createTable = make([]string, len(opt.tables))
		for i, tbl := range opt.tables {
			var createTableSql string
			switch tbl.Kind {
			case catalog.SystemOrdinaryRel:
				createTableSql, err = getCreateTable(db, tbl.Name)
				if err != nil {
					return err
				}
			case catalog.SystemExternalRel:
				createTableSql, err = getCreateTable(db, tbl.Name)
				if err != nil {
					return err
				}
			case catalog.SystemViewRel:
				createTableSql, err = getCreateView(db, tbl.Name)
				if err != nil {
					return err
				}
			default:
				err = moerr.NewNotSupported(ctx, fmt.Sprintf("table: %s table type: %s", tbl.Name, tbl.Kind))
				return err
			}
			createTable[i] = createTableSql
		}
		bufPool := &sync.Pool{
			New: func() any {
				return &bytes.Buffer{}
			},
		}
		left, right := 0, len(createTable)-1
		for left < right {
			for left < len(createTable) && opt.tables[left].Kind != catalog.SystemViewRel {
				left++
			}
			for right >= 0 && opt.tables[right].Kind == catalog.SystemViewRel {
				right--
			}
			if left >= right {
				break
			}
			createTable[left], createTable[right] = createTable[right], createTable[left]
			opt.tables[left], opt.tables[right] = opt.tables[right], opt.tables[left]
		}
		adjustViewOrder(createTable, opt.tables, left)
		for i, create := range createTable {
			tbl := opt.tables[i]
			switch tbl.Kind {
			case catalog.SystemOrdinaryRel:
				fmt.Printf("DROP TABLE IF EXISTS `%s`;\n", tbl.Name)
				showCreateTable(create, false)
				if !opt.noData {
					err = genOutput(db, tbl.Name, bufPool, opt.netBufferLength, opt.localInfile, &opt.csvConf, opt.where)
					if err != nil {
						return err
					}
				}
			case catalog.SystemExternalRel:
				fmt.Printf("/*!EXTERNAL TABLE `%s`*/\n", tbl.Name)
				fmt.Printf("DROP TABLE IF EXISTS `%s`;\n", tbl.Name)
				showCreateTable(create, true)
			case catalog.SystemViewRel:
				fmt.Printf("DROP VIEW IF EXISTS `%s`;\n", tbl.Name)
				showCreateTable(create, true)
			default:
				err = moerr.NewNotSupported(ctx, fmt.Sprintf("table: %s table type: %s", tbl.Name, tbl.Kind))
				return err
			}
		}
	}
	// reset foreign_key_checks variable to dump file
	fmt.Println("SET foreign_key_checks = 1;")
	return err
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

func adjustViewOrder(createTable []string, tables Tables, start int) {
	viewName := make([]string, 0)
	viewPos := make(map[string]int)
	cnt := len(tables)
	for i := start; i < cnt; i++ {
		viewPos[tables[i].Name] = i - start
		viewName = append(viewName, tables[i].Name)
	}
	viewCount := make([]int, len(viewName))
	viewRef := make([][]int, len(viewName))
	for i := start; i < cnt; i++ {
		for j := start; j < cnt; j++ {
			if i == j {
				continue
			}
			if strings.Count(createTable[i], tables[j].Name) > 0 {
				viewCount[viewPos[tables[i].Name]]++
				viewRef[viewPos[tables[j].Name]] = append(viewRef[viewPos[tables[j].Name]], viewPos[tables[i].Name])
			}
		}
	}
	order := 0
	orderArr := make([]int, 0)
	visit := make([]bool, len(viewName))
	for order < len(viewName) {
		for i := 0; i < len(viewName); i++ {
			if viewCount[i] == 0 && !visit[i] {
				visit[i] = true
				order++
				orderArr = append(orderArr, i)
				for j := 0; j < len(viewRef[i]); j++ {
					viewCount[viewRef[i][j]]--
				}
			}
		}
	}
	newCreate := make([]string, cnt)
	newTables := make([]Table, cnt)
	for i := 0; i < len(orderArr); i++ {
		newCreate[i] = createTable[orderArr[i]+start]
		newTables[i] = tables[orderArr[i]+start]
	}
	_ = copy(createTable[start:], newCreate)
	_ = copy(tables[start:], newTables)
}

func showCreateTable(createSql string, withNextLine bool) {
	var suffix string
	if !strings.HasSuffix(createSql, ";") {
		suffix = ";"
	}
	if withNextLine {
		suffix += "\n\n"
	}
	fmt.Printf("%s%s\n", createSql, suffix)
}
func getDatabaseType(ctx context.Context, db string, isSys bool) (Db, error) {
	sql := "select datname, dat_type from mo_catalog.mo_database where datname = '" + db + "'"
	if isSys {
		sql += " and account_id = 0"
	}
	r, err := conn.Query(sql)
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
	if len(dbs) != 1 {
		return Db{}, moerr.NewInvalidInput(ctx, fmt.Sprintf("database %s not exists", db))
	}
	return dbs[0], nil
}

func getTables(ctx context.Context, db string, tables Tables, isSys bool) (Tables, error) {
	sql := "select relname,relkind from mo_catalog.mo_tables where reldatabase = '" + db + "'"
	if isSys {
		sql += " and account_id = 0"
	}
	tableNames := make(map[string]bool, len(tables))
	if len(tables) > 0 {
		sql += " and relname in ("
		for i, tbl := range tables {
			if i != 0 {
				sql += ","
			}
			sql += "'" + tbl.Name + "'"
			tableNames[tbl.Name] = false
		}
		sql += ")"
	}
	r, err := conn.Query(sql) //TODO: after unified sys table prefix, add condition in where clause
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
		if strings.HasPrefix(table, "__mo_") || strings.HasPrefix(table, "%!%") { //TODO: after adding condition in where clause, remove this
			continue
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

	return tables, nil
}

func getCreateDB(db string) (string, error) {
	r := conn.QueryRow("show create database `" + db + "`")
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
	r := conn.QueryRow("show create table `" + db + "`.`" + tbl + "`")
	var create string
	err := r.Scan(&tbl, &create)
	if err != nil {
		return "", err
	}
	return create, nil
}

func getCreateView(db, tbl string) (string, error) {
	r := conn.QueryRow("show create table `" + db + "`.`" + tbl + "`")
	var create string
	var character_set_client string
	var collation_connection string
	err := r.Scan(&tbl, &create, &character_set_client, &collation_connection)
	if err != nil {
		return "", err
	}
	return create, nil
}

func showInsert(r *sql.Rows, args []any, cols []*Column, tbl string, bufPool *sync.Pool, netBufferLength int) error {
	var err error
	buf := bufPool.Get().(*bytes.Buffer)
	curBuf := bufPool.Get().(*bytes.Buffer)
	buf.Grow(netBufferLength)
	initInert := "INSERT INTO `" + tbl + "` VALUES "
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
		if buf.Len() > preLen {
			buf.WriteString(";\n")
			_, err = buf.WriteTo(os.Stdout)
			if err != nil {
				return err
			}
			continue
		}
		if curBuf.Len() > 0 {
			continue
		}
		buf.Reset()
		curBuf.Reset()
		break
	}
	bufPool.Put(buf)
	bufPool.Put(curBuf)
	fmt.Printf("\n\n\n")
	return nil
}

func showLoad(r *sql.Rows, rowResults []any, cols []*Column, db string, tbl string, localInfile bool, csvConf *csvConfig) error {
	fname := fmt.Sprintf("%s_%s.%s", db, tbl, "csv")
	pwd := os.Getenv("PWD")
	f, err := os.Create(fname)
	if err != nil {
		return err
	}
	defer f.Close()

	err = toCsv(r, f, rowResults, cols, csvConf)
	if err != nil {
		return err
	}
	//#30590: path and delimiter are interpolated as SQL string literals;
	// double-quote escaping keeps them valid in both sql_modes.
	fileLiteral := sqlStringLiteral(fmt.Sprintf("%s/%s", pwd, fname))
	delimLiteral := sqlStringLiteral(string(csvConf.fieldDelimiter))
	if localInfile {
		fmt.Printf("LOAD DATA LOCAL INFILE %s INTO TABLE `%s` FIELDS TERMINATED BY %s ENCLOSED BY '\"' LINES TERMINATED BY '\\n' PARALLEL 'FALSE';\n", fileLiteral, tbl, delimLiteral)
	} else {
		fmt.Printf("LOAD DATA INFILE %s INTO TABLE `%s` FIELDS TERMINATED BY %s ENCLOSED BY '\"' LINES TERMINATED BY '\\n' PARALLEL 'FALSE';\n", fileLiteral, tbl, delimLiteral)
	}
	return nil
}

// toCsv converts the result from mo to csv file
func toCsv(r *sql.Rows, output io.Writer, rowResults []any, cols []*Column, csvConf *csvConfig) error {
	var err error
	csvWriter := csv.NewWriter(output)
	csvWriter.Comma = csvConf.fieldDelimiter
	line := make([]string, len(rowResults))

	for r.Next() {
		err = r.Scan(rowResults...)
		if err != nil {
			return err
		}
		err = toCsvLine(csvWriter, rowResults, cols, line, csvConf.enableEscape)
		if err != nil {
			return err
		}
	}
	return err
}

// toCsvFields converts the result from mo to string
func toCsvFields(rowResults []any, cols []*Column, line []string, enableEscape bool) {
	for i, v := range rowResults {
		dt, format := convertValue2(v, cols[i].Type)
		str := fmt.Sprintf(format, dt)
		if enableEscape && isStringType(cols[i].Type) && str != string(nullBytes) {
			str = escapeChars(str)
		}

		line[i] = escapeString(str)
	}
}

func escapeString(s string) string {
	if s == "\\N" {
		return s
	}
	var builder strings.Builder
	for _, char := range s {
		if char == '\\' {
			builder.WriteString("\\")
		}
		builder.WriteRune(char)
	}
	return builder.String()
}

// toCsvLine converts the result from mo to csv single line
func toCsvLine(csvWriter *csv.Writer, rowResults []any, cols []*Column, line []string, enableEscape bool) error {
	var err error
	toCsvFields(rowResults, cols, line, enableEscape)
	err = csvWriter.Write(line)
	if err != nil {
		return err
	}
	csvWriter.Flush()
	return err
}

func genOutput(db string, tbl string, bufPool *sync.Pool, netBufferLength int, localInfile bool, csvConf *csvConfig, condition string) error {
	querySql := "select * from `" + db + "`.`" + tbl + "`"
	if condition != "" {
		querySql += " where " + condition
	}
	r, err := conn.Query(querySql)
	if err != nil {
		return err
	}
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
		return showInsert(r, rowResults, cols, tbl, bufPool, netBufferLength)
	}
	return showLoad(r, rowResults, cols, db, tbl, localInfile, csvConf)
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
		}
		return quoteSQLStringLiteral(val)
	}
	// bool can be somehow identified as varchar
	if typ == "varchar" && (string(ret) == "true" || string(ret) == "false") {
		return string(ret)
	}

	switch typ {
	case "float", "double":
		//#30596/#30598: NaN/±Inf are not valid unquoted numeric literals,
		// near-max FLOAT text parses out of range on restore, and -0
		// loses its sign; those must stay quoted.
		return floatSQLValue(string(ret), typ == "float")
	case "int", "tinyint", "smallint", "bigint", "unsigned bigint", "unsigned int", "unsigned tinyint", "unsigned smallint", "bool", "boolean":
		return string(ret)
	case "vecf32", "vecf64":
		return string(ret)
	case "bit":
		//#30587: \0 escapes break under NO_BACKSLASH_ESCAPES and a raw
		// quote byte would close the literal; join _binary pieces with
		// CHAR(0) instead.
		return bitSQLValue(ret)
	default:
		//#30587: backslash-escaped literals break under
		// NO_BACKSLASH_ESCAPES; emit a sql_mode-independent token.
		return quoteSQLStringLiteral(string(ret))
	}
}

// floatSQLValue renders FLOAT/DOUBLE wire text as a restorable SQL token.
// NaN and ±Inf are not valid unquoted numeric literals; a magnitude above
// MaxFloat32 is rejected on restore when parsed as an unquoted FLOAT literal;
// and an unquoted -0 restores as positive zero. Such values are emitted as
// quoted string literals, which the restore cast reproduces exactly — the
// same path the CSV roundtrip uses. (matrixone#30596, matrixone#30598)
func floatSQLValue(val string, isFloat bool) string {
	f, err := strconv.ParseFloat(val, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return sqlStringLiteral(val)
	}
	if f == 0 && strings.HasPrefix(val, "-") {
		return sqlStringLiteral(val)
	}
	if isFloat && (f > math.MaxFloat32 || f < -math.MaxFloat32) {
		return sqlStringLiteral(val)
	}
	return val
}

// sqlStringLiteral quotes s as a SQL string literal using doubled single
// quotes, which parses the same with and without NO_BACKSLASH_ESCAPES.
func sqlStringLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// quoteSQLStringLiteral builds a SQL string token equal to s whose parsing
// does not depend on sql_mode: single quotes are doubled, and each backslash
// byte is carried by CHAR(92), so no backslash escape is ever relied upon.
// (matrixone#30587)
func quoteSQLStringLiteral(s string) string {
	parts := strings.Split(s, "\\")
	if len(parts) == 1 {
		return sqlStringLiteral(s)
	}
	lits := make([]string, len(parts))
	for i, p := range parts {
		lits[i] = sqlStringLiteral(p)
	}
	return "CONCAT(" + strings.Join(lits, ",CHAR(92),") + ")"
}

// bitSQLValue encodes BIT bytes as _binary literal pieces joined by CHAR(0):
// the \0 escape breaks under NO_BACKSLASH_ESCAPES, and a raw quote byte would
// close the literal early. (matrixone#30587)
func bitSQLValue(raw []byte) string {
	parts := bytes.Split(raw, []byte{0})
	lits := make([]string, len(parts))
	for i, p := range parts {
		lits[i] = "_binary " + sqlStringLiteral(string(p))
	}
	if len(lits) == 1 {
		return lits[0]
	}
	return "CONCAT(" + strings.Join(lits, ",CHAR(0),") + ")"
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

// checkFieldDelimiter checks string is valid utf8 character and returns rune
func checkFieldDelimiter(ctx context.Context, s string) (rune, error) {
	if utf8.ValidString(s) {
		if utf8.RuneCountInString(s) > 1 {
			return rune(0), moerr.NewInvalidInput(ctx, "there are multiple utf8 characters for csv field delimiter. only one utf8 character is allowed")
		}
		runCh, _ := utf8.DecodeRuneInString(s)
		if runCh == utf8.RuneError {
			return rune(0), moerr.NewInvalidInput(ctx, "csv field delimiter is invalid utf8 character")
		}
		//#30590: a backslash delimiter has no SQL literal form for the
		// generated LOAD DATA that parses consistently across sql_modes;
		// refuse it instead of claiming a successful unparsable export.
		if runCh == '\\' {
			return rune(0), moerr.NewInvalidInput(ctx, "csv field delimiter cannot be a backslash")
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

// escapeChars
func escapeChars(text string) string {
	charSet := ""
	for _, char := range escapeCharSet {
		charSet += regexp.QuoteMeta(string(char))
	}
	re := regexp.MustCompile(`[` + charSet + `]`)
	return re.ReplaceAllStringFunc(text, func(s string) string {
		return "\\" + s
	})
}

func isStringType(typ string) bool {
	typ = strings.ToLower(typ)
	return typ == "text" || typ == "char" || typ == "varchar" || typ == "json"
}
