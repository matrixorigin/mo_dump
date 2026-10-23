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
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// spoolPrefix names the hidden staging file that holds the complete SQL
// script until the whole dump succeeded. A failed dump never publishes a
// half script, so a restore can not drop a database it then fails to
// recreate (matrixorigin/matrixone#30577).
const spoolPrefix = ".mo-dump-script-"

// csvArtifact tracks one staged csv file on its way to publish or discard.
type csvArtifact struct {
	tmp       string // staged hidden path
	dst       string // published path
	published bool   // renamed to dst
	dropped   bool   // removed early or during discard
}

// dumpSink owns everything a single dump run writes: the SQL script, staged
// in a temporary file until the run completes, and per-table csv data files,
// staged beside their published name. The sink records write errors
// (bufio.Writer hides them until flush) so the run can stop early.
type dumpSink struct {
	w  io.Writer     // SQL text destination
	bw *bufio.Writer // buffered wrapper over f, nil for direct sinks
	f  *os.File      // SQL spool file, nil for direct sinks
	// csv ports the staged per-table csv files; publishedAll is set only
	// after the complete run was published, so a discard triggered by a
	// failed run also removes files that were already renamed before the
	// failure: a partial new backup set never survives.
	csv          []csvArtifact
	publishedAll bool
	err          error // first write error, sticky
}

// newDumpSink creates the staging owner for one dump run. The spool goes to
// the current directory when it is writable — same volume as the csv files
// and the operator's outputs, and not limited by a small tmpfs TMPDIR — with
// the system temp directory as fallback.
func newDumpSink() (*dumpSink, error) {
	f, err := os.CreateTemp(".", spoolPrefix+"*")
	if err != nil {
		f, err = os.CreateTemp("", spoolPrefix+"*")
		if err != nil {
			return nil, err
		}
	}
	bw := bufio.NewWriterSize(f, defaultNetBufferLength)
	return &dumpSink{w: bw, bw: bw, f: f}, nil
}

// sinkOver returns a direct, unbuffered sink over w. It is the seam tests
// use to observe output without the staging lifecycle.
func sinkOver(w io.Writer) *dumpSink {
	return &dumpSink{w: w}
}

// fail records the first write error; later writes become no-ops.
func (s *dumpSink) fail(err error) {
	if err != nil && s.err == nil {
		s.err = err
	}
}

// Err returns the first write error, if any.
func (s *dumpSink) Err() error {
	return s.err
}

// printf writes one piece of the SQL script, recording the first error.
func (s *dumpSink) printf(format string, a ...any) {
	if s.err != nil {
		return
	}
	if _, err := fmt.Fprintf(s.w, format, a...); err != nil {
		s.fail(err)
	}
}

// Write implements io.Writer for staged data streams such as INSERT batches.
func (s *dumpSink) Write(p []byte) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	n, err := s.w.Write(p)
	if err != nil {
		s.fail(err)
	}
	return n, err
}

// csvFileName is the published name of one table's csv data file, the one
// the generated LOAD DATA statement references.
func csvFileName(db, tbl string) string {
	return fmt.Sprintf("%s_%s.%s", db, tbl, "csv")
}

// startCsv stages the csv data file of one table in a hidden temporary file.
// It returns the staged file and the name it will be published under after
// the whole run succeeds; the LOAD DATA statement references that name.
// Both names are resolved against the current directory, which is also
// where the final file has to live.
func (s *dumpSink) startCsv(db, tbl string) (*os.File, string, error) {
	f, err := os.CreateTemp(".", "."+csvFileName(db, tbl)+".mo-dump-")
	if err != nil {
		return nil, "", err
	}
	// CreateTemp returns the directory-qualified name ("./.<file>"); work
	// on the base name alone and strip exactly the one leading dot that hides
	// the staged file.
	staged := filepath.Base(f.Name())
	dst := strings.TrimPrefix(staged, ".")
	s.csv = append(s.csv, csvArtifact{tmp: staged, dst: dst})
	return f, dst, nil
}

// abortCsv removes a staged csv file after the table failed; a later
// failed-run discard must not remove it again.
func (s *dumpSink) abortCsv(f *os.File) {
	name := filepath.Base(f.Name())
	_ = f.Close()
	_ = os.Remove(name)
	for i := range s.csv {
		if s.csv[i].tmp == name {
			s.csv[i].dropped = true
		}
	}
}

// discard drops every artifact a failed run leaves behind: the staged SQL
// script, all staged csv files, and any csv file already published by a
// partially succeeded publish — a new backup set that was never completed
// must vanish rather than coexist with the operator's previous files. After
// a complete publish it is a no-op: nothing is owned anymore.
func (s *dumpSink) discard() {
	if s.publishedAll {
		return
	}
	if s.f != nil {
		_ = s.f.Close()
		_ = os.Remove(s.f.Name())
		s.f = nil
	}
	for i := range s.csv {
		if s.csv[i].dst != "" && s.csv[i].published {
			_ = os.Remove(s.csv[i].dst)
			s.csv[i].published = false
		}
		if s.csv[i].dropped {
			continue
		}
		_ = os.Remove(s.csv[i].tmp)
		s.csv[i].dropped = true
	}
}

// publish makes the completed dump visible: csv data files are renamed to
// their published name first — every name is unique to this run, so previous
// dump files can never be overwritten — then the SQL script is streamed to
// stdout. Any failure before the end leaves the removal to discard.
func (s *dumpSink) publish() error {
	if s.err != nil {
		return s.err
	}
	if s.bw != nil {
		if err := s.bw.Flush(); err != nil {
			return err
		}
	}
	for i := range s.csv {
		a := &s.csv[i]
		if a.dropped || a.published {
			continue
		}
		if err := os.Rename(a.tmp, a.dst); err != nil {
			return err
		}
		a.published = true
	}
	if s.f != nil {
		if _, err := s.f.Seek(0, io.SeekStart); err != nil {
			return err
		}
		if _, err := io.Copy(os.Stdout, s.f); err != nil {
			return err
		}
		if err := s.f.Close(); err != nil {
			return err
		}
		_ = os.Remove(s.f.Name())
		s.f = nil
	}
	s.publishedAll = true
	return nil
}
