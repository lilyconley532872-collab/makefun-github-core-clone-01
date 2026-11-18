// Package main implements the github-core-clone-01 forge in Go (stdlib only).
//
// Port of the TypeScript entry (app.ts / store.ts / git-objects.ts / git-http.ts /
// rest.ts / graphql.ts / mcp.ts), keeping the wire behavior identical so the sealed
// suite's conformance checks pass unchanged. The performance wins over the Node entry
// come from: a compiled static binary, zero-alloc hot paths (byte-slice object store,
// precomputed response bytes), goroutine parallelism for concurrent clones, and
// cached packs for identical fetches (git objects are immutable, so a fetch response
// for fixed wants/haves/depth is correct forever).
package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
)

// ---------------------------------------------------------------------------
// git objects
// ---------------------------------------------------------------------------

// ObjType is a git object type. Stored as a small int for speed; the wire uses names.
type ObjType uint8

const (
	ObjCommit ObjType = 1
	ObjTree   ObjType = 2
	ObjBlob   ObjType = 3
	ObjTag    ObjType = 4
)

func (t ObjType) Name() string {
	switch t {
	case ObjCommit:
		return "commit"
	case ObjTree:
		return "tree"
	case ObjBlob:
		return "blob"
	default:
		return "tag"
	}
}

func typeFromCode(code int) (ObjType, bool) {
	switch code {
	case 1:
		return ObjCommit, true
	case 2:
		return ObjTree, true
	case 3:
		return ObjBlob, true
	case 4:
		return ObjTag, true
	}
	return 0, false
}

func typeCode(t ObjType) int { return int(t) }

// GitObject is one stored object: its type and raw bytes (no header).
type GitObject struct {
	Type ObjType
	Data []byte
}

const ZeroOID = "0000000000000000000000000000000000000000"

func isObjectID(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < 40; i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// hashObject returns the hex SHA-1 of "<type> <len>\0<data>".
func hashObject(t ObjType, data []byte) string {
	h := sha1.New()
	// Build the header without an extra allocation for the common case.
	var hdr [32]byte
	n := copy(hdr[:], t.Name())
	n += copy(hdr[n:], " ")
	n += copy(hdr[n:], itoa(len(data)))
	hdr[n] = 0
	n++
	h.Write(hdr[:n])
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// itoa renders a small non-negative int into decimal without fmt (hot path).
func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}

// ---------------------------------------------------------------------------
// trees / commits / tags
// ---------------------------------------------------------------------------

type TreeEntry struct {
	Mode string
	Name string
	OID  string
}

// parseTree parses a tree object's bytes into entries (no per-entry heap churn
// beyond the returned slice; names are strings since they go into maps/JSON).
func parseTree(data []byte) ([]TreeEntry, error) {
	var out []TreeEntry
	off := 0
	for off < len(data) {
		sp := bytes.IndexByte(data[off:], ' ')
		if sp < 0 {
			return nil, errors.New("tree entry has no mode separator")
		}
		sp += off
		nul := bytes.IndexByte(data[sp+1:], 0)
		if nul < 0 {
			return nil, errors.New("tree entry has no name terminator")
		}
		nul += sp + 1
		if nul+21 > len(data) {
			return nil, errors.New("tree entry has a truncated object id")
		}
		out = append(out, TreeEntry{
			Mode: string(data[off:sp]),
			Name: string(data[sp+1 : nul]),
			OID:  hex.EncodeToString(data[nul+1 : nul+21]),
		})
		off = nul + 21
	}
	return out, nil
}

func compareTreeNames(a, b TreeEntry) int {
	an, bn := a.Name, b.Name
	if a.Mode == "40000" {
		an += "/"
	}
	if b.Mode == "40000" {
		bn += "/"
	}
	return bytes.Compare([]byte(an), []byte(bn))
}

// buildTree serializes entries in git tree order.
func buildTree(entries []TreeEntry) []byte {
	sorted := make([]TreeEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool { return compareTreeNames(sorted[i], sorted[j]) < 0 })
	var buf bytes.Buffer
	for _, e := range sorted {
		buf.WriteString(e.Mode)
		buf.WriteByte(' ')
		buf.WriteString(e.Name)
		buf.WriteByte(0)
		raw, _ := hex.DecodeString(e.OID)
		buf.Write(raw)
	}
	return buf.Bytes()
}

type CommitView struct {
	Tree      string
	Parents   []string
	Author    string
	Committer string
	Message   string
}

func parseCommit(data []byte) (CommitView, error) {
	var cv CommitView
	blank := bytes.Index(data, []byte("\n\n"))
	hdrEnd := len(data)
	if blank >= 0 {
		hdrEnd = blank
		cv.Message = string(data[blank+2:])
	}
	for _, line := range bytes.Split(data[:hdrEnd], []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		sp := bytes.IndexByte(line, ' ')
		key, val := string(line), ""
		if sp >= 0 {
			key, val = string(line[:sp]), string(line[sp+1:])
		}
		switch key {
		case "tree":
			cv.Tree = val
		case "parent":
			cv.Parents = append(cv.Parents, val)
		case "author":
			cv.Author = val
		case "committer":
			cv.Committer = val
		}
	}
	if !isObjectID(cv.Tree) {
		return cv, errors.New("commit has no valid tree header")
	}
	return cv, nil
}

type TagView struct {
	Object  string
	Type    string
	Tag     string
	Tagger  string
	Message string
}

func parseTag(data []byte) (TagView, error) {
	var tv TagView
	blank := bytes.Index(data, []byte("\n\n"))
	hdrEnd := len(data)
	if blank >= 0 {
		hdrEnd = blank
		tv.Message = string(data[blank+2:])
	}
	for _, line := range bytes.Split(data[:hdrEnd], []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		sp := bytes.IndexByte(line, ' ')
		if sp < 0 {
			return tv, errors.New("tag header has no value")
		}
		switch string(line[:sp]) {
		case "object":
			tv.Object = string(line[sp+1:])
		case "type":
			tv.Type = string(line[sp+1:])
		case "tag":
			tv.Tag = string(line[sp+1:])
		case "tagger":
			tv.Tagger = string(line[sp+1:])
		}
	}
	return tv, nil
}

const forgeIdentity = "Forge <forge@localhost>"
const forgeTimestamp = 1735689600 // 2025-01-01T00:00:00Z

func buildCommit(tree string, parents []string, message string) []byte {
	var buf bytes.Buffer
	buf.WriteString("tree " + tree + "\n")
	for _, p := range parents {
		buf.WriteString("parent " + p + "\n")
	}
	who := fmt.Sprintf("%s %d +0000", forgeIdentity, forgeTimestamp)
	buf.WriteString("author " + who + "\n")
	buf.WriteString("committer " + who + "\n")
	buf.WriteString("\n")
	buf.WriteString(message)
	return buf.Bytes()
}

// ---------------------------------------------------------------------------
// reachability
// ---------------------------------------------------------------------------

// collectReachable returns every oid reachable from roots.
func collectReachable(roots []string, lookup func(string) *GitObject) (map[string]bool, error) {
	seen := map[string]bool{}
	var stack []string
	for _, r := range roots {
		if isObjectID(r) {
			stack = append(stack, r)
		}
	}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[oid] {
			continue
		}
		obj := lookup(oid)
		if obj == nil {
			return nil, fmt.Errorf("object %s is missing from the store", oid)
		}
		seen[oid] = true
		switch obj.Type {
		case ObjCommit:
			cv, err := parseCommit(obj.Data)
			if err != nil {
				return nil, err
			}
			stack = append(stack, cv.Tree)
			stack = append(stack, cv.Parents...)
		case ObjTag:
			tv, err := parseTag(obj.Data)
			if err != nil {
				return nil, err
			}
			stack = append(stack, tv.Object)
		case ObjTree:
			entries, err := parseTree(obj.Data)
			if err != nil {
				return nil, err
			}
			for _, e := range entries {
				if e.Mode == "160000" {
					continue
				}
				stack = append(stack, e.OID)
			}
		}
	}
	return seen, nil
}

func isAncestor(ancestor, descendant string, lookup func(string) *GitObject) bool {
	if ancestor == descendant {
		return true
	}
	seen := map[string]bool{}
	stack := []string{descendant}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		obj := lookup(oid)
		if obj == nil {
			continue
		}
		switch obj.Type {
		case ObjCommit:
			cv, err := parseCommit(obj.Data)
			if err != nil {
				continue
			}
			for _, p := range cv.Parents {
				if p == ancestor {
					return true
				}
				stack = append(stack, p)
			}
		case ObjTag:
			tv, err := parseTag(obj.Data)
			if err != nil {
				continue
			}
			if tv.Object == ancestor {
				return true
			}
			stack = append(stack, tv.Object)
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// pkt-lines
// ---------------------------------------------------------------------------

const pktLineMax = 65520

func pktLine(data []byte) []byte {
	out := make([]byte, 0, len(data)+4)
	out = append(out, []byte(fmt.Sprintf("%04x", len(data)+4))...)
	return append(out, data...)
}

func pktFlush() []byte { return []byte("0000") }
func pktDelim() []byte { return []byte("0001") }

// pktEncode builds a pkt-line stream from string/[]byte parts plus "flush"/"delim".
func pktEncode(parts ...interface{}) []byte {
	var buf bytes.Buffer
	for _, p := range parts {
		switch v := p.(type) {
		case string:
			if v == "flush" {
				buf.WriteString("0000")
			} else if v == "delim" {
				buf.WriteString("0001")
			} else {
				buf.Write(pktLine([]byte(v)))
			}
		case []byte:
			buf.Write(pktLine(v))
		}
	}
	return buf.Bytes()
}

type pktPacket struct {
	kind    string // "data" | "flush" | "delim" | "end"
	payload []byte
}

func decodePktLines(b []byte) ([]pktPacket, error) {
	var out []pktPacket
	off := 0
	for off < len(b) {
		if off+4 > len(b) {
			return nil, errors.New("truncated pkt-line")
		}
		hdr := string(b[off : off+4])
		var length int
		if _, err := fmt.Sscanf(hdr, "%04x", &length); err != nil {
			return nil, fmt.Errorf("invalid pkt-line length %q", hdr)
		}
		// Sscanf %04x accepts non-hex too; validate strictly.
		for _, c := range hdr {
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
				return nil, fmt.Errorf("invalid pkt-line length %q", hdr)
			}
		}
		switch length {
		case 0:
			out = append(out, pktPacket{kind: "flush"})
			off += 4
		case 1:
			out = append(out, pktPacket{kind: "delim"})
			off += 4
		case 2:
			out = append(out, pktPacket{kind: "end"})
			off += 4
		case 3:
			return nil, errors.New("invalid pkt-line length 0003")
		default:
			if length > pktLineMax {
				return nil, fmt.Errorf("pkt-line length %d exceeds %d", length, pktLineMax)
			}
			if off+length > len(b) {
				return nil, errors.New("truncated pkt-line")
			}
			out = append(out, pktPacket{kind: "data", payload: b[off+4 : off+length]})
			off += length
		}
	}
	return out, nil
}

func sidebandPacket(band byte, data []byte) []byte {
	p := make([]byte, 0, len(data)+1)
	p = append(p, band)
	p = append(p, data...)
	return pktLine(p)
}

func sidebandChunks(data []byte) [][]byte {
	maxPayload := pktLineMax - 4 - 1
	var out [][]byte
	for off := 0; off < len(data); off += maxPayload {
		end := off + maxPayload
		if end > len(data) {
			end = len(data)
		}
		out = append(out, sidebandPacket(1, data[off:end]))
	}
	if len(out) == 0 {
		out = append(out, sidebandPacket(1, nil))
	}
	return out
}

// ---------------------------------------------------------------------------
// packfiles (full objects only, no deltas on write — legal: the client resolves
// what it receives). Write path precomputes oids ONCE (the TS entry re-hashed
// on every sort comparison).
// ---------------------------------------------------------------------------

func writePack(objects []GitObject) []byte {
	type ent struct {
		obj GitObject
		oid string
	}
	ents := make([]ent, len(objects))
	for i, o := range objects {
		ents[i] = ent{o, hashObject(o.Type, o.Data)}
	}
	sort.Slice(ents, func(i, j int) bool { return ents[i].oid < ents[j].oid })
	var buf bytes.Buffer
	hdr := make([]byte, 12)
	copy(hdr, "PACK")
	hdr[4], hdr[5], hdr[6], hdr[7] = 0, 0, 0, 2
	n := len(ents)
	hdr[8] = byte(n >> 24)
	hdr[9] = byte(n >> 16)
	hdr[10] = byte(n >> 8)
	hdr[11] = byte(n)
	buf.Write(hdr)
	for _, e := range ents {
		var cw bytes.Buffer
		zw := zlib.NewWriter(&cw)
		zw.Write(e.obj.Data)
		zw.Close()
		compressed := cw.Bytes()
		size := len(e.obj.Data)
		// varint size header: low 4 bits in first byte, then 7 bits/byte.
		var sizeBytes []byte
		first := byte(typeCode(e.obj.Type)<<4) | byte(size&0x0f)
		size >>= 4
		if size > 0 {
			first |= 0x80
		}
		sizeBytes = append(sizeBytes, first)
		for size > 0 {
			b := byte(size & 0x7f)
			size >>= 7
			if size > 0 {
				b |= 0x80
			}
			sizeBytes = append(sizeBytes, b)
		}
		buf.Write(sizeBytes)
		buf.Write(compressed)
	}
	body := buf.Bytes()
	sum := sha1.Sum(body)
	out := make([]byte, 0, len(body)+20)
	out = append(out, body...)
	out = append(out, sum[:]...)
	return out
}

func writeEmptyPack() []byte { return writePack(nil) }

type packEntry struct {
	offset int
	typ    ObjType
	size   int
	data   []byte
	oid    string
}

type packReadResult struct {
	version      int
	declared     int
	objects      map[string]*GitObject
	entries      []packEntry
	checksumOK   bool
	deltaCount   int
}

func readVarintSize(b []byte, start int) (size, next, first int, err error) {
	if start >= len(b) {
		return 0, 0, 0, errors.New("pack entry has no size header")
	}
	by := b[start]
	first = int(by)
	size = int(by & 0x0f)
	shift := uint(4)
	off := start + 1
	for by&0x80 != 0 {
		if off >= len(b) {
			return 0, 0, 0, errors.New("pack entry size varint is truncated")
		}
		by = b[off]
		off++
		size += int(by&0x7f) << shift
		shift += 7
		if shift > 63 {
			return 0, 0, 0, errors.New("pack entry size varint is too long")
		}
	}
	return size, off, first, nil
}

func readOfsDeltaBase(b []byte, start int) (baseOff, next int, err error) {
	if start >= len(b) {
		return 0, 0, errors.New("ofs-delta has no base offset")
	}
	by := b[start]
	off := start + 1
	val := int(by & 0x7f)
	for by&0x80 != 0 {
		if off >= len(b) {
			return 0, 0, errors.New("ofs-delta base offset is truncated")
		}
		by = b[off]
		off++
		val = (val+1)*128 + int(by&0x7f)
	}
	return val, off, nil
}

// inflateCounted inflates exactly one zlib stream starting at off and reports the
// exact offset just past it. Verified: zlib.NewReader over a *bytes.Reader leaves
// the reader positioned exactly at the stream end (no read-ahead over-count).
func inflateCounted(b []byte, off int) (data []byte, next int, err error) {
	br := bytes.NewReader(b[off:])
	zr, err := zlib.NewReader(br)
	if err != nil {
		return nil, 0, err
	}
	defer zr.Close()
	data, err = io.ReadAll(zr)
	if err != nil {
		return nil, 0, err
	}
	pos, _ := br.Seek(0, io.SeekCurrent)
	return data, off + int(pos), nil
}

func applyDelta(base, delta []byte) ([]byte, error) {
	off := 0
	readSize := func() (int, error) {
		v, shift := 0, uint(0)
		for {
			if off >= len(delta) {
				return 0, errors.New("delta size varint is truncated")
			}
			by := delta[off]
			off++
			v += int(by&0x7f) << shift
			shift += 7
			if by&0x80 == 0 {
				break
			}
			if shift > 63 {
				return 0, errors.New("delta size varint is too long")
			}
		}
		return v, nil
	}
	srcSize, err := readSize()
	if err != nil {
		return nil, err
	}
	tgtSize, err := readSize()
	if err != nil {
		return nil, err
	}
	if srcSize != len(base) {
		return nil, fmt.Errorf("delta expects a %d-byte base, got %d", srcSize, len(base))
	}
	out := make([]byte, tgtSize)
	written := 0
	for off < len(delta) {
		op := delta[off]
		off++
		if op&0x80 != 0 {
			copyOff, copySize := 0, 0
			shift := uint(0)
			for bit := 0; bit < 4; bit++ {
				if op&(1<<uint(bit)) != 0 {
					if off >= len(delta) {
						return nil, errors.New("delta copy offset is truncated")
					}
					copyOff += int(delta[off]) << shift
					off++
				}
				shift += 8
			}
			shift = 0
			for bit := 4; bit < 7; bit++ {
				if op&(1<<uint(bit)) != 0 {
					if off >= len(delta) {
						return nil, errors.New("delta copy size is truncated")
					}
					copySize += int(delta[off]) << shift
					off++
				}
				shift += 8
			}
			if copySize == 0 {
				copySize = 0x10000
			}
			if copyOff+copySize > len(base) || written+copySize > tgtSize {
				return nil, errors.New("delta copy is out of range")
			}
			copy(out[written:written+copySize], base[copyOff:copyOff+copySize])
			written += copySize
		} else if op != 0 {
			lit := int(op & 0x7f)
			if off+lit > len(delta) || written+lit > tgtSize {
				return nil, errors.New("delta literal is out of range")
			}
			copy(out[written:written+lit], delta[off:off+lit])
			off += lit
			written += lit
		} else {
			return nil, errors.New("delta opcode 0 is invalid")
		}
	}
	if written != tgtSize {
		return nil, fmt.Errorf("delta produced %d of %d bytes", written, tgtSize)
	}
	return out, nil
}

// readPack parses a packfile v2/v3, resolving OFS_DELTA and REF_DELTA entries.
// resolveBase supplies a base object a thin pack leaves out.
func readPack(pack []byte, resolveBase func(string) *GitObject) (*packReadResult, error) {
	if len(pack) < 32 {
		return nil, fmt.Errorf("pack is %d bytes, too short", len(pack))
	}
	if string(pack[:4]) != "PACK" {
		return nil, errors.New("pack does not start with PACK")
	}
	version := int(pack[4])<<24 | int(pack[5])<<16 | int(pack[6])<<8 | int(pack[7])
	if version != 2 && version != 3 {
		return nil, fmt.Errorf("unsupported pack version %d", version)
	}
	declared := int(pack[8])<<24 | int(pack[9])<<16 | int(pack[10])<<8 | int(pack[11])
	trailerStart := len(pack) - 20
	trailer := hex.EncodeToString(pack[trailerStart:])
	computed := sha1.Sum(pack[:trailerStart])
	checksumOK := trailer == hex.EncodeToString(computed[:])

	type rawEntry struct {
		offset     int
		typeCode   int
		size       int
		data       []byte
		baseOffset int
		baseOID    string
		resolved   []byte
		resType    ObjType
		hasRes     bool
		oid        string
	}
	raw := make([]*rawEntry, 0, declared)
	byOffset := map[int]*rawEntry{}
	off := 12
	for i := 0; i < declared; i++ {
		entryOff := off
		size, next, first, err := readVarintSize(pack, off)
		if err != nil {
			return nil, err
		}
		off = next
		tc := (first >> 4) & 0x07
		e := &rawEntry{offset: entryOff, typeCode: tc, size: size, baseOffset: -1}
		if tc == 6 {
			baseOff, next2, err := readOfsDeltaBase(pack, off)
			if err != nil {
				return nil, err
			}
			off = next2
			if baseOff <= 0 || baseOff > entryOff {
				return nil, fmt.Errorf("ofs-delta at %d names an invalid base offset %d", entryOff, baseOff)
			}
			e.baseOffset = entryOff - baseOff
		} else if tc == 7 {
			if off+20 > trailerStart {
				return nil, errors.New("ref-delta base id is truncated")
			}
			e.baseOID = hex.EncodeToString(pack[off : off+20])
			off += 20
		} else if _, ok := typeFromCode(tc); !ok {
			return nil, fmt.Errorf("pack entry %d has unknown type code %d", i, tc)
		}
		data, nextOff, err := inflateCounted(pack, off)
		if err != nil {
			return nil, err
		}
		if len(data) != size {
			return nil, fmt.Errorf("pack entry at %d declares %d bytes but inflated to %d", entryOff, size, len(data))
		}
		e.data = data
		off = nextOff
		raw = append(raw, e)
		byOffset[entryOff] = e
	}

	deltaCount := 0
	var resolve func(e *rawEntry, seen map[int]bool) (ObjType, []byte, error)
	resolve = func(e *rawEntry, seen map[int]bool) (ObjType, []byte, error) {
		if e.hasRes {
			return e.resType, e.resolved, nil
		}
		if seen[e.offset] {
			return 0, nil, errors.New("delta cycle in pack")
		}
		seen[e.offset] = true
		if e.typeCode == 6 || e.typeCode == 7 {
			deltaCount++
			var baseT ObjType
			var baseD []byte
			if e.typeCode == 6 {
				be, ok := byOffset[e.baseOffset]
				if !ok {
					return 0, nil, fmt.Errorf("ofs-delta at %d names a missing base offset %d", e.offset, e.baseOffset)
				}
				var err error
				baseT, baseD, err = resolve(be, seen)
				if err != nil {
					return 0, nil, err
				}
			} else {
				var found *rawEntry
				for _, c := range raw {
					if c.oid == e.baseOID {
						found = c
						break
					}
				}
				if found != nil {
					var err error
					baseT, baseD, err = resolve(found, seen)
					if err != nil {
						return 0, nil, err
					}
				} else if resolveBase != nil {
					if b := resolveBase(e.baseOID); b != nil {
						baseT, baseD = b.Type, b.Data
					}
				}
				if baseD == nil {
					return 0, nil, fmt.Errorf("ref-delta at %d names a missing base %s", e.offset, e.baseOID)
				}
			}
			data, err := applyDelta(baseD, e.data)
			if err != nil {
				return 0, nil, err
			}
			e.resType, e.resolved, e.hasRes = baseT, data, true
			return baseT, data, nil
		}
		t, _ := typeFromCode(e.typeCode)
		e.resType, e.resolved, e.hasRes = t, e.data, true
		return t, e.data, nil
	}

	objects := make(map[string]*GitObject, len(raw))
	entries := make([]packEntry, 0, len(raw))
	for _, e := range raw {
		t, data, err := resolve(e, map[int]bool{})
		if err != nil {
			return nil, err
		}
		oid := hashObject(t, data)
		e.oid = oid
		objects[oid] = &GitObject{Type: t, Data: data}
		entries = append(entries, packEntry{offset: e.offset, typ: t, size: len(data), data: data, oid: oid})
	}
	return &packReadResult{version: version, declared: declared, objects: objects, entries: entries, checksumOK: checksumOK, deltaCount: deltaCount}, nil
}
