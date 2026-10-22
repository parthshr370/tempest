package toolio

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrivateSpillStartsAtByteZeroAndCapsPrefix(t *testing.T) {
	path := filepath.Join(t.TempDir(), "output.log")
	accumulator := NewOutputAccumulator(OutputAccumulatorOptions{MaxBytes: 4, MaxLines: 10, SpillPath: path, MaxTotalBytes: 8})
	if err := accumulator.OpenSpill(); err != nil {
		t.Fatal(err)
	}
	if err := accumulator.Append([]byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := accumulator.Append([]byte("second")); !errors.Is(err, ErrOutputLimit) {
		t.Fatal(err)
	}
	if err := accumulator.Append([]byte("no")); !errors.Is(err, ErrOutputLimit) {
		t.Fatal(err)
	}
	if err := accumulator.Finish(); err != nil {
		t.Fatal(err)
	}
	if err := accumulator.Sync(); err != nil {
		t.Fatal(err)
	}
	snapshot, err := accumulator.Snapshot(SnapshotOptions{})
	if err != nil || !snapshot.Truncated {
		t.Fatal(snapshot, err)
	}
	if err = accumulator.CloseTempFile(); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil || string(content) != "firstsec" {
		t.Fatal(string(content), err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal(info, err)
	}
}
func TestSpillExclusiveAndMemoryBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "output.log")
	first := NewOutputAccumulator(OutputAccumulatorOptions{SpillPath: path, MaxBytes: 64 << 10, MaxTotalBytes: 16 << 20})
	if err := first.OpenSpill(); err != nil {
		t.Fatal(err)
	}
	defer first.CloseTempFile()
	second := NewOutputAccumulator(OutputAccumulatorOptions{SpillPath: path})
	if err := second.OpenSpill(); err == nil {
		t.Fatal("overwrote existing log")
	}
	chunk := []byte(strings.Repeat("x", 4096))
	for range 1024 {
		if err := first.Append(chunk); err != nil {
			t.Fatal(err)
		}
		if len(first.rawChunks) != 0 || len(first.tailText) > 256<<10 {
			t.Fatal("unbounded output memory")
		}
	}
}
func TestOutputWriteErrorIsNotSuccess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "output.log")
	accumulator := NewOutputAccumulator(OutputAccumulatorOptions{SpillPath: path})
	if err := accumulator.OpenSpill(); err != nil {
		t.Fatal(err)
	}
	if err := accumulator.tempFile.Close(); err != nil {
		t.Fatal(err)
	}
	if err := accumulator.Append([]byte("failed")); err == nil {
		t.Fatal("ignored disk write failure")
	}
}
