// mcp.go — the Model Context Protocol endpoint (POST /mcp, JSON-RPC 2.0 over
// Streamable HTTP). Port of mcp.ts. Serves both eras the suite's client speaks:
// the modern 2026-07-28 `server/discover` flow and the legacy handshake
// (initialize + notifications/initialized). Plain application/json replies; no SSE.
// Authentication is decided before any tool exists: no (or a wrong) bearer token
// -> HTTP 401.

package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

const mcpServerName = "github-core-clone"
const mcpServerVersion = "1.0.0"
const mcpSessionID = "forge-1"

var mcpSupportedVersions = []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

var mcpServerInfo = map[string]interface{}{"name": mcpServerName, "version": mcpServerVersion}

// The six tools the suite needs (canonical github-mcp-server names).
type mcpToolProp struct {
	Type        string   `json:"type"`
	Description string   `json:"description"`
	Enum        []string `json:"enum,omitempty"`
}

type mcpTool struct {
	Name        string                  `json:"name"`
	Description string                  `json:"description"`
	InputSchema mcpToolInputSchema      `json:"inputSchema"`
}

type mcpToolInputSchema struct {
	Type       string                 `json:"type"`
	Properties map[string]mcpToolProp `json:"properties"`
	Required   []string               `json:"required"`
}

var mcpTools = []mcpTool{
	{
		Name:        "create_repo",
		Description: "Create a repository owned by the authenticated user.",
		InputSchema: mcpToolInputSchema{
			Type: "object",
			Properties: map[string]mcpToolProp{
				"owner":       {Type: "string", Description: "The repository owner (defaults to the authenticated user)."},
				"name":        {Type: "string", Description: "The repository name."},
				"description": {Type: "string", Description: "A short description of the repository."},
				"private":     {Type: "boolean", Description: "Whether the repository is private."},
			},
			Required: []string{"name"},
		},
	},
	{
		Name:        "get_repo",
		Description: "Get a repository by owner and name.",
		InputSchema: mcpToolInputSchema{
			Type: "object",
			Properties: map[string]mcpToolProp{
				"owner": {Type: "string", Description: "The repository owner."},
				"repo":  {Type: "string", Description: "The repository name."},
			},
			Required: []string{"owner", "repo"},
		},
	},
	{
		Name:        "create_issue",
		Description: "Create an issue in a repository.",
		InputSchema: mcpToolInputSchema{
			Type: "object",
			Properties: map[string]mcpToolProp{
				"owner": {Type: "string", Description: "The repository owner."},
				"repo":  {Type: "string", Description: "The repository name."},
				"title": {Type: "string", Description: "The issue title."},
				"body":  {Type: "string", Description: "The issue body."},
			},
			Required: []string{"owner", "repo", "title"},
		},
	},
	{
		Name:        "list_issues",
		Description: "List the issues of a repository.",
		InputSchema: mcpToolInputSchema{
			Type: "object",
			Properties: map[string]mcpToolProp{
				"owner": {Type: "string", Description: "The repository owner."},
				"repo":  {Type: "string", Description: "The repository name."},
				"state": {Type: "string", Description: "Filter by state: open, closed or all.", Enum: []string{"open", "closed", "all"}},
			},
			Required: []string{"owner", "repo"},
		},
	},
	{
		Name:        "create_pull_request",
		Description: "Create a pull request in a repository.",
		InputSchema: mcpToolInputSchema{
			Type: "object",
			Properties: map[string]mcpToolProp{
				"owner": {Type: "string", Description: "The repository owner."},
				"repo":  {Type: "string", Description: "The repository name."},
				"title": {Type: "string", Description: "The pull request title."},
				"head":  {Type: "string", Description: "The head branch (the branch with the changes)."},
				"base":  {Type: "string", Description: "The base branch (the branch the changes merge into)."},
				"body":  {Type: "string", Description: "The pull request body."},
			},
			Required: []string{"owner", "repo", "title", "head", "base"},
		},
	},
	{
		Name:        "merge_pull_request",
		Description: "Merge a pull request (squash) in a repository.",
		InputSchema: mcpToolInputSchema{
			Type: "object",
			Properties: map[string]mcpToolProp{
				"owner":       {Type: "string", Description: "The repository owner."},
				"repo":        {Type: "string", Description: "The repository name."},
				"pull_number": {Type: "integer", Description: "The pull request number."},
			},
			Required: []string{"owner", "repo", "pull_number"},
		},
	},
}

var mcpToolRequired = map[string][]string{
	"create_repo":        {"name"},
	"get_repo":           {"owner", "repo"},
	"create_issue":       {"owner", "repo", "title"},
	"list_issues":        {"owner", "repo"},
	"create_pull_request": {"owner", "repo", "title", "head", "base"},
	"merge_pull_request": {"owner", "repo", "pull_number"},
}

type mcpToolResult struct {
	Content           []mcpToolContent `json:"content"`
	StructuredContent interface{}      `json:"structuredContent"`
	IsError           bool             `json:"isError"`
}

type mcpToolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func mcpToolOk(payload interface{}) mcpToolResult {
	text, _ := json.Marshal(payload)
	return mcpToolResult{
		Content:           []mcpToolContent{{Type: "text", Text: string(text)}},
		StructuredContent: payload,
		IsError:           false,
	}
}

func mcpToolError(message string) mcpToolResult {
	return mcpToolResult{
		Content:           []mcpToolContent{{Type: "text", Text: message}},
		StructuredContent: map[string]interface{}{"error": message},
		IsError:           true,
	}
}

func mcpRPCResult(id interface{}, result interface{}) response {
	return jsonResp(200, map[string]interface{}{
		"jsonrpc": "2.0", "id": id, "result": result,
	}, map[string]string{"mcp-session-id": mcpSessionID})
}

func mcpRPCError(id interface{}, code int, message string) response {
	return jsonResp(200, map[string]interface{}{
		"jsonrpc": "2.0", "id": id, "error": map[string]interface{}{"code": code, "message": message},
	}, map[string]string{"mcp-session-id": mcpSessionID})
}

func mcpArgString(args map[string]interface{}, name string) string {
	if s, ok := args[name].(string); ok && s != "" {
		return s
	}
	return ""
}

func mcpArgNumber(args map[string]interface{}, name string) (int, bool) {
	switch v := args[name].(type) {
	case float64:
		if v == float64(int(v)) {
			return int(v), true
		}
	case string:
		if n, err := strconv.Atoi(v); err == nil {
			return n, true
		}
	}
	return 0, false
}

// ownerFor: the owner this call acts as, binding the token to the first owner it names.
func mcpOwnerFor(s *Store, token string, args map[string]interface{}) string {
	named := mcpArgString(args, "owner")
	bound, ok := s.loginFor(token)
	if ok {
		if named != "" && named != bound {
			return ""
		}
		return bound
	}
	if named == "" {
		return ""
	}
	return s.bindLogin(token, named)
}

func mcpRepoFor(s *Store, token string, args map[string]interface{}) (*Repo, string) {
	owner := mcpOwnerFor(s, token, args)
	if owner == "" {
		return nil, "The repository owner is unknown: the token names no login yet"
	}
	name := mcpArgString(args, "repo")
	if name == "" {
		return nil, "Missing required argument: repo"
	}
	repo := s.getRepo(owner, name)
	if repo == nil {
		return nil, "Repository " + owner + "/" + name + " not found"
	}
	return repo, ""
}

func mcpCallTool(s *Store, token, name string, args map[string]interface{}) mcpToolResult {
	switch name {
	case "create_repo":
		repoName := mcpArgString(args, "name")
		if repoName == "" {
			return mcpToolError("Missing required argument: name")
		}
		owner := mcpOwnerFor(s, token, args)
		if owner == "" {
			return mcpToolError("The repository owner is unknown: the token names no login yet")
		}
		if s.getRepo(owner, repoName) != nil {
			return mcpToolError("Repository " + owner + "/" + repoName + " already exists")
		}
		private := false
		if b, ok := args["private"].(bool); ok {
			private = b
		}
		repo := s.createRepo(&owner, token, repoName, private, mcpArgString(args, "description"))
		return mcpToolOk(repoJSON(s, repo))
	case "get_repo":
		repo, errMsg := mcpRepoFor(s, token, args)
		if errMsg != "" {
			return mcpToolError(errMsg)
		}
		return mcpToolOk(repoJSON(s, repo))
	case "create_issue":
		repo, errMsg := mcpRepoFor(s, token, args)
		if errMsg != "" {
			return mcpToolError(errMsg)
		}
		title := mcpArgString(args, "title")
		if title == "" {
			return mcpToolError("Missing required argument: title")
		}
		issue := s.createIssue(repo, title, mcpArgString(args, "body"), []string{})
		return mcpToolOk(issueJSON(s, repo, issue))
	case "list_issues":
		repo, errMsg := mcpRepoFor(s, token, args)
		if errMsg != "" {
			return mcpToolError(errMsg)
		}
		state := mcpArgString(args, "state")
		if state == "" {
			state = "open"
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
		items := make([]interface{}, 0, len(list))
		for _, is := range list {
			items = append(items, issueJSON(s, repo, is))
		}
		return mcpToolOk(map[string]interface{}{"issues": items})
	case "create_pull_request":
		repo, errMsg := mcpRepoFor(s, token, args)
		if errMsg != "" {
			return mcpToolError(errMsg)
		}
		title := mcpArgString(args, "title")
		head := mcpArgString(args, "head")
		base := mcpArgString(args, "base")
		if title == "" {
			return mcpToolError("Missing required argument: title")
		}
		if head == "" {
			return mcpToolError("Missing required argument: head")
		}
		if base == "" {
			return mcpToolError("Missing required argument: base")
		}
		if _, ok := s.refCommit(repo, "refs/heads/"+head); !ok {
			return mcpToolError("Head branch '" + head + "' does not exist")
		}
		if _, ok := s.refCommit(repo, "refs/heads/"+base); !ok {
			return mcpToolError("Base branch '" + base + "' does not exist")
		}
		for _, p := range repo.Pulls {
			if p.Head == head && p.Base == base && p.State == "open" {
				return mcpToolError("A pull request already exists for this head and base")
			}
		}
		pull := s.createPull(repo, title, mcpArgString(args, "body"), head, base)
		return mcpToolOk(pullJSON(s, repo, pull))
	case "merge_pull_request":
		repo, errMsg := mcpRepoFor(s, token, args)
		if errMsg != "" {
			return mcpToolError(errMsg)
		}
		number, ok := mcpArgNumber(args, "pull_number")
		if !ok {
			return mcpToolError("Missing required argument: pull_number")
		}
		var pull *PullRequest
		for _, p := range repo.Pulls {
			if p.Number == number {
				pull = p
				break
			}
		}
		if pull == nil {
			return mcpToolError("Pull request #" + strconv.Itoa(number) + " not found")
		}
		if pull.Merged {
			return mcpToolError("Pull request is already merged")
		}
		if pull.State != "open" {
			return mcpToolError("Pull request is closed")
		}
		if sha := s.mergePull(repo, pull); sha == "" {
			return mcpToolError("Merge conflict")
		}
		return mcpToolOk(pullJSON(s, repo, pull))
	default:
		return mcpToolError("Unknown tool: " + name)
	}
}

func routeMcp(s *Store, req *http.Request, segments []string, token string, body []byte) (response, bool) {
	if len(segments) != 1 || segments[0] != "mcp" {
		return response{}, false
	}

	// A session close is best-effort: any 2xx answer.
	if req.Method == "DELETE" {
		return jsonResp(200, map[string]interface{}{}, nil), true
	}
	if req.Method == "GET" {
		return errorResp(405, "Method not allowed"), true
	}
	if req.Method != "POST" {
		return errorResp(405, "Method not allowed"), true
	}

	if !tokenAccepted(token) {
		return errorResp(401, "Requires authentication"), true
	}

	var parsed interface{}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return jsonResp(400, map[string]interface{}{
			"jsonrpc": "2.0", "id": nil,
			"error": map[string]interface{}{"code": -32700, "message": "Parse error"},
		}, nil), true
	}
	rec, ok := parsed.(map[string]interface{})
	if !ok {
		return jsonResp(400, map[string]interface{}{
			"jsonrpc": "2.0", "id": nil,
			"error": map[string]interface{}{"code": -32600, "message": "Invalid Request"},
		}, nil), true
	}
	method, ok := rec["method"].(string)
	if !ok {
		id, _ := rec["id"]
		return jsonResp(400, map[string]interface{}{
			"jsonrpc": "2.0", "id": id,
			"error": map[string]interface{}{"code": -32600, "message": "Invalid Request"},
		}, nil), true
	}
	id, hasID := rec["id"]
	params := map[string]interface{}{}
	if p, ok := rec["params"].(map[string]interface{}); ok {
		params = p
	}

	// A notification (no id) gets no JSON-RPC answer.
	if !hasID {
		return response{status: 202, noBody: true}, true
	}

	switch method {
	case "server/discover":
		return mcpRPCResult(id, map[string]interface{}{
			"serverInfo":        mcpServerInfo,
			"capabilities":      map[string]interface{}{"tools": map[string]interface{}{"listChanged": false}},
			"supportedVersions": mcpSupportedVersions,
			"_meta": map[string]interface{}{
				"io.modelcontextprotocol/serverInfo":        mcpServerInfo,
				"io.modelcontextprotocol/protocolVersion": "2026-07-28",
			},
		}), true
	case "initialize":
		requested := ""
		if v, ok := params["protocolVersion"].(string); ok {
			requested = v
		}
		negotiated := "2025-11-25"
		for _, v := range mcpSupportedVersions {
			if v == requested {
				negotiated = requested
				break
			}
		}
		return mcpRPCResult(id, map[string]interface{}{
			"protocolVersion": negotiated,
			"capabilities":    map[string]interface{}{"tools": map[string]interface{}{"listChanged": false}},
			"serverInfo":      mcpServerInfo,
		}), true
	case "ping":
		return mcpRPCResult(id, map[string]interface{}{}), true
	case "tools/list":
		return mcpRPCResult(id, map[string]interface{}{"tools": mcpTools}), true
	case "tools/call":
		toolName := ""
		if n, ok := params["name"].(string); ok {
			toolName = n
		}
		args := map[string]interface{}{}
		if a, ok := params["arguments"].(map[string]interface{}); ok {
			args = a
		}
		known := false
		for _, t := range mcpTools {
			if t.Name == toolName {
				known = true
				break
			}
		}
		if !known {
			return mcpRPCError(id, -32602, "Unknown tool: "+toolName), true
		}
		var missing []string
		for _, req := range mcpToolRequired[toolName] {
			v, present := args[req]
			if !present || v == nil || v == "" {
				missing = append(missing, req)
			}
		}
		if len(missing) > 0 {
			return mcpRPCResult(id, mcpToolError("Missing required argument(s): "+strings.Join(missing, ", "))), true
		}
		return mcpRPCResult(id, mcpCallTool(s, token, toolName, args)), true
	default:
		return mcpRPCError(id, -32601, "Method not found: "+method), true
	}
}
