// graphql.go — a small, targeted GraphQL executor over the forge's store.
// Port of graphql.ts: lexer, parser, executor, and the schema the suite's
// GraphQL and MCP clients send (viewer, repository, node, __type, the issue /
// pull-request queries, and the five mutations). A batch (a JSON array of
// operations) returns an array of results in order.

package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// --- lexer ---

type gqlTokenKind int

const (
	tokName gqlTokenKind = iota
	tokString
	tokInt
	tokFloat
	tokPunct
	tokEOF
)

type gqlToken struct {
	kind  gqlTokenKind
	value string
}

type gqlLexer struct {
	src string
	pos int
}

func (l *gqlLexer) peekChar() byte {
	if l.pos < len(l.src) {
		return l.src[l.pos]
	}
	return 0
}

func (l *gqlLexer) nextChar() byte {
	c := l.peekChar()
	l.pos++
	return c
}

func (l *gqlLexer) tokenize() ([]gqlToken, error) {
	var tokens []gqlToken
	for {
		l.skipIgnored()
		if l.pos >= len(l.src) {
			tokens = append(tokens, gqlToken{kind: tokEOF})
			return tokens, nil
		}
		c := l.peekChar()
		switch {
		case c == '"':
			tok, err := l.readString()
			if err != nil {
				return nil, err
			}
			tokens = append(tokens, tok)
		case c == '.' && strings.HasPrefix(l.src[l.pos:], "..."):
			l.pos += 3
			tokens = append(tokens, gqlToken{kind: tokPunct, value: "..."})
		case strings.ContainsRune("!$&():=@[]{|}", rune(c)):
			l.pos++
			tokens = append(tokens, gqlToken{kind: tokPunct, value: string(c)})
		case c >= '0' && c <= '9' || c == '-':
			tokens = append(tokens, l.readNumber())
		case c == '_' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z':
			tokens = append(tokens, l.readName())
		default:
			return nil, fmt.Errorf("GraphQL: unexpected character %q", string(c))
		}
	}
}

func (l *gqlLexer) skipIgnored() {
	for {
		c := l.peekChar()
		if c == 0 {
			return
		}
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',' {
			l.pos++
			continue
		}
		// U+FEFF byte-order mark (UTF-8: EF BB BF).
		if strings.HasPrefix(l.src[l.pos:], "\uFEFF") {
			l.pos += 3
			continue
		}
		if c == '#' {
			for l.pos < len(l.src) && l.peekChar() != '\n' {
				l.pos++
			}
			continue
		}
		return
	}
}

func (l *gqlLexer) readString() (gqlToken, error) {
	if strings.HasPrefix(l.src[l.pos:], `"""`) {
		l.pos += 3
		start := l.pos
		end := strings.Index(l.src[l.pos:], `"""`)
		if end < 0 {
			return gqlToken{}, fmt.Errorf("GraphQL: unterminated block string")
		}
		value := l.src[start : l.pos+end]
		l.pos += end + 3
		return gqlToken{kind: tokString, value: value}, nil
	}
	l.pos++ // opening quote
	var sb strings.Builder
	for {
		c := l.nextChar()
		if c == 0 {
			return gqlToken{}, fmt.Errorf("GraphQL: unterminated string")
		}
		if c == '"' {
			break
		}
		if c == '\\' {
			esc := l.nextChar()
			switch esc {
			case 'n':
				sb.WriteByte('\n')
			case 't':
				sb.WriteByte('\t')
			case 'r':
				sb.WriteByte('\r')
			case 'u':
				hex := l.src[l.pos : min(l.pos+4, len(l.src))]
				l.pos += 4
				n, _ := strconv.ParseInt(hex, 16, 32)
				sb.WriteRune(rune(n))
			default:
				sb.WriteByte(esc)
			}
			continue
		}
		sb.WriteByte(c)
	}
	return gqlToken{kind: tokString, value: sb.String()}, nil
}

func (l *gqlLexer) readNumber() gqlToken {
	start := l.pos
	if l.peekChar() == '-' {
		l.pos++
	}
	for isDigit(l.peekChar()) {
		l.pos++
	}
	isFloat := false
	if l.peekChar() == '.' && isDigit(l.srcByte(l.pos+1)) {
		isFloat = true
		l.pos++
		for isDigit(l.peekChar()) {
			l.pos++
		}
	}
	if c := l.peekChar(); c == 'e' || c == 'E' {
		isFloat = true
		l.pos++
		if c2 := l.peekChar(); c2 == '+' || c2 == '-' {
			l.pos++
		}
		for isDigit(l.peekChar()) {
			l.pos++
		}
	}
	kind := tokInt
	if isFloat {
		kind = tokFloat
	}
	return gqlToken{kind: kind, value: l.src[start:l.pos]}
}

func (l *gqlLexer) srcByte(i int) byte {
	if i < len(l.src) {
		return l.src[i]
	}
	return 0
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func (l *gqlLexer) readName() gqlToken {
	start := l.pos
	for {
		c := l.peekChar()
		if c == '_' || c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' {
			l.pos++
		} else {
			break
		}
	}
	return gqlToken{kind: tokName, value: l.src[start:l.pos]}
}

// --- parser ---

type gqlField struct {
	alias      string
	name       string
	args       map[string]interface{}
	selections []gqlSelection
}

type gqlFragment struct {
	typeCondition string
	selections    []gqlSelection
}

type gqlSpread struct {
	name string
}

type gqlSelection interface{ isSelection() }

func (gqlField) isSelection()    {}
func (gqlFragment) isSelection() {}
func (gqlSpread) isSelection()   {}

type gqlOperation struct {
	typ       string // "query" | "mutation"
	name      string
	variables map[string]interface{}
	selections []gqlSelection
}

type gqlFragmentDef struct {
	name          string
	typeCondition string
	selections    []gqlSelection
}

type gqlDocument struct {
	operations []*gqlOperation
	fragments  map[string]*gqlFragmentDef
}

type gqlParser struct {
	tokens []gqlToken
	index  int
}

func (p *gqlParser) peek() gqlToken  { return p.tokens[p.index] }
func (p *gqlParser) next() gqlToken  { t := p.tokens[p.index]; p.index++; return t }

func (p *gqlParser) expectPunct(value string) error {
	t := p.next()
	if t.kind != tokPunct || t.value != value {
		return fmt.Errorf("GraphQL: expected %s, got %s", value, t.value)
	}
	return nil
}

func (p *gqlParser) expectName(want ...string) (string, error) {
	t := p.next()
	if t.kind != tokName {
		return "", fmt.Errorf("GraphQL: expected name, got %s", t.value)
	}
	if len(want) > 0 && t.value != want[0] {
		return "", fmt.Errorf("GraphQL: expected name %s, got %s", want[0], t.value)
	}
	return t.value, nil
}

func (p *gqlParser) parseDocument() (*gqlDocument, error) {
	doc := &gqlDocument{fragments: map[string]*gqlFragmentDef{}}
	for p.peek().kind != tokEOF {
		t := p.peek()
		if t.kind == tokPunct && t.value == "{" {
			op, err := p.parseOperation("query", "")
			if err != nil {
				return nil, err
			}
			doc.operations = append(doc.operations, op)
			continue
		}
		if t.kind == tokName && t.value == "fragment" {
			frag, err := p.parseFragmentDefinition()
			if err != nil {
				return nil, err
			}
			doc.fragments[frag.name] = frag
			continue
		}
		if t.kind == tokName && (t.value == "query" || t.value == "mutation") {
			typ := p.next().value
			name := ""
			if p.peek().kind == tokName {
				name = p.next().value
			}
			op, err := p.parseOperation(typ, name)
			if err != nil {
				return nil, err
			}
			doc.operations = append(doc.operations, op)
			continue
		}
		return nil, fmt.Errorf("GraphQL: unexpected token %s", t.value)
	}
	return doc, nil
}

func (p *gqlParser) parseOperation(typ, name string) (*gqlOperation, error) {
	variables := map[string]interface{}{}
	if p.peek().kind == tokPunct && p.peek().value == "(" {
		p.next()
		for !(p.peek().kind == tokPunct && p.peek().value == ")") {
			if err := p.expectPunct("$"); err != nil {
				return nil, err
			}
			varName, err := p.expectName()
			if err != nil {
				return nil, err
			}
			if err := p.expectPunct(":"); err != nil {
				return nil, err
			}
			if err := p.parseType(); err != nil {
				return nil, err
			}
			var defaultValue interface{}
			if p.peek().kind == tokPunct && p.peek().value == "=" {
				p.next()
				defaultValue, err = p.parseValue()
				if err != nil {
					return nil, err
				}
			}
			variables[varName] = defaultValue
		}
		if err := p.expectPunct(")"); err != nil {
			return nil, err
		}
	}
	sels, err := p.parseSelectionSet()
	if err != nil {
		return nil, err
	}
	return &gqlOperation{typ: typ, name: name, variables: variables, selections: sels}, nil
}

func (p *gqlParser) parseType() error {
	if p.peek().kind == tokPunct && p.peek().value == "[" {
		p.next()
		if err := p.parseType(); err != nil {
			return err
		}
		if err := p.expectPunct("]"); err != nil {
			return err
		}
	} else {
		if _, err := p.expectName(); err != nil {
			return err
		}
	}
	if p.peek().kind == tokPunct && p.peek().value == "!" {
		p.next()
	}
	return nil
}

func (p *gqlParser) parseFragmentDefinition() (*gqlFragmentDef, error) {
	if _, err := p.expectName("fragment"); err != nil {
		return nil, err
	}
	name, err := p.expectName()
	if err != nil {
		return nil, err
	}
	if _, err := p.expectName("on"); err != nil {
		return nil, err
	}
	tc, err := p.expectName()
	if err != nil {
		return nil, err
	}
	sels, err := p.parseSelectionSet()
	if err != nil {
		return nil, err
	}
	return &gqlFragmentDef{name: name, typeCondition: tc, selections: sels}, nil
}

func (p *gqlParser) parseSelectionSet() ([]gqlSelection, error) {
	if err := p.expectPunct("{"); err != nil {
		return nil, err
	}
	var sels []gqlSelection
	for !(p.peek().kind == tokPunct && p.peek().value == "}") {
		sel, err := p.parseSelection()
		if err != nil {
			return nil, err
		}
		sels = append(sels, sel)
	}
	if err := p.expectPunct("}"); err != nil {
		return nil, err
	}
	return sels, nil
}

func (p *gqlParser) parseSelection() (gqlSelection, error) {
	if p.peek().kind == tokPunct && p.peek().value == "..." {
		p.next()
		if p.peek().kind == tokName && p.peek().value == "on" {
			p.next()
			tc, err := p.expectName()
			if err != nil {
				return nil, err
			}
			sels, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			return gqlFragment{typeCondition: tc, selections: sels}, nil
		}
		if p.peek().kind == tokName {
			return gqlSpread{name: p.next().value}, nil
		}
		sels, err := p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
		return gqlFragment{selections: sels}, nil
	}
	first, err := p.expectName()
	if err != nil {
		return nil, err
	}
	alias, name := "", first
	if p.peek().kind == tokPunct && p.peek().value == ":" {
		p.next()
		alias = first
		name, err = p.expectName()
		if err != nil {
			return nil, err
		}
	}
	args := map[string]interface{}{}
	if p.peek().kind == tokPunct && p.peek().value == "(" {
		p.next()
		for !(p.peek().kind == tokPunct && p.peek().value == ")") {
			argName, err := p.expectName()
			if err != nil {
				return nil, err
			}
			if err := p.expectPunct(":"); err != nil {
				return nil, err
			}
			val, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			args[argName] = val
		}
		if err := p.expectPunct(")"); err != nil {
			return nil, err
		}
	}
	// Directives (@skip, @include, ...): parsed and ignored.
	for p.peek().kind == tokPunct && p.peek().value == "@" {
		p.next()
		if _, err := p.expectName(); err != nil {
			return nil, err
		}
		if p.peek().kind == tokPunct && p.peek().value == "(" {
			p.next()
			depth := 1
			for depth > 0 {
				t := p.next()
				if t.kind == tokPunct && t.value == "(" {
					depth++
				}
				if t.kind == tokPunct && t.value == ")" {
					depth--
				}
			}
		}
	}
	var sels []gqlSelection
	if p.peek().kind == tokPunct && p.peek().value == "{" {
		sels, err = p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
	}
	return gqlField{alias: alias, name: name, args: args, selections: sels}, nil
}

// gqlVar marks a $variable reference; gqlEnum marks a bare enum name.
type gqlVar struct{ name string }
type gqlEnum struct{ name string }

func (p *gqlParser) parseValue() (interface{}, error) {
	t := p.next()
	switch t.kind {
	case tokString:
		return t.value, nil
	case tokInt:
		n, err := strconv.ParseInt(t.value, 10, 64)
		if err != nil {
			return nil, err
		}
		return float64(n), nil
	case tokFloat:
		f, err := strconv.ParseFloat(t.value, 64)
		if err != nil {
			return nil, err
		}
		return f, nil
	case tokPunct:
		switch t.value {
		case "$":
			name, err := p.expectName()
			if err != nil {
				return nil, err
			}
			return gqlVar{name}, nil
		case "[":
			var values []interface{}
			for !(p.peek().kind == tokPunct && p.peek().value == "]") {
				v, err := p.parseValue()
				if err != nil {
					return nil, err
				}
				values = append(values, v)
			}
			if err := p.expectPunct("]"); err != nil {
				return nil, err
			}
			return values, nil
		case "{":
			obj := map[string]interface{}{}
			for !(p.peek().kind == tokPunct && p.peek().value == "}") {
				key, err := p.expectName()
				if err != nil {
					return nil, err
				}
				if err := p.expectPunct(":"); err != nil {
					return nil, err
				}
				v, err := p.parseValue()
				if err != nil {
					return nil, err
				}
				obj[key] = v
			}
			if err := p.expectPunct("}"); err != nil {
				return nil, err
			}
			return obj, nil
		}
	case tokName:
		switch t.value {
		case "true":
			return true, nil
		case "false":
			return false, nil
		case "null":
			return nil, nil
		default:
			return gqlEnum{t.value}, nil
		}
	}
	return nil, fmt.Errorf("GraphQL: unexpected value %s", t.value)
}

// --- executor ---

type gqlError struct{ msg string }

func (e *gqlError) Error() string { return e.msg }

// gqlObject is one resolved node: its __typename plus the resolver's payload.
type gqlObject struct {
	typename string
	data     map[string]interface{}
}

type gqlCtx struct {
	store     *Store
	login     *string
	token     string
	variables map[string]interface{}
	fragments map[string]*gqlFragmentDef
}

type gqlResolver func(obj *gqlObject, args map[string]interface{}, gql *gqlCtx) interface{}

func resolveValue(value interface{}, variables map[string]interface{}) interface{} {
	switch v := value.(type) {
	case []interface{}:
		out := make([]interface{}, len(v))
		for i, e := range v {
			out[i] = resolveValue(e, variables)
		}
		return out
	case map[string]interface{}:
		out := map[string]interface{}{}
		for k, e := range v {
			out[k] = resolveValue(e, variables)
		}
		return out
	case gqlVar:
		if val, ok := variables[v.name]; ok && val != nil {
			return val
		}
		return nil
	case gqlEnum:
		return v.name
	}
	return value
}

func resolveArgs(args map[string]interface{}, variables map[string]interface{}) map[string]interface{} {
	out := map[string]interface{}{}
	for k, v := range args {
		out[k] = resolveValue(v, variables)
	}
	return out
}

func argString(args map[string]interface{}, name string) string {
	if s, ok := args[name].(string); ok {
		return s
	}
	return ""
}

func argInt(args map[string]interface{}, name string, fallback int) int {
	if f, ok := args[name].(float64); ok && !math.IsNaN(f) && !math.IsInf(f, 0) {
		return int(math.Floor(f))
	}
	return fallback
}

func encodeCursor(n int) string {
	return base64.StdEncoding.EncodeToString([]byte("cursor:" + strconv.Itoa(n)))
}

func decodeCursor(cursor string) (int, bool) {
	text, err := base64.StdEncoding.DecodeString(cursor)
	if err != nil {
		return 0, false
	}
	if !strings.HasPrefix(string(text), "cursor:") {
		return 0, false
	}
	n, err := strconv.Atoi(string(text)[len("cursor:"):])
	if err != nil {
		return 0, false
	}
	return n, true
}

func executeSelections(obj *gqlObject, selections []gqlSelection, gql *gqlCtx) (map[string]interface{}, error) {
	if obj == nil {
		return nil, nil
	}
	out := map[string]interface{}{}
	for _, sel := range selections {
		switch s := sel.(type) {
		case gqlSpread:
			if frag, ok := gql.fragments[s.name]; ok {
				sub, err := executeSelections(obj, frag.selections, gql)
				if err != nil {
					return nil, err
				}
				for k, v := range sub {
					out[k] = v
				}
			}
			continue
		case gqlFragment:
			if s.typeCondition != "" && s.typeCondition != obj.typename {
				continue
			}
			sub, err := executeSelections(obj, s.selections, gql)
			if err != nil {
				return nil, err
			}
			for k, v := range sub {
				out[k] = v
			}
			continue
		case gqlField:
			responseName := s.name
			if s.alias != "" {
				responseName = s.alias
			}
			if s.name == "__typename" {
				out[responseName] = obj.typename
				continue
			}
			fields := gqlSchema[obj.typename]
			resolver, ok := fields[s.name]
			if !ok {
				return nil, &gqlError{fmt.Sprintf("Cannot query field %q on type %q (%sOn%s)", s.name, obj.typename, s.name, obj.typename)}
			}
			args := resolveArgs(s.args, gql.variables)
			value := resolver(obj, args, gql)
			executed, err := executeValue(value, s.selections, gql)
			if err != nil {
				return nil, err
			}
			out[responseName] = executed
		}
	}
	return out, nil
}

func executeValue(value interface{}, selections []gqlSelection, gql *gqlCtx) (interface{}, error) {
	if value == nil {
		return nil, nil
	}
	if arr, ok := value.([]interface{}); ok {
		out := make([]interface{}, len(arr))
		for i, e := range arr {
			v, err := executeValue(e, selections, gql)
			if err != nil {
				return nil, err
			}
			out[i] = v
		}
		return out, nil
	}
	if obj, ok := value.(*gqlObject); ok {
		if len(selections) == 0 {
			return nil, nil
		}
		return executeSelections(obj, selections, gql)
	}
	return value, nil
}

func connectionObject(typename string, items []interface{}, first int, after *string) *gqlObject {
	start := 0
	if after != nil {
		if decoded, ok := decodeCursor(*after); ok {
			start = decoded + 1
		}
	}
	if start > len(items) {
		start = len(items)
	}
	end := start + first
	if end > len(items) {
		end = len(items)
	}
	pageItems := items[start:end]
	hasNextPage := start+len(pageItems) < len(items)
	var endCursor, startCursor interface{}
	if len(pageItems) > 0 {
		endCursor = encodeCursor(start + len(pageItems) - 1)
		startCursor = encodeCursor(start)
	}
	return &gqlObject{
		typename: typename,
		data: map[string]interface{}{
			"items":      pageItems,
			"totalCount": len(items),
			"pageInfo": &gqlObject{
				typename: "PageInfo",
				data: map[string]interface{}{
					"hasNextPage":     hasNextPage,
					"hasPreviousPage": start > 0,
					"startCursor":     startCursor,
					"endCursor":       endCursor,
				},
			},
		},
	}
}

// --- schema ---

func repoObject(repo *Repo) *gqlObject {
	return &gqlObject{typename: "Repository", data: map[string]interface{}{"repo": repo}}
}

func issueObject(repo *Repo, issue *Issue) *gqlObject {
	return &gqlObject{typename: "Issue", data: map[string]interface{}{"repo": repo, "issue": issue}}
}

func pullObject(repo *Repo, pull *PullRequest) *gqlObject {
	return &gqlObject{typename: "PullRequest", data: map[string]interface{}{"repo": repo, "pull": pull}}
}

func issuesConnection(repo *Repo, args map[string]interface{}) *gqlObject {
	first := argInt(args, "first", 30)
	if first < 0 {
		first = 0
	}
	if first > 100 {
		first = 100
	}
	var after *string
	if s, ok := args["after"].(string); ok {
		after = &s
	}
	var states []string
	if arr, ok := args["states"].([]interface{}); ok {
		for _, e := range arr {
			if s, ok := e.(string); ok {
				states = append(states, s)
			}
		}
	}
	var list []*Issue
	for _, is := range repo.Issues {
		list = append(list, is)
	}
	if states != nil {
		wanted := map[string]bool{}
		for _, st := range states {
			wanted[strings.ToUpper(st)] = true
		}
		var filtered []*Issue
		for _, is := range list {
			if wanted[strings.ToUpper(is.State)] {
				filtered = append(filtered, is)
			}
		}
		list = filtered
	}
	direction := "DESC"
	if orderBy, ok := args["orderBy"].(map[string]interface{}); ok {
		if d, ok := orderBy["direction"].(string); ok {
			direction = strings.ToUpper(d)
		}
	}
	if direction == "ASC" {
		sort.Slice(list, func(i, j int) bool { return list[i].CreatedSeq < list[j].CreatedSeq })
	} else {
		sort.Slice(list, func(i, j int) bool { return list[i].CreatedSeq > list[j].CreatedSeq })
	}
	items := make([]interface{}, 0, len(list))
	for _, is := range list {
		items = append(items, issueObject(repo, is))
	}
	return connectionObject("IssueConnection", items, first, after)
}

func pullsConnection(repo *Repo, args map[string]interface{}) *gqlObject {
	first := argInt(args, "first", 30)
	if first < 0 {
		first = 0
	}
	if first > 100 {
		first = 100
	}
	var after *string
	if s, ok := args["after"].(string); ok {
		after = &s
	}
	list := make([]*PullRequest, len(repo.Pulls))
	copy(list, repo.Pulls)
	sort.Slice(list, func(i, j int) bool { return list[i].CreatedSeq > list[j].CreatedSeq })
	items := make([]interface{}, 0, len(list))
	for _, p := range list {
		items = append(items, pullObject(repo, p))
	}
	return connectionObject("PullRequestConnection", items, first, after)
}

func bindToRepo(gql *gqlCtx, repo *Repo) {
	if gql.login == nil {
		login := gql.store.bindLogin(gql.token, repo.Owner)
		gql.login = &login
	}
}

func repoFromGlobalID(gql *gqlCtx, id string) *Repo {
	decoded := decodeGlobalID(id)
	if decoded == nil || decoded.kind != "Repository" {
		return nil
	}
	return gql.store.repoByID(decoded.repoID)
}

func issueFromGlobalID(gql *gqlCtx, id string) (*Repo, *Issue) {
	decoded := decodeGlobalID(id)
	if decoded == nil || decoded.kind != "Issue" {
		return nil, nil
	}
	repo := gql.store.repoByID(decoded.repoID)
	if repo == nil {
		return nil, nil
	}
	for _, is := range repo.Issues {
		if is.Number == decoded.number {
			return repo, is
		}
	}
	return nil, nil
}

func pullFromGlobalID(gql *gqlCtx, id string) (*Repo, *PullRequest) {
	decoded := decodeGlobalID(id)
	if decoded == nil || decoded.kind != "PullRequest" {
		return nil, nil
	}
	repo := gql.store.repoByID(decoded.repoID)
	if repo == nil {
		return nil, nil
	}
	for _, p := range repo.Pulls {
		if p.Number == decoded.number {
			return repo, p
		}
	}
	return nil, nil
}

var gqlSchema map[string]map[string]gqlResolver

func init() {
	gqlSchema = map[string]map[string]gqlResolver{
		"Query": {
			"viewer": func(_obj *gqlObject, _args map[string]interface{}, gql *gqlCtx) interface{} {
				if gql.login == nil {
					return nil
				}
				return &gqlObject{typename: "User", data: map[string]interface{}{"login": *gql.login}}
			},
			"repository": func(_obj *gqlObject, args map[string]interface{}, gql *gqlCtx) interface{} {
				owner, name := argString(args, "owner"), argString(args, "name")
				if owner == "" || name == "" {
					return nil
				}
				if gql.login == nil {
					login := gql.store.bindLogin(gql.token, owner)
					gql.login = &login
				}
				repo := gql.store.getRepo(owner, name)
				if repo == nil || repo.Owner != *gql.login {
					return nil
				}
				return repoObject(repo)
			},
			"node": func(_obj *gqlObject, args map[string]interface{}, gql *gqlCtx) interface{} {
				id := argString(args, "id")
				decoded := decodeGlobalID(id)
				if decoded == nil {
					return nil
				}
				repo := gql.store.repoByID(decoded.repoID)
				if repo == nil {
					return nil
				}
				switch decoded.kind {
				case "Repository":
					return repoObject(repo)
				case "Issue":
					for _, is := range repo.Issues {
						if is.Number == decoded.number {
							return issueObject(repo, is)
						}
					}
					return nil
				case "PullRequest":
					for _, p := range repo.Pulls {
						if p.Number == decoded.number {
							return pullObject(repo, p)
						}
					}
					return nil
				case "IssueComment":
					for _, is := range repo.Issues {
						if is.Number != decoded.number {
							continue
						}
						for i := range is.Comments {
							if is.Comments[i].ID == decoded.commentID {
								return &gqlObject{typename: "IssueComment", data: map[string]interface{}{"repo": repo, "issue": is, "comment": &is.Comments[i]}}
							}
						}
					}
					return nil
				}
				return nil
			},
			"__type": func(_obj *gqlObject, args map[string]interface{}, _gql *gqlCtx) interface{} {
				name := argString(args, "name")
				info, ok := typeInfo[name]
				if !ok {
					return nil
				}
				return &gqlObject{typename: "__Type", data: map[string]interface{}{"name": name, "fields": info.fields, "enumValues": info.enumValues}}
			},
		},
		"Mutation": {
			"createIssue": func(_obj *gqlObject, args map[string]interface{}, gql *gqlCtx) interface{} {
				input, _ := args["input"].(map[string]interface{})
				repo := repoFromGlobalID(gql, argString(input, "repositoryId"))
				if repo == nil {
					panic(&gqlError{"Repository not found"})
				}
				bindToRepo(gql, repo)
				title := argString(input, "title")
				body := argString(input, "body")
				issue := gql.store.createIssue(repo, title, body, []string{})
				return &gqlObject{typename: "CreateIssuePayload", data: map[string]interface{}{"issue": issueObject(repo, issue)}}
			},
			"closeIssue": func(_obj *gqlObject, args map[string]interface{}, gql *gqlCtx) interface{} {
				input, _ := args["input"].(map[string]interface{})
				repo, issue := issueFromGlobalID(gql, argString(input, "issueId"))
				if issue == nil {
					panic(&gqlError{"Issue not found"})
				}
				issue.State = "closed"
				repo.bump()
				return &gqlObject{typename: "CloseIssuePayload", data: map[string]interface{}{"issue": issueObject(repo, issue)}}
			},
			"createPullRequest": func(_obj *gqlObject, args map[string]interface{}, gql *gqlCtx) interface{} {
				input, _ := args["input"].(map[string]interface{})
				repo := repoFromGlobalID(gql, argString(input, "repositoryId"))
				if repo == nil {
					panic(&gqlError{"Repository not found"})
				}
				bindToRepo(gql, repo)
				base, head := argString(input, "baseRefName"), argString(input, "headRefName")
				title := argString(input, "title")
				if _, ok := gql.store.refCommit(repo, "refs/heads/"+head); !ok {
					panic(&gqlError{"Head branch '" + head + "' does not exist"})
				}
				if _, ok := gql.store.refCommit(repo, "refs/heads/"+base); !ok {
					panic(&gqlError{"Base branch '" + base + "' does not exist"})
				}
				for _, p := range repo.Pulls {
					if p.Head == head && p.Base == base && p.State == "open" {
						panic(&gqlError{"A pull request already exists for this head and base"})
					}
				}
				pull := gql.store.createPull(repo, title, "", head, base)
				return &gqlObject{typename: "CreatePullRequestPayload", data: map[string]interface{}{"pullRequest": pullObject(repo, pull)}}
			},
			"mergePullRequest": func(_obj *gqlObject, args map[string]interface{}, gql *gqlCtx) interface{} {
				input, _ := args["input"].(map[string]interface{})
				repo, pull := pullFromGlobalID(gql, argString(input, "pullRequestId"))
				if pull == nil {
					panic(&gqlError{"Pull request not found"})
				}
				if pull.Merged {
					panic(&gqlError{"Pull request is already merged"})
				}
				if pull.State != "open" {
					panic(&gqlError{"Pull request is closed"})
				}
				if sha := gql.store.mergePull(repo, pull); sha == "" {
					panic(&gqlError{"Merge conflict"})
				}
				return &gqlObject{typename: "MergePullRequestPayload", data: map[string]interface{}{"pullRequest": pullObject(repo, pull)}}
			},
			"addComment": func(_obj *gqlObject, args map[string]interface{}, gql *gqlCtx) interface{} {
				input, _ := args["input"].(map[string]interface{})
				repo, issue := issueFromGlobalID(gql, argString(input, "subjectId"))
				if issue == nil {
					panic(&gqlError{"Subject not found"})
				}
				body := argString(input, "body")
				comment := IssueComment{ID: repo.CommentSeq, Body: body, CreatedSeq: gql.store.nextSeq()}
				repo.CommentSeq++
				issue.Comments = append(issue.Comments, comment)
				repo.bump()
				return &gqlObject{
					typename: "AddCommentPayload",
					data: map[string]interface{}{
						"commentEdge": &gqlObject{
							typename: "IssueCommentEdge",
							data: map[string]interface{}{
								"node": &gqlObject{typename: "IssueComment", data: map[string]interface{}{"repo": repo, "issue": issue, "comment": &issue.Comments[len(issue.Comments)-1]}},
							},
						},
					},
				}
			},
		},
		"Repository": {
			"id":           func(obj *gqlObject, _args map[string]interface{}, _gql *gqlCtx) interface{} { return repoGlobalID(obj.data["repo"].(*Repo)) },
			"databaseId":   func(obj *gqlObject, _args map[string]interface{}, _gql *gqlCtx) interface{} { return obj.data["repo"].(*Repo).ID },
			"name":         func(obj *gqlObject, _args map[string]interface{}, _gql *gqlCtx) interface{} { return obj.data["repo"].(*Repo).Name },
			"nameWithOwner": func(obj *gqlObject, _args map[string]interface{}, _gql *gqlCtx) interface{} {
				r := obj.data["repo"].(*Repo)
				return r.Owner + "/" + r.Name
			},
			"owner": func(obj *gqlObject, _args map[string]interface{}, _gql *gqlCtx) interface{} {
				return &gqlObject{typename: "User", data: map[string]interface{}{"login": obj.data["repo"].(*Repo).Owner}}
			},
			"defaultBranchRef": func(obj *gqlObject, _args map[string]interface{}, gql *gqlCtx) interface{} {
				repo := obj.data["repo"].(*Repo)
				if _, ok := gql.store.refCommit(repo, "refs/heads/"+repo.DefaultBranch); !ok {
					return nil
				}
				return &gqlObject{typename: "Ref", data: map[string]interface{}{"name": repo.DefaultBranch}}
			},
			"viewerPermission": func(_obj *gqlObject, _args map[string]interface{}, _gql *gqlCtx) interface{} { return "ADMIN" },
			"hasIssuesEnabled": func(_obj *gqlObject, _args map[string]interface{}, _gql *gqlCtx) interface{} { return true },
			"issue": func(obj *gqlObject, args map[string]interface{}, _gql *gqlCtx) interface{} {
				repo := obj.data["repo"].(*Repo)
				for _, is := range repo.Issues {
					if is.Number == argInt(args, "number", 0) {
						return issueObject(repo, is)
					}
				}
				return nil
			},
			"issues":      func(obj *gqlObject, args map[string]interface{}, _gql *gqlCtx) interface{} { return issuesConnection(obj.data["repo"].(*Repo), args) },
			"pullRequests": func(obj *gqlObject, args map[string]interface{}, _gql *gqlCtx) interface{} { return pullsConnection(obj.data["repo"].(*Repo), args) },
			"pullRequest": func(obj *gqlObject, args map[string]interface{}, _gql *gqlCtx) interface{} {
				repo := obj.data["repo"].(*Repo)
				for _, p := range repo.Pulls {
					if p.Number == argInt(args, "number", 0) {
						return pullObject(repo, p)
					}
				}
				return nil
			},
		},
		"Issue": {
			"id":     func(obj *gqlObject, _args map[string]interface{}, _gql *gqlCtx) interface{} { return issueGlobalID(obj.data["repo"].(*Repo), obj.data["issue"].(*Issue)) },
			"number": func(obj *gqlObject, _args map[string]interface{}, _gql *gqlCtx) interface{} { return obj.data["issue"].(*Issue).Number },
			"title":  func(obj *gqlObject, _args map[string]interface{}, _gql *gqlCtx) interface{} { return obj.data["issue"].(*Issue).Title },
			"state":  func(obj *gqlObject, _args map[string]interface{}, _gql *gqlCtx) interface{} { return strings.ToUpper(obj.data["issue"].(*Issue).State) },
			"body":   func(obj *gqlObject, _args map[string]interface{}, _gql *gqlCtx) interface{} { return obj.data["issue"].(*Issue).Body },
			"url": func(obj *gqlObject, _args map[string]interface{}, _gql *gqlCtx) interface{} {
				r, is := obj.data["repo"].(*Repo), obj.data["issue"].(*Issue)
				return "/" + r.Owner + "/" + r.Name + "/issues/" + strconv.Itoa(is.Number)
			},
		},
		"PullRequest": {
			"id":     func(obj *gqlObject, _args map[string]interface{}, _gql *gqlCtx) interface{} { return pullGlobalID(obj.data["repo"].(*Repo), obj.data["pull"].(*PullRequest)) },
			"number": func(obj *gqlObject, _args map[string]interface{}, _gql *gqlCtx) interface{} { return obj.data["pull"].(*PullRequest).Number },
			"state": func(obj *gqlObject, _args map[string]interface{}, _gql *gqlCtx) interface{} {
				p := obj.data["pull"].(*PullRequest)
				if p.Merged {
					return "MERGED"
				}
				return strings.ToUpper(p.State)
			},
			"merged":      func(obj *gqlObject, _args map[string]interface{}, _gql *gqlCtx) interface{} { return obj.data["pull"].(*PullRequest).Merged },
			"headRefName": func(obj *gqlObject, _args map[string]interface{}, _gql *gqlCtx) interface{} { return obj.data["pull"].(*PullRequest).Head },
			"baseRefName": func(obj *gqlObject, _args map[string]interface{}, _gql *gqlCtx) interface{} { return obj.data["pull"].(*PullRequest).Base },
			"mergeable":   func(obj *gqlObject, _args map[string]interface{}, gql *gqlCtx) interface{} { return gql.store.pullMergeable(obj.data["repo"].(*Repo), obj.data["pull"].(*PullRequest)) },
			"files": func(obj *gqlObject, args map[string]interface{}, gql *gqlCtx) interface{} {
				repo, pull := obj.data["repo"].(*Repo), obj.data["pull"].(*PullRequest)
				first := argInt(args, "first", 30)
				if first < 0 {
					first = 0
				}
				if first > 100 {
					first = 100
				}
				var after *string
				if s, ok := args["after"].(string); ok {
					after = &s
				}
				files := gql.store.pullFiles(repo, pull)
				items := make([]interface{}, 0, len(files))
				for _, f := range files {
					items = append(items, &gqlObject{
						typename: "PullRequestChangedFile",
						data: map[string]interface{}{
							"path": f.Filename, "additions": f.Additions, "deletions": f.Deletions, "changes": f.Changes,
						},
					})
				}
				return connectionObject("PullRequestChangedFileConnection", items, first, after)
			},
		},
		"User": {
			"login": func(obj *gqlObject, _args map[string]interface{}, _gql *gqlCtx) interface{} { return obj.data["login"].(string) },
			"id":    func(_obj *gqlObject, _args map[string]interface{}, _gql *gqlCtx) interface{} { return 1 },
		},
		"Ref": {
			"name": func(obj *gqlObject, _args map[string]interface{}, _gql *gqlCtx) interface{} { return obj.data["name"].(string) },
		},
		"IssueConnection": {
			"nodes":      func(obj *gqlObject, _args map[string]interface{}, _gql *gqlCtx) interface{} { return obj.data["items"].([]interface{}) },
			"pageInfo":   func(obj *gqlObject, _args map[string]interface{}, _gql *gqlCtx) interface{} { return obj.data["pageInfo"].(*gqlObject) },
			"totalCount": func(obj *gqlObject, _args map[string]interface{}, _gql *gqlCtx) interface{} { return obj.data["totalCount"].(int) },
		},
		"PullRequestConnection": {
			"nodes":      func(obj *gqlObject, _args map[string]interface{}, _gql *gqlCtx) interface{} { return obj.data["items"].([]interface{}) },
			"pageInfo":   func(obj *gqlObject, _args map[string]interface{}, _gql *gqlCtx) interface{} { return obj.data["pageInfo"].(*gqlObject) },
			"totalCount": func(obj *gqlObject, _args map[string]interface{}, _gql *gqlCtx) interface{} { return obj.data["totalCount"].(int) },
		},
		"PullRequestChangedFileConnection": {
			"nodes":      func(obj *gqlObject, _args map[string]interface{}, _gql *gqlCtx) interface{} { return obj.data["items"].([]interface{}) },
			"pageInfo":   func(obj *gqlObject, _args map[string]interface{}, _gql *gqlCtx) interface{} { return obj.data["pageInfo"].(*gqlObject) },
			"totalCount": func(obj *gqlObject, _args map[string]interface{}, _gql *gqlCtx) interface{} { return obj.data["totalCount"].(int) },
		},
		"PullRequestChangedFile": {
			"path":      func(obj *gqlObject, _args map[string]interface{}, _gql *gqlCtx) interface{} { return obj.data["path"].(string) },
			"additions": func(obj *gqlObject, _args map[string]interface{}, _gql *gqlCtx) interface{} { return obj.data["additions"].(int) },
			"deletions": func(obj *gqlObject, _args map[string]interface{}, _gql *gqlCtx) interface{} { return obj.data["deletions"].(int) },
			"changes":   func(obj *gqlObject, _args map[string]interface{}, _gql *gqlCtx) interface{} { return obj.data["changes"].(int) },
		},
		"IssueComment": {
			"id": func(obj *gqlObject, _args map[string]interface{}, _gql *gqlCtx) interface{} {
				return commentGlobalID(obj.data["repo"].(*Repo), obj.data["issue"].(*Issue), obj.data["comment"].(*IssueComment))
			},
			"body": func(obj *gqlObject, _args map[string]interface{}, _gql *gqlCtx) interface{} { return obj.data["comment"].(*IssueComment).Body },
		},
		"IssueCommentEdge": {
			"node": func(obj *gqlObject, _args map[string]interface{}, _gql *gqlCtx) interface{} { return obj.data["node"].(*gqlObject) },
		},
		"PageInfo": {
			"hasNextPage":     func(obj *gqlObject, _args map[string]interface{}, _gql *gqlCtx) interface{} { return obj.data["hasNextPage"].(bool) },
			"hasPreviousPage": func(obj *gqlObject, _args map[string]interface{}, _gql *gqlCtx) interface{} { return obj.data["hasPreviousPage"].(bool) },
			"startCursor":     func(obj *gqlObject, _args map[string]interface{}, _gql *gqlCtx) interface{} { return obj.data["startCursor"] },
			"endCursor":       func(obj *gqlObject, _args map[string]interface{}, _gql *gqlCtx) interface{} { return obj.data["endCursor"] },
		},
		"__Type": {
			"name": func(obj *gqlObject, _args map[string]interface{}, _gql *gqlCtx) interface{} { return obj.data["name"].(string) },
			"kind": func(obj *gqlObject, _args map[string]interface{}, _gql *gqlCtx) interface{} {
				if obj.data["enumValues"] == nil {
					return "OBJECT"
				}
				return "ENUM"
			},
			"fields": func(obj *gqlObject, _args map[string]interface{}, _gql *gqlCtx) interface{} {
				fields, _ := obj.data["fields"].([]string)
				if fields == nil {
					return nil
				}
				out := make([]interface{}, 0, len(fields))
				for _, name := range fields {
					out = append(out, &gqlObject{typename: "__Field", data: map[string]interface{}{"name": name}})
				}
				return out
			},
			"enumValues": func(obj *gqlObject, _args map[string]interface{}, _gql *gqlCtx) interface{} {
				values, _ := obj.data["enumValues"].([]string)
				if values == nil {
					return nil
				}
				out := make([]interface{}, 0, len(values))
				for _, name := range values {
					out = append(out, &gqlObject{typename: "__EnumValue", data: map[string]interface{}{"name": name}})
				}
				return out
			},
		},
		"__Field": {
			"name": func(obj *gqlObject, _args map[string]interface{}, _gql *gqlCtx) interface{} { return obj.data["name"].(string) },
		},
		"__EnumValue": {
			"name": func(obj *gqlObject, _args map[string]interface{}, _gql *gqlCtx) interface{} { return obj.data["name"].(string) },
		},
		"CreateIssuePayload": {
			"issue": func(obj *gqlObject, _args map[string]interface{}, _gql *gqlCtx) interface{} { return obj.data["issue"].(*gqlObject) },
		},
		"CloseIssuePayload": {
			"issue": func(obj *gqlObject, _args map[string]interface{}, _gql *gqlCtx) interface{} { return obj.data["issue"].(*gqlObject) },
		},
		"CreatePullRequestPayload": {
			"pullRequest": func(obj *gqlObject, _args map[string]interface{}, _gql *gqlCtx) interface{} { return obj.data["pullRequest"].(*gqlObject) },
		},
		"MergePullRequestPayload": {
			"pullRequest": func(obj *gqlObject, _args map[string]interface{}, _gql *gqlCtx) interface{} { return obj.data["pullRequest"].(*gqlObject) },
		},
		"AddCommentPayload": {
			"commentEdge": func(obj *gqlObject, _args map[string]interface{}, _gql *gqlCtx) interface{} { return obj.data["commentEdge"].(*gqlObject) },
		},
	}
}

type typeInfoEntry struct {
	fields     []string
	enumValues []string
}

var typeInfo = map[string]typeInfoEntry{
	"Issue": {
		fields: []string{"id", "number", "title", "state", "body", "url"},
	},
	"PullRequest": {
		fields: []string{"id", "number", "title", "state", "merged", "headRefName", "baseRefName", "mergeable", "files"},
	},
	"Repository": {
		fields: []string{"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest"},
	},
	"SearchType": {
		enumValues: []string{"ISSUE", "PULL_REQUEST", "REPOSITORY", "USER", "DISCUSSION", "COMMIT", "MILESTONE", "MARKETPLACE_LISTING"},
	},
	"IssueComment": {
		fields: []string{"id", "body"},
	},
	"User": {
		fields: []string{"login", "id"},
	},
}

// --- HTTP entry point ---

func executeOperation(store *Store, token string, doc *gqlDocument, op *gqlOperation, variables map[string]interface{}) (result map[string]interface{}) {
	var login *string
	if l, ok := store.loginFor(token); ok {
		login = &l
	}
	merged := map[string]interface{}{}
	for k, v := range op.variables {
		merged[k] = v
	}
	for k, v := range variables {
		merged[k] = v
	}
	gql := &gqlCtx{store: store, login: login, token: token, variables: merged, fragments: doc.fragments}
	defer func() {
		if r := recover(); r != nil {
			if ge, ok := r.(*gqlError); ok {
				result = map[string]interface{}{"data": nil, "errors": []interface{}{map[string]interface{}{"message": ge.msg}}}
				return
			}
			panic(r)
		}
	}()
	rootType := "Query"
	if op.typ == "mutation" {
		rootType = "Mutation"
	}
	root := &gqlObject{typename: rootType, data: map[string]interface{}{}}
	data, err := executeSelections(root, op.selections, gql)
	if err != nil {
		if ge, ok := err.(*gqlError); ok {
			return map[string]interface{}{"data": nil, "errors": []interface{}{map[string]interface{}{"message": ge.msg}}}
		}
		panic(err)
	}
	return map[string]interface{}{"data": data}
}

func routeGraphql(s *Store, req *http.Request, segments []string, token string, body []byte) (response, bool) {
	if len(segments) != 2 || segments[0] != "api" || segments[1] != "graphql" {
		return response{}, false
	}
	if !tokenAccepted(token) {
		return errorResp(401, "Requires authentication"), true
	}
	var parsed interface{}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return errorResp(400, "Invalid JSON in request body"), true
	}

	runOne := func(entry interface{}) map[string]interface{} {
		rec, ok := entry.(map[string]interface{})
		if !ok {
			return map[string]interface{}{"errors": []interface{}{map[string]interface{}{"message": "Must provide query string."}}}
		}
		query, ok := rec["query"].(string)
		if !ok {
			return map[string]interface{}{"errors": []interface{}{map[string]interface{}{"message": "Must provide query string."}}}
		}
		variables := map[string]interface{}{}
		if v, ok := rec["variables"].(map[string]interface{}); ok {
			variables = v
		}
		lexer := &gqlLexer{src: query}
		tokens, err := lexer.tokenize()
		if err != nil {
			return map[string]interface{}{"data": nil, "errors": []interface{}{map[string]interface{}{"message": err.Error()}}}
		}
		parser := &gqlParser{tokens: tokens}
		doc, err := parser.parseDocument()
		if err != nil {
			return map[string]interface{}{"data": nil, "errors": []interface{}{map[string]interface{}{"message": err.Error()}}}
		}
		var op *gqlOperation
		if name, ok := rec["operationName"].(string); ok {
			for _, o := range doc.operations {
				if o.name == name {
					op = o
					break
				}
			}
		} else if len(doc.operations) > 0 {
			op = doc.operations[0]
		}
		if op == nil {
			return map[string]interface{}{"data": nil, "errors": []interface{}{map[string]interface{}{"message": "No operation found"}}}
		}
		return executeOperation(s, token, doc, op, variables)
	}

	if arr, ok := parsed.([]interface{}); ok {
		out := make([]interface{}, 0, len(arr))
		for _, entry := range arr {
			out = append(out, runOne(entry))
		}
		return jsonResp(200, out, nil), true
	}
	return jsonResp(200, runOne(parsed), nil), true
}
