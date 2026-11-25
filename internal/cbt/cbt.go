// Package cbt derives changed-block information for ZFS volumes from an
// incremental send stream (`zfs send -i <bookmark> <snapshot> | zstream dump -v`).
//
// ZFS volumes store their data in object 1. WRITE records name byte ranges
// whose content changed; FREE records name ranges that became holes (zeros).
// Both are dirty for backup purposes. Anything that cannot be interpreted
// with certainty makes the result "full" (caller must read everything).
package cbt

import (
	"bufio"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// ZvolDataObject is the DMU object holding a zvol's data.
const ZvolDataObject = 1

// Result is the set of dirty chunks for a volume of a given size.
type Result struct {
	Full        bool     // change set unknown: back up everything
	Reason      string   // why Full is set
	Dirty       []uint64 // chunks that must be read and written
	Zero        []uint64 // chunks entirely freed (holes) and not written: write the zero chunk without reading
	Records     int
	ChangedByte uint64 // bytes covered by WRITE/FREE records
	SawEnd      bool
}

// Parse reads `zstream dump -v` output and returns the dirty chunk indexes
// for chunkSize-aligned chunks of a volume with volSize bytes.
func Parse(r io.Reader, chunkSize, volSize uint64) (*Result, error) {
	if chunkSize == 0 || volSize == 0 {
		return nil, fmt.Errorf("invalid chunk size %d / volume size %d", chunkSize, volSize)
	}
	nChunks := (volSize + chunkSize - 1) / chunkSize
	written := map[uint64]bool{}
	freed := map[uint64][][2]uint64{} // chunk -> freed [start,end) intervals
	res := &Result{}
	clip := func(off, length uint64) (uint64, uint64, bool) {
		if off >= volSize || length == 0 {
			return 0, 0, false
		}
		end := off + length
		if end > volSize || end < off {
			end = volSize
		}
		return off, end, true
	}
	mark := func(off, length uint64) {
		off, end, ok := clip(off, length)
		if !ok {
			return
		}
		res.ChangedByte += end - off
		for c := off / chunkSize; c <= (end-1)/chunkSize && c < nChunks; c++ {
			written[c] = true
		}
	}
	markFree := func(off, length uint64) {
		off, end, ok := clip(off, length)
		if !ok {
			return
		}
		res.ChangedByte += end - off
		for c := off / chunkSize; c <= (end-1)/chunkSize && c < nChunks; c++ {
			cs, ce := c*chunkSize, min((c+1)*chunkSize, volSize)
			freed[c] = append(freed[c], [2]uint64{max(off, cs), min(end, ce)})
		}
	}
	sawBegin := false
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		rec, kv := parseLine(line)
		switch rec {
		case "BEGIN":
			sawBegin = true
		case "END":
			res.SawEnd = true
		case "WRITE", "WRITE_EMBEDDED", "WRITE_BYREF":
			if kv["object"] != "1" {
				continue
			}
			off, err1 := strconv.ParseUint(kv["offset"], 10, 64)
			size := kv["logical_size"]
			if size == "" {
				size = kv["length"]
			}
			n, err2 := strconv.ParseUint(size, 10, 64)
			if err1 != nil || err2 != nil {
				return fullf(res, "unparsable %s record: %q", rec, line), nil
			}
			res.Records++
			mark(off, n)
		case "FREE":
			if kv["object"] != "1" {
				continue
			}
			off, err := strconv.ParseUint(kv["offset"], 10, 64)
			if err != nil {
				return fullf(res, "unparsable FREE record: %q", line), nil
			}
			res.Records++
			if kv["length"] == "-1" {
				if off < volSize {
					markFree(off, volSize-off)
				}
				continue
			}
			n, err := strconv.ParseUint(kv["length"], 10, 64)
			if err != nil {
				return fullf(res, "unparsable FREE length: %q", line), nil
			}
			markFree(off, n)
		case "FREEOBJECTS":
			first, _ := strconv.ParseUint(kv["firstobj"], 10, 64)
			num, _ := strconv.ParseUint(kv["numobjs"], 10, 64)
			if first <= ZvolDataObject && ZvolDataObject < first+num {
				return fullf(res, "stream frees the zvol data object"), nil
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if !sawBegin || !res.SawEnd {
		return fullf(res, "incomplete send stream (BEGIN=%v END=%v)", sawBegin, res.SawEnd), nil
	}
	for c := range written {
		res.Dirty = append(res.Dirty, c)
	}
	for c, iv := range freed {
		if written[c] {
			continue
		}
		cs, ce := c*chunkSize, min((c+1)*chunkSize, volSize)
		n := unionLen(iv)
		// Only full-size chunks entirely covered by holes can be sent as the
		// zero chunk; anything else is read.
		if n >= ce-cs && ce-cs == chunkSize {
			res.Zero = append(res.Zero, c)
		} else {
			res.Dirty = append(res.Dirty, c)
		}
	}
	sort.Slice(res.Dirty, func(i, j int) bool { return res.Dirty[i] < res.Dirty[j] })
	sort.Slice(res.Zero, func(i, j int) bool { return res.Zero[i] < res.Zero[j] })
	return res, nil
}

func fullf(res *Result, f string, a ...any) *Result {
	res.Full, res.Reason, res.Dirty, res.Zero = true, fmt.Sprintf(f, a...), nil, nil
	return res
}

// parseLine turns "WRITE object = 1 type = 23 offset = 5" into ("WRITE", map).
func parseLine(line string) (string, map[string]string) {
	f := strings.Fields(line)
	if len(f) == 0 {
		return "", nil
	}
	kv := map[string]string{}
	for i := 1; i+2 < len(f); i++ {
		if f[i+1] == "=" {
			kv[f[i]] = f[i+2]
			i += 2
		}
	}
	return f[0], kv
}

// unionLen returns the length covered by the union of [start,end) intervals.
func unionLen(iv [][2]uint64) uint64 {
	sort.Slice(iv, func(i, j int) bool { return iv[i][0] < iv[j][0] })
	var total, curS, curE uint64
	for i, x := range iv {
		if i == 0 || x[0] > curE {
			total += curE - curS
			curS, curE = x[0], x[1]
			continue
		}
		curE = max(curE, x[1])
	}
	return total + curE - curS
}
