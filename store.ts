// store.ts — the forge's in-memory store: repositories, git objects, issues, pull requests.
// One store per server process. No wall clock in any stored value: ordering uses a monotonic
// counter and server-made commits use a fixed timestamp, so repeated runs stay deterministic.

import {
  buildCommit,
  buildTree,
  collectReachable,
  hashObject,
  isAncestor,
  isObjectId,
  parseCommit,
  parseTag,
  parseTree,
  ZERO_OID,
  type GitObject,
  type GitObjectType,
  type TreeEntry,
} from './git-objects.ts';

export const FORGE_IDENTITY = 'Forge <forge@localhost>';
export const FORGE_TIMESTAMP = 1735689600; // 2025-01-01T00:00:00Z
export const FORGE_DATE = '2025-01-01T00:00:00Z';
export const DEFAULT_BRANCH = 'main';

export interface Label {
  name: string;
  color: string;
}

export interface IssueComment {
  id: number;
  body: string;
  createdSeq: number;
}

export interface Issue {
  id: number;
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  labels: string[];
  createdSeq: number;
  comments: IssueComment[];
}

export interface PullRequest {
  id: number;
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  merged: boolean;
  mergedAt: string | null;
  mergeCommitSha: string | null;
  head: string;
  base: string;
  headSha: string;
  baseSha: string;
  createdSeq: number;
}

export interface Repo {
  id: number;
  owner: string;
  name: string;
  description: string;
  isPrivate: boolean;
  defaultBranch: string;
  createdSeq: number;
  objects: Map<string, GitObject>;
  refs: Map<string, string>;
  issues: Issue[];
  pulls: PullRequest[];
  labels: Map<string, Label>;
  numberSeq: number; // shared issue + PR numbering, GitHub-style
  commentSeq: number;
}

/** Global (Relay) ids: opaque base64 of a typed key, stable across reads. */
export function globalId(kind: string, key: string): string {
  return Buffer.from(`${kind}:${key}`, 'utf8').toString('base64');
}

export function parseGlobalId(id: string): { kind: string; key: string } | null {
  try {
    const text = Buffer.from(id, 'base64').toString('utf8');
    const colon = text.indexOf(':');
    if (colon < 0) return null;
    return { kind: text.slice(0, colon), key: text.slice(colon + 1) };
  } catch {
    return null;
  }
}

export function repoGlobalId(repo: Repo): string {
  return globalId('Repository', String(repo.id));
}

export function issueGlobalId(repo: Repo, issue: Issue): string {
  return globalId('Issue', `${repo.id}:${issue.number}`);
}

export function pullGlobalId(repo: Repo, pull: PullRequest): string {
  return globalId('PullRequest', `${repo.id}:${pull.number}`);
}

export function commentGlobalId(repo: Repo, issue: Issue, comment: IssueComment): string {
  return globalId('IssueComment', `${repo.id}:${issue.number}:${comment.id}`);
}

/** Decode an opaque global id (Repository:<id> / Issue:<repo>:<n> / PullRequest:<repo>:<n> / IssueComment:<repo>:<n>:<id>). */
export function decodeGlobalId(id: string): { kind: string; repoId: number; number: number; commentId: number } | null {
  let text: string;
  try {
    text = Buffer.from(id, 'base64').toString('utf8');
  } catch {
    return null;
  }
  const parts = text.split(':');
  if (parts.length < 2) return null;
  const kind = parts[0];
  const repoId = Number.parseInt(parts[1], 10);
  if (!Number.isInteger(repoId) || repoId <= 0) return null;
  if (kind === 'Repository' && parts.length === 2) return { kind, repoId, number: 0, commentId: 0 };
  if ((kind === 'Issue' || kind === 'PullRequest') && parts.length === 3) {
    const number = Number.parseInt(parts[2], 10);
    if (!Number.isInteger(number) || number <= 0) return null;
    return { kind, repoId, number, commentId: 0 };
  }
  if (kind === 'IssueComment' && parts.length === 4) {
    const number = Number.parseInt(parts[2], 10);
    const commentId = Number.parseInt(parts[3], 10);
    if (!Number.isInteger(number) || number <= 0 || !Number.isInteger(commentId) || commentId <= 0) return null;
    return { kind, repoId, number, commentId };
  }
  return null;
}

function repoKey(owner: string, name: string): string {
  return `${owner}\n${name}`;
}

/** Count the lines of a text the way git does: newlines, plus a final partial line. */
function countLines(text: string): number {
  if (text === '') return 0;
  const newlines = (text.match(/\n/g) ?? []).length;
  return text.endsWith('\n') ? newlines : newlines + 1;
}

/** A line diff: lines added and removed between two texts (simple LCS on lines). */
export function lineDiff(oldText: string, newText: string): { additions: number; deletions: number } {
  const oldLines = oldText === '' ? [] : oldText.split('\n');
  const newLines = newText === '' ? [] : newText.split('\n');
  const m = oldLines.length;
  const n = newLines.length;
  // LCS lengths, O(m*n) — contents here are small.
  const dp: number[][] = Array.from({ length: m + 1 }, () => new Array<number>(n + 1).fill(0));
  for (let i = m - 1; i >= 0; i -= 1) {
    for (let j = n - 1; j >= 0; j -= 1) {
      dp[i][j] = oldLines[i] === newLines[j] ? dp[i + 1][j + 1] + 1 : Math.max(dp[i + 1][j], dp[i][j + 1]);
    }
  }
  const common = dp[0][0];
  return { additions: n - common, deletions: m - common };
}

export class Store {
  repos = new Map<string, Repo>();
  pendingRepos = new Map<string, Repo[]>(); // token -> repos created before the login bound
  tokenLogin = new Map<string, string>(); // token -> bound login
  repoIdSeq = 1;
  seq = 1; // monotonic creation-order counter

  nextSeq(): number {
    const value = this.seq;
    this.seq += 1;
    return value;
  }

  loginFor(token: string): string | null {
    return this.tokenLogin.get(token) ?? null;
  }

  /** Bind a token to the first owner it is seen naming; move its pending repos under that owner. */
  bindLogin(token: string, owner: string): string {
    const existing = this.tokenLogin.get(token);
    if (existing !== undefined) return existing;
    this.tokenLogin.set(token, owner);
    const pending = this.pendingRepos.get(token) ?? [];
    this.pendingRepos.delete(token);
    for (const repo of pending) {
      repo.owner = owner;
      this.repos.set(repoKey(owner, repo.name), repo);
    }
    return owner;
  }

  createRepo(owner: string | null, token: string, name: string, isPrivate: boolean, description: string): Repo {
    const repo: Repo = {
      id: this.repoIdSeq,
      owner: owner ?? '',
      name,
      description,
      isPrivate,
      defaultBranch: DEFAULT_BRANCH,
      createdSeq: this.nextSeq(),
      objects: new Map(),
      refs: new Map(),
      issues: [],
      pulls: [],
      labels: new Map(),
      numberSeq: 1,
      commentSeq: 1,
    };
    this.repoIdSeq += 1;
    if (owner === null) {
      const list = this.pendingRepos.get(token) ?? [];
      list.push(repo);
      this.pendingRepos.set(token, list);
    } else {
      this.repos.set(repoKey(owner, name), repo);
    }
    return repo;
  }

  getRepo(owner: string, name: string): Repo | undefined {
    return this.repos.get(repoKey(owner, name));
  }

  repoById(id: number): Repo | undefined {
    for (const repo of this.repos.values()) {
      if (repo.id === id) return repo;
    }
    return undefined;
  }

  deleteRepo(owner: string, name: string): boolean {
    return this.repos.delete(repoKey(owner, name));
  }

  /** All repos of an owner, in creation order (pending ones included when unbound). */
  reposOf(owner: string, token: string): Repo[] {
    const bound = this.loginFor(token);
    const out: Repo[] = [];
    for (const repo of this.repos.values()) if (repo.owner === owner) out.push(repo);
    if (bound === null) {
      // Unbound: the caller has not named the owner yet; pending repos belong to the token.
      for (const repo of this.pendingRepos.get(token) ?? []) out.push(repo);
    }
    out.sort((a, b) => a.createdSeq - b.createdSeq);
    return out;
  }

  // -------------------------------------------------------------------------------------------
  // git objects and refs
  // -------------------------------------------------------------------------------------------

  lookup(repo: Repo): (oid: string) => GitObject | undefined {
    return (oid: string) => repo.objects.get(oid);
  }

  addObject(repo: Repo, object: GitObject): string {
    const oid = hashObject(object.type, object.data);
    repo.objects.set(oid, object);
    return oid;
  }

  refTip(repo: Repo, ref: string): string | undefined {
    return repo.refs.get(ref);
  }

  /** The commit a ref resolves to (following an annotated tag once). */
  refCommit(repo: Repo, ref: string): string | undefined {
    const oid = repo.refs.get(ref);
    if (oid === undefined) return undefined;
    const object = repo.objects.get(oid);
    if (object === undefined) return undefined;
    if (object.type === 'tag') return parseTag(object.data).object;
    if (object.type === 'commit') return oid;
    return undefined;
  }

  /** Resolve a ref name, branch name or oid to a commit oid. */
  resolveToCommit(repo: Repo, ref: string): string | undefined {
    if (isObjectId(ref)) {
      const object = repo.objects.get(ref);
      if (object === undefined) return undefined;
      if (object.type === 'commit') return ref;
      if (object.type === 'tag') return parseTag(object.data).object;
      return undefined;
    }
    const full = ref.startsWith('refs/') ? ref : `refs/heads/${ref}`;
    const commit = this.refCommit(repo, full);
    if (commit !== undefined) return commit;
    // A tag name also resolves.
    if (!ref.startsWith('refs/')) {
      const tagCommit = this.refCommit(repo, `refs/tags/${ref}`);
      if (tagCommit !== undefined) return tagCommit;
    }
    return undefined;
  }

  /** The tree oid at a commit. */
  commitTree(repo: Repo, commitOid: string): string | undefined {
    const object = repo.objects.get(commitOid);
    if (object === undefined || object.type !== 'commit') return undefined;
    return parseCommit(object.data).tree;
  }

  /** The flat file map (path -> {oid, mode}) of a tree, recursively. */
  flattenTree(repo: Repo, treeOid: string, prefix = ''): Map<string, { oid: string; mode: string }> {
    const out = new Map<string, { oid: string; mode: string }>();
    const walk = (oid: string, path: string): void => {
      const object = repo.objects.get(oid);
      if (object === undefined || object.type !== 'tree') return;
      for (const entry of parseTree(object.data)) {
        const childPath = path === '' ? entry.name : `${path}/${entry.name}`;
        if (entry.mode === '40000') walk(entry.oid, childPath);
        else if (entry.mode !== '160000') out.set(childPath, { oid: entry.oid, mode: entry.mode });
      }
    };
    walk(treeOid, prefix);
    return out;
  }

  /** Build (and store) the tree for a file map. Returns the root tree oid. */
  buildTreeFromFiles(repo: Repo, files: Map<string, { oid: string; mode: string }>): string {
    // Group files by directory, bottom-up.
    const dirs = new Map<string, Map<string, { oid: string; mode: string }>>();
    const ensureDir = (dir: string): Map<string, { oid: string; mode: string }> => {
      let map = dirs.get(dir);
      if (map === undefined) {
        map = new Map();
        dirs.set(dir, map);
      }
      return map;
    };
    ensureDir('');
    for (const [path, file] of files) {
      const slash = path.lastIndexOf('/');
      const dir = slash < 0 ? '' : path.slice(0, slash);
      const name = slash < 0 ? path : path.slice(slash + 1);
      ensureDir(dir).set(name, { oid: file.oid, mode: file.mode });
      // Ensure every ancestor directory exists in the map.
      let parent = dir;
      while (parent !== '') {
        const idx = parent.lastIndexOf('/');
        const grand = idx < 0 ? '' : parent.slice(0, idx);
        ensureDir(grand);
        parent = grand;
      }
    }
    // Build trees depth-first from the deepest directory up. The root ('') has depth 0;
    // ''.split('/') is [''] (length 1), so depth must be computed explicitly.
    const depthOf = (dir: string): number => (dir === '' ? 0 : dir.split('/').length);
    const dirPaths = [...dirs.keys()].sort((a, b) => depthOf(b) - depthOf(a));
    const treeOidByDir = new Map<string, string>();
    for (const dir of dirPaths) {
      const entries: TreeEntry[] = [];
      for (const [name, file] of dirs.get(dir)!) entries.push({ mode: file.mode, name, oid: file.oid });
      // Child directories become tree entries.
      for (const [childDir, childOid] of treeOidByDir) {
        const slash = childDir.lastIndexOf('/');
        const parent = slash < 0 ? '' : childDir.slice(0, slash);
        if (parent !== dir) continue;
        const name = slash < 0 ? childDir : childDir.slice(slash + 1);
        entries.push({ mode: '40000', name, oid: childOid });
      }
      const data = buildTree(entries);
      const oid = this.addObject(repo, { type: 'tree', data });
      treeOidByDir.set(dir, oid);
    }
    return treeOidByDir.get('')!;
  }

  /**
   * Commit one file change on a branch: create/update the file, or delete it when `content` is null.
   * Returns the new commit oid. Creates the branch (from the default branch tip) when missing.
   */
  commitFile(
    repo: Repo,
    branch: string,
    path: string,
    content: Buffer | null,
    message: string,
  ): { commitOid: string; blobOid: string | null } {
    const ref = `refs/heads/${branch}`;
    let parentOid = repo.refs.get(ref);
    if (parentOid === undefined) {
      // A new branch starts at the default branch tip (or unborn).
      parentOid = repo.refs.get(`refs/heads/${repo.defaultBranch}`);
    }
    let files = new Map<string, { oid: string; mode: string }>();
    if (parentOid !== undefined) {
      const treeOid = this.commitTree(repo, parentOid);
      if (treeOid !== undefined) files = this.flattenTree(repo, treeOid);
    }
    let blobOid: string | null = null;
    if (content === null) {
      files.delete(path);
    } else {
      blobOid = this.addObject(repo, { type: 'blob', data: content });
      files.set(path, { oid: blobOid, mode: '100644' });
    }
    const treeOid = this.buildTreeFromFiles(repo, files);
    const commitData = buildCommit({
      tree: treeOid,
      parents: parentOid === undefined ? [] : [parentOid],
      message: message.endsWith('\n') ? message : `${message}\n`,
    });
    const commitOid = this.addObject(repo, { type: 'commit', data: commitData });
    repo.refs.set(ref, commitOid);
    return { commitOid, blobOid };
  }

  /** The file map of a branch tip (empty when the branch is unborn). */
  filesAtBranch(repo: Repo, branch: string): Map<string, { oid: string; mode: string }> {
    const tip = this.refCommit(repo, `refs/heads/${branch}`);
    if (tip === undefined) return new Map();
    const treeOid = this.commitTree(repo, tip);
    if (treeOid === undefined) return new Map();
    return this.flattenTree(repo, treeOid);
  }

  /** Commits reachable from a ref, newest first (parents before children is not guaranteed). */
  commitsFrom(repo: Repo, ref: string): string[] {
    const tip = this.resolveToCommit(repo, ref);
    if (tip === undefined) return [];
    const out: string[] = [];
    const seen = new Set<string>();
    const queue: string[] = [tip];
    while (queue.length) {
      const oid = queue.shift()!;
      if (seen.has(oid)) continue;
      seen.add(oid);
      const object = repo.objects.get(oid);
      if (object === undefined || object.type !== 'commit') continue;
      out.push(oid);
      for (const parent of parseCommit(object.data).parents) queue.push(parent);
    }
    return out;
  }

  // -------------------------------------------------------------------------------------------
  // issues and pull requests
  // -------------------------------------------------------------------------------------------

  createIssue(repo: Repo, title: string, body: string, labels: string[]): Issue {
    const number = repo.numberSeq;
    repo.numberSeq += 1;
    const issue: Issue = {
      id: this.nextSeq() + 100000,
      number,
      title,
      body,
      state: 'open',
      labels: [...labels],
      createdSeq: this.nextSeq(),
      comments: [],
    };
    repo.issues.push(issue);
    return issue;
  }

  createPull(repo: Repo, title: string, body: string, head: string, base: string): PullRequest {
    const headSha = this.refCommit(repo, `refs/heads/${head}`) ?? ZERO_OID;
    const baseSha = this.refCommit(repo, `refs/heads/${base}`) ?? ZERO_OID;
    const number = repo.numberSeq;
    repo.numberSeq += 1;
    const pull: PullRequest = {
      id: this.nextSeq() + 100000,
      number,
      title,
      body,
      state: 'open',
      merged: false,
      mergedAt: null,
      mergeCommitSha: null,
      head,
      base,
      headSha,
      baseSha,
      createdSeq: this.nextSeq(),
    };
    repo.pulls.push(pull);
    return pull;
  }

  /** The changed files of a pull request: diff of base tip tree vs head tip tree. */
  pullFiles(repo: Repo, pull: PullRequest): Array<{ filename: string; status: string; additions: number; deletions: number; changes: number }> {
    const baseFiles = this.filesAtBranch(repo, pull.base);
    const headFiles = this.filesAtBranch(repo, pull.head);
    const paths = new Set([...baseFiles.keys(), ...headFiles.keys()]);
    const out: Array<{ filename: string; status: string; additions: number; deletions: number; changes: number }> = [];
    for (const path of [...paths].sort()) {
      const base = baseFiles.get(path);
      const head = headFiles.get(path);
      if (base !== undefined && head !== undefined && base.oid === head.oid) continue;
      let status: string;
      let additions = 0;
      let deletions = 0;
      if (base === undefined && head !== undefined) {
        status = 'added';
        const data = repo.objects.get(head.oid)?.data ?? Buffer.alloc(0);
        additions = countLines(data.toString('utf8'));
      } else if (base !== undefined && head === undefined) {
        status = 'removed';
        const data = repo.objects.get(base.oid)?.data ?? Buffer.alloc(0);
        deletions = countLines(data.toString('utf8'));
      } else {
        status = 'modified';
        const oldText = (repo.objects.get(base!.oid)?.data ?? Buffer.alloc(0)).toString('utf8');
        const newText = (repo.objects.get(head!.oid)?.data ?? Buffer.alloc(0)).toString('utf8');
        const diff = lineDiff(oldText, newText);
        additions = diff.additions;
        deletions = diff.deletions;
      }
      out.push({ filename: path, status, additions, deletions, changes: additions + deletions });
    }
    return out;
  }

  /** Would a squash merge of this pull request conflict? (same file changed on both sides differently) */
  pullConflicts(repo: Repo, pull: PullRequest): boolean {
    const baseTip = this.refCommit(repo, `refs/heads/${pull.base}`);
    const headTip = this.refCommit(repo, `refs/heads/${pull.head}`);
    if (baseTip === undefined || headTip === undefined) return true;
    // Merge base: the common ancestor of head and base tips (first common commit found walking back).
    const mergeBase = this.mergeBase(repo, headTip, baseTip);
    if (mergeBase === null) return false;
    const baseFiles = this.filesAtBranch(repo, pull.base);
    const headFiles = this.filesAtBranch(repo, pull.head);
    const mergeBaseFiles = (() => {
      const treeOid = this.commitTree(repo, mergeBase);
      return treeOid === undefined ? new Map<string, { oid: string; mode: string }>() : this.flattenTree(repo, treeOid);
    })();
    const paths = new Set([...mergeBaseFiles.keys(), ...baseFiles.keys(), ...headFiles.keys()]);
    for (const path of paths) {
      const origin = mergeBaseFiles.get(path)?.oid;
      const baseOid = baseFiles.get(path)?.oid;
      const headOid = headFiles.get(path)?.oid;
      const baseChanged = baseOid !== origin;
      const headChanged = headOid !== origin;
      if (baseChanged && headChanged && baseOid !== headOid) return true;
    }
    return false;
  }

  /** The merge base of two commits: the first common ancestor in a breadth-first walk. */
  mergeBase(repo: Repo, a: string, b: string): string | null {
    if (a === b) return a;
    const ancestorsOfA = new Set<string>();
    const queueA: string[] = [a];
    while (queueA.length) {
      const oid = queueA.shift()!;
      if (ancestorsOfA.has(oid)) continue;
      ancestorsOfA.add(oid);
      const object = repo.objects.get(oid);
      if (object !== undefined && object.type === 'commit') queueA.push(...parseCommit(object.data).parents);
    }
    const queueB: string[] = [b];
    const seenB = new Set<string>();
    while (queueB.length) {
      const oid = queueB.shift()!;
      if (seenB.has(oid)) continue;
      seenB.add(oid);
      if (ancestorsOfA.has(oid)) return oid;
      const object = repo.objects.get(oid);
      if (object !== undefined && object.type === 'commit') queueB.push(...parseCommit(object.data).parents);
    }
    return null;
  }

  /** Squash-merge a pull request onto its base branch. Returns the merge commit oid, or null on conflict. */
  mergePull(repo: Repo, pull: PullRequest): string | null {
    if (pull.state !== 'open' || pull.merged) return null;
    if (this.pullConflicts(repo, pull)) return null;
    const baseTip = this.refCommit(repo, `refs/heads/${pull.base}`);
    if (baseTip === undefined) return null;
    const headFiles = this.filesAtBranch(repo, pull.head);
    // The squash result: the base tree with the head's files applied (deletions included).
    const mergeBase = this.mergeBase(repo, this.refCommit(repo, `refs/heads/${pull.head}`) ?? '', baseTip);
    const originFiles = mergeBase === null ? new Map<string, { oid: string; mode: string }>() : (() => {
      const treeOid = this.commitTree(repo, mergeBase);
      return treeOid === undefined ? new Map<string, { oid: string; mode: string }>() : this.flattenTree(repo, treeOid);
    })();
    const baseFiles = this.filesAtBranch(repo, pull.base);
    const merged = new Map(baseFiles);
    for (const [path, file] of headFiles) merged.set(path, file);
    for (const path of originFiles.keys()) {
      if (!headFiles.has(path) && baseFiles.has(path) && originFiles.has(path)) {
        // Deleted on the head side (and present at the merge base): drop it.
        if (baseFiles.get(path)?.oid === originFiles.get(path)?.oid) merged.delete(path);
      }
    }
    const treeOid = this.buildTreeFromFiles(repo, merged);
    const message = `Merge pull request #${pull.number} from ${pull.head}\n\n${pull.title}\n`;
    const commitData = buildCommit({ tree: treeOid, parents: [baseTip], message });
    const mergeOid = this.addObject(repo, { type: 'commit', data: commitData });
    repo.refs.set(`refs/heads/${pull.base}`, mergeOid);
    pull.state = 'closed';
    pull.merged = true;
    pull.mergedAt = FORGE_DATE;
    pull.mergeCommitSha = mergeOid;
    pull.baseSha = mergeOid;
    return mergeOid;
  }

  /** Mergeable state for a pull request, as GraphQL reports it. */
  pullMergeable(repo: Repo, pull: PullRequest): string {
    if (pull.state !== 'open') return 'UNKNOWN';
    return this.pullConflicts(repo, pull) ? 'CONFLICTING' : 'MERGEABLE';
  }
}

export { collectReachable, isAncestor, ZERO_OID };
export type { GitObject, GitObjectType };
