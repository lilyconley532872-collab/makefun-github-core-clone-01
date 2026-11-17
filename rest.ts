// rest.ts — GitHub REST v3: repositories, branches, commits, contents, issues, pulls, labels.
// Every reply is application/json with a non-empty X-GitHub-Request-Id (app.ts adds the header).

import { errorResp, isRecord, jsonResp, parseJsonBody, type ReqCtx, type Resp } from './context.ts';
import {
  commentGlobalId,
  FORGE_DATE,
  issueGlobalId,
  pullGlobalId,
  repoGlobalId,
  type Issue,
  type IssueComment,
  type PullRequest,
  type Repo,
  type Store,
} from './store.ts';
import { isObjectId, parseCommit } from './git-objects.ts';

const SHOP_TOKEN = process.env.MAKEFUN_SHOP_TOKEN ?? '';

/** Is this bearer token accepted? The trial's token when set, else any non-empty token (local runs). */
export function tokenAccepted(token: string | null): boolean {
  if (token === null || token === '') return false;
  if (SHOP_TOKEN !== '') return token === SHOP_TOKEN;
  return true;
}

/** The login this request acts as, binding the token to the first owner it names. */
export function loginForRequest(store: Store, ctx: ReqCtx, owner: string): string | null {
  if (ctx.token === null || !tokenAccepted(ctx.token)) return null;
  const bound = store.loginFor(ctx.token);
  if (bound !== null) return bound === owner ? bound : null;
  return store.bindLogin(ctx.token, owner);
}

interface Page {
  perPage: number;
  page: number;
}

function pageOf(ctx: ReqCtx): Page {
  const perPageRaw = Number.parseInt(ctx.query.get('per_page') ?? '30', 10);
  const pageRaw = Number.parseInt(ctx.query.get('page') ?? '1', 10);
  return {
    perPage: Number.isFinite(perPageRaw) && perPageRaw > 0 ? Math.min(perPageRaw, 100) : 30,
    page: Number.isFinite(pageRaw) && pageRaw > 0 ? pageRaw : 1,
  };
}

function paginate<T>(ctx: ReqCtx, items: readonly T[]): { page: T[]; link: string | null } {
  const { perPage, page } = pageOf(ctx);
  const start = (page - 1) * perPage;
  const slice = items.slice(start, start + perPage);
  let link: string | null = null;
  const totalPages = Math.max(1, Math.ceil(items.length / perPage));
  const params = new URLSearchParams(ctx.query);
  const base = `http://${ctx.host}/${ctx.segments.join('/')}`;
  const links: string[] = [];
  if (page < totalPages) {
    params.set('page', String(page + 1));
    params.set('per_page', String(perPage));
    links.push(`<${base}?${params.toString()}>; rel="next"`);
  }
  if (totalPages > 1) {
    params.set('page', String(totalPages));
    params.set('per_page', String(perPage));
    links.push(`<${base}?${params.toString()}>; rel="last"`);
    params.set('page', '1');
    links.push(`<${base}?${params.toString()}>; rel="first"`);
  }
  if (page > 1 && totalPages > 1) {
    params.set('page', String(page - 1));
    params.set('per_page', String(perPage));
    links.push(`<${base}?${params.toString()}>; rel="prev"`);
  }
  if (links.length > 0) link = links.join(', ');
  return { page: [...slice], link };
}

function linkHeader(link: string | null): Record<string, string> {
  return link === null ? {} : { link };
}

// ---------------------------------------------------------------------------------------------
// serializers
// ---------------------------------------------------------------------------------------------

export function repoJson(store: Store, repo: Repo): Record<string, unknown> {
  return {
    id: repo.id,
    node_id: repoGlobalId(repo),
    name: repo.name,
    full_name: `${repo.owner}/${repo.name}`,
    private: repo.isPrivate,
    owner: { login: repo.owner, id: 1, type: 'User' },
    description: repo.description,
    default_branch: repo.defaultBranch,
    has_issues: true,
    has_pull_requests: true,
    html_url: `/${repo.owner}/${repo.name}`,
    url: `/api/v3/repos/${repo.owner}/${repo.name}`,
    created_at: FORGE_DATE,
    updated_at: FORGE_DATE,
  };
}

function labelJson(label: { name: string; color: string }): Record<string, unknown> {
  return { name: label.name, color: label.color, description: '' };
}

export function issueJson(store: Store, repo: Repo, issue: Issue): Record<string, unknown> {
  return {
    id: issue.id,
    node_id: issueGlobalId(repo, issue),
    number: issue.number,
    title: issue.title,
    body: issue.body,
    state: issue.state,
    labels: issue.labels.map((name) => {
      const label = repo.labels.get(name);
      return labelJson(label ?? { name, color: 'ededed' });
    }),
    user: { login: repo.owner, id: 1, type: 'User' },
    comments: issue.comments.length,
    html_url: `/${repo.owner}/${repo.name}/issues/${issue.number}`,
    url: `/api/v3/repos/${repo.owner}/${repo.name}/issues/${issue.number}`,
    created_at: FORGE_DATE,
    updated_at: FORGE_DATE,
  };
}

export function pullJson(store: Store, repo: Repo, pull: PullRequest): Record<string, unknown> {
  const headSha = store.refCommit(repo, `refs/heads/${pull.head}`) ?? pull.headSha;
  const baseSha = store.refCommit(repo, `refs/heads/${pull.base}`) ?? pull.baseSha;
  return {
    id: pull.id,
    node_id: pullGlobalId(repo, pull),
    number: pull.number,
    title: pull.title,
    body: pull.body,
    state: pull.state,
    merged: pull.merged,
    merged_at: pull.mergedAt,
    merge_commit_sha: pull.mergeCommitSha,
    head: { ref: pull.head, sha: headSha, label: `${repo.owner}:${pull.head}` },
    base: { ref: pull.base, sha: baseSha, label: `${repo.owner}:${pull.base}` },
    user: { login: repo.owner, id: 1, type: 'User' },
    html_url: `/${repo.owner}/${repo.name}/pull/${pull.number}`,
    url: `/api/v3/repos/${repo.owner}/${repo.name}/pulls/${pull.number}`,
    created_at: FORGE_DATE,
    updated_at: FORGE_DATE,
  };
}

function commentJson(store: Store, repo: Repo, issue: Issue, comment: IssueComment): Record<string, unknown> {
  return {
    id: comment.id,
    node_id: commentGlobalId(repo, issue, comment),
    body: comment.body,
    user: { login: repo.owner, id: 1, type: 'User' },
    html_url: `/${repo.owner}/${repo.name}/issues/${issue.number}#issuecomment-${comment.id}`,
    url: `/api/v3/repos/${repo.owner}/${repo.name}/issues/${issue.number}/comments/${comment.id}`,
    created_at: FORGE_DATE,
    updated_at: FORGE_DATE,
  };
}

// ---------------------------------------------------------------------------------------------
// routing
// ---------------------------------------------------------------------------------------------

/**
 * Handle a /api/v3/* request (except /api/graphql, which lives in graphql.ts).
 * Returns null when the path is not a REST route at all.
 */
export function routeRest(store: Store, ctx: ReqCtx): Resp | null {
  const segments = ctx.segments;
  if (segments[0] !== 'api' || segments[1] !== 'v3') return null;
  const rest = segments.slice(2);
  const method = ctx.method;

  // The API root and meta need no token (GitHub answers them unauthenticated).
  if (rest.length === 0 && method === 'GET') {
    return jsonResp(200, { current_user_url: '/api/v3/user', rate_limit_url: '/api/v3/rate_limit' }, { 'x-oauth-scopes': 'repo, read:org' });
  }
  if (rest.length === 1 && rest[0] === 'meta' && method === 'GET') {
    return jsonResp(200, { installed_version: '1.0.0' });
  }

  // Everything else needs a bearer token.
  if (!tokenAccepted(ctx.token)) return errorResp(401, 'Requires authentication');

  // /user/*
  if (rest[0] === 'user') {
    if (rest.length === 1 && method === 'GET') {
      const login = ctx.token !== null ? store.loginFor(ctx.token) : null;
      if (login === null) return errorResp(404, 'Not Found');
      return jsonResp(200, { login, id: 1, type: 'User' });
    }
    if (rest.length === 2 && rest[1] === 'repos') {
      if (method === 'GET') {
        const login = ctx.token !== null ? store.loginFor(ctx.token) : null;
        const list = login === null ? store.reposOf('', ctx.token!) : store.reposOf(login, ctx.token!);
        const { page, link } = paginate(ctx, list.map((repo) => repoJson(store, repo)));
        return jsonResp(200, page, linkHeader(link));
      }
      if (method === 'POST') {
        let body: unknown;
        try {
          body = parseJsonBody(ctx);
        } catch {
          return errorResp(400, 'Invalid JSON in request body');
        }
        if (!isRecord(body)) return errorResp(400, 'Invalid request body');
        const name = typeof body.name === 'string' ? body.name : '';
        if (name === '' || !/^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$/.test(name)) return errorResp(422, 'Invalid repository name');
        const login = ctx.token !== null ? store.loginFor(ctx.token) : null;
        if (login !== null && store.getRepo(login, name) !== undefined) {
          return errorResp(422, 'Repository already exists');
        }
        if (login === null) {
          const pending = store.pendingRepos.get(ctx.token!) ?? [];
          if (pending.some((repo) => repo.name === name)) return errorResp(422, 'Repository already exists');
        }
        const repo = store.createRepo(login, ctx.token!, name, body.private === true, typeof body.description === 'string' ? body.description : '');
        return jsonResp(201, repoJson(store, repo));
      }
    }
    return errorResp(404, 'Not Found');
  }

  // /repos/{owner}/{name}/...
  if (rest[0] === 'repos' && rest.length >= 3) {
    const owner = rest[1];
    const name = rest[2];
    const tail = rest.slice(3);
    const login = loginForRequest(store, ctx, owner);
    if (login === null) return errorResp(404, 'Not Found');
    const repo = store.getRepo(owner, name);
    if (repo === undefined) return errorResp(404, 'Not Found');
    return routeRepo(store, ctx, repo, tail);
  }

  return errorResp(404, 'Not Found');
}

function routeRepo(store: Store, ctx: ReqCtx, repo: Repo, tail: string[]): Resp {
  const method = ctx.method;

  if (tail.length === 0) {
    if (method === 'GET') return jsonResp(200, repoJson(store, repo));
    if (method === 'PATCH') {
      let body: unknown;
      try {
        body = parseJsonBody(ctx);
      } catch {
        return errorResp(400, 'Invalid JSON in request body');
      }
      if (isRecord(body)) {
        if (typeof body.description === 'string') repo.description = body.description;
        if (typeof body.default_branch === 'string' && body.default_branch !== '') repo.defaultBranch = body.default_branch;
      }
      return jsonResp(200, repoJson(store, repo));
    }
    if (method === 'DELETE') {
      store.deleteRepo(repo.owner, repo.name);
      return { status: 204, body: undefined };
    }
    return errorResp(404, 'Not Found');
  }

  const head = tail[0];

  if (head === 'branches') {
    if (tail.length === 1 && method === 'GET') {
      const branches: Array<Record<string, unknown>> = [];
      const names = [...repo.refs.keys()].filter((ref) => ref.startsWith('refs/heads/')).map((ref) => ref.slice('refs/heads/'.length));
      names.sort();
      const ordered = [repo.defaultBranch, ...names.filter((name) => name !== repo.defaultBranch)];
      for (const name of ordered) {
        const sha = store.refCommit(repo, `refs/heads/${name}`);
        if (sha === undefined) continue;
        branches.push({ name, commit: { sha }, protected: false });
      }
      return jsonResp(200, branches);
    }
    if (tail.length === 2 && method === 'GET') {
      const sha = store.refCommit(repo, `refs/heads/${tail[1]}`);
      if (sha === undefined) return errorResp(404, 'Not Found');
      return jsonResp(200, { name: tail[1], commit: { sha }, protected: false });
    }
    return errorResp(404, 'Not Found');
  }

  if (head === 'commits') {
    if (method === 'GET') {
      const ref = ctx.query.get('sha') ?? repo.defaultBranch;
      const oids = store.commitsFrom(repo, ref);
      const out = oids.map((oid) => {
        const object = repo.objects.get(oid)!;
        const parsed = parseCommit(object.data);
        const author = parseIdentity(parsed.author);
        const committer = parseIdentity(parsed.committer);
        return {
          sha: oid,
          commit: {
            // Git stores the message with a trailing newline; the API returns it without one.
            message: parsed.message.replace(/\n+$/, ''),
            author: { name: author.name, email: author.email, date: FORGE_DATE },
            committer: { name: committer.name, email: committer.email, date: FORGE_DATE },
          },
          author: { login: repo.owner },
          committer: { login: repo.owner },
        };
      });
      return jsonResp(200, out);
    }
    return errorResp(404, 'Not Found');
  }

  if (head === 'git') {
    if (tail[1] === 'refs') {
      if (method === 'POST' && tail.length === 2) {
        let body: unknown;
        try {
          body = parseJsonBody(ctx);
        } catch {
          return errorResp(400, 'Invalid JSON in request body');
        }
        if (!isRecord(body) || typeof body.ref !== 'string' || typeof body.sha !== 'string') return errorResp(422, 'Invalid ref');
        const ref = body.ref;
        const sha = body.sha;
        if (!ref.startsWith('refs/heads/') && !ref.startsWith('refs/tags/')) return errorResp(422, 'Invalid ref');
        if (!isObjectId(sha) || repo.objects.get(sha) === undefined) return errorResp(422, 'Invalid sha');
        if (repo.refs.has(ref)) return errorResp(422, 'Reference already exists');
        const type = repo.objects.get(sha)!.type;
        if (ref.startsWith('refs/heads/') && type !== 'commit') return errorResp(422, 'Invalid sha');
        repo.refs.set(ref, sha);
        return jsonResp(201, { ref, object: { sha, type } });
      }
      if (method === 'GET' && tail.length >= 3) {
        // /git/refs/heads/{branch} or /git/refs/tags/{tag} (or a full ref path).
        const refPath = tail.slice(2).join('/');
        const full = refPath.startsWith('refs/') ? refPath : `refs/${refPath}`;
        const sha = repo.refs.get(full);
        if (sha === undefined) return errorResp(404, 'Not Found');
        return jsonResp(200, { ref: full, object: { sha, type: repo.objects.get(sha)?.type ?? 'commit' } });
      }
    }
    return errorResp(404, 'Not Found');
  }

  if (head === 'contents') {
    return routeContents(store, ctx, repo, tail.slice(1));
  }

  if (head === 'labels') {
    if (method === 'POST' && tail.length === 1) {
      let body: unknown;
      try {
        body = parseJsonBody(ctx);
      } catch {
        return errorResp(400, 'Invalid JSON in request body');
      }
      if (!isRecord(body) || typeof body.name !== 'string' || body.name === '') return errorResp(422, 'Invalid label');
      const existing = repo.labels.get(body.name);
      if (existing !== undefined) return jsonResp(201, labelJson(existing));
      const label = { name: body.name, color: typeof body.color === 'string' ? body.color : 'ededed' };
      repo.labels.set(label.name, label);
      return jsonResp(201, labelJson(label));
    }
    if (method === 'GET' && tail.length === 1) {
      return jsonResp(200, [...repo.labels.values()].map(labelJson));
    }
    return errorResp(404, 'Not Found');
  }

  if (head === 'issues') {
    return routeIssues(store, ctx, repo, tail.slice(1));
  }

  if (head === 'pulls') {
    return routePulls(store, ctx, repo, tail.slice(1));
  }

  return errorResp(404, 'Not Found');
}

function parseIdentity(line: string): { name: string; email: string } {
  const match = /^(.*?) <([^>]*)>/.exec(line);
  if (match === null) return { name: line, email: '' };
  return { name: match[1], email: match[2] };
}

// ---------------------------------------------------------------------------------------------
// contents
// ---------------------------------------------------------------------------------------------

function routeContents(store: Store, ctx: ReqCtx, repo: Repo, pathSegments: string[]): Resp {
  const method = ctx.method;
  const path = pathSegments.join('/');
  const refQuery = ctx.query.get('ref') ?? undefined;

  if (method === 'GET') {
    const branch = refQuery ?? repo.defaultBranch;
    const files = store.filesAtBranch(repo, branch);
    if (path === '') {
      // Root listing.
      const entries = dirEntries(repo, files, '');
      return jsonResp(200, entries);
    }
    const file = files.get(path);
    if (file !== undefined) {
      const data = repo.objects.get(file.oid)?.data ?? Buffer.alloc(0);
      return jsonResp(200, {
        type: 'file',
        encoding: 'base64',
        size: data.length,
        name: path.split('/').pop() ?? path,
        path,
        content: data.toString('base64'),
        sha: file.oid,
      });
    }
    // A directory?
    const prefix = `${path}/`;
    const hasChildren = [...files.keys()].some((key) => key.startsWith(prefix));
    if (hasChildren) return jsonResp(200, dirEntries(repo, files, path));
    return errorResp(404, 'Not Found');
  }

  if (method === 'PUT') {
    let body: unknown;
    try {
      body = parseJsonBody(ctx);
    } catch {
      return errorResp(400, 'Invalid JSON in request body');
    }
    if (!isRecord(body) || typeof body.message !== 'string' || typeof body.content !== 'string') {
      return errorResp(422, 'Invalid content');
    }
    const branch = typeof body.branch === 'string' && body.branch !== '' ? body.branch : repo.defaultBranch;
    const files = store.filesAtBranch(repo, branch);
    const existing = files.get(path);
    const sha = typeof body.sha === 'string' ? body.sha : undefined;
    if (existing !== undefined) {
      if (sha === undefined) return errorResp(409, 'sha is required to update an existing file');
      if (sha !== existing.oid) return errorResp(409, 'sha does not match the current file');
    } else if (sha !== undefined) {
      return errorResp(409, 'sha does not match: the file does not exist');
    }
    let content: Buffer;
    try {
      content = Buffer.from(body.content.replace(/[\r\n\t ]+/g, ''), 'base64');
    } catch {
      return errorResp(422, 'Invalid base64 content');
    }
    const { commitOid, blobOid } = store.commitFile(repo, branch, path, content, body.message);
    const status = existing === undefined ? 201 : 200;
    return jsonResp(status, {
      content: { name: path.split('/').pop() ?? path, path, sha: blobOid, size: content.length, encoding: 'base64', type: 'file' },
      commit: { sha: commitOid, message: body.message },
    });
  }

  if (method === 'DELETE') {
    let body: unknown = {};
    try {
      body = parseJsonBody(ctx);
    } catch {
      return errorResp(400, 'Invalid JSON in request body');
    }
    const branch = isRecord(body) && typeof body.branch === 'string' && body.branch !== '' ? body.branch : repo.defaultBranch;
    const files = store.filesAtBranch(repo, branch);
    const existing = files.get(path);
    if (existing === undefined) return errorResp(404, 'Not Found');
    const sha = isRecord(body) && typeof body.sha === 'string' ? body.sha : undefined;
    if (sha !== undefined && sha !== existing.oid) return errorResp(409, 'sha does not match the current file');
    const message = isRecord(body) && typeof body.message === 'string' ? body.message : `Delete ${path}`;
    const { commitOid } = store.commitFile(repo, branch, path, null, message);
    return jsonResp(200, { commit: { sha: commitOid, message } });
  }

  return errorResp(404, 'Not Found');
}

function dirEntries(repo: Repo, files: Map<string, { oid: string; mode: string }>, dir: string): Array<Record<string, unknown>> {
  const prefix = dir === '' ? '' : `${dir}/`;
  const seen = new Map<string, { type: string; oid: string }>();
  for (const [path, file] of files) {
    if (!path.startsWith(prefix)) continue;
    const rest = path.slice(prefix.length);
    if (rest === '') continue;
    const slash = rest.indexOf('/');
    if (slash < 0) {
      seen.set(rest, { type: 'file', oid: file.oid });
    } else {
      const name = rest.slice(0, slash);
      if (!seen.has(name)) seen.set(name, { type: 'dir', oid: '' });
    }
  }
  const out: Array<Record<string, unknown>> = [];
  for (const name of [...seen.keys()].sort()) {
    const entry = seen.get(name)!;
    out.push({
      type: entry.type,
      name,
      path: prefix + name,
      sha: entry.oid === '' ? '0'.repeat(40) : entry.oid,
      size: entry.type === 'file' ? (repo.objects.get(entry.oid)?.data.length ?? 0) : 0,
    });
  }
  return out;
}

// ---------------------------------------------------------------------------------------------
// issues
// ---------------------------------------------------------------------------------------------

function routeIssues(store: Store, ctx: ReqCtx, repo: Repo, tail: string[]): Resp {
  const method = ctx.method;

  if (tail.length === 0) {
    if (method === 'POST') {
      let body: unknown;
      try {
        body = parseJsonBody(ctx);
      } catch {
        return errorResp(400, 'Invalid JSON in request body');
      }
      if (!isRecord(body) || typeof body.title !== 'string' || body.title === '') return errorResp(422, 'Invalid issue');
      const labels = Array.isArray(body.labels) ? body.labels.filter((entry): entry is string => typeof entry === 'string') : [];
      for (const label of labels) {
        if (!repo.labels.has(label)) return errorResp(422, `Label '${label}' does not exist`);
      }
      const issue = store.createIssue(repo, body.title, typeof body.body === 'string' ? body.body : '', labels);
      return jsonResp(201, issueJson(store, repo, issue));
    }
    if (method === 'GET') {
      const state = ctx.query.get('state') ?? 'open';
      const sort = ctx.query.get('sort') ?? 'created';
      const direction = ctx.query.get('direction') ?? 'desc';
      let list = [...repo.issues];
      if (state === 'open') list = list.filter((issue) => issue.state === 'open');
      else if (state === 'closed') list = list.filter((issue) => issue.state === 'closed');
      if (sort === 'created') {
        list.sort((a, b) => (direction === 'asc' ? a.createdSeq - b.createdSeq : b.createdSeq - a.createdSeq));
      }
      const { page, link } = paginate(ctx, list.map((issue) => issueJson(store, repo, issue)));
      return jsonResp(200, page, linkHeader(link));
    }
    return errorResp(404, 'Not Found');
  }

  const number = Number.parseInt(tail[0], 10);
  if (!Number.isInteger(number) || number <= 0) return errorResp(404, 'Not Found');
  const issue = repo.issues.find((entry) => entry.number === number);
  if (issue === undefined) return errorResp(404, 'Not Found');

  if (tail.length === 1) {
    if (method === 'GET') return jsonResp(200, issueJson(store, repo, issue));
    if (method === 'PATCH') {
      let body: unknown;
      try {
        body = parseJsonBody(ctx);
      } catch {
        return errorResp(400, 'Invalid JSON in request body');
      }
      if (isRecord(body)) {
        if (body.state === 'open' || body.state === 'closed') issue.state = body.state;
        if (typeof body.title === 'string') issue.title = body.title;
        if (typeof body.body === 'string') issue.body = body.body;
      }
      return jsonResp(200, issueJson(store, repo, issue));
    }
    return errorResp(404, 'Not Found');
  }

  if (tail[1] === 'comments') {
    if (method === 'POST' && tail.length === 2) {
      let body: unknown;
      try {
        body = parseJsonBody(ctx);
      } catch {
        return errorResp(400, 'Invalid JSON in request body');
      }
      if (!isRecord(body) || typeof body.body !== 'string') return errorResp(422, 'Invalid comment');
      const comment = { id: repo.commentSeq, body: body.body, createdSeq: store.nextSeq() };
      repo.commentSeq += 1;
      issue.comments.push(comment);
      return jsonResp(201, commentJson(store, repo, issue, comment));
    }
    if (method === 'GET' && tail.length === 2) {
      return jsonResp(200, issue.comments.map((comment) => commentJson(store, repo, issue, comment)));
    }
    return errorResp(404, 'Not Found');
  }

  return errorResp(404, 'Not Found');
}

// ---------------------------------------------------------------------------------------------
// pull requests
// ---------------------------------------------------------------------------------------------

function routePulls(store: Store, ctx: ReqCtx, repo: Repo, tail: string[]): Resp {
  const method = ctx.method;

  if (tail.length === 0) {
    if (method === 'POST') {
      let body: unknown;
      try {
        body = parseJsonBody(ctx);
      } catch {
        return errorResp(400, 'Invalid JSON in request body');
      }
      if (!isRecord(body) || typeof body.title !== 'string' || typeof body.head !== 'string' || typeof body.base !== 'string') {
        return errorResp(422, 'Invalid pull request');
      }
      const head = body.head;
      const base = body.base;
      if (store.refCommit(repo, `refs/heads/${head}`) === undefined) return errorResp(422, `Head branch '${head}' does not exist`);
      if (store.refCommit(repo, `refs/heads/${base}`) === undefined) return errorResp(422, `Base branch '${base}' does not exist`);
      const duplicate = repo.pulls.find((pull) => pull.head === head && pull.base === base && pull.state === 'open');
      if (duplicate !== undefined) return errorResp(422, 'A pull request already exists for this head and base');
      const pull = store.createPull(repo, body.title, typeof body.body === 'string' ? body.body : '', head, base);
      return jsonResp(201, pullJson(store, repo, pull));
    }
    if (method === 'GET') {
      const state = ctx.query.get('state') ?? 'open';
      let list = [...repo.pulls];
      if (state === 'open') list = list.filter((pull) => pull.state === 'open');
      else if (state === 'closed') list = list.filter((pull) => pull.state === 'closed');
      list.sort((a, b) => b.createdSeq - a.createdSeq);
      const { page, link } = paginate(ctx, list.map((pull) => pullJson(store, repo, pull)));
      return jsonResp(200, page, linkHeader(link));
    }
    return errorResp(404, 'Not Found');
  }

  const number = Number.parseInt(tail[0], 10);
  if (!Number.isInteger(number) || number <= 0) return errorResp(404, 'Not Found');
  const pull = repo.pulls.find((entry) => entry.number === number);
  if (pull === undefined) return errorResp(404, 'Not Found');

  if (tail.length === 1) {
    if (method === 'GET') return jsonResp(200, pullJson(store, repo, pull));
    return errorResp(404, 'Not Found');
  }

  if (tail[1] === 'files' && method === 'GET' && tail.length === 2) {
    const files = store.pullFiles(repo, pull).map((file) => ({
      filename: file.filename,
      status: file.status,
      additions: file.additions,
      deletions: file.deletions,
      changes: file.changes,
    }));
    return jsonResp(200, files);
  }

  if (tail[1] === 'merge' && method === 'PUT' && tail.length === 2) {
    if (pull.merged) return errorResp(405, 'Pull request is already merged');
    if (pull.state !== 'open') return errorResp(405, 'Pull request is closed');
    const mergeSha = store.mergePull(repo, pull);
    if (mergeSha === null) return errorResp(405, 'Merge conflict');
    return jsonResp(200, { merged: true, sha: mergeSha, message: 'Pull request successfully merged' });
  }

  return errorResp(404, 'Not Found');
}
