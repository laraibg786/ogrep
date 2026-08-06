package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/laraibg786/ogrep/internal/core/domain"
)

func TestResolveRoots(t *testing.T) {
	cases := []struct {
		name              string
		pathArgs          []string
		stdinHasPipedData bool
		want              []string
	}{
		{"explicit paths pass through unchanged", []string{"a", "b"}, false, []string{"a", "b"}},
		{"explicit paths pass through unchanged even when stdin has piped data", []string{"a", "b"}, true, []string{"a", "b"}},
		{"explicit dash passes through unchanged", []string{"-"}, false, []string{"-"}},
		{"no args, stdin has piped data: implicit stdin", nil, true, []string{domain.StdinPath}},
		{"no args, stdin has nothing piped (terminal, /dev/null, closed fd): default to cwd", nil, false, []string{"."}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveRoots(tc.pathArgs, tc.stdinHasPipedData)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("resolveRoots(%v, %v) = %v, want %v", tc.pathArgs, tc.stdinHasPipedData, got, tc.want)
			}
		})
	}
}

// TestStdinHasPipedDataRejectsCharDevicesAndClosedFDs is a regression
// test for the /dev/null-in-CI bug: a character device (which /dev/null
// is, exactly like a real tty) and a closed/unreadable descriptor must
// both report false, or `ogrep pattern` with stdin redirected from
// /dev/null (the default in cron/CI/`docker run` without -i) would
// silently search empty input instead of ".".
func TestStdinHasPipedDataRejectsCharDevicesAndClosedFDs(t *testing.T) {
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("opening %s: %v", os.DevNull, err)
	}
	defer devNull.Close()
	if stdinHasPipedData(devNull) {
		t.Error("stdinHasPipedData(/dev/null) = true, want false (a character device, not real data)")
	}

	closed, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("opening %s: %v", os.DevNull, err)
	}
	closed.Close()
	if stdinHasPipedData(closed) {
		t.Error("stdinHasPipedData(closed file) = true, want false (Stat should error)")
	}
}

// TestStdinHasPipedDataAcceptsRegularFile confirms input redirected
// from a real file (`ogrep pattern < file.txt`, as opposed to a `cmd |
// ogrep` pipe) is also treated as real data to search, matching rg.
func TestStdinHasPipedDataAcceptsRegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "in.txt")
	if err := os.WriteFile(path, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if !stdinHasPipedData(f) {
		t.Error("stdinHasPipedData(regular file) = false, want true")
	}
}
