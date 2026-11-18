// store.go — the forge's in-memory store: repositories, git objects, issues, pulls.
// Port of store.ts, same semantics (fixed forge identity/timestamp, monotonic
// creation counter, no wall clock in stored values) so replays stay deterministic.

package main

import (
	"encoding/base64"
	"sort"
	"strconv"
	"strings"
)

const forgeDate = "2025-01-01T00:00:00Z"
const defaultBranch = "main"

type Label struct {
	Name  string
	Color string
}

type IssueComment struct {
	ID         int
	Body       string
	CreatedSeq int
}

type Issue struct {
	ID         int
	Number     int
	Title      string
	Body       string
	State      string // "open" | "closed"
	Labels     []string
	CreatedSeq int
	Comments   []IssueComment
	// nodeID is precomputed at creation: globalId("Issue", repoID:number) never changes.
	nodeID string
	// cachedJSON is the serialized GET-issue response body, or nil. Valid only
	// while cachedVersion == the owning repo's version (any repo mutation bumps it).
	cachedJSON    []byte
	cachedVersion int
}

type PullRequest struct {
	ID          int
	Number      int
	Title       string
	Body        string
	State       string // "open" | "closed"
	Merged      bool
	MergedAt    *string
	MergeCommit *string
	Head        string
	Base        string
	HeadSha     string
	BaseSha     string
	CreatedSeq  int
}

type Repo struct {
	ID            int
	Owner         string
	Name          string
	Description   string
	Private       bool
	DefaultBranch string
	CreatedSeq    int
	Objects       map[string]*GitObject
	Refs          map[string]string
	Issues        []*Issue
	Pulls         []*PullRequest
	Labels        map[string]*Label
	NumberSeq     int
	CommentSeq    int
	// version bumps on every mutation of the repo; response caches key on it.
	version int
}

func (r *Repo) bump() { r.version++ }

func globalID(kind, key string) string {
	return base64.StdEncoding.EncodeToString([]byte(kind + ":" + key))
}

func repoGlobalID(r *Repo) string { return globalID("Repository", strconv.Itoa(r.ID)) }

func issueGlobalID(r *Repo, is *Issue) string {
	if is.nodeID != "" {
		return is.nodeID
	}
	return globalID("Issue", strconv.Itoa(r.ID)+":"+strconv.Itoa(is.Number))
}

func pullGlobalID(r *Repo, p *PullRequest) string {
	return globalID("PullRequest", strconv.Itoa(r.ID)+":"+strconv.Itoa(p.Number))
}

func commentGlobalID(r *Repo, is *Issue, c *IssueComment) string {
	return globalID("IssueComment", strconv.Itoa(r.ID)+":"+strconv.Itoa(is.Number)+":"+strconv.Itoa(c.ID))
}

type decodedGlobalID struct {
	kind      string
	repoID    int
	number    int
	commentID int
}

// decodeGlobalID parses a base64 "Kind:repoId:number[:commentId]" global id.
func decodeGlobalID(id string) *decodedGlobalID {
	text, err := base64.StdEncoding.DecodeString(id)
	if err != nil {
		return nil
	}
	parts := strings.Split(string(text), ":")
	if len(parts) < 2 {
		return nil
	}
	kind := parts[0]
	repoID, err := strconv.Atoi(parts[1])
	if err != nil || repoID <= 0 {
		return nil
	}
	if kind == "Repository" && len(parts) == 2 {
		return &decodedGlobalID{kind: kind, repoID: repoID}
	}
	if (kind == "Issue" || kind == "PullRequest") && len(parts) == 3 {
		number, err := strconv.Atoi(parts[2])
		if err != nil || number <= 0 {
			return nil
		}
		return &decodedGlobalID{kind: kind, repoID: repoID, number: number}
	}
	if kind == "IssueComment" && len(parts) == 4 {
		number, err1 := strconv.Atoi(parts[2])
		commentID, err2 := strconv.Atoi(parts[3])
		if err1 != nil || err2 != nil || number <= 0 || commentID <= 0 {
			return nil
		}
		return &decodedGlobalID{kind: kind, repoID: repoID, number: number, commentID: commentID}
	}
	return nil
}

func repoKey(owner, name string) string { return owner + "\n" + name }

func countLines(text string) int {
	if text == "" {
		return 0
	}
	n := strings.Count(text, "\n")
	if strings.HasSuffix(text, "\n") {
		return n
	}
	return n + 1
}

// lineDiff: lines added/removed between two texts (simple LCS on lines).
func lineDiff(oldText, newText string) (add, del int) {
	var oldLines, newLines []string
	if oldText != "" {
		oldLines = strings.Split(oldText, "\n")
	}
	if newText != "" {
		newLines = strings.Split(newText, "\n")
	}
	m, n := len(oldLines), len(newLines)
	// LCS with two rows (O(m*n) time, O(n) space).
	prev := make([]int, n+1)
	cur := make([]int, n+1)
	for i := m - 1; i >= 0; i-- {
		for j := n - 1; j >= 0; j-- {
			if oldLines[i] == newLines[j] {
				cur[j] = prev[j+1] + 1
			} else if prev[j] > cur[j+1] {
				cur[j] = prev[j]
			} else {
				cur[j] = cur[j+1]
			}
		}
		prev, cur = cur, prev
	}
	common := prev[0]
	return n - common, m - common
}

type Store struct {
	Repos       map[string]*Repo
	pending     map[string][]*Repo
	tokenLogin  map[string]string
	repoIDSeq   int
	seq         int
}

func NewStore() *Store {
	return &Store{
		Repos:      map[string]*Repo{},
		pending:    map[string][]*Repo{},
		tokenLogin: map[string]string{},
		repoIDSeq:  1,
		seq:        1,
	}
}

func (s *Store) nextSeq() int { v := s.seq; s.seq++; return v }

func (s *Store) loginFor(token string) (string, bool) {
	v, ok := s.tokenLogin[token]
	return v, ok
}

func (s *Store) bindLogin(token, owner string) string {
	if ex, ok := s.tokenLogin[token]; ok {
		return ex
	}
	s.tokenLogin[token] = owner
	pending := s.pending[token]
	delete(s.pending, token)
	for _, r := range pending {
		r.Owner = owner
		s.Repos[repoKey(owner, r.Name)] = r
	}
	return owner
}

func (s *Store) createRepo(owner *string, token, name string, private bool, description string) *Repo {
	r := &Repo{
		ID:            s.repoIDSeq,
		Name:          name,
		Description:   description,
		Private:       private,
		DefaultBranch: defaultBranch,
		CreatedSeq:    s.nextSeq(),
		Objects:       map[string]*GitObject{},
		Refs:          map[string]string{},
		Labels:        map[string]*Label{},
		NumberSeq:     1,
		CommentSeq:    1,
	}
	s.repoIDSeq++
	if owner == nil {
		r.Owner = ""
		s.pending[token] = append(s.pending[token], r)
	} else {
		r.Owner = *owner
		s.Repos[repoKey(*owner, name)] = r
	}
	return r
}

func (s *Store) getRepo(owner, name string) *Repo { return s.Repos[repoKey(owner, name)] }

func (s *Store) repoByID(id int) *Repo {
	for _, r := range s.Repos {
		if r.ID == id {
			return r
		}
	}
	return nil
}

func (s *Store) deleteRepo(owner, name string) bool {
	_, ok := s.Repos[repoKey(owner, name)]
	delete(s.Repos, repoKey(owner, name))
	return ok
}

func (s *Store) reposOf(owner, token string) []*Repo {
	bound, hasBound := s.tokenLogin[token]
	var out []*Repo
	for _, r := range s.Repos {
		if r.Owner == owner {
			out = append(out, r)
		}
	}
	if !hasBound {
		_ = bound
		out = append(out, s.pending[token]...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedSeq < out[j].CreatedSeq })
	return out
}

// --- git objects / refs ---

func (s *Store) lookup(r *Repo) func(string) *GitObject {
	return func(oid string) *GitObject { return r.Objects[oid] }
}

func (s *Store) addObject(r *Repo, obj *GitObject) string {
	oid := hashObject(obj.Type, obj.Data)
	r.Objects[oid] = obj
	return oid
}

func (s *Store) refTip(r *Repo, ref string) (string, bool) {
	v, ok := r.Refs[ref]
	return v, ok
}

func (s *Store) refCommit(r *Repo, ref string) (string, bool) {
	oid, ok := r.Refs[ref]
	if !ok {
		return "", false
	}
	obj := r.Objects[oid]
	if obj == nil {
		return "", false
	}
	if obj.Type == ObjTag {
		tv, err := parseTag(obj.Data)
		if err != nil {
			return "", false
		}
		return tv.Object, true
	}
	if obj.Type == ObjCommit {
		return oid, true
	}
	return "", false
}

func (s *Store) resolveToCommit(r *Repo, ref string) (string, bool) {
	if isObjectID(ref) {
		obj := r.Objects[ref]
		if obj == nil {
			return "", false
		}
		if obj.Type == ObjCommit {
			return ref, true
		}
		if obj.Type == ObjTag {
			tv, err := parseTag(obj.Data)
			if err != nil {
				return "", false
			}
			return tv.Object, true
		}
		return "", false
	}
	full := ref
	if !strings.HasPrefix(ref, "refs/") {
		full = "refs/heads/" + ref
	}
	if c, ok := s.refCommit(r, full); ok {
		return c, true
	}
	if !strings.HasPrefix(ref, "refs/") {
		if c, ok := s.refCommit(r, "refs/tags/"+ref); ok {
			return c, true
		}
	}
	return "", false
}

func (s *Store) commitTree(r *Repo, commitOID string) (string, bool) {
	obj := r.Objects[commitOID]
	if obj == nil || obj.Type != ObjCommit {
		return "", false
	}
	cv, err := parseCommit(obj.Data)
	if err != nil {
		return "", false
	}
	return cv.Tree, true
}

type fileRef struct {
	OID  string
	Mode string
}

// flattenTree: the flat file map (path -> fileRef) of a tree, recursively.
func (s *Store) flattenTree(r *Repo, treeOID, prefix string) map[string]fileRef {
	out := map[string]fileRef{}
	var walk func(oid, path string)
	walk = func(oid, path string) {
		obj := r.Objects[oid]
		if obj == nil || obj.Type != ObjTree {
			return
		}
		entries, err := parseTree(obj.Data)
		if err != nil {
			return
		}
		for _, e := range entries {
			child := e.Name
			if path != "" {
				child = path + "/" + e.Name
			}
			if e.Mode == "40000" {
				walk(e.OID, child)
			} else if e.Mode != "160000" {
				out[child] = fileRef{OID: e.OID, Mode: e.Mode}
			}
		}
	}
	walk(treeOID, prefix)
	return out
}

// buildTreeFromFiles builds (and stores) the tree for a file map.
func (s *Store) buildTreeFromFiles(r *Repo, files map[string]fileRef) string {
	dirs := map[string]map[string]fileRef{}
	ensure := func(dir string) map[string]fileRef {
		m, ok := dirs[dir]
		if !ok {
			m = map[string]fileRef{}
			dirs[dir] = m
		}
		return m
	}
	ensure("")
	for path, f := range files {
		slash := strings.LastIndex(path, "/")
		dir, name := "", path
		if slash >= 0 {
			dir, name = path[:slash], path[slash+1:]
		}
		ensure(dir)[name] = f
		parent := dir
		for parent != "" {
			idx := strings.LastIndex(parent, "/")
			grand := ""
			if idx >= 0 {
				grand = parent[:idx]
			}
			ensure(grand)
			parent = grand
		}
	}
	depthOf := func(dir string) int {
		if dir == "" {
			return 0
		}
		return strings.Count(dir, "/") + 1
	}
	var dirPaths []string
	for d := range dirs {
		dirPaths = append(dirPaths, d)
	}
	sort.Slice(dirPaths, func(i, j int) bool { return depthOf(dirPaths[i]) > depthOf(dirPaths[j]) })
	treeByDir := map[string]string{}
	for _, dir := range dirPaths {
		var entries []TreeEntry
		for name, f := range dirs[dir] {
			entries = append(entries, TreeEntry{Mode: f.Mode, Name: name, OID: f.OID})
		}
		for childDir, childOID := range treeByDir {
			slash := strings.LastIndex(childDir, "/")
			parent, name := "", childDir
			if slash >= 0 {
				parent, name = childDir[:slash], childDir[slash+1:]
			}
			if parent != dir {
				continue
			}
			entries = append(entries, TreeEntry{Mode: "40000", Name: name, OID: childOID})
		}
		data := buildTree(entries)
		oid := s.addObject(r, &GitObject{Type: ObjTree, Data: data})
		treeByDir[dir] = oid
	}
	return treeByDir[""]
}

// commitFile commits one file change on a branch. Returns new commit + blob oids.
func (s *Store) commitFile(r *Repo, branch, path string, content []byte, hasContent bool, message string) (commitOID string, blobOID string) {
	ref := "refs/heads/" + branch
	parentOID, hasParent := r.Refs[ref]
	if !hasParent {
		parentOID, hasParent = r.Refs["refs/heads/"+r.DefaultBranch]
	}
	files := map[string]fileRef{}
	if hasParent {
		if treeOID, ok := s.commitTree(r, parentOID); ok {
			files = s.flattenTree(r, treeOID, "")
		}
	}
	blobOID = ""
	if !hasContent {
		delete(files, path)
	} else {
		blobOID = s.addObject(r, &GitObject{Type: ObjBlob, Data: content})
		files[path] = fileRef{OID: blobOID, Mode: "100644"}
	}
	treeOID := s.buildTreeFromFiles(r, files)
	var parents []string
	if hasParent {
		parents = []string{parentOID}
	}
	msg := message
	if !strings.HasSuffix(msg, "\n") {
		msg += "\n"
	}
	commitData := buildCommit(treeOID, parents, msg)
	commitOID = s.addObject(r, &GitObject{Type: ObjCommit, Data: commitData})
	r.Refs[ref] = commitOID
	r.bump()
	return commitOID, blobOID
}

func (s *Store) filesAtBranch(r *Repo, branch string) map[string]fileRef {
	tip, ok := s.refCommit(r, "refs/heads/"+branch)
	if !ok {
		return map[string]fileRef{}
	}
	treeOID, ok := s.commitTree(r, tip)
	if !ok {
		return map[string]fileRef{}
	}
	return s.flattenTree(r, treeOID, "")
}

// commitsFrom returns commits reachable from a ref, newest first (BFS order).
func (s *Store) commitsFrom(r *Repo, ref string) []string {
	tip, ok := s.resolveToCommit(r, ref)
	if !ok {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	queue := []string{tip}
	for len(queue) > 0 {
		oid := queue[0]
		queue = queue[1:]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		obj := r.Objects[oid]
		if obj == nil || obj.Type != ObjCommit {
			continue
		}
		out = append(out, oid)
		cv, err := parseCommit(obj.Data)
		if err != nil {
			continue
		}
		queue = append(queue, cv.Parents...)
	}
	return out
}

// --- issues / pulls ---

func (s *Store) createIssue(r *Repo, title, body string, labels []string) *Issue {
	number := r.NumberSeq
	r.NumberSeq++
	is := &Issue{
		ID:         s.nextSeq() + 100000,
		Number:     number,
		Title:      title,
		Body:       body,
		State:      "open",
		Labels:     append([]string{}, labels...),
		CreatedSeq: s.nextSeq(),
	}
	is.nodeID = globalID("Issue", strconv.Itoa(r.ID)+":"+strconv.Itoa(number))
	r.Issues = append(r.Issues, is)
	r.bump()
	return is
}

func (s *Store) createPull(r *Repo, title, body, head, base string) *PullRequest {
	headSha, ok := s.refCommit(r, "refs/heads/"+head)
	if !ok {
		headSha = ZeroOID
	}
	baseSha, ok := s.refCommit(r, "refs/heads/"+base)
	if !ok {
		baseSha = ZeroOID
	}
	number := r.NumberSeq
	r.NumberSeq++
	p := &PullRequest{
		ID:         s.nextSeq() + 100000,
		Number:     number,
		Title:      title,
		Body:       body,
		State:      "open",
		Head:       head,
		Base:       base,
		HeadSha:    headSha,
		BaseSha:    baseSha,
		CreatedSeq: s.nextSeq(),
	}
	r.Pulls = append(r.Pulls, p)
	r.bump()
	return p
}

type pullFile struct {
	Filename  string
	Status    string
	Additions int
	Deletions int
	Changes   int
}

func (s *Store) pullFiles(r *Repo, p *PullRequest) []pullFile {
	baseFiles := s.filesAtBranch(r, p.Base)
	headFiles := s.filesAtBranch(r, p.Head)
	paths := map[string]bool{}
	for k := range baseFiles {
		paths[k] = true
	}
	for k := range headFiles {
		paths[k] = true
	}
	var sorted []string
	for k := range paths {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	var out []pullFile
	for _, path := range sorted {
		base, hasBase := baseFiles[path]
		head, hasHead := headFiles[path]
		if hasBase && hasHead && base.OID == head.OID {
			continue
		}
		var status string
		var add, del int
		switch {
		case !hasBase && hasHead:
			status = "added"
			data := r.Objects[head.OID].Data
			add = countLines(string(data))
		case hasBase && !hasHead:
			status = "removed"
			data := r.Objects[base.OID].Data
			del = countLines(string(data))
		default:
			status = "modified"
			oldText := string(r.Objects[base.OID].Data)
			newText := string(r.Objects[head.OID].Data)
			add, del = lineDiff(oldText, newText)
		}
		out = append(out, pullFile{Filename: path, Status: status, Additions: add, Deletions: del, Changes: add + del})
	}
	return out
}

func (s *Store) pullConflicts(r *Repo, p *PullRequest) bool {
	baseTip, ok1 := s.refCommit(r, "refs/heads/"+p.Base)
	headTip, ok2 := s.refCommit(r, "refs/heads/"+p.Head)
	if !ok1 || !ok2 {
		return true
	}
	mergeBase := s.mergeBase(r, headTip, baseTip)
	if mergeBase == "" {
		return false
	}
	baseFiles := s.filesAtBranch(r, p.Base)
	headFiles := s.filesAtBranch(r, p.Head)
	var mergeBaseFiles map[string]fileRef
	if treeOID, ok := s.commitTree(r, mergeBase); ok {
		mergeBaseFiles = s.flattenTree(r, treeOID, "")
	} else {
		mergeBaseFiles = map[string]fileRef{}
	}
	paths := map[string]bool{}
	for k := range mergeBaseFiles {
		paths[k] = true
	}
	for k := range baseFiles {
		paths[k] = true
	}
	for k := range headFiles {
		paths[k] = true
	}
	for path := range paths {
		origin, hasOrigin := mergeBaseFiles[path]
		base, hasBase := baseFiles[path]
		head, hasHead := headFiles[path]
		originOID := ""
		if hasOrigin {
			originOID = origin.OID
		}
		baseOID, headOID := "", ""
		if hasBase {
			baseOID = base.OID
		}
		if hasHead {
			headOID = head.OID
		}
		baseChanged := baseOID != originOID
		headChanged := headOID != originOID
		if baseChanged && headChanged && baseOID != headOID {
			return true
		}
	}
	return false
}

func (s *Store) mergeBase(r *Repo, a, b string) string {
	if a == b {
		return a
	}
	ancestors := map[string]bool{}
	queue := []string{a}
	for len(queue) > 0 {
		oid := queue[0]
		queue = queue[1:]
		if ancestors[oid] {
			continue
		}
		ancestors[oid] = true
		if obj := r.Objects[oid]; obj != nil && obj.Type == ObjCommit {
			if cv, err := parseCommit(obj.Data); err == nil {
				queue = append(queue, cv.Parents...)
			}
		}
	}
	seen := map[string]bool{}
	queueB := []string{b}
	for len(queueB) > 0 {
		oid := queueB[0]
		queueB = queueB[1:]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		if ancestors[oid] {
			return oid
		}
		if obj := r.Objects[oid]; obj != nil && obj.Type == ObjCommit {
			if cv, err := parseCommit(obj.Data); err == nil {
				queueB = append(queueB, cv.Parents...)
			}
		}
	}
	return ""
}

func (s *Store) mergePull(r *Repo, p *PullRequest) string {
	if p.State != "open" || p.Merged {
		return ""
	}
	if s.pullConflicts(r, p) {
		return ""
	}
	baseTip, ok := s.refCommit(r, "refs/heads/"+p.Base)
	if !ok {
		return ""
	}
	headFiles := s.filesAtBranch(r, p.Head)
	headTip, _ := s.refCommit(r, "refs/heads/"+p.Head)
	mergeBase := s.mergeBase(r, headTip, baseTip)
	originFiles := map[string]fileRef{}
	if mergeBase != "" {
		if treeOID, ok := s.commitTree(r, mergeBase); ok {
			originFiles = s.flattenTree(r, treeOID, "")
		}
	}
	baseFiles := s.filesAtBranch(r, p.Base)
	merged := map[string]fileRef{}
	for k, v := range baseFiles {
		merged[k] = v
	}
	for k, v := range headFiles {
		merged[k] = v
	}
	for path := range originFiles {
		if _, hasHead := headFiles[path]; !hasHead {
			if base, hasBase := baseFiles[path]; hasBase {
				if origin, hasOrigin := originFiles[path]; hasOrigin && base.OID == origin.OID {
					delete(merged, path)
				}
			}
		}
	}
	treeOID := s.buildTreeFromFiles(r, merged)
	message := "Merge pull request #" + strconv.Itoa(p.Number) + " from " + p.Head + "\n\n" + p.Title + "\n"
	commitData := buildCommit(treeOID, []string{baseTip}, message)
	mergeOID := s.addObject(r, &GitObject{Type: ObjCommit, Data: commitData})
	r.Refs["refs/heads/"+p.Base] = mergeOID
	p.State = "closed"
	p.Merged = true
	mergedAt := forgeDate
	p.MergedAt = &mergedAt
	p.MergeCommit = &mergeOID
	p.BaseSha = mergeOID
	r.bump()
	return mergeOID
}

func (s *Store) pullMergeable(r *Repo, p *PullRequest) string {
	if p.State != "open" {
		return "UNKNOWN"
	}
	if s.pullConflicts(r, p) {
		return "CONFLICTING"
	}
	return "MERGEABLE"
}
