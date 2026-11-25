package cbt

import (
	"slices"
	"strings"
	"testing"
)

const mib = 1 << 20

// Real output captured from `zfs send -i ds#a ds@b | zstream dump -v` (ZFS 2.3.9).
const realDump = `BEGIN record
OBJECT object = 1 type = 23 bonustype = 0 blksz = 16384 bonuslen = 0 dn_slots = 1 raw_bonuslen = 0 flags = 0 maxblkid = 0 indblkshift = 0 nlevels = 0 nblkptr = 0
FREE object = 1 offset = 8388608 length = -1
WRITE object = 1 type = 23 checksum type = 7 compression type = 0 flags = 0 offset = 1638400 logical_size = 16384 compressed_size = 0 payload_size = 16384 props = 2001f001f salt = 0000000000000000 iv = 000000000000000000000000 mac = 00000000000000000000000000000000
WRITE object = 1 type = 23 checksum type = 7 compression type = 0 flags = 0 offset = 1654784 logical_size = 16384 compressed_size = 0 payload_size = 16384 props = 2001f001f salt = 0000000000000000 iv = 000000000000000000000000 mac = 00000000000000000000000000000000
WRITE object = 1 type = 23 checksum type = 7 compression type = 0 flags = 0 offset = 1671168 logical_size = 16384 compressed_size = 0 payload_size = 16384 props = 2001f001f salt = 0000000000000000 iv = 000000000000000000000000 mac = 00000000000000000000000000000000
FREE object = 1 offset = 6291456 length = 1048576
FREE object = 2 offset = 512 length = -1
FREE object = 2 offset = 512 length = 1024
END checksum = 180b8587c10c/26d8d72603c9ddb/df1ed693c5af981f/9158b54b9767ede6
`

func TestParseRealDump(t *testing.T) {
	// 64 MiB volume, 4 MiB chunks: writes at ~1.6 MiB -> chunk 0, free at 6 MiB -> chunk 1,
	// free from 8 MiB to end -> chunks 2..15.
	r, err := Parse(strings.NewReader(realDump), 4*mib, 64*mib)
	if err != nil || r.Full {
		t.Fatalf("%+v %v", r, err)
	}
	// chunk 0: written; chunk 1 (4-8 MiB): only 1 MiB freed -> must be read;
	// chunks 2..15 entirely freed -> zero chunks without reading.
	if !slices.Equal(r.Dirty, []uint64{0, 1}) {
		t.Fatalf("dirty %v", r.Dirty)
	}
	if !slices.Equal(r.Zero, []uint64{2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}) {
		t.Fatalf("zero %v", r.Zero)
	}
	if r.Records != 5 {
		t.Fatalf("records %d", r.Records)
	}
}

func TestParseSmallChange(t *testing.T) {
	dump := "BEGIN record\nWRITE object = 1 type = 23 offset = 12582912 logical_size = 16384\n" +
		"WRITE object = 1 type = 23 offset = 16760832 logical_size = 32768\nEND checksum = x\n"
	// second write spans 16760832..16793600 -> crosses the 16 MiB boundary (chunks 3 and 4)
	r, _ := Parse(strings.NewReader(dump), 4*mib, 64*mib)
	if !slices.Equal(r.Dirty, []uint64{3, 4}) || r.ChangedByte != 49152 {
		t.Fatalf("%+v", r)
	}
}

func TestParseNoChanges(t *testing.T) {
	r, _ := Parse(strings.NewReader("BEGIN record\nEND checksum = x\n"), 4*mib, 64*mib)
	if r.Full || len(r.Dirty) != 0 {
		t.Fatalf("%+v", r)
	}
}

func TestUnsafeStreamsFallBackToFull(t *testing.T) {
	for name, dump := range map[string]string{
		"truncated":        "BEGIN record\nWRITE object = 1 offset = 0 logical_size = 16384\n",
		"no begin":         "WRITE object = 1 offset = 0 logical_size = 16384\nEND checksum = x\n",
		"freeobjects zvol": "BEGIN record\nFREEOBJECTS firstobj = 0 numobjs = 5\nEND checksum = x\n",
		"garbage offset":   "BEGIN record\nWRITE object = 1 offset = abc logical_size = 16384\nEND checksum = x\n",
	} {
		r, err := Parse(strings.NewReader(dump), 4*mib, 64*mib)
		if err != nil || !r.Full || r.Reason == "" {
			t.Errorf("%s: must fall back to full, got %+v %v", name, r, err)
		}
	}
}

func TestOtherObjectsIgnoredAndBounds(t *testing.T) {
	dump := "BEGIN record\nWRITE object = 2 offset = 0 logical_size = 16384\n" +
		"WRITE object = 1 offset = 67108864 logical_size = 16384\n" + // beyond volume
		"WRITE object = 1 offset = 67092480 logical_size = 16384\nEND checksum = x\n" // last block
	r, _ := Parse(strings.NewReader(dump), 4*mib, 64*mib)
	if !slices.Equal(r.Dirty, []uint64{15}) {
		t.Fatalf("%+v", r)
	}
	// volume size not a multiple of the chunk size: the short tail chunk is read, not zeroed
	r, _ = Parse(strings.NewReader("BEGIN record\nFREE object = 1 offset = 0 length = -1\nEND checksum = x\n"), 4*mib, 10*mib)
	if !slices.Equal(r.Zero, []uint64{0, 1}) || !slices.Equal(r.Dirty, []uint64{2}) {
		t.Fatalf("%+v", r)
	}
	// written + freed in the same chunk -> read
	r, _ = Parse(strings.NewReader("BEGIN record\nFREE object = 1 offset = 0 length = 4194304\nWRITE object = 1 offset = 16384 logical_size = 16384\nEND checksum = x\n"), 4*mib, 64*mib)
	if !slices.Equal(r.Dirty, []uint64{0}) || len(r.Zero) != 0 {
		t.Fatalf("%+v", r)
	}
}

func TestOverlappingFreesNeverZeroAPartiallyFreedChunk(t *testing.T) {
	// two FREE records covering the same 2 MiB: union is 2 MiB, chunk must be read
	dump := "BEGIN record\nFREE object = 1 offset = 0 length = 2097152\nFREE object = 1 offset = 0 length = 2097152\nEND checksum = x\n"
	r, _ := Parse(strings.NewReader(dump), 4*mib, 64*mib)
	if len(r.Zero) != 0 || !slices.Equal(r.Dirty, []uint64{0}) {
		t.Fatalf("%+v", r)
	}
	// adjacent frees covering the whole chunk -> zero
	dump = "BEGIN record\nFREE object = 1 offset = 0 length = 2097152\nFREE object = 1 offset = 2097152 length = 2097152\nEND checksum = x\n"
	r, _ = Parse(strings.NewReader(dump), 4*mib, 64*mib)
	if !slices.Equal(r.Zero, []uint64{0}) || len(r.Dirty) != 0 {
		t.Fatalf("%+v", r)
	}
	if unionLen([][2]uint64{{0, 10}, {5, 20}, {30, 40}}) != 30 {
		t.Fatal("unionLen")
	}
}
