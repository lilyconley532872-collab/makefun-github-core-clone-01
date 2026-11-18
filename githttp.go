// githttp.go — git smart HTTP: advertisements, ls-refs, fetch (v2 + v0), receive-pack.
// Port of git-http.ts with two performance additions:
//   - fetchResponseCache: a fetch response is a pure function of (wants, haves, shallow,
//     depth, protocol) over an immutable object graph, so identical fetches (the 50
//     concurrent depth-1 clones) share ONE computed response, including in-flight
//     deduplication (a tiny singleflight) so concurrent duplicates wait instead of
//     re-computing. Responses for wants that do not exist yet are never cached.
//   - index-based queues instead of shift() (O(n) per dequeue in the TS port).

package main

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
)

const agentName = "github-core-clone/1.0"

var v2Capabilities = []string{
	"agent=github-core-clone/1.0",
	"ls-refs=unborn",
	"fetch=shallow filter",
	"object-format=sha1",
}

const v0UploadCapabilities = "symref=HEAD:refs/heads/main agent=github-core-clone/1.0 side-band-64k thin-pack ofs-delta shallow"
const v0ReceiveCapabilities = "report-status side-band-64k delete-refs ofs-delta agent=github-core-clone/1.0"

func peeledTarget(r *Repo, oid string) (string, bool) {
	obj := r.Objects[oid]
	if obj == nil || obj.Type != ObjTag {
		return "", false
	}
	tv, err := parseTag(obj.Data)
	if err != nil {
		return "", false
	}
	target := r.Objects[tv.Object]
	if target == nil {
		return "", false
	}
	if target.Type == ObjCommit {
		return tv.Object, true
	}
	if target.Type == ObjTag {
		return peeledTarget(r, tv.Object)
	}
	return "", false
}

type refEntry struct {
	name string
	oid  string
}

func sortedRefs(r *Repo) []refEntry {
	out := make([]refEntry, 0, len(r.Refs))
	for name, oid := range r.Refs {
		out = append(out, refEntry{name, oid})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

func headOID(r *Repo) string {
	if oid, ok := r.Refs["refs/heads/main"]; ok {
		return oid
	}
	if oid, ok := r.Refs["refs/heads/"+r.DefaultBranch]; ok {
		return oid
	}
	return ZeroOID
}

func advertiseV2() []byte {
	var parts []interface{}
	parts = append(parts, "version 2\n")
	for _, c := range v2Capabilities {
		parts = append(parts, c+"\n")
	}
	parts = append(parts, "flush")
	return pktEncode(parts...)
}

func advertiseV0(r *Repo, service string) []byte {
	caps := v0UploadCapabilities
	if service == "git-receive-pack" {
		caps = v0ReceiveCapabilities
	}
	var buf bytes.Buffer
	buf.Write(pktEncode("# service="+service+"\n", "flush"))
	refs := sortedRefs(r)
	var lines []interface{}
	if len(refs) == 0 {
		lines = append(lines, ZeroOID+" capabilities^{}\x00"+caps+"\n")
	} else {
		head := headOID(r)
		lines = append(lines, head+" HEAD\x00"+caps+"\n")
		for _, ref := range refs {
			lines = append(lines, ref.oid+" "+ref.name+"\n")
		}
		for _, ref := range refs {
			if !strings.HasPrefix(ref.name, "refs/tags/") {
				continue
			}
			if peeled, ok := peeledTarget(r, ref.oid); ok {
				lines = append(lines, peeled+" "+ref.name+"^{}\n")
			}
		}
	}
	buf.Write(pktEncode(lines...))
	buf.Write(pktEncode("flush"))
	return buf.Bytes()
}

// --- ls-refs (protocol v2) ---

type lsRefsOptions struct {
	peel     bool
	symrefs  bool
	unborn   bool
	prefixes []string
}

func lsRefsResponse(r *Repo, opt lsRefsOptions) []byte {
	var lines []interface{}
	refs := sortedRefs(r)
	head := headOID(r)
	includeHead := len(opt.prefixes) == 0
	if len(refs) == 0 {
		if includeHead {
			if opt.symrefs {
				lines = append(lines, "unborn HEAD symref-target:refs/heads/main\n")
			} else {
				lines = append(lines, "unborn HEAD\n")
			}
		}
	} else {
		if includeHead {
			if opt.symrefs {
				lines = append(lines, head+" HEAD symref-target:refs/heads/main\n")
			} else {
				lines = append(lines, head+" HEAD\n")
			}
		}
		for _, ref := range refs {
			if len(opt.prefixes) > 0 {
				match := false
				for _, p := range opt.prefixes {
					if strings.HasPrefix(ref.name, p) {
						match = true
						break
					}
				}
				if !match {
					continue
				}
			}
			line := ref.oid + " " + ref.name
			if opt.peel {
				if peeled, ok := peeledTarget(r, ref.oid); ok {
					line += " peeled:" + peeled
				}
			}
			lines = append(lines, line+"\n")
		}
	}
	lines = append(lines, "flush")
	return pktEncode(lines...)
}

// --- fetch computation ---

type fetchRequest struct {
	wants   []string
	haves   []string
	shallow []string
	depth   int // -1 = none
}

type fetchPlan struct {
	objects   map[string]*GitObject
	shallow   []string
	unshallow []string
	acks      []string
}

// treeClosure: objects reachable from a tree only (trees + blobs, no commits).
func treeClosure(r *Repo, treeOID string) map[string]bool {
	out := map[string]bool{}
	queue := []string{treeOID}
	for len(queue) > 0 {
		oid := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if out[oid] {
			continue
		}
		obj := r.Objects[oid]
		if obj == nil {
			continue
		}
		out[oid] = true
		if obj.Type == ObjTree {
			entries, err := parseTree(obj.Data)
			if err != nil {
				continue
			}
			for _, e := range entries {
				if e.Mode == "160000" {
					continue
				}
				queue = append(queue, e.OID)
			}
		}
	}
	return out
}

func computeFetch(r *Repo, req fetchRequest) fetchPlan {
	lookup := func(oid string) *GitObject { return r.Objects[oid] }

	// Objects the client already holds: reachable from haves, not crossing a
	// prior shallow boundary.
	held := map[string]bool{}
	{
		boundarySet := map[string]bool{}
		for _, s := range req.shallow {
			boundarySet[s] = true
		}
		queue := []string{}
		for _, h := range req.haves {
			if isObjectID(h) {
				queue = append(queue, h)
			}
		}
		seenCommits := map[string]bool{}
		for head := 0; head < len(queue); head++ {
			oid := queue[head]
			obj := lookup(oid)
			if obj == nil {
				continue
			}
			if obj.Type == ObjCommit {
				if seenCommits[oid] {
					continue
				}
				seenCommits[oid] = true
				held[oid] = true
				cv, err := parseCommit(obj.Data)
				if err != nil {
					continue
				}
				for to := range treeClosure(r, cv.Tree) {
					held[to] = true
				}
				if boundarySet[oid] {
					continue
				}
				queue = append(queue, cv.Parents...)
			} else if obj.Type == ObjTag {
				held[oid] = true
				tv, err := parseTag(obj.Data)
				if err == nil {
					queue = append(queue, tv.Object)
				}
			} else {
				held[oid] = true
			}
		}
	}

	// Commits to include: walk from wants, depth-limited.
	depth := req.depth
	wantedCommits := map[string]int{}
	{
		type item struct {
			oid      string
			distance int
		}
		queue := []item{}
		for _, w := range req.wants {
			if isObjectID(w) {
				queue = append(queue, item{w, 0})
			}
		}
		seen := map[string]bool{}
		for head := 0; head < len(queue); head++ {
			it := queue[head]
			if seen[it.oid] {
				continue
			}
			seen[it.oid] = true
			obj := lookup(it.oid)
			if obj == nil {
				continue
			}
			if obj.Type == ObjTag {
				wantedCommits[it.oid] = it.distance
				tv, err := parseTag(obj.Data)
				if err == nil {
					queue = append(queue, item{tv.Object, it.distance})
				}
				continue
			}
			if obj.Type != ObjCommit {
				continue
			}
			if depth >= 0 && it.distance >= depth {
				continue
			}
			wantedCommits[it.oid] = it.distance
			cv, err := parseCommit(obj.Data)
			if err != nil {
				continue
			}
			for _, p := range cv.Parents {
				queue = append(queue, item{p, it.distance + 1})
			}
		}
	}

	// Objects to send: each wanted commit + its tree closure, minus held.
	objects := map[string]*GitObject{}
	for oid := range wantedCommits {
		obj := lookup(oid)
		if obj == nil {
			continue
		}
		if obj.Type == ObjTag {
			if !held[oid] {
				objects[oid] = obj
			}
			continue
		}
		if obj.Type != ObjCommit {
			continue
		}
		if !held[oid] {
			objects[oid] = obj
		}
		cv, err := parseCommit(obj.Data)
		if err != nil {
			continue
		}
		for to := range treeClosure(r, cv.Tree) {
			if held[to] {
				continue
			}
			if tobj := lookup(to); tobj != nil {
				objects[to] = tobj
			}
		}
	}

	// Shallow-info.
	var shallow, unshallow []string
	if depth >= 0 {
		for oid, distance := range wantedCommits {
			if distance != depth-1 {
				continue
			}
			obj := lookup(oid)
			if obj == nil || obj.Type != ObjCommit {
				continue
			}
			cv, err := parseCommit(obj.Data)
			if err == nil && len(cv.Parents) > 0 {
				shallow = append(shallow, oid)
			}
		}
		boundarySet := map[string]bool{}
		for _, s := range req.shallow {
			boundarySet[s] = true
		}
		for old := range boundarySet {
			if d, ok := wantedCommits[old]; ok && d < depth-1 {
				unshallow = append(unshallow, old)
			}
		}
	} else if len(req.shallow) > 0 {
		for _, old := range req.shallow {
			if _, ok := wantedCommits[old]; ok {
				unshallow = append(unshallow, old)
			}
		}
	}

	// ACKs: each have that is an ancestor of (or equal to) a want.
	var acks []string
	for _, have := range req.haves {
		if !isObjectID(have) {
			continue
		}
		for _, want := range req.wants {
			if !isObjectID(want) {
				continue
			}
			if have == want || isAncestor(have, want, lookup) {
				acks = append(acks, have)
				break
			}
		}
	}

	return fetchPlan{objects: objects, shallow: shallow, unshallow: unshallow, acks: acks}
}

func fetchV2Response(r *Repo, req fetchRequest) []byte {
	plan := computeFetch(r, req)
	var parts [][]byte
	var ackLines []interface{}
	ackLines = append(ackLines, "acknowledgments\n")
	if len(plan.acks) > 0 {
		for _, ack := range plan.acks {
			ackLines = append(ackLines, "ACK "+ack+"\n")
		}
	} else {
		ackLines = append(ackLines, "NAK\n")
	}
	ackLines = append(ackLines, "ready\n", "delim")
	parts = append(parts, pktEncode(ackLines...))
	if len(plan.shallow) > 0 || len(plan.unshallow) > 0 {
		var lines []interface{}
		lines = append(lines, "shallow-info\n")
		for _, oid := range plan.shallow {
			lines = append(lines, "shallow "+oid+"\n")
		}
		for _, oid := range plan.unshallow {
			lines = append(lines, "unshallow "+oid+"\n")
		}
		lines = append(lines, "delim")
		parts = append(parts, pktEncode(lines...))
	}
	objs := make([]GitObject, 0, len(plan.objects))
	for _, o := range plan.objects {
		objs = append(objs, *o)
	}
	pack := writePack(objs)
	parts = append(parts, pktEncode("packfile\n"))
	for _, chunk := range sidebandChunks(pack) {
		parts = append(parts, chunk)
	}
	parts = append(parts, pktEncode("flush"))
	return bytes.Join(parts, nil)
}

func fetchV0Response(r *Repo, req fetchRequest) []byte {
	plan := computeFetch(r, req)
	var parts [][]byte
	var lines []interface{}
	if len(plan.acks) > 0 {
		for _, ack := range plan.acks {
			lines = append(lines, "ACK "+ack+"\n")
		}
	} else {
		lines = append(lines, "NAK\n")
	}
	for _, oid := range plan.shallow {
		lines = append(lines, "shallow "+oid+"\n")
	}
	for _, oid := range plan.unshallow {
		lines = append(lines, "unshallow "+oid+"\n")
	}
	if len(lines) > 0 {
		parts = append(parts, pktEncode(lines...))
	}
	objs := make([]GitObject, 0, len(plan.objects))
	for _, o := range plan.objects {
		objs = append(objs, *o)
	}
	var pack []byte
	if len(objs) == 0 {
		pack = writeEmptyPack()
	} else {
		pack = writePack(objs)
	}
	for _, chunk := range sidebandChunks(pack) {
		parts = append(parts, chunk)
	}
	parts = append(parts, pktEncode("flush"))
	return bytes.Join(parts, nil)
}

// --- fetch response cache (singleflight) ---

// fetchCacheKey is the full identity of a fetch response. Git objects are
// immutable and a fetch's reachable set depends only on these inputs, so the
// cached bytes stay correct forever — EXCEPT when a want oid does not exist yet
// (a later push could create it); those responses are never stored.
func fetchCacheKey(v2 bool, req fetchRequest) string {
	var b strings.Builder
	if v2 {
		b.WriteString("v2|")
	} else {
		b.WriteString("v0|")
	}
	b.WriteString(strconv.Itoa(req.depth))
	b.WriteByte('|')
	for _, w := range req.wants {
		b.WriteString(w)
		b.WriteByte(',')
	}
	b.WriteByte('|')
	for _, h := range req.haves {
		b.WriteString(h)
		b.WriteByte(',')
	}
	b.WriteByte('|')
	for _, s := range req.shallow {
		b.WriteString(s)
		b.WriteByte(',')
	}
	return b.String()
}

type fetchCall struct {
	done chan struct{}
	resp []byte
}

type fetchCache struct {
	mu       sync.Mutex
	done     map[string][]byte
	inflight map[string]*fetchCall
	order    []string
}

func newFetchCache() *fetchCache {
	return &fetchCache{
		done:     map[string][]byte{},
		inflight: map[string]*fetchCall{},
	}
}

// getOrCompute returns the cached response for key, computing it once even when
// many goroutines ask concurrently. cacheable=false results are not stored.
func (c *fetchCache) getOrCompute(key string, compute func() ([]byte, bool)) []byte {
	c.mu.Lock()
	if resp, ok := c.done[key]; ok {
		c.mu.Unlock()
		return resp
	}
	if call, ok := c.inflight[key]; ok {
		c.mu.Unlock()
		<-call.done
		return call.resp
	}
	call := &fetchCall{done: make(chan struct{})}
	c.inflight[key] = call
	c.mu.Unlock()

	resp, cacheable := compute()
	call.resp = resp
	c.mu.Lock()
	delete(c.inflight, key)
	if cacheable {
		c.done[key] = resp
		c.order = append(c.order, key)
		if len(c.order) > 64 {
			oldest := c.order[0]
			c.order = c.order[1:]
			delete(c.done, oldest)
		}
	}
	close(call.done)
	c.mu.Unlock()
	return resp
}

// --- request parsing ---

type parsedFetchArgs struct {
	command  string
	wants    []string
	haves    []string
	shallow  []string
	depth    int // -1 = none
	prefixes []string
	peel     bool
	symrefs  bool
	unborn   bool
}

func stripNL(s string) string { return strings.TrimSuffix(s, "\n") }

func parseV2Request(body []byte) (parsedFetchArgs, error) {
	packets, err := decodePktLines(body)
	if err != nil {
		return parsedFetchArgs{}, err
	}
	out := parsedFetchArgs{depth: -1}
	inArgs := false
	for _, p := range packets {
		if p.kind == "flush" {
			break
		}
		if p.kind == "delim" {
			inArgs = true
			continue
		}
		if p.kind != "data" {
			continue
		}
		line := string(p.payload)
		if !inArgs {
			if strings.HasPrefix(line, "command=") {
				cmd := stripNL(line[len("command="):])
				out.command = cmd
			}
			continue
		}
		text := stripNL(line)
		switch {
		case text == "peel":
			out.peel = true
		case text == "symrefs":
			out.symrefs = true
		case text == "unborn":
			out.unborn = true
		case strings.HasPrefix(text, "want "):
			out.wants = append(out.wants, strings.TrimSpace(text[5:]))
		case strings.HasPrefix(text, "have "):
			out.haves = append(out.haves, strings.TrimSpace(text[5:]))
		case strings.HasPrefix(text, "shallow "):
			out.shallow = append(out.shallow, strings.TrimSpace(text[8:]))
		case strings.HasPrefix(text, "deepen "):
			if n, err := strconv.Atoi(strings.TrimSpace(text[7:])); err == nil {
				out.depth = n
			}
		case strings.HasPrefix(text, "ref-prefix "):
			out.prefixes = append(out.prefixes, strings.TrimSpace(text[11:]))
		}
	}
	if out.command == "" {
		return out, errors.New("upload-pack request has no command")
	}
	return out, nil
}

func parseV0Fetch(body []byte) (parsedFetchArgs, error) {
	packets, err := decodePktLines(body)
	if err != nil {
		return parsedFetchArgs{}, err
	}
	out := parsedFetchArgs{command: "fetch", depth: -1}
	section := 0
	for _, p := range packets {
		if p.kind == "flush" {
			section++
			continue
		}
		if p.kind != "data" {
			continue
		}
		text := stripNL(string(p.payload))
		if section == 0 {
			switch {
			case strings.HasPrefix(text, "want "):
				rest := text[5:]
				if sp := strings.IndexByte(rest, ' '); sp >= 0 {
					out.wants = append(out.wants, rest[:sp])
				} else {
					out.wants = append(out.wants, strings.TrimSpace(rest))
				}
			case strings.HasPrefix(text, "shallow "):
				out.shallow = append(out.shallow, strings.TrimSpace(text[8:]))
			case strings.HasPrefix(text, "deepen "):
				if n, err := strconv.Atoi(strings.TrimSpace(text[7:])); err == nil {
					out.depth = n
				}
			}
		} else {
			if strings.HasPrefix(text, "have ") {
				out.haves = append(out.haves, strings.TrimSpace(text[5:]))
			}
		}
	}
	return out, nil
}

type pushCommand struct {
	oldOID string
	newOID string
	ref    string
}

func isHex4(s string) bool {
	if len(s) != 4 {
		return false
	}
	for i := 0; i < 4; i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// parseReceivePack parses a receive-pack body: command pkt-lines to the flush,
// then raw pack bytes.
func parseReceivePack(body []byte) ([]pushCommand, []byte, error) {
	offset := 0
	var commands []pushCommand
	for {
		if offset+4 > len(body) {
			return nil, nil, errors.New("truncated receive-pack body")
		}
		header := string(body[offset : offset+4])
		if !isHex4(header) {
			return nil, nil, fmt.Errorf("invalid pkt-line length %q", header)
		}
		length, _ := strconv.ParseInt(header, 16, 32)
		if length == 0 {
			offset += 4
			break
		}
		if length == 1 || length == 2 {
			return nil, nil, errors.New("unexpected control packet in receive-pack commands")
		}
		if length > pktLineMax {
			return nil, nil, errors.New("pkt-line too long in receive-pack")
		}
		if offset+int(length) > len(body) {
			return nil, nil, errors.New("truncated receive-pack command")
		}
		payload := string(body[offset+4 : offset+int(length)])
		offset += int(length)
		line := stripNL(payload)
		if nul := strings.IndexByte(line, 0); nul >= 0 {
			line = line[:nul]
		}
		parts := strings.Split(line, " ")
		if len(parts) < 3 {
			preview := line
			if len(preview) > 60 {
				preview = preview[:60]
			}
			return nil, nil, fmt.Errorf("bad push command %q", preview)
		}
		commands = append(commands, pushCommand{oldOID: parts[0], newOID: parts[1], ref: strings.Join(parts[2:], " ")})
	}
	var pack []byte
	if offset < len(body) {
		pack = body[offset:]
	}
	return commands, pack, nil
}

// --- receive-pack ---

type pushResult struct {
	ref    string
	ok     bool
	reason string
}

type pushOutcome struct {
	unpackOK     bool
	unpackDetail string
	results      []pushResult
}

func applyPush(s *Store, r *Repo, body []byte) pushOutcome {
	commands, pack, err := parseReceivePack(body)
	if err != nil {
		return pushOutcome{unpackOK: false, unpackDetail: err.Error()}
	}
	if len(commands) == 0 {
		return pushOutcome{unpackOK: false, unpackDetail: "no commands"}
	}
	if pack != nil && len(pack) > 0 {
		read, err := readPack(pack, func(oid string) *GitObject { return r.Objects[oid] })
		if err != nil {
			return pushOutcome{unpackOK: false, unpackDetail: err.Error()}
		}
		for oid, obj := range read.objects {
			r.Objects[oid] = obj
		}
	}
	var results []pushResult
	for _, cmd := range commands {
		current, hasCurrent := r.Refs[cmd.ref]
		if !strings.HasPrefix(cmd.ref, "refs/") || strings.ContainsAny(cmd.ref, " \x00\x01\x02\x03\x04\x05\x06\x07\x08\x09\x0a\x0b\x0c\x0d\x0e\x0f\x10\x11\x12\x13\x14\x15\x16\x17\x18\x19\x1a\x1b\x1c\x1d\x1e\x1f") {
			results = append(results, pushResult{cmd.ref, false, "invalid ref"})
			continue
		}
		if cmd.newOID == ZeroOID {
			if !hasCurrent {
				results = append(results, pushResult{cmd.ref, false, "remote ref does not exist"})
			} else if cmd.oldOID != ZeroOID && cmd.oldOID != current {
				results = append(results, pushResult{cmd.ref, false, "stale info"})
			} else {
				delete(r.Refs, cmd.ref)
				results = append(results, pushResult{cmd.ref, true, ""})
			}
			continue
		}
		obj := r.Objects[cmd.newOID]
		if obj == nil {
			results = append(results, pushResult{cmd.ref, false, "object not found"})
			continue
		}
		if strings.HasPrefix(cmd.ref, "refs/heads/") && obj.Type != ObjCommit {
			results = append(results, pushResult{cmd.ref, false, "not a commit"})
			continue
		}
		if strings.HasPrefix(cmd.ref, "refs/tags/") && obj.Type != ObjCommit && obj.Type != ObjTag {
			results = append(results, pushResult{cmd.ref, false, "not a commit or tag"})
			continue
		}
		if cmd.oldOID == ZeroOID {
			if hasCurrent {
				results = append(results, pushResult{cmd.ref, false, "already exists"})
				continue
			}
			r.Refs[cmd.ref] = cmd.newOID
			results = append(results, pushResult{cmd.ref, true, ""})
			continue
		}
		if !hasCurrent {
			results = append(results, pushResult{cmd.ref, false, "remote ref does not exist"})
			continue
		}
		if current != cmd.oldOID {
			results = append(results, pushResult{cmd.ref, false, "stale info"})
			continue
		}
		if cmd.newOID == cmd.oldOID {
			results = append(results, pushResult{cmd.ref, true, ""})
			continue
		}
		if !isAncestor(cmd.oldOID, cmd.newOID, func(oid string) *GitObject { return r.Objects[oid] }) {
			results = append(results, pushResult{cmd.ref, false, "non-fast-forward"})
			continue
		}
		r.Refs[cmd.ref] = cmd.newOID
		results = append(results, pushResult{cmd.ref, true, ""})
	}
	r.bump()
	return pushOutcome{unpackOK: true, unpackDetail: "ok", results: results}
}

func pushReportResponse(outcome pushOutcome) []byte {
	var lines []string
	if outcome.unpackOK {
		lines = append(lines, "unpack ok")
	} else {
		lines = append(lines, "unpack "+outcome.unpackDetail)
	}
	for _, res := range outcome.results {
		if res.ok {
			lines = append(lines, "ok "+res.ref)
		} else {
			lines = append(lines, "ng "+res.ref+" "+res.reason)
		}
	}
	text := strings.Join(lines, "\n") + "\n"
	parts := [][]byte{sidebandPacket(1, []byte(text)), pktEncode("flush")}
	return bytes.Join(parts, nil)
}

// --- HTTP entry point ---

// handleGitHTTPResponse serves one git smart-HTTP request. A refusal (unknown repo,
// non-fast-forward, malformed body) is an HTTP 200 with an ng line / a 400, never
// a crash: the server must stay up for the next call.
func handleGitHTTPResponse(s *Store, cache *fetchCache, req *http.Request, owner, repoName string, tail []string, token string, body []byte) response {
	if !tokenAccepted(token) {
		return errorResp(401, "Requires authentication")
	}
	login := loginForRequest(s, token, owner)
	if login == "" {
		return errorResp(404, "Not Found")
	}
	repo := s.getRepo(owner, repoName)
	if repo == nil {
		return errorResp(404, "Not Found")
	}
	isV2 := req.Header.Get("Git-Protocol") == "version=2"

	if len(tail) == 2 && tail[0] == "info" && tail[1] == "refs" && req.Method == "GET" {
		service := req.URL.Query().Get("service")
		if service == "git-upload-pack" {
			if isV2 {
				return rawResp(200, advertiseV2(), "application/x-git-upload-pack-advertisement", nil)
			}
			return rawResp(200, advertiseV0(repo, "git-upload-pack"), "application/x-git-upload-pack-advertisement", nil)
		}
		if service == "git-receive-pack" {
			return rawResp(200, advertiseV0(repo, "git-receive-pack"), "application/x-git-receive-pack-advertisement", nil)
		}
		return errorResp(400, "Unknown git service")
	}

	if len(tail) == 1 && tail[0] == "git-upload-pack" && req.Method == "POST" {
		if isV2 {
			parsed, err := parseV2Request(body)
			if err != nil {
				return errorResp(400, err.Error())
			}
			if parsed.command == "ls-refs" {
				return rawResp(200, lsRefsResponse(repo, lsRefsOptions{peel: parsed.peel, symrefs: parsed.symrefs, unborn: parsed.unborn, prefixes: parsed.prefixes}), "application/x-git-upload-pack-result", nil)
			}
			if parsed.command == "fetch" {
				freq := fetchRequest{wants: parsed.wants, haves: parsed.haves, shallow: parsed.shallow, depth: parsed.depth}
				key := fetchCacheKey(true, freq)
				resp := cache.getOrCompute(key, func() ([]byte, bool) {
					allExist := true
					for _, wnt := range freq.wants {
						if repo.Objects[wnt] == nil {
							allExist = false
							break
						}
					}
					return fetchV2Response(repo, freq), allExist
				})
				return rawResp(200, resp, "application/x-git-upload-pack-result", nil)
			}
			return errorResp(400, "Unknown upload-pack command: "+parsed.command)
		}
		parsed, err := parseV0Fetch(body)
		if err != nil {
			return errorResp(400, err.Error())
		}
		freq := fetchRequest{wants: parsed.wants, haves: parsed.haves, shallow: parsed.shallow, depth: parsed.depth}
		key := fetchCacheKey(false, freq)
		resp := cache.getOrCompute(key, func() ([]byte, bool) {
			allExist := true
			for _, wnt := range freq.wants {
				if repo.Objects[wnt] == nil {
					allExist = false
					break
				}
			}
			return fetchV0Response(repo, freq), allExist
		})
		return rawResp(200, resp, "application/x-git-upload-pack-result", nil)
	}

	if len(tail) == 1 && tail[0] == "git-receive-pack" && req.Method == "POST" {
		outcome := applyPush(s, repo, body)
		return rawResp(200, pushReportResponse(outcome), "application/x-git-receive-pack-result", nil)
	}

	return errorResp(404, "Not Found")
}
