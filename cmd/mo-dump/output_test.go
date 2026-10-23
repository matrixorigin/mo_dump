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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// chdirToTemp moves the test into an empty scratch directory. The dump sink
// stages its files next to the current directory, so tests that exercise the
// staging lifecycle must not pollute the package directory.
func chdirToTemp(t *testing.T) {
	t.Helper()
	old, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(t.TempDir()))
	t.Cleanup(func() { _ = os.Chdir(old) })
}

// readDirSnapshot lists every non-directory entry of the current directory.
func readDirSnapshot(t *testing.T) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	files := make(map[string]bool, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			files[e.Name()] = true
		}
	}
	return files
}

// hasNoStagingLeftover fails when any hidden staging file remains. Published
// csv names legitimately carry the ".mo-dump-<id>" run suffix; only a
// leading dot marks staged or spool files as leftovers.
func hasNoStagingLeftover(t *testing.T) {
	t.Helper()
	for name := range readDirSnapshot(t) {
		require.False(t, strings.HasPrefix(name, "."), "staging/partial files must be cleaned: %s", name)
	}
}

// stageCsvOne stages one table csv, writes the given data and closes it.
func stageCsvOne(t *testing.T, sink *dumpSink, db, tbl string, data string) (string, string) {
	t.Helper()
	f, published, err := sink.startCsv(db, tbl)
	require.NoError(t, err)
	_, err = f.WriteString(data)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	return f.Name(), published
}

// TestDumpSinkPublishAndDiscard proves the two release paths: publish exposes
// the staged csv data and streams the SQL spool, and discard leaves no
// staging file behind.
func TestDumpSinkPublishAndDiscard(t *testing.T) {
	t.Run("publish", func(t *testing.T) {
		chdirToTemp(t)
		// A previous run's csv file under the legacy fixed name must never
		// be touched by a later dump: published names are unique per run.
		require.NoError(t, os.WriteFile(csvFileName("db", "t"), []byte("old backup\n"), 0o644))

		sink, err := newDumpSink()
		require.NoError(t, err)
		sink.printf("SET foreign_key_checks = 0;\n\n")
		require.NoError(t, sink.Err())

		staged, published := stageCsvOne(t, sink, "db", "t", "1,2\n")

		// publish streams the spool to real stdout; capture it with a pipe.
		pipeR, pipeW, err := os.Pipe()
		require.NoError(t, err)
		oldStdout := os.Stdout
		os.Stdout = pipeW
		pubErr := sink.publish()
		require.NoError(t, pipeW.Close())
		os.Stdout = oldStdout
		require.NoError(t, pubErr)

		restored := make([]byte, 64)
		n, err := pipeR.Read(restored)
		require.NoError(t, err)
		require.NoError(t, pipeR.Close())
		require.Equal(t, "SET foreign_key_checks = 0;\n\n", string(restored[:n]))

		// The staged csv was published under its unique name; both the staged
		// hidden file and the legacy previous backup are untouched.
		data, err := os.ReadFile(published)
		require.NoError(t, err)
		require.Equal(t, "1,2\n", string(data))
		require.NoFileExists(t, staged)
		legacy, err := os.ReadFile(csvFileName("db", "t"))
		require.NoError(t, err)
		require.Equal(t, "old backup\n", string(legacy), "a previous dump file must never be replaced")
		hasNoStagingLeftover(t)

		// discard after a complete publish is a no-op; the published files
		// survive.
		sink.discard()
		require.FileExists(t, published)
		require.FileExists(t, csvFileName("db", "t"))
		require.Len(t, readDirSnapshot(t), 2)
	})

	t.Run("publish rename failure removes the partial set", func(t *testing.T) {
		chdirToTemp(t)
		// A previous backup set (legacy name) that must survive a failed run.
		legacyFile := csvFileName("db", "t")
		require.NoError(t, os.WriteFile(legacyFile, []byte("old backup\n"), 0o644))

		sink := sinkOver(discardWriter{})
		_, firstPublished := stageCsvOne(t, sink, "db", "t", "new t\n")
		_, secondPublished := stageCsvOne(t, sink, "db", "v", "new v\n")
		// Make the second rename fail deterministically: its published name
		// is occupied by a directory.
		require.NoError(t, os.Mkdir(secondPublished, 0o755))

		err := sink.publish()
		var linkErr *os.LinkError
		require.ErrorAs(t, err, &linkErr)
		require.True(t, sink.csv[0].published, "first rename happened before the failure")

		sink.discard()

		// The partially published files of the failed run are gone, the
		// old backup is intact, and only the blocking directory remains.
		require.NoFileExists(t, firstPublished)
		require.NoFileExists(t, sink.csv[1].tmp)
		legacy, err := os.ReadFile(legacyFile)
		require.NoError(t, err)
		require.Equal(t, "old backup\n", string(legacy))
		require.DirExists(t, secondPublished)
		hasNoStagingLeftover(t)
	})

	t.Run("discard", func(t *testing.T) {
		chdirToTemp(t)
		sink, err := newDumpSink()
		require.NoError(t, err)
		sink.printf("DROP DATABASE IF EXISTS `victim`;")

		f, _, err := sink.startCsv("db", "t")
		require.NoError(t, err)
		abortedTemp := f.Name()
		sink.abortCsv(f)

		keptTemp, keptPublished := stageCsvOne(t, sink, "db", "v", "1\n")

		sink.discard()

		require.NoFileExists(t, abortedTemp)
		require.NoFileExists(t, keptTemp)
		require.NoFileExists(t, keptPublished)
		require.NoFileExists(t, csvFileName("db", "t"))
		require.NoFileExists(t, csvFileName("db", "v"))
		require.Empty(t, readDirSnapshot(t))
	})
}

// sinkErrWriter fails every write with a fixed error.
type sinkErrWriter struct{ err error }

func (w sinkErrWriter) Write([]byte) (int, error) { return 0, w.err }

// discardWriter accepts writes from the sink without effect.
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// TestDumpSinkWriteErrorSticky proves the first write error is captured,
// later writes stop consuming the writer, and publish refuses to publish.
func TestDumpSinkWriteErrorSticky(t *testing.T) {
	boom := os.ErrClosed
	sink := sinkOver(sinkErrWriter{err: boom})
	sink.printf("SET foreign_key_checks = 0;\n")
	require.ErrorIs(t, sink.Err(), boom)
	sink.printf("more\n") // must attempt no further write
	_, err := sink.Write([]byte("bytes"))
	require.ErrorIs(t, err, boom)
	require.ErrorIs(t, sink.publish(), boom)
}

// TestDumpSinkAbortedCsvKeepsPreviousFile covers the reason csv staging
// exists: a run that fails while writing table data must leave a previously
// published csv file intact.
func TestDumpSinkAbortedCsvKeepsPreviousFile(t *testing.T) {
	chdirToTemp(t)
	require.NoError(t, os.WriteFile(csvFileName("db", "t"), []byte("old data\n"), 0o644))

	sink := sinkOver(discardWriter{})
	f, published, err := sink.startCsv("db", "t")
	require.NoError(t, err)
	sink.abortCsv(f)
	sink.discard()

	data, err := os.ReadFile(csvFileName("db", "t"))
	require.NoError(t, err)
	require.Equal(t, "old data\n", string(data))
	require.NoFileExists(t, published)
	files := readDirSnapshot(t)
	require.Len(t, files, 1)
	require.True(t, files[csvFileName("db", "t")])
}

// TestDumpSinkSpoolFallback keeps the whole-script spool working when the
// current directory is read-only: the spool moves to the system temp dir.
func TestDumpSinkSpoolFallback(t *testing.T) {
	stageDir := t.TempDir()
	require.NoError(t, os.Chmod(stageDir, 0o555))
	oldWd, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(stageDir))
	t.Cleanup(func() { _ = os.Chdir(oldWd) })

	sink, err := newDumpSink()
	require.NoError(t, err)
	require.NotEqual(t, stageDir, filepath.Dir(sink.f.Name()), "spool moved to the system temp dir")
	sink.printf("SET foreign_key_checks = 0;\n")
	require.NoError(t, sink.Err())
	spoolName := sink.f.Name()
	sink.discard()
	require.NoFileExists(t, spoolName)
}
