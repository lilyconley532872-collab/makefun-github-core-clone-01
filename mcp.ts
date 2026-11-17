// mcp.ts — the Model Context Protocol endpoint (POST /mcp, JSON-RPC 2.0 over Streamable HTTP).
// Serves both eras the suite's client speaks: the modern 2026-07-28 `server/discover` flow and the
// legacy handshake (initialize + notifications/initialized). Plain application/json replies; no SSE.
// Authentication is decided before any tool exists: no (or a wrong) bearer token -> HTTP 401.

import { errorResp, isRecord, jsonResp, type ReqCtx, type Resp } from './context.ts';
import { tokenAccepted } from './rest.ts';
import { issueJson, pullJson, repoJson } from './rest.ts';
import type { Repo, Store } from './store.ts';

const SERVER_NAME = 'github-core-clone';
const SERVER_VERSION = '1.0.0';
const SESSION_ID = 'forge-1';
const SUPPORTED_VERSIONS: readonly string[] = Object.freeze(['2026-07-28', '2025-11-25', '2025-06-18', '2025-03-26', '2024-11-05']);

const SERVER_INFO = Object.freeze({ name: SERVER_NAME, version: SERVER_VERSION });

// The six tools the suite needs (canonical github-mcp-server names), frozen so two tools/list
// answers are byte-identical. `required` holds only properties the suite can fill.
const TOOLS = Object.freeze([
  Object.freeze({
    name: 'create_repo',
    description: 'Create a repository owned by the authenticated user.',
    inputSchema: Object.freeze({
      type: 'object',
      properties: Object.freeze({
        owner: Object.freeze({ type: 'string', description: 'The repository owner (defaults to the authenticated user).' }),
        name: Object.freeze({ type: 'string', description: 'The repository name.' }),
        description: Object.freeze({ type: 'string', description: 'A short description of the repository.' }),
        private: Object.freeze({ type: 'boolean', description: 'Whether the repository is private.' }),
      }),
      required: Object.freeze(['name']),
    }),
  }),
  Object.freeze({
    name: 'get_repo',
    description: 'Get a repository by owner and name.',
    inputSchema: Object.freeze({
      type: 'object',
      properties: Object.freeze({
        owner: Object.freeze({ type: 'string', description: 'The repository owner.' }),
        repo: Object.freeze({ type: 'string', description: 'The repository name.' }),
      }),
      required: Object.freeze(['owner', 'repo']),
    }),
  }),
  Object.freeze({
    name: 'create_issue',
    description: 'Create an issue in a repository.',
    inputSchema: Object.freeze({
      type: 'object',
      properties: Object.freeze({
        owner: Object.freeze({ type: 'string', description: 'The repository owner.' }),
        repo: Object.freeze({ type: 'string', description: 'The repository name.' }),
        title: Object.freeze({ type: 'string', description: 'The issue title.' }),
        body: Object.freeze({ type: 'string', description: 'The issue body.' }),
      }),
      required: Object.freeze(['owner', 'repo', 'title']),
    }),
  }),
  Object.freeze({
    name: 'list_issues',
    description: 'List the issues of a repository.',
    inputSchema: Object.freeze({
      type: 'object',
      properties: Object.freeze({
        owner: Object.freeze({ type: 'string', description: 'The repository owner.' }),
        repo: Object.freeze({ type: 'string', description: 'The repository name.' }),
        state: Object.freeze({ type: 'string', description: 'Filter by state: open, closed or all.', enum: ['open', 'closed', 'all'] }),
      }),
      required: Object.freeze(['owner', 'repo']),
    }),
  }),
  Object.freeze({
    name: 'create_pull_request',
    description: 'Create a pull request in a repository.',
    inputSchema: Object.freeze({
      type: 'object',
      properties: Object.freeze({
        owner: Object.freeze({ type: 'string', description: 'The repository owner.' }),
        repo: Object.freeze({ type: 'string', description: 'The repository name.' }),
        title: Object.freeze({ type: 'string', description: 'The pull request title.' }),
        head: Object.freeze({ type: 'string', description: 'The head branch (the branch with the changes).' }),
        base: Object.freeze({ type: 'string', description: 'The base branch (the branch the changes merge into).' }),
        body: Object.freeze({ type: 'string', description: 'The pull request body.' }),
      }),
      required: Object.freeze(['owner', 'repo', 'title', 'head', 'base']),
    }),
  }),
  Object.freeze({
    name: 'merge_pull_request',
    description: 'Merge a pull request (squash) in a repository.',
    inputSchema: Object.freeze({
      type: 'object',
      properties: Object.freeze({
        owner: Object.freeze({ type: 'string', description: 'The repository owner.' }),
        repo: Object.freeze({ type: 'string', description: 'The repository name.' }),
        pull_number: Object.freeze({ type: 'integer', description: 'The pull request number.' }),
      }),
      required: Object.freeze(['owner', 'repo', 'pull_number']),
    }),
  }),
]);

interface ToolResult {
  content: Array<{ type: 'text'; text: string }>;
  structuredContent: unknown;
  isError: boolean;
}

function toolOk(payload: unknown): ToolResult {
  return { content: [{ type: 'text', text: JSON.stringify(payload) }], structuredContent: payload, isError: false };
}

function toolError(message: string): ToolResult {
  const payload = { error: message };
  return { content: [{ type: 'text', text: message }], structuredContent: payload, isError: true };
}

function rpcResult(id: unknown, result: unknown): Resp {
  return jsonResp(200, { jsonrpc: '2.0', id, result }, { 'mcp-session-id': SESSION_ID });
}

function rpcError(id: unknown, code: number, message: string): Resp {
  return jsonResp(200, { jsonrpc: '2.0', id, error: { code, message } }, { 'mcp-session-id': SESSION_ID });
}

function argString(args: Record<string, unknown>, name: string): string | null {
  const value = args[name];
  return typeof value === 'string' && value !== '' ? value : null;
}

function argNumber(args: Record<string, unknown>, name: string): number | null {
  const value = args[name];
  if (typeof value === 'number' && Number.isInteger(value)) return value;
  if (typeof value === 'string' && /^\d+$/.test(value)) return Number.parseInt(value, 10);
  return null;
}

/** The owner this call acts as, binding the token to the first owner it names. */
function ownerFor(store: Store, token: string, args: Record<string, unknown>): string | null {
  const named = argString(args, 'owner');
  const bound = store.loginFor(token);
  if (bound !== null) {
    if (named !== null && named !== bound) return null;
    return bound;
  }
  if (named === null) return null;
  return store.bindLogin(token, named);
}

function repoFor(store: Store, token: string, args: Record<string, unknown>): { owner: string; repo: Repo } | { error: string } {
  const owner = ownerFor(store, token, args);
  if (owner === null) return { error: 'The repository owner is unknown: the token names no login yet' };
  const name = argString(args, 'repo');
  if (name === null) return { error: 'Missing required argument: repo' };
  const repo = store.getRepo(owner, name);
  if (repo === undefined) return { error: `Repository ${owner}/${name} not found` };
  return { owner, repo };
}

function callTool(store: Store, token: string, name: string, args: Record<string, unknown>): ToolResult {
  switch (name) {
    case 'create_repo': {
      const repoName = argString(args, 'name');
      if (repoName === null) return toolError('Missing required argument: name');
      const owner = ownerFor(store, token, args);
      if (owner === null) return toolError('The repository owner is unknown: the token names no login yet');
      if (store.getRepo(owner, repoName) !== undefined) return toolError(`Repository ${owner}/${repoName} already exists`);
      const repo = store.createRepo(
        owner,
        token,
        repoName,
        args.private === true,
        typeof args.description === 'string' ? args.description : '',
      );
      return toolOk(repoJson(store, repo));
    }
    case 'get_repo': {
      const found = repoFor(store, token, args);
      if ('error' in found) return toolError(found.error);
      return toolOk(repoJson(store, found.repo));
    }
    case 'create_issue': {
      const found = repoFor(store, token, args);
      if ('error' in found) return toolError(found.error);
      const title = argString(args, 'title');
      if (title === null) return toolError('Missing required argument: title');
      const issue = store.createIssue(found.repo, title, typeof args.body === 'string' ? args.body : '', []);
      return toolOk(issueJson(store, found.repo, issue));
    }
    case 'list_issues': {
      const found = repoFor(store, token, args);
      if ('error' in found) return toolError(found.error);
      const state = argString(args, 'state') ?? 'open';
      let list = [...found.repo.issues];
      if (state === 'open') list = list.filter((issue) => issue.state === 'open');
      else if (state === 'closed') list = list.filter((issue) => issue.state === 'closed');
      return toolOk({ issues: list.map((issue) => issueJson(store, found.repo, issue)) });
    }
    case 'create_pull_request': {
      const found = repoFor(store, token, args);
      if ('error' in found) return toolError(found.error);
      const title = argString(args, 'title');
      const head = argString(args, 'head');
      const base = argString(args, 'base');
      if (title === null) return toolError('Missing required argument: title');
      if (head === null) return toolError('Missing required argument: head');
      if (base === null) return toolError('Missing required argument: base');
      if (store.refCommit(found.repo, `refs/heads/${head}`) === undefined) return toolError(`Head branch '${head}' does not exist`);
      if (store.refCommit(found.repo, `refs/heads/${base}`) === undefined) return toolError(`Base branch '${base}' does not exist`);
      const duplicate = found.repo.pulls.find((pull) => pull.head === head && pull.base === base && pull.state === 'open');
      if (duplicate !== undefined) return toolError('A pull request already exists for this head and base');
      const pull = store.createPull(found.repo, title, typeof args.body === 'string' ? args.body : '', head, base);
      return toolOk(pullJson(store, found.repo, pull));
    }
    case 'merge_pull_request': {
      const found = repoFor(store, token, args);
      if ('error' in found) return toolError(found.error);
      const number = argNumber(args, 'pull_number');
      if (number === null) return toolError('Missing required argument: pull_number');
      const pull = found.repo.pulls.find((entry) => entry.number === number);
      if (pull === undefined) return toolError(`Pull request #${number} not found`);
      if (pull.merged) return toolError('Pull request is already merged');
      if (pull.state !== 'open') return toolError('Pull request is closed');
      const sha = store.mergePull(found.repo, pull);
      if (sha === null) return toolError('Merge conflict');
      return toolOk(pullJson(store, found.repo, pull));
    }
    default:
      return toolError(`Unknown tool: ${name}`);
  }
}

const TOOL_REQUIRED: Record<string, readonly string[]> = {
  create_repo: ['name'],
  get_repo: ['owner', 'repo'],
  create_issue: ['owner', 'repo', 'title'],
  list_issues: ['owner', 'repo'],
  create_pull_request: ['owner', 'repo', 'title', 'head', 'base'],
  merge_pull_request: ['owner', 'repo', 'pull_number'],
};

export function routeMcp(store: Store, ctx: ReqCtx): Resp | null {
  if (ctx.segments.length !== 1 || ctx.segments[0] !== 'mcp') return null;

  // A session close is best-effort: any 2xx answer.
  if (ctx.method === 'DELETE') return { status: 200, body: {} };
  if (ctx.method === 'GET') return errorResp(405, 'Method not allowed');
  if (ctx.method !== 'POST') return errorResp(405, 'Method not allowed');

  // Authentication is decided before any tool exists.
  if (!tokenAccepted(ctx.token)) return errorResp(401, 'Requires authentication');

  let parsed: unknown;
  try {
    parsed = JSON.parse(ctx.body.toString('utf8'));
  } catch {
    return jsonResp(400, { jsonrpc: '2.0', id: null, error: { code: -32700, message: 'Parse error' } });
  }
  if (!isRecord(parsed) || typeof parsed.method !== 'string') {
    const id = isRecord(parsed) && 'id' in parsed ? parsed.id : null;
    return jsonResp(400, { jsonrpc: '2.0', id, error: { code: -32600, message: 'Invalid Request' } });
  }
  const id = 'id' in parsed ? parsed.id : undefined;
  const method = parsed.method;
  const params = isRecord(parsed.params) ? parsed.params : {};

  // A notification (no id) gets no JSON-RPC answer.
  if (id === undefined) {
    return { status: 202, body: undefined };
  }

  switch (method) {
    case 'server/discover': {
      return rpcResult(id, {
        serverInfo: SERVER_INFO,
        capabilities: { tools: { listChanged: false } },
        supportedVersions: [...SUPPORTED_VERSIONS],
        _meta: {
          'io.modelcontextprotocol/serverInfo': SERVER_INFO,
          'io.modelcontextprotocol/protocolVersion': '2026-07-28',
        },
      });
    }
    case 'initialize': {
      const requested = typeof params.protocolVersion === 'string' ? params.protocolVersion : '';
      const negotiated = (SUPPORTED_VERSIONS as readonly string[]).includes(requested) ? requested : '2025-11-25';
      return rpcResult(id, {
        protocolVersion: negotiated,
        capabilities: { tools: { listChanged: false } },
        serverInfo: SERVER_INFO,
      });
    }
    case 'ping': {
      return rpcResult(id, {});
    }
    case 'tools/list': {
      return rpcResult(id, { tools: TOOLS });
    }
    case 'tools/call': {
      const toolName = typeof params.name === 'string' ? params.name : '';
      const args = isRecord(params.arguments) ? (params.arguments as Record<string, unknown>) : {};
      const known = TOOLS.some((tool) => tool.name === toolName);
      if (!known) return rpcError(id, -32602, `Unknown tool: ${toolName}`);
      const missing = (TOOL_REQUIRED[toolName] ?? []).filter((required) => args[required] === undefined || args[required] === null || args[required] === '');
      if (missing.length > 0) {
        return rpcResult(id, toolError(`Missing required argument(s): ${missing.join(', ')}`));
      }
      return rpcResult(id, callTool(store, ctx.token!, toolName, args));
    }
    default:
      return rpcError(id, -32601, `Method not found: ${method}`);
  }
}
