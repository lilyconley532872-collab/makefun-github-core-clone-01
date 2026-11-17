// graphql.ts — a small, targeted GraphQL executor over the forge's store.
// Supports the operation documents the suite's GraphQL and MCP clients send:
// viewer, repository, node, __type, the issue/pull-request queries, and the five mutations.
// A batch (a JSON array of operations) returns an array of results in order.

import { errorResp, isRecord, jsonResp, type ReqCtx, type Resp } from './context.ts';
import { tokenAccepted } from './rest.ts';
import {
  commentGlobalId,
  decodeGlobalId,
  issueGlobalId,
  pullGlobalId,
  repoGlobalId,
  type Issue,
  type IssueComment,
  type PullRequest,
  type Repo,
  type Store,
} from './store.ts';

// ---------------------------------------------------------------------------------------------
// lexer
// ---------------------------------------------------------------------------------------------

interface Token {
  kind: 'name' | 'string' | 'int' | 'float' | 'punct' | 'eof';
  value: string;
}

class Lexer {
  private pos = 0;
  private readonly source: string;

  constructor(source: string) {
    this.source = source;
  }

  private peekChar(): string {
    return this.pos < this.source.length ? this.source[this.pos] : '';
  }

  private nextChar(): string {
    const char = this.peekChar();
    this.pos += 1;
    return char;
  }

  tokenize(): Token[] {
    const tokens: Token[] = [];
    for (;;) {
      this.skipIgnored();
      if (this.pos >= this.source.length) {
        tokens.push({ kind: 'eof', value: '' });
        return tokens;
      }
      const char = this.peekChar();
      if (char === '"') {
        tokens.push(this.readString());
        continue;
      }
      if (char === '.' && this.source.startsWith('...', this.pos)) {
        this.pos += 3;
        tokens.push({ kind: 'punct', value: '...' });
        continue;
      }
      if ('!$&():=@[]{|}'.includes(char)) {
        this.pos += 1;
        tokens.push({ kind: 'punct', value: char });
        continue;
      }
      if (/[0-9-]/.test(char)) {
        tokens.push(this.readNumber());
        continue;
      }
      if (/[_A-Za-z]/.test(char)) {
        tokens.push(this.readName());
        continue;
      }
      throw new Error(`GraphQL: unexpected character ${JSON.stringify(char)}`);
    }
  }

  private skipIgnored(): void {
    for (;;) {
      const char = this.peekChar();
      if (char === '' ) return;
      if (char === ' ' || char === '\t' || char === '\n' || char === '\r' || char === ',' || char === '﻿') {
        this.pos += 1;
        continue;
      }
      if (char === '#') {
        while (this.pos < this.source.length && this.peekChar() !== '\n') this.pos += 1;
        continue;
      }
      return;
    }
  }

  private readString(): Token {
    // A block string (""") or a normal double-quoted string.
    if (this.source.startsWith('"""', this.pos)) {
      this.pos += 3;
      const start = this.pos;
      const end = this.source.indexOf('"""', this.pos);
      if (end < 0) throw new Error('GraphQL: unterminated block string');
      const value = this.source.slice(start, end);
      this.pos = end + 3;
      return { kind: 'string', value };
    }
    this.pos += 1; // opening quote
    let value = '';
    for (;;) {
      const char = this.nextChar();
      if (char === '') throw new Error('GraphQL: unterminated string');
      if (char === '"') break;
      if (char === '\\') {
        const escaped = this.nextChar();
        if (escaped === 'n') value += '\n';
        else if (escaped === 't') value += '\t';
        else if (escaped === 'r') value += '\r';
        else if (escaped === 'u') {
          const hex = this.source.slice(this.pos, this.pos + 4);
          this.pos += 4;
          value += String.fromCharCode(Number.parseInt(hex, 16));
        } else value += escaped;
        continue;
      }
      value += char;
    }
    return { kind: 'string', value };
  }

  private readNumber(): Token {
    const start = this.pos;
    if (this.peekChar() === '-') this.pos += 1;
    while (/[0-9]/.test(this.peekChar())) this.pos += 1;
    let isFloat = false;
    if (this.peekChar() === '.' && /[0-9]/.test(this.source[this.pos + 1] ?? '')) {
      isFloat = true;
      this.pos += 1;
      while (/[0-9]/.test(this.peekChar())) this.pos += 1;
    }
    if (this.peekChar() === 'e' || this.peekChar() === 'E') {
      isFloat = true;
      this.pos += 1;
      if (this.peekChar() === '+' || this.peekChar() === '-') this.pos += 1;
      while (/[0-9]/.test(this.peekChar())) this.pos += 1;
    }
    return { kind: isFloat ? 'float' : 'int', value: this.source.slice(start, this.pos) };
  }

  private readName(): Token {
    const start = this.pos;
    while (/[_0-9A-Za-z]/.test(this.peekChar())) this.pos += 1;
    return { kind: 'name', value: this.source.slice(start, this.pos) };
  }
}

// ---------------------------------------------------------------------------------------------
// parser
// ---------------------------------------------------------------------------------------------

interface FieldNode {
  kind: 'field';
  alias?: string;
  name: string;
  args: Record<string, unknown>;
  selections?: SelectionNode[];
}

interface FragmentNode {
  kind: 'fragment';
  typeCondition?: string;
  selections: SelectionNode[];
}

interface SpreadNode {
  kind: 'spread';
  name: string;
}

type SelectionNode = FieldNode | FragmentNode | SpreadNode;

interface OperationNode {
  kind: 'operation';
  type: 'query' | 'mutation';
  name?: string;
  variables: Record<string, unknown>;
  selections: SelectionNode[];
}

interface FragmentDef {
  name: string;
  typeCondition: string;
  selections: SelectionNode[];
}

interface DocumentNode {
  operations: OperationNode[];
  fragments: Map<string, FragmentDef>;
}

class Parser {
  private index = 0;
  private readonly tokens: Token[];

  constructor(tokens: Token[]) {
    this.tokens = tokens;
  }

  private peek(): Token {
    return this.tokens[this.index];
  }

  private next(): Token {
    const token = this.tokens[this.index];
    this.index += 1;
    return token;
  }

  private expectPunct(value: string): void {
    const token = this.next();
    if (token.kind !== 'punct' || token.value !== value) throw new Error(`GraphQL: expected ${value}, got ${token.value}`);
  }

  private expectName(value?: string): string {
    const token = this.next();
    if (token.kind !== 'name' || (value !== undefined && token.value !== value)) {
      throw new Error(`GraphQL: expected name${value === undefined ? '' : ` ${value}`}, got ${token.value}`);
    }
    return token.value;
  }

  parseDocument(): DocumentNode {
    const document: DocumentNode = { operations: [], fragments: new Map() };
    while (this.peek().kind !== 'eof') {
      const token = this.peek();
      if (token.kind === 'punct' && token.value === '{') {
        document.operations.push(this.parseOperation('query', undefined));
        continue;
      }
      if (token.kind === 'name' && token.value === 'fragment') {
        const fragment = this.parseFragmentDefinition();
        document.fragments.set(fragment.name, fragment);
        continue;
      }
      if (token.kind === 'name' && (token.value === 'query' || token.value === 'mutation')) {
        const type = this.next().value as 'query' | 'mutation';
        let name: string | undefined;
        if (this.peek().kind === 'name') name = this.next().value;
        document.operations.push(this.parseOperation(type, name));
        continue;
      }
      throw new Error(`GraphQL: unexpected token ${token.value}`);
    }
    return document;
  }

  private parseOperation(type: 'query' | 'mutation', name: string | undefined): OperationNode {
    const variables: Record<string, unknown> = {};
    if (this.peek().kind === 'punct' && this.peek().value === '(') {
      this.next();
      while (!(this.peek().kind === 'punct' && this.peek().value === ')')) {
        this.expectPunct('$');
        const varName = this.expectName();
        this.expectPunct(':');
        this.parseType();
        let defaultValue: unknown;
        if (this.peek().kind === 'punct' && this.peek().value === '=') {
          this.next();
          defaultValue = this.parseValue();
        }
        variables[varName] = defaultValue ?? null;
      }
      this.expectPunct(')');
    }
    const selections = this.parseSelectionSet();
    return { kind: 'operation', type, name, variables, selections };
  }

  private parseType(): void {
    if (this.peek().kind === 'punct' && this.peek().value === '[') {
      this.next();
      this.parseType();
      this.expectPunct(']');
    } else {
      this.expectName();
    }
    if (this.peek().kind === 'punct' && this.peek().value === '!') this.next();
  }

  private parseFragmentDefinition(): FragmentDef {
    this.expectName('fragment');
    const name = this.expectName();
    this.expectName('on');
    const typeCondition = this.expectName();
    const selections = this.parseSelectionSet();
    return { name, typeCondition, selections };
  }

  private parseSelectionSet(): SelectionNode[] {
    this.expectPunct('{');
    const selections: SelectionNode[] = [];
    while (!(this.peek().kind === 'punct' && this.peek().value === '}')) {
      selections.push(this.parseSelection());
    }
    this.expectPunct('}');
    return selections;
  }

  private parseSelection(): SelectionNode {
    if (this.peek().kind === 'punct' && this.peek().value === '...') {
      this.next();
      if (this.peek().kind === 'name' && this.peek().value === 'on') {
        this.next();
        const typeCondition = this.expectName();
        return { kind: 'fragment', typeCondition, selections: this.parseSelectionSet() };
      }
      if (this.peek().kind === 'name') {
        return { kind: 'spread', name: this.next().value };
      }
      return { kind: 'fragment', selections: this.parseSelectionSet() };
    }
    const first = this.expectName();
    let alias: string | undefined;
    let name = first;
    if (this.peek().kind === 'punct' && this.peek().value === ':') {
      this.next();
      alias = first;
      name = this.expectName();
    }
    const args: Record<string, unknown> = {};
    if (this.peek().kind === 'punct' && this.peek().value === '(') {
      this.next();
      while (!(this.peek().kind === 'punct' && this.peek().value === ')')) {
        const argName = this.expectName();
        this.expectPunct(':');
        args[argName] = this.parseValue();
      }
      this.expectPunct(')');
    }
    // Directives (@skip, @include, ...): parsed and ignored.
    while (this.peek().kind === 'punct' && this.peek().value === '@') {
      this.next();
      this.expectName();
      if (this.peek().kind === 'punct' && this.peek().value === '(') {
        this.next();
        let depth = 1;
        while (depth > 0) {
          const token = this.next();
          if (token.kind === 'punct' && token.value === '(') depth += 1;
          if (token.kind === 'punct' && token.value === ')') depth -= 1;
        }
      }
    }
    let selections: SelectionNode[] | undefined;
    if (this.peek().kind === 'punct' && this.peek().value === '{') {
      selections = this.parseSelectionSet();
    }
    return { kind: 'field', alias, name, args, selections };
  }

  private parseValue(): unknown {
    const token = this.next();
    if (token.kind === 'string') return token.value;
    if (token.kind === 'int') return Number.parseInt(token.value, 10);
    if (token.kind === 'float') return Number.parseFloat(token.value);
    if (token.kind === 'punct' && token.value === '$') return { __var: this.expectName() };
    if (token.kind === 'punct' && token.value === '[') {
      const values: unknown[] = [];
      while (!(this.peek().kind === 'punct' && this.peek().value === ']')) values.push(this.parseValue());
      this.expectPunct(']');
      return values;
    }
    if (token.kind === 'punct' && token.value === '{') {
      const object: Record<string, unknown> = {};
      while (!(this.peek().kind === 'punct' && this.peek().value === '}')) {
        const key = this.expectName();
        this.expectPunct(':');
        object[key] = this.parseValue();
      }
      this.expectPunct('}');
      return object;
    }
    if (token.kind === 'name') {
      if (token.value === 'true') return true;
      if (token.value === 'false') return false;
      if (token.value === 'null') return null;
      return { __enum: token.value };
    }
    throw new Error(`GraphQL: unexpected value ${token.value}`);
  }
}

// ---------------------------------------------------------------------------------------------
// executor
// ---------------------------------------------------------------------------------------------

class GqlError extends Error {}

interface GqlObject {
  __typename: string;
  [key: string]: unknown;
}

interface GqlCtx {
  store: Store;
  /** The login bound to this request's token (null while unbound). */
  login: string | null;
  token: string;
  variables: Record<string, unknown>;
  fragments: Map<string, FragmentDef>;
}

type Resolver = (obj: GqlObject, args: Record<string, unknown>, gql: GqlCtx) => unknown;

function isGqlObject(value: unknown): value is GqlObject {
  return isRecord(value) && typeof value.__typename === 'string';
}

function resolveValue(value: unknown, variables: Record<string, unknown>): unknown {
  if (Array.isArray(value)) return value.map((entry) => resolveValue(entry, variables));
  if (isRecord(value)) {
    if (typeof value.__var === 'string') return variables[value.__var] ?? null;
    if (typeof value.__enum === 'string') return value.__enum;
    const out: Record<string, unknown> = {};
    for (const [key, entry] of Object.entries(value)) out[key] = resolveValue(entry, variables);
    return out;
  }
  return value;
}

function resolveArgs(args: Record<string, unknown>, variables: Record<string, unknown>): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const [key, value] of Object.entries(args)) out[key] = resolveValue(value, variables);
  return out;
}

function argString(args: Record<string, unknown>, name: string): string {
  const value = args[name];
  return typeof value === 'string' ? value : '';
}

function argInt(args: Record<string, unknown>, name: string, fallback: number): number {
  const value = args[name];
  return typeof value === 'number' && Number.isFinite(value) ? Math.floor(value) : fallback;
}

function encodeCursor(number: number): string {
  return Buffer.from(`cursor:${number}`).toString('base64');
}

function decodeCursor(cursor: string): number | null {
  try {
    const text = Buffer.from(cursor, 'base64').toString('utf8');
    const match = /^cursor:(\d+)$/.exec(text);
    return match === null ? null : Number.parseInt(match[1], 10);
  } catch {
    return null;
  }
}

function executeSelections(obj: GqlObject | null, selections: SelectionNode[], gql: GqlCtx): Record<string, unknown> | null {
  if (obj === null) return null;
  const out: Record<string, unknown> = {};
  for (const selection of selections) {
    if (selection.kind === 'spread') {
      const fragment = gql.fragments.get(selection.name);
      if (fragment !== undefined) {
        const sub = executeSelections(obj, fragment.selections, gql);
        if (sub !== null) Object.assign(out, sub);
      }
      continue;
    }
    if (selection.kind === 'fragment') {
      if (selection.typeCondition !== undefined && selection.typeCondition !== obj.__typename) continue;
      const sub = executeSelections(obj, selection.selections, gql);
      if (sub !== null) Object.assign(out, sub);
      continue;
    }
    const responseName = selection.alias ?? selection.name;
    if (selection.name === '__typename') {
      out[responseName] = obj.__typename;
      continue;
    }
    const fields = SCHEMA[obj.__typename];
    const resolver = fields?.[selection.name];
    if (resolver === undefined) {
      throw new GqlError(`Cannot query field "${selection.name}" on type "${obj.__typename}" (${selection.name}On${obj.__typename})`);
    }
    const args = resolveArgs(selection.args, gql.variables);
    const value = resolver(obj, args, gql);
    out[responseName] = executeValue(value, selection.selections, gql);
  }
  return out;
}

function executeValue(value: unknown, selections: SelectionNode[] | undefined, gql: GqlCtx): unknown {
  if (value === null || value === undefined) return null;
  if (Array.isArray(value)) return value.map((entry) => executeValue(entry, selections, gql));
  if (isGqlObject(value)) {
    if (selections === undefined || selections.length === 0) return null;
    return executeSelections(value, selections, gql);
  }
  return value;
}

function connectionObject(typename: string, items: GqlObject[], first: number, after: string | null): GqlObject {
  // Items are already in their final order; `after` is a cursor over the list's own index.
  let start = 0;
  if (after !== null) {
    const decoded = decodeCursor(after);
    if (decoded !== null) start = decoded + 1;
  }
  const page = items.slice(start, start + first);
  const hasNextPage = start + page.length < items.length;
  const endCursor = page.length === 0 ? null : encodeCursor(start + page.length - 1);
  const startCursor = page.length === 0 ? null : encodeCursor(start);
  return {
    __typename: typename,
    items: page,
    totalCount: items.length,
    pageInfo: {
      __typename: 'PageInfo',
      hasNextPage,
      hasPreviousPage: start > 0,
      startCursor,
      endCursor,
    },
  };
}

// ---------------------------------------------------------------------------------------------
// the schema: one resolver table per type
// ---------------------------------------------------------------------------------------------

function repoObject(repo: Repo): GqlObject {
  return { __typename: 'Repository', repo };
}

function issueObject(repo: Repo, issue: Issue): GqlObject {
  return { __typename: 'Issue', repo, issue };
}

function pullObject(repo: Repo, pull: PullRequest): GqlObject {
  return { __typename: 'PullRequest', repo, pull };
}

function issuesConnection(repo: Repo, args: Record<string, unknown>): GqlObject {
  const first = Math.min(Math.max(argInt(args, 'first', 30), 0), 100);
  const after = typeof args.after === 'string' ? args.after : null;
  const states = Array.isArray(args.states) ? (args.states as string[]) : null;
  let list = [...repo.issues];
  if (states !== null) {
    const wanted = new Set(states.map((state) => String(state).toUpperCase()));
    list = list.filter((issue) => wanted.has(issue.state.toUpperCase()));
  }
  const orderBy = isRecord(args.orderBy) ? args.orderBy : {};
  const direction = typeof orderBy.direction === 'string' ? orderBy.direction.toUpperCase() : 'DESC';
  list.sort((a, b) => (direction === 'ASC' ? a.createdSeq - b.createdSeq : b.createdSeq - a.createdSeq));
  return connectionObject('IssueConnection', list.map((issue) => issueObject(repo, issue)), first, after);
}

function pullsConnection(repo: Repo, args: Record<string, unknown>): GqlObject {
  const first = Math.min(Math.max(argInt(args, 'first', 30), 0), 100);
  const after = typeof args.after === 'string' ? args.after : null;
  const list = [...repo.pulls].sort((a, b) => b.createdSeq - a.createdSeq);
  return connectionObject('PullRequestConnection', list.map((pull) => pullObject(repo, pull)), first, after);
}

function bindToRepo(gql: GqlCtx, repo: Repo): void {
  if (gql.login === null) {
    gql.login = gql.store.bindLogin(gql.token, repo.owner);
  }
}

const SCHEMA: Record<string, Record<string, Resolver>> = {
  Query: {
    viewer: (_obj, _args, gql) => (gql.login === null ? null : { __typename: 'User', login: gql.login }),
    repository: (_obj, args, gql) => {
      const owner = argString(args, 'owner');
      const name = argString(args, 'name');
      if (owner === '' || name === '') return null;
      if (gql.login === null) gql.login = gql.store.bindLogin(gql.token, owner);
      const repo = gql.store.getRepo(owner, name);
      if (repo === undefined || repo.owner !== gql.login) return null;
      return repoObject(repo);
    },
    node: (_obj, args, gql) => {
      const id = argString(args, 'id');
      const decoded = decodeGlobalId(id);
      if (decoded === null) return null;
      const repo = gql.store.repoById(decoded.repoId);
      if (repo === undefined) return null;
      if (decoded.kind === 'Repository') return repoObject(repo);
      if (decoded.kind === 'Issue') {
        const issue = repo.issues.find((entry) => entry.number === decoded.number);
        return issue === undefined ? null : issueObject(repo, issue);
      }
      if (decoded.kind === 'PullRequest') {
        const pull = repo.pulls.find((entry) => entry.number === decoded.number);
        return pull === undefined ? null : pullObject(repo, pull);
      }
      if (decoded.kind === 'IssueComment') {
        const issue = repo.issues.find((entry) => entry.number === decoded.number);
        const comment = issue?.comments.find((entry) => entry.id === decoded.commentId);
        if (issue === undefined || comment === undefined) return null;
        return { __typename: 'IssueComment', repo, issue, comment };
      }
      return null;
    },
    __type: (_obj, args) => {
      const name = argString(args, 'name');
      const info = TYPE_INFO[name];
      if (info === undefined) return null;
      return { __typename: '__Type', name, fields: info.fields, enumValues: info.enumValues };
    },
  },
  Mutation: {
    createIssue: (_obj, args, gql) => {
      const input = isRecord(args.input) ? args.input : {};
      const repo = repoFromGlobalId(gql, argString(input, 'repositoryId'));
      if (repo === null) throw new GqlError('Repository not found');
      bindToRepo(gql, repo);
      const title = argString(input, 'title');
      const body = typeof input.body === 'string' ? input.body : '';
      const issue = gql.store.createIssue(repo, title, body, []);
      return { __typename: 'CreateIssuePayload', issue: issueObject(repo, issue) };
    },
    closeIssue: (_obj, args, gql) => {
      const input = isRecord(args.input) ? args.input : {};
      const found = issueFromGlobalId(gql, argString(input, 'issueId'));
      if (found === null) throw new GqlError('Issue not found');
      found.issue.state = 'closed';
      return { __typename: 'CloseIssuePayload', issue: issueObject(found.repo, found.issue) };
    },
    createPullRequest: (_obj, args, gql) => {
      const input = isRecord(args.input) ? args.input : {};
      const repo = repoFromGlobalId(gql, argString(input, 'repositoryId'));
      if (repo === null) throw new GqlError('Repository not found');
      bindToRepo(gql, repo);
      const base = argString(input, 'baseRefName');
      const head = argString(input, 'headRefName');
      const title = argString(input, 'title');
      if (gql.store.refCommit(repo, `refs/heads/${head}`) === undefined) throw new GqlError(`Head branch '${head}' does not exist`);
      if (gql.store.refCommit(repo, `refs/heads/${base}`) === undefined) throw new GqlError(`Base branch '${base}' does not exist`);
      const duplicate = repo.pulls.find((pull) => pull.head === head && pull.base === base && pull.state === 'open');
      if (duplicate !== undefined) throw new GqlError('A pull request already exists for this head and base');
      const pull = gql.store.createPull(repo, title, '', head, base);
      return { __typename: 'CreatePullRequestPayload', pullRequest: pullObject(repo, pull) };
    },
    mergePullRequest: (_obj, args, gql) => {
      const input = isRecord(args.input) ? args.input : {};
      const found = pullFromGlobalId(gql, argString(input, 'pullRequestId'));
      if (found === null) throw new GqlError('Pull request not found');
      if (found.pull.merged) throw new GqlError('Pull request is already merged');
      if (found.pull.state !== 'open') throw new GqlError('Pull request is closed');
      const sha = gql.store.mergePull(found.repo, found.pull);
      if (sha === null) throw new GqlError('Merge conflict');
      return { __typename: 'MergePullRequestPayload', pullRequest: pullObject(found.repo, found.pull) };
    },
    addComment: (_obj, args, gql) => {
      const input = isRecord(args.input) ? args.input : {};
      const found = issueFromGlobalId(gql, argString(input, 'subjectId'));
      if (found === null) throw new GqlError('Subject not found');
      const body = argString(input, 'body');
      const comment = { id: found.repo.commentSeq, body, createdSeq: gql.store.nextSeq() };
      found.repo.commentSeq += 1;
      found.issue.comments.push(comment);
      return {
        __typename: 'AddCommentPayload',
        commentEdge: {
          __typename: 'IssueCommentEdge',
          node: { __typename: 'IssueComment', repo: found.repo, issue: found.issue, comment },
        },
      };
    },
  },
  Repository: {
    id: (obj) => repoGlobalId(obj.repo as Repo),
    databaseId: (obj) => (obj.repo as Repo).id,
    name: (obj) => (obj.repo as Repo).name,
    nameWithOwner: (obj) => `${(obj.repo as Repo).owner}/${(obj.repo as Repo).name}`,
    owner: (obj) => ({ __typename: 'User', login: (obj.repo as Repo).owner }),
    defaultBranchRef: (obj, _args, gql) => {
      const repo = obj.repo as Repo;
      return gql.store.refCommit(repo, `refs/heads/${repo.defaultBranch}`) === undefined
        ? null
        : { __typename: 'Ref', name: repo.defaultBranch };
    },
    viewerPermission: () => 'ADMIN',
    hasIssuesEnabled: () => true,
    issue: (obj, args) => {
      const repo = obj.repo as Repo;
      const issue = repo.issues.find((entry) => entry.number === argInt(args, 'number', 0));
      return issue === undefined ? null : issueObject(repo, issue);
    },
    issues: (obj, args) => issuesConnection(obj.repo as Repo, args),
    pullRequests: (obj, args) => pullsConnection(obj.repo as Repo, args),
    pullRequest: (obj, args) => {
      const repo = obj.repo as Repo;
      const pull = repo.pulls.find((entry) => entry.number === argInt(args, 'number', 0));
      return pull === undefined ? null : pullObject(repo, pull);
    },
  },
  Issue: {
    id: (obj) => issueGlobalId(obj.repo as Repo, obj.issue as Issue),
    number: (obj) => (obj.issue as Issue).number,
    title: (obj) => (obj.issue as Issue).title,
    state: (obj) => (obj.issue as Issue).state.toUpperCase(),
    body: (obj) => (obj.issue as Issue).body,
    url: (obj) => `/${(obj.repo as Repo).owner}/${(obj.repo as Repo).name}/issues/${(obj.issue as Issue).number}`,
  },
  PullRequest: {
    id: (obj) => pullGlobalId(obj.repo as Repo, obj.pull as PullRequest),
    number: (obj) => (obj.pull as PullRequest).number,
    state: (obj) => {
      const pull = obj.pull as PullRequest;
      return pull.merged ? 'MERGED' : pull.state.toUpperCase();
    },
    merged: (obj) => (obj.pull as PullRequest).merged,
    headRefName: (obj) => (obj.pull as PullRequest).head,
    baseRefName: (obj) => (obj.pull as PullRequest).base,
    mergeable: (obj, _args, gql) => gql.store.pullMergeable(obj.repo as Repo, obj.pull as PullRequest),
    files: (obj, args, gql) => {
      const repo = obj.repo as Repo;
      const pull = obj.pull as PullRequest;
      const first = Math.min(Math.max(argInt(args, 'first', 30), 0), 100);
      const after = typeof args.after === 'string' ? args.after : null;
      const files = gql.store.pullFiles(repo, pull).map((file) => ({
        __typename: 'PullRequestChangedFile',
        path: file.filename,
        additions: file.additions,
        deletions: file.deletions,
        changes: file.changes,
      }));
      return connectionObject('PullRequestChangedFileConnection', files, first, after);
    },
  },
  User: {
    login: (obj) => obj.login as string,
    id: () => 1,
  },
  Ref: {
    name: (obj) => obj.name as string,
  },
  IssueConnection: {
    nodes: (obj) => obj.items as GqlObject[],
    pageInfo: (obj) => obj.pageInfo as GqlObject,
    totalCount: (obj) => obj.totalCount as number,
  },
  PullRequestConnection: {
    nodes: (obj) => obj.items as GqlObject[],
    pageInfo: (obj) => obj.pageInfo as GqlObject,
    totalCount: (obj) => obj.totalCount as number,
  },
  PullRequestChangedFileConnection: {
    nodes: (obj) => obj.items as GqlObject[],
    pageInfo: (obj) => obj.pageInfo as GqlObject,
    totalCount: (obj) => obj.totalCount as number,
  },
  PullRequestChangedFile: {
    path: (obj) => obj.path as string,
    additions: (obj) => obj.additions as number,
    deletions: (obj) => obj.deletions as number,
    changes: (obj) => obj.changes as number,
  },
  IssueComment: {
    id: (obj) => commentGlobalId(obj.repo as Repo, obj.issue as Issue, obj.comment as IssueComment),
    body: (obj) => (obj.comment as { body: string }).body,
  },
  IssueCommentEdge: {
    node: (obj) => obj.node as GqlObject,
  },
  PageInfo: {
    hasNextPage: (obj) => obj.hasNextPage as boolean,
    hasPreviousPage: (obj) => obj.hasPreviousPage as boolean,
    startCursor: (obj) => obj.startCursor as string | null,
    endCursor: (obj) => obj.endCursor as string | null,
  },
  __Type: {
    name: (obj) => obj.name as string,
    kind: (obj) => ((obj.enumValues as string[] | null) === null ? 'OBJECT' : 'ENUM'),
    fields: (obj) => {
      const fields = obj.fields as string[] | null;
      return fields === null ? null : fields.map((name) => ({ __typename: '__Field', name }));
    },
    enumValues: (obj) => {
      const values = obj.enumValues as string[] | null;
      return values === null ? null : values.map((name) => ({ __typename: '__EnumValue', name }));
    },
  },
  __Field: {
    name: (obj) => obj.name as string,
  },
  __EnumValue: {
    name: (obj) => obj.name as string,
  },
  CreateIssuePayload: {
    issue: (obj) => obj.issue as GqlObject,
  },
  CloseIssuePayload: {
    issue: (obj) => obj.issue as GqlObject,
  },
  CreatePullRequestPayload: {
    pullRequest: (obj) => obj.pullRequest as GqlObject,
  },
  MergePullRequestPayload: {
    pullRequest: (obj) => obj.pullRequest as GqlObject,
  },
  AddCommentPayload: {
    commentEdge: (obj) => obj.commentEdge as GqlObject,
  },
};

function repoFromGlobalId(gql: GqlCtx, id: string): Repo | null {
  const decoded = decodeGlobalId(id);
  if (decoded === null || decoded.kind !== 'Repository') return null;
  return gql.store.repoById(decoded.repoId) ?? null;
}

function issueFromGlobalId(gql: GqlCtx, id: string): { repo: Repo; issue: Issue } | null {
  const decoded = decodeGlobalId(id);
  if (decoded === null || decoded.kind !== 'Issue') return null;
  const repo = gql.store.repoById(decoded.repoId);
  if (repo === undefined) return null;
  const issue = repo.issues.find((entry) => entry.number === decoded.number);
  return issue === undefined ? null : { repo, issue };
}

function pullFromGlobalId(gql: GqlCtx, id: string): { repo: Repo; pull: PullRequest } | null {
  const decoded = decodeGlobalId(id);
  if (decoded === null || decoded.kind !== 'PullRequest') return null;
  const repo = gql.store.repoById(decoded.repoId);
  if (repo === undefined) return null;
  const pull = repo.pulls.find((entry) => entry.number === decoded.number);
  return pull === undefined ? null : { repo, pull };
}

// The fields the suite's introspection checks read (plus the enum it checks).
const TYPE_INFO: Record<string, { fields: string[] | null; enumValues: string[] | null }> = {
  Issue: {
    fields: ['id', 'number', 'title', 'state', 'body', 'url'],
    enumValues: null,
  },
  PullRequest: {
    fields: ['id', 'number', 'title', 'state', 'merged', 'headRefName', 'baseRefName', 'mergeable', 'files'],
    enumValues: null,
  },
  Repository: {
    fields: [
      'id',
      'databaseId',
      'name',
      'nameWithOwner',
      'owner',
      'defaultBranchRef',
      'viewerPermission',
      'hasIssuesEnabled',
      'issue',
      'issues',
      'pullRequests',
      'pullRequest',
    ],
    enumValues: null,
  },
  SearchType: {
    fields: null,
    enumValues: ['ISSUE', 'PULL_REQUEST', 'REPOSITORY', 'USER', 'DISCUSSION', 'COMMIT', 'MILESTONE', 'MARKETPLACE_LISTING'],
  },
  IssueComment: {
    fields: ['id', 'body'],
    enumValues: null,
  },
  User: {
    fields: ['login', 'id'],
    enumValues: null,
  },
};

// ---------------------------------------------------------------------------------------------
// HTTP entry point
// ---------------------------------------------------------------------------------------------

function executeOperation(store: Store, token: string, document: DocumentNode, operation: OperationNode, variables: Record<string, unknown>): Record<string, unknown> {
  const gql: GqlCtx = {
    store,
    login: store.loginFor(token),
    token,
    variables: { ...operation.variables, ...variables },
    fragments: document.fragments,
  };
  try {
    const root: GqlObject = { __typename: operation.type === 'mutation' ? 'Mutation' : 'Query' };
    const data = executeSelections(root, operation.selections, gql);
    return { data };
  } catch (error) {
    if (error instanceof GqlError) {
      return { data: null, errors: [{ message: error.message }] };
    }
    throw error;
  }
}

export function routeGraphql(store: Store, ctx: ReqCtx): Resp | null {
  if (ctx.segments.join('/') !== 'api/graphql') return null;
  if (!tokenAccepted(ctx.token)) return errorResp(401, 'Requires authentication');

  let body: unknown;
  try {
    body = JSON.parse(ctx.body.toString('utf8'));
  } catch {
    return errorResp(400, 'Invalid JSON in request body');
  }

  const runOne = (entry: unknown): Record<string, unknown> => {
    if (!isRecord(entry) || typeof entry.query !== 'string') {
      return { errors: [{ message: 'Must provide query string.' }] };
    }
    const variables = isRecord(entry.variables) ? (entry.variables as Record<string, unknown>) : {};
    let document: DocumentNode;
    try {
      document = new Parser(new Lexer(entry.query).tokenize()).parseDocument();
    } catch (error) {
      return { data: null, errors: [{ message: error instanceof Error ? error.message : 'GraphQL parse error' }] };
    }
    const operation =
      typeof entry.operationName === 'string'
        ? document.operations.find((op) => op.name === entry.operationName)
        : document.operations[0];
    if (operation === undefined) {
      return { data: null, errors: [{ message: 'No operation found' }] };
    }
    return executeOperation(store, ctx.token!, document, operation, variables);
  };

  if (Array.isArray(body)) {
    return jsonResp(200, body.map(runOne));
  }
  return jsonResp(200, runOne(body));
}
