// rest.go — GitHub REST v3: repositories, branches, commits, contents, issues,
// pulls, labels. Port of rest.ts. Every reply is application/json with a non-empty
// X-GitHub-Request-Id (server.go adds the header).
//
// Performance: GET /repos/{owner}/{repo}/issues/{number} — the bench's read
// workload (2,000 sequential GETs of one issue) — serves pre-serialized JSON bytes
// cached on the Issue, valid while the repo version is unchanged. Any repo mutation
// bumps the version, so the cache can never go stale.

package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var repoNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)
var identityRe = regexp.MustCompile(`^(.*?) <([^>]*)>`)

type page struct {
	perPage int
	page    int
}

func pageOf(r *http.Request) page {
	perPage, err := strconv.Atoi(r.URL.Query().Get("per_page"))
	if err != nil || perPage <= 0 {
		perPage = 30
	} else if perPage > 100 {
		perPage = 100
	}
	pageNum, err := strconv.Atoi(r.URL.Query().Get("page"))
	if err != nil || pageNum <= 0 {
		pageNum = 1
	}
	return page{perPage: perPage, page: pageNum}
}

// paginate slices items and builds the GitHub Link header (next/last/first/prev).
func paginate(r *http.Request, items []interface{}) ([]interface{}, string) {
	p := pageOf(r)
	start := (p.page - 1) * p.perPage
	if start > len(items) {
		start = len(items)
	}
	end := start + p.perPage
	if end > len(items) {
		end = len(items)
	}
	slice := items[start:end]
	totalPages := (len(items) + p.perPage - 1) / p.perPage
	if totalPages < 1 {
		totalPages = 1
	}
	base := "http://" + r.Host + r.URL.Path
	var links []string
	mk := func(pageNum int) string {
		q := r.URL.Query()
		q.Set("page", strconv.Itoa(pageNum))
		q.Set("per_page", strconv.Itoa(p.perPage))
		return "<" + base + "?" + q.Encode() + ">"
	}
	if p.page < totalPages {
		links = append(links, mk(p.page+1)+`; rel="next"`)
	}
	if totalPages > 1 {
		links = append(links, mk(totalPages)+`; rel="last"`)
		links = append(links, mk(1)+`; rel="first"`)
	}
	if p.page > 1 && totalPages > 1 {
		links = append(links, mk(p.page-1)+`; rel="prev"`)
	}
	link := ""
	if len(links) > 0 {
		link = strings.Join(links, ", ")
	}
	return slice, link
}

func linkHeader(link string) map[string]string {
	if link == "" {
		return nil
	}
	return map[string]string{"Link": link}
}

// --- serializers ---

func repoJSON(s *Store, r *Repo) map[string]interface{} {
	return map[string]interface{}{
		"id":                 r.ID,
		"node_id":            repoGlobalID(r),
		"name":               r.Name,
		"full_name":          r.Owner + "/" + r.Name,
		"private":            r.Private,
		"owner":              map[string]interface{}{"login": r.Owner, "id": 1, "type": "User"},
		"description":        r.Description,
		"default_branch":     r.DefaultBranch,
		"has_issues":         true,
		"has_pull_requests":  true,
		"html_url":           "/" + r.Owner + "/" + r.Name,
		"url":                "/api/v3/repos/" + r.Owner + "/" + r.Name,
		"created_at":         forgeDate,
		"updated_at":         forgeDate,
	}
}

func labelJSON(l *Label) map[string]interface{} {
	return map[string]interface{}{"name": l.Name, "color": l.Color, "description": ""}
}

func issueJSON(s *Store, r *Repo, is *Issue) map[string]interface{} {
	labels := make([]interface{}, 0, len(is.Labels))
	for _, name := range is.Labels {
		if l, ok := r.Labels[name]; ok {
			labels = append(labels, labelJSON(l))
		} else {
			labels = append(labels, labelJSON(&Label{Name: name, Color: "ededed"}))
		}
	}
	return map[string]interface{}{
		"id":         is.ID,
		"node_id":    issueGlobalID(r, is),
		"number":     is.Number,
		"title":      is.Title,
		"body":       is.Body,
		"state":      is.State,
		"labels":     labels,
		"user":       map[string]interface{}{"login": r.Owner, "id": 1, "type": "User"},
		"comments":   len(is.Comments),
		"html_url":   "/" + r.Owner + "/" + r.Name + "/issues/" + strconv.Itoa(is.Number),
		"url":        "/api/v3/repos/" + r.Owner + "/" + r.Name + "/issues/" + strconv.Itoa(is.Number),
		"created_at": forgeDate,
		"updated_at": forgeDate,
	}
}

// issueJSONBytes returns the serialized GET-issue body, cached per repo version.
func issueJSONBytes(s *Store, r *Repo, is *Issue) []byte {
	if is.cachedJSON != nil && is.cachedVersion == r.version {
		return is.cachedJSON
	}
	body, _ := json.Marshal(issueJSON(s, r, is))
	is.cachedJSON = body
	is.cachedVersion = r.version
	return body
}

func pullJSON(s *Store, r *Repo, p *PullRequest) map[string]interface{} {
	headSha := p.HeadSha
	if sha, ok := s.refCommit(r, "refs/heads/"+p.Head); ok {
		headSha = sha
	}
	baseSha := p.BaseSha
	if sha, ok := s.refCommit(r, "refs/heads/"+p.Base); ok {
		baseSha = sha
	}
	var mergedAt interface{}
	if p.MergedAt != nil {
		mergedAt = *p.MergedAt
	}
	var mergeCommit interface{}
	if p.MergeCommit != nil {
		mergeCommit = *p.MergeCommit
	}
	return map[string]interface{}{
		"id":               p.ID,
		"node_id":          pullGlobalID(r, p),
		"number":           p.Number,
		"title":            p.Title,
		"body":             p.Body,
		"state":            p.State,
		"merged":           p.Merged,
		"merged_at":        mergedAt,
		"merge_commit_sha": mergeCommit,
		"head":             map[string]interface{}{"ref": p.Head, "sha": headSha, "label": r.Owner + ":" + p.Head},
		"base":             map[string]interface{}{"ref": p.Base, "sha": baseSha, "label": r.Owner + ":" + p.Base},
		"user":             map[string]interface{}{"login": r.Owner, "id": 1, "type": "User"},
		"html_url":         "/" + r.Owner + "/" + r.Name + "/pull/" + strconv.Itoa(p.Number),
		"url":              "/api/v3/repos/" + r.Owner + "/" + r.Name + "/pulls/" + strconv.Itoa(p.Number),
		"created_at":       forgeDate,
		"updated_at":       forgeDate,
	}
}

func commentJSON(s *Store, r *Repo, is *Issue, c *IssueComment) map[string]interface{} {
	return map[string]interface{}{
		"id":         c.ID,
		"node_id":    commentGlobalID(r, is, c),
		"body":       c.Body,
		"user":       map[string]interface{}{"login": r.Owner, "id": 1, "type": "User"},
		"html_url":   "/" + r.Owner + "/" + r.Name + "/issues/" + strconv.Itoa(is.Number) + "#issuecomment-" + strconv.Itoa(c.ID),
		"url":        "/api/v3/repos/" + r.Owner + "/" + r.Name + "/issues/" + strconv.Itoa(is.Number) + "/comments/" + strconv.Itoa(c.ID),
		"created_at": forgeDate,
		"updated_at": forgeDate,
	}
}

// --- routing ---

func routeRest(s *Store, req *http.Request, segments []string, token string, body []byte) (response, bool) {
	if len(segments) < 2 || segments[0] != "api" || segments[1] != "v3" {
		return response{}, false
	}
	rest := segments[2:]
	method := req.Method

	if len(rest) == 0 && method == "GET" {
		return jsonResp(200, map[string]interface{}{
			"current_user_url": "/api/v3/user",
			"rate_limit_url":   "/api/v3/rate_limit",
		}, map[string]string{"x-oauth-scopes": "repo, read:org"}), true
	}
	if len(rest) == 1 && rest[0] == "meta" && method == "GET" {
		return jsonResp(200, map[string]interface{}{"installed_version": "1.0.0"}, nil), true
	}

	if !tokenAccepted(token) {
		return errorResp(401, "Requires authentication"), true
	}

	if len(rest) > 0 && rest[0] == "user" {
		if len(rest) == 1 && method == "GET" {
			login, ok := s.loginFor(token)
			if !ok {
				return errorResp(404, "Not Found"), true
			}
			return jsonResp(200, map[string]interface{}{"login": login, "id": 1, "type": "User"}, nil), true
		}
		if len(rest) == 2 && rest[1] == "repos" {
			if method == "GET" {
				login, ok := s.loginFor(token)
				owner := ""
				if ok {
					owner = login
				}
				list := s.reposOf(owner, token)
				items := make([]interface{}, 0, len(list))
				for _, repo := range list {
					items = append(items, repoJSON(s, repo))
				}
				pageItems, link := paginate(req, items)
				return jsonResp(200, pageItems, linkHeader(link)), true
			}
			if method == "POST" {
				parsed, err := parseJSONBody(body)
				if err != nil {
					return errorResp(400, "Invalid JSON in request body"), true
				}
				rec := asRecord(parsed)
				if rec == nil {
					return errorResp(400, "Invalid request body"), true
				}
				name := strField(rec, "name")
				if name == "" || !repoNameRe.MatchString(name) {
					return errorResp(422, "Invalid repository name"), true
				}
				login, hasLogin := s.loginFor(token)
				if hasLogin && s.getRepo(login, name) != nil {
					return errorResp(422, "Repository already exists"), true
				}
				if !hasLogin {
					for _, repo := range s.pending[token] {
						if repo.Name == name {
							return errorResp(422, "Repository already exists"), true
						}
					}
				}
				var ownerPtr *string
				if hasLogin {
					ownerPtr = &login
				}
				private := false
				if b, ok := rec["private"].(bool); ok {
					private = b
				}
				repo := s.createRepo(ownerPtr, token, name, private, strField(rec, "description"))
				return jsonResp(201, repoJSON(s, repo), nil), true
			}
		}
		return errorResp(404, "Not Found"), true
	}

	if len(rest) >= 3 && rest[0] == "repos" {
		owner, name := rest[1], rest[2]
		tail := rest[3:]
		login := loginForRequest(s, token, owner)
		if login == "" {
			return errorResp(404, "Not Found"), true
		}
		repo := s.getRepo(owner, name)
		if repo == nil {
			return errorResp(404, "Not Found"), true
		}
		return routeRepo(s, req, repo, tail, token, body), true
	}

	return errorResp(404, "Not Found"), true
}

func routeRepo(s *Store, req *http.Request, repo *Repo, tail []string, token string, body []byte) response {
	method := req.Method

	if len(tail) == 0 {
		if method == "GET" {
			return jsonResp(200, repoJSON(s, repo), nil)
		}
		if method == "PATCH" {
			parsed, err := parseJSONBody(body)
			if err != nil {
				return errorResp(400, "Invalid JSON in request body")
			}
			if rec := asRecord(parsed); rec != nil {
				if d, ok := rec["description"].(string); ok {
					repo.Description = d
				}
				if b, ok := rec["default_branch"].(string); ok && b != "" {
					repo.DefaultBranch = b
				}
				repo.bump()
			}
			return jsonResp(200, repoJSON(s, repo), nil)
		}
		if method == "DELETE" {
			s.deleteRepo(repo.Owner, repo.Name)
			return response{status: 204, noBody: true}
		}
		return errorResp(404, "Not Found")
	}

	head := tail[0]

	if head == "branches" {
		if len(tail) == 1 && method == "GET" {
			var names []string
			for ref := range repo.Refs {
				if strings.HasPrefix(ref, "refs/heads/") {
					names = append(names, ref[len("refs/heads/"):])
				}
			}
			sort.Strings(names)
			ordered := []string{repo.DefaultBranch}
			for _, n := range names {
				if n != repo.DefaultBranch {
					ordered = append(ordered, n)
				}
			}
			var branches []interface{}
			for _, name := range ordered {
				sha, ok := s.refCommit(repo, "refs/heads/"+name)
				if !ok {
					continue
				}
				branches = append(branches, map[string]interface{}{
					"name": name, "commit": map[string]interface{}{"sha": sha}, "protected": false,
				})
			}
			return jsonResp(200, branches, nil)
		}
		if len(tail) == 2 && method == "GET" {
			sha, ok := s.refCommit(repo, "refs/heads/"+tail[1])
			if !ok {
				return errorResp(404, "Not Found")
			}
			return jsonResp(200, map[string]interface{}{
				"name": tail[1], "commit": map[string]interface{}{"sha": sha}, "protected": false,
			}, nil)
		}
		return errorResp(404, "Not Found")
	}

	if head == "commits" {
		if method == "GET" {
			ref := req.URL.Query().Get("sha")
			if ref == "" {
				ref = repo.DefaultBranch
			}
			oids := s.commitsFrom(repo, ref)
			out := make([]interface{}, 0, len(oids))
			for _, oid := range oids {
				obj := repo.Objects[oid]
				if obj == nil {
					continue
				}
				cv, err := parseCommit(obj.Data)
				if err != nil {
					continue
				}
				author := parseIdentity(cv.Author)
				committer := parseIdentity(cv.Committer)
				out = append(out, map[string]interface{}{
					"sha": oid,
					"commit": map[string]interface{}{
						"message":   strings.TrimRight(cv.Message, "\n"),
						"author":    map[string]interface{}{"name": author.name, "email": author.email, "date": forgeDate},
						"committer": map[string]interface{}{"name": committer.name, "email": committer.email, "date": forgeDate},
					},
					"author":    map[string]interface{}{"login": repo.Owner},
					"committer": map[string]interface{}{"login": repo.Owner},
				})
			}
			return jsonResp(200, out, nil)
		}
		return errorResp(404, "Not Found")
	}

	if head == "git" {
		if len(tail) >= 2 && tail[1] == "refs" {
			if method == "POST" && len(tail) == 2 {
				parsed, err := parseJSONBody(body)
				if err != nil {
					return errorResp(400, "Invalid JSON in request body")
				}
				rec := asRecord(parsed)
				if rec == nil || strField(rec, "ref") == "" || strField(rec, "sha") == "" {
					return errorResp(422, "Invalid ref")
				}
				ref, sha := strField(rec, "ref"), strField(rec, "sha")
				if !strings.HasPrefix(ref, "refs/heads/") && !strings.HasPrefix(ref, "refs/tags/") {
					return errorResp(422, "Invalid ref")
				}
				if !isObjectID(sha) || repo.Objects[sha] == nil {
					return errorResp(422, "Invalid sha")
				}
				if _, ok := repo.Refs[ref]; ok {
					return errorResp(422, "Reference already exists")
				}
				typ := repo.Objects[sha].Type
				if strings.HasPrefix(ref, "refs/heads/") && typ != ObjCommit {
					return errorResp(422, "Invalid sha")
				}
				repo.Refs[ref] = sha
				repo.bump()
				return jsonResp(201, map[string]interface{}{
					"ref": ref, "object": map[string]interface{}{"sha": sha, "type": typ.Name()},
				}, nil)
			}
			if method == "GET" && len(tail) >= 3 {
				refPath := strings.Join(tail[2:], "/")
				full := refPath
				if !strings.HasPrefix(refPath, "refs/") {
					full = "refs/" + refPath
				}
				sha, ok := repo.Refs[full]
				if !ok {
					return errorResp(404, "Not Found")
				}
				typ := "commit"
				if obj := repo.Objects[sha]; obj != nil {
					typ = obj.Type.Name()
				}
				return jsonResp(200, map[string]interface{}{
					"ref": full, "object": map[string]interface{}{"sha": sha, "type": typ},
				}, nil)
			}
		}
		return errorResp(404, "Not Found")
	}

	if head == "contents" {
		return routeContents(s, req, repo, tail[1:], body)
	}

	if head == "labels" {
		if method == "POST" && len(tail) == 1 {
			parsed, err := parseJSONBody(body)
			if err != nil {
				return errorResp(400, "Invalid JSON in request body")
			}
			rec := asRecord(parsed)
			if rec == nil || strField(rec, "name") == "" {
				return errorResp(422, "Invalid label")
			}
			name := strField(rec, "name")
			if existing, ok := repo.Labels[name]; ok {
				return jsonResp(201, labelJSON(existing), nil)
			}
			color := strField(rec, "color")
			if color == "" {
				color = "ededed"
			}
			label := &Label{Name: name, Color: color}
			repo.Labels[name] = label
			repo.bump()
			return jsonResp(201, labelJSON(label), nil)
		}
		if method == "GET" && len(tail) == 1 {
			var out []interface{}
			for _, l := range repo.Labels {
				out = append(out, labelJSON(l))
			}
			return jsonResp(200, out, nil)
		}
		return errorResp(404, "Not Found")
	}

	if head == "issues" {
		return routeIssues(s, req, repo, tail[1:], body)
	}

	if head == "pulls" {
		return routePulls(s, req, repo, tail[1:], body)
	}

	return errorResp(404, "Not Found")
}

type identity struct{ name, email string }

func parseIdentity(line string) identity {
	m := identityRe.FindStringSubmatch(line)
	if m == nil {
		return identity{name: line}
	}
	return identity{name: m[1], email: m[2]}
}

// --- contents ---

func routeContents(s *Store, req *http.Request, repo *Repo, pathSegments []string, body []byte) response {
	method := req.Method
	path := strings.Join(pathSegments, "/")
	refQuery := req.URL.Query().Get("ref")

	if method == "GET" {
		branch := refQuery
		if branch == "" {
			branch = repo.DefaultBranch
		}
		files := s.filesAtBranch(repo, branch)
		if path == "" {
			return jsonResp(200, dirEntries(repo, files, ""), nil)
		}
		if file, ok := files[path]; ok {
			data := []byte{}
			if obj := repo.Objects[file.OID]; obj != nil {
				data = obj.Data
			}
			name := path
			if idx := strings.LastIndex(path, "/"); idx >= 0 {
				name = path[idx+1:]
			}
			return jsonResp(200, map[string]interface{}{
				"type": "file", "encoding": "base64", "size": len(data),
				"name": name, "path": path,
				"content": base64.StdEncoding.EncodeToString(data),
				"sha":     file.OID,
			}, nil)
		}
		prefix := path + "/"
		for key := range files {
			if strings.HasPrefix(key, prefix) {
				return jsonResp(200, dirEntries(repo, files, path), nil)
			}
		}
		return errorResp(404, "Not Found")
	}

	if method == "PUT" {
		parsed, err := parseJSONBody(body)
		if err != nil {
			return errorResp(400, "Invalid JSON in request body")
		}
		rec := asRecord(parsed)
		msg, msgOK := rec["message"].(string)
		contentStr, contentOK := rec["content"].(string)
		if rec == nil || !msgOK || !contentOK {
			return errorResp(422, "Invalid content")
		}
		branch := strField(rec, "branch")
		if branch == "" {
			branch = repo.DefaultBranch
		}
		files := s.filesAtBranch(repo, branch)
		existing, hasExisting := files[path]
		sha, hasSha := rec["sha"].(string)
		if hasExisting {
			if !hasSha {
				return errorResp(409, "sha is required to update an existing file")
			}
			if sha != existing.OID {
				return errorResp(409, "sha does not match the current file")
			}
		} else if hasSha {
			return errorResp(409, "sha does not match: the file does not exist")
		}
		cleaned := regexp.MustCompile(`[\r\n\t ]+`).ReplaceAllString(contentStr, "")
		content, err := base64.StdEncoding.DecodeString(cleaned)
		if err != nil {
			return errorResp(422, "Invalid base64 content")
		}
		commitOID, blobOID := s.commitFile(repo, branch, path, content, true, msg)
		status := 201
		if hasExisting {
			status = 200
		}
		name := path
		if idx := strings.LastIndex(path, "/"); idx >= 0 {
			name = path[idx+1:]
		}
		return jsonResp(status, map[string]interface{}{
			"content": map[string]interface{}{"name": name, "path": path, "sha": blobOID, "size": len(content), "encoding": "base64", "type": "file"},
			"commit":  map[string]interface{}{"sha": commitOID, "message": msg},
		}, nil)
	}

	if method == "DELETE" {
		parsed, _ := parseJSONBody(body)
		rec := asRecord(parsed)
		branch := ""
		if rec != nil {
			branch = strField(rec, "branch")
		}
		if branch == "" {
			branch = repo.DefaultBranch
		}
		files := s.filesAtBranch(repo, branch)
		existing, ok := files[path]
		if !ok {
			return errorResp(404, "Not Found")
		}
		if rec != nil {
			if sha, isStr := rec["sha"].(string); isStr && sha != existing.OID {
				return errorResp(409, "sha does not match the current file")
			}
		}
		message := "Delete " + path
		if rec != nil {
			if m := strField(rec, "message"); m != "" {
				message = m
			}
		}
		commitOID, _ := s.commitFile(repo, branch, path, nil, false, message)
		return jsonResp(200, map[string]interface{}{
			"commit": map[string]interface{}{"sha": commitOID, "message": message},
		}, nil)
	}

	return errorResp(404, "Not Found")
}

func dirEntries(repo *Repo, files map[string]fileRef, dir string) []interface{} {
	prefix := ""
	if dir != "" {
		prefix = dir + "/"
	}
	seen := map[string]struct {
		typ string
		oid string
	}{}
	for path, file := range files {
		if !strings.HasPrefix(path, prefix) {
			continue
		}
		rest := path[len(prefix):]
		if rest == "" {
			continue
		}
		if slash := strings.IndexByte(rest, '/'); slash < 0 {
			seen[rest] = struct {
				typ string
				oid string
			}{"file", file.OID}
		} else {
			name := rest[:slash]
			if _, ok := seen[name]; !ok {
				seen[name] = struct {
					typ string
					oid string
				}{"dir", ""}
			}
		}
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]interface{}, 0, len(names))
	for _, name := range names {
		entry := seen[name]
		sha := entry.oid
		if sha == "" {
			sha = strings.Repeat("0", 40)
		}
		size := 0
		if entry.typ == "file" {
			if obj := repo.Objects[entry.oid]; obj != nil {
				size = len(obj.Data)
			}
		}
		out = append(out, map[string]interface{}{
			"type": entry.typ, "name": name, "path": prefix + name, "sha": sha, "size": size,
		})
	}
	return out
}

// --- issues ---

func routeIssues(s *Store, req *http.Request, repo *Repo, tail []string, body []byte) response {
	method := req.Method

	if len(tail) == 0 {
		if method == "POST" {
			parsed, err := parseJSONBody(body)
			if err != nil {
				return errorResp(400, "Invalid JSON in request body")
			}
			rec := asRecord(parsed)
			if rec == nil || strField(rec, "title") == "" {
				return errorResp(422, "Invalid issue")
			}
			var labels []string
			if arr, ok := rec["labels"].([]interface{}); ok {
				for _, e := range arr {
					if str, ok := e.(string); ok {
						labels = append(labels, str)
					}
				}
			}
			for _, label := range labels {
				if _, ok := repo.Labels[label]; !ok {
					return errorResp(422, "Label '"+label+"' does not exist")
				}
			}
			issue := s.createIssue(repo, strField(rec, "title"), strField(rec, "body"), labels)
			return jsonResp(201, issueJSON(s, repo, issue), nil)
		}
		if method == "GET" {
			state := req.URL.Query().Get("state")
			if state == "" {
				state = "open"
			}
			sortBy := req.URL.Query().Get("sort")
			if sortBy == "" {
				sortBy = "created"
			}
			direction := req.URL.Query().Get("direction")
			if direction == "" {
				direction = "desc"
			}
			var list []*Issue
			for _, is := range repo.Issues {
				if state == "open" && is.State != "open" {
					continue
				}
				if state == "closed" && is.State != "closed" {
					continue
				}
				list = append(list, is)
			}
			if sortBy == "created" {
				if direction == "asc" {
					sort.Slice(list, func(i, j int) bool { return list[i].CreatedSeq < list[j].CreatedSeq })
				} else {
					sort.Slice(list, func(i, j int) bool { return list[i].CreatedSeq > list[j].CreatedSeq })
				}
			}
			items := make([]interface{}, 0, len(list))
			for _, is := range list {
				items = append(items, issueJSON(s, repo, is))
			}
			pageItems, link := paginate(req, items)
			return jsonResp(200, pageItems, linkHeader(link))
		}
		return errorResp(404, "Not Found")
	}

	number, err := strconv.Atoi(tail[0])
	if err != nil || number <= 0 {
		return errorResp(404, "Not Found")
	}
	var issue *Issue
	for _, is := range repo.Issues {
		if is.Number == number {
			issue = is
			break
		}
	}
	if issue == nil {
		return errorResp(404, "Not Found")
	}

	if len(tail) == 1 {
		if method == "GET" {
			// Hot path (bench read workload): serve cached serialized bytes.
			return rawResp(200, issueJSONBytes(s, repo, issue), "application/json", nil)
		}
		if method == "PATCH" {
			parsed, err := parseJSONBody(body)
			if err != nil {
				return errorResp(400, "Invalid JSON in request body")
			}
			if rec := asRecord(parsed); rec != nil {
				if st, ok := rec["state"].(string); ok && (st == "open" || st == "closed") {
					issue.State = st
				}
				if t, ok := rec["title"].(string); ok {
					issue.Title = t
				}
				if b, ok := rec["body"].(string); ok {
					issue.Body = b
				}
				repo.bump()
			}
			return jsonResp(200, issueJSON(s, repo, issue), nil)
		}
		return errorResp(404, "Not Found")
	}

	if tail[1] == "comments" {
		if method == "POST" && len(tail) == 2 {
			parsed, err := parseJSONBody(body)
			if err != nil {
				return errorResp(400, "Invalid JSON in request body")
			}
			rec := asRecord(parsed)
			if rec == nil {
				return errorResp(422, "Invalid comment")
			}
			bodyText, ok := rec["body"].(string)
			if !ok {
				return errorResp(422, "Invalid comment")
			}
			comment := &IssueComment{ID: repo.CommentSeq, Body: bodyText, CreatedSeq: s.nextSeq()}
			repo.CommentSeq++
			issue.Comments = append(issue.Comments, *comment)
			repo.bump()
			return jsonResp(201, commentJSON(s, repo, issue, comment), nil)
		}
		if method == "GET" && len(tail) == 2 {
			out := make([]interface{}, 0, len(issue.Comments))
			for i := range issue.Comments {
				out = append(out, commentJSON(s, repo, issue, &issue.Comments[i]))
			}
			return jsonResp(200, out, nil)
		}
		return errorResp(404, "Not Found")
	}

	return errorResp(404, "Not Found")
}

// --- pull requests ---

func routePulls(s *Store, req *http.Request, repo *Repo, tail []string, body []byte) response {
	method := req.Method

	if len(tail) == 0 {
		if method == "POST" {
			parsed, err := parseJSONBody(body)
			if err != nil {
				return errorResp(400, "Invalid JSON in request body")
			}
			rec := asRecord(parsed)
			if rec == nil || strField(rec, "title") == "" || strField(rec, "head") == "" || strField(rec, "base") == "" {
				return errorResp(422, "Invalid pull request")
			}
			head, base := strField(rec, "head"), strField(rec, "base")
			if _, ok := s.refCommit(repo, "refs/heads/"+head); !ok {
				return errorResp(422, "Head branch '"+head+"' does not exist")
			}
			if _, ok := s.refCommit(repo, "refs/heads/"+base); !ok {
				return errorResp(422, "Base branch '"+base+"' does not exist")
			}
			for _, p := range repo.Pulls {
				if p.Head == head && p.Base == base && p.State == "open" {
					return errorResp(422, "A pull request already exists for this head and base")
				}
			}
			pull := s.createPull(repo, strField(rec, "title"), strField(rec, "body"), head, base)
			return jsonResp(201, pullJSON(s, repo, pull), nil)
		}
		if method == "GET" {
			state := req.URL.Query().Get("state")
			if state == "" {
				state = "open"
			}
			var list []*PullRequest
			for _, p := range repo.Pulls {
				if state == "open" && p.State != "open" {
					continue
				}
				if state == "closed" && p.State != "closed" {
					continue
				}
				list = append(list, p)
			}
			sort.Slice(list, func(i, j int) bool { return list[i].CreatedSeq > list[j].CreatedSeq })
			items := make([]interface{}, 0, len(list))
			for _, p := range list {
				items = append(items, pullJSON(s, repo, p))
			}
			pageItems, link := paginate(req, items)
			return jsonResp(200, pageItems, linkHeader(link))
		}
		return errorResp(404, "Not Found")
	}

	number, err := strconv.Atoi(tail[0])
	if err != nil || number <= 0 {
		return errorResp(404, "Not Found")
	}
	var pull *PullRequest
	for _, p := range repo.Pulls {
		if p.Number == number {
			pull = p
			break
		}
	}
	if pull == nil {
		return errorResp(404, "Not Found")
	}

	if len(tail) == 1 {
		if method == "GET" {
			return jsonResp(200, pullJSON(s, repo, pull), nil)
		}
		return errorResp(404, "Not Found")
	}

	if tail[1] == "files" && method == "GET" && len(tail) == 2 {
		files := s.pullFiles(repo, pull)
		out := make([]interface{}, 0, len(files))
		for _, f := range files {
			out = append(out, map[string]interface{}{
				"filename": f.Filename, "status": f.Status,
				"additions": f.Additions, "deletions": f.Deletions, "changes": f.Changes,
			})
		}
		return jsonResp(200, out, nil)
	}

	if tail[1] == "merge" && method == "PUT" && len(tail) == 2 {
		if pull.Merged {
			return errorResp(405, "Pull request is already merged")
		}
		if pull.State != "open" {
			return errorResp(405, "Pull request is closed")
		}
		mergeSha := s.mergePull(repo, pull)
		if mergeSha == "" {
			return errorResp(405, "Merge conflict")
		}
		return jsonResp(200, map[string]interface{}{
			"merged": true, "sha": mergeSha, "message": "Pull request successfully merged",
		}, nil)
	}

	return errorResp(404, "Not Found")
}