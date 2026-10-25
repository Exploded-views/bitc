package agent

import (
	"fmt"
	"strings"
	"testing"
)

func TestTailBufKeepsOnlyTheLast64KiB(t *testing.T) {
	var b tailBuf
	chunk := strings.Repeat("x", 1000)
	for i := 0; i < 5000; i++ {
		fmt.Fprintf(&b, "%s%04d\n", chunk, i)
	}
	if len(b.b) > 2*stderrTail {
		t.Errorf("tail buffer holds %d bytes, want at most %d", len(b.b), 2*stderrTail)
	}
	got := b.String()
	if len(got) != stderrTail || !strings.HasSuffix(got, "4999\n") {
		t.Errorf("tail is %d bytes ending %q, want %d ending 4999", len(got), got[len(got)-6:], stderrTail)
	}
	var one tailBuf
	_, _ = one.Write([]byte(strings.Repeat("y", 3*stderrTail)))
	if len(one.String()) != stderrTail || len(one.b) > stderrTail {
		t.Errorf("one huge write left %d bytes", len(one.b))
	}
}

func TestListingWriterStopsAtTheLimitWithBoundedMemory(t *testing.T) {
	w := newListingWriter(1<<40, inodeSpare+1000)
	rec := "100644 blob 0123456789012345678901234567890123456789      12\tsome/path/file\x00"
	data := []byte(strings.Repeat(rec, 5000))
	for off := 0; off < len(data); off += 7 { // awkward chunking splits records
		end := min(off+7, len(data))
		if _, err := w.Write(data[off:end]); err != nil {
			t.Fatal(err)
		}
		if len(w.partial) > len(rec) {
			t.Fatalf("partial record buffer grew to %d bytes", len(w.partial))
		}
	}
	select {
	case <-w.Stopped():
	default:
		t.Fatalf("the writer never asked to stop")
	}
	if w.entries != 1000 {
		t.Errorf("counted %d entries, want it to stop at exactly 1000", w.entries)
	}
	if err := w.refusal(); err == nil || err.Error() != "checkout would exceed inodes" {
		t.Errorf("refusal %v", err)
	}
}

func TestListingWriterSaturatesInsteadOfWrapping(t *testing.T) {
	w := newListingWriter(^uint64(0), ^uint64(0))
	w.entries, w.bytesNeeded = ^uint64(0)-1, ^uint64(0)-1
	if _, err := w.Write([]byte("100644 blob 0123456789012345678901234567890123456789 4611686018427387903\ta\x00")); err != nil {
		t.Fatal(err)
	}
	if w.entries != ^uint64(0) || w.bytesNeeded != ^uint64(0) {
		t.Errorf("entries %d bytes %d, want both saturated", w.entries, w.bytesNeeded)
	}
	if w.refusal() == nil {
		t.Errorf("a saturated total was not refused")
	}
}
