// git-http.ts — git smart HTTP: advertisements, ls-refs, fetch (v2 + v0), receive-pack.
// The wire shapes come from the kit's git-client.ts; this module answers them.

import {
  decodePktLines,
  encodePktLines,
  isAncestor,
  isObjectId,
  parseCommit,
  parseTag,
  parseTree,
  readPack,
  sidebandChunks,
  sidebandPacket,
  writeEmptyPack,
  writePack,
  ZERO_OID,
  type GitObject,
} from './git-objects.ts';
import { errorResp, rawResp, type ReqCtx, type Resp } from './context.ts';
import { loginForRequest, tokenAccepted } from './rest.ts';
import { type Repo, type Store } from './store.ts';

export const AGENT = 'github-core-clone/1.0';

const V2_CAPABILITIES = ['agent=github-core-clone/1.0', 'ls-refs=unborn', 'fetch=shallow filter', 'object-format=sha1'];
const V0_UPLOAD_CAPABILITIES = 'symref=HEAD:refs/heads/main agent=github-core-clone/1.0 side-band-64k thin-pack ofs-delta shallow';
const V0_RECEIVE_CAPABILITIES = 'report-status side-band-64k delete-refs ofs-delta agent=github-core-clone/1.0';

/** The ref a tag object peels to (its commit target), or undefined for a lightweight tag. */
function peeledTarget(repo: Repo, oid: string): string | undefined {
  const object = repo.objects.get(oid);
  if (object === undefined || object.type !== 'tag') return undefined;
  const target = parseTag(object.data).object;
  const targetObject = repo.objects.get(target);
  if (targetObject === undefined) return undefined;
  if (targetObject.type === 'commit') return target;
  if (targetObject.type === 'tag') return peeledTarget(repo, target);
  return undefined;
}

function sortedRefs(repo: Repo): Array<{ name: string; oid: string }> {
  const out: Array<{ name: string; oid: string }> = [];
  for (const [name, oid] of repo.refs) out.push({ name, oid });
  out.sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
  return out;
}

function headOid(repo: Repo): string {
  return repo.refs.get('refs/heads/main') ?? repo.refs.get(`refs/heads/${repo.defaultBranch}`) ?? ZERO_OID;
}

/** Protocol v2 capability advertisement (no refs: ls-refs carries them). */
export function advertiseV2(): Buffer {
  const lines: Array<string | 'flush'> = ['version 2\n', ...V2_CAPABILITIES.map((cap) => `${cap}\n`), 'flush'];
  return encodePktLines(lines);
}

/** Protocol v0 advertisement: service preamble, refs (HEAD first, capabilities on its line), peeled tags. */
export function advertiseV0(repo: Repo, service: 'git-upload-pack' | 'git-receive-pack'): Buffer {
  const caps = service === 'git-upload-pack' ? V0_UPLOAD_CAPABILITIES : V0_RECEIVE_CAPABILITIES;
  const parts: Buffer[] = [encodePktLines([`# service=${service}\n`, 'flush'])];
  const refs = sortedRefs(repo);
  const lines: string[] = [];
  if (refs.length === 0) {
    lines.push(`${ZERO_OID} capabilities^{}\u0000${caps}\n`);
  } else {
    const head = headOid(repo);
    lines.push(`${head} HEAD\u0000${caps}\n`);
    for (const ref of refs) lines.push(`${ref.oid} ${ref.name}\n`);
    for (const ref of refs) {
      if (!ref.name.startsWith('refs/tags/')) continue;
      const peeled = peeledTarget(repo, ref.oid);
      if (peeled !== undefined) lines.push(`${peeled} ${ref.name}^{}\n`);
    }
  }
  parts.push(encodePktLines(lines));
  parts.push(encodePktLines(['flush']));
  return Buffer.concat(parts);
}

// ---------------------------------------------------------------------------------------------
// ls-refs (protocol v2)
// ---------------------------------------------------------------------------------------------

export interface LsRefsOptions {
  peel: boolean;
  symrefs: boolean;
  unborn: boolean;
  prefixes: string[];
}

export function lsRefsResponse(repo: Repo, options: LsRefsOptions): Buffer {
  const lines: string[] = [];
  const refs = sortedRefs(repo);
  const head = headOid(repo);
  const includeHead = options.prefixes.length === 0;
  if (refs.length === 0) {
    if (includeHead) {
      lines.push(options.symrefs ? 'unborn HEAD symref-target:refs/heads/main\n' : 'unborn HEAD\n');
    }
  } else {
    if (includeHead) {
      lines.push(options.symrefs ? `${head} HEAD symref-target:refs/heads/main\n` : `${head} HEAD\n`);
    }
    for (const ref of refs) {
      if (options.prefixes.length > 0 && !options.prefixes.some((prefix) => ref.name.startsWith(prefix))) continue;
      let line = `${ref.oid} ${ref.name}`;
      if (options.peel) {
        const peeled = peeledTarget(repo, ref.oid);
        if (peeled !== undefined) line += ` peeled:${peeled}`;
      }
      lines.push(`${line}\n`);
    }
  }
  return encodePktLines([...lines, 'flush']);
}

// ---------------------------------------------------------------------------------------------
// fetch computation
// ---------------------------------------------------------------------------------------------

export interface FetchRequest {
  wants: string[];
  haves: string[];
  shallow: string[];
  depth: number | null;
}

interface FetchPlan {
  objects: Map<string, GitObject>;
  shallow: string[];
  unshallow: string[];
  acks: string[];
}

/** Objects reachable from a tree only (trees + blobs, no commits). */
function treeClosure(repo: Repo, treeOid: string, lookup: (oid: string) => GitObject | undefined): Set<string> {
  const out = new Set<string>();
  const queue: string[] = [treeOid];
  while (queue.length) {
    const oid = queue.pop()!;
    if (out.has(oid)) continue;
    const object = lookup(oid);
    if (object === undefined) continue;
    out.add(oid);
    if (object.type === 'tree') {
      for (const entry of parseTree(object.data)) {
        if (entry.mode === '160000') continue;
        queue.push(entry.oid);
      }
    }
  }
  return out;
}

/**
 * Compute a fetch: the objects to send, the new shallow boundaries, the unshallowed ones, and ACKs.
 * `wants` are commit/tag oids; `haves` are oids the client holds; `shallow` are its prior boundaries.
 */
export function computeFetch(repo: Repo, request: FetchRequest): FetchPlan {
  const lookup = (oid: string): GitObject | undefined => repo.objects.get(oid);

  // The objects the client already holds: reachable from haves, not crossing a prior shallow boundary.
  const held = new Set<string>();
  {
    const boundarySet = new Set(request.shallow);
    const queue: string[] = [];
    for (const have of request.haves) {
      if (!isObjectId(have)) continue;
      queue.push(have);
    }
    const seenCommits = new Set<string>();
    while (queue.length) {
      const oid = queue.pop()!;
      const object = lookup(oid);
      if (object === undefined) continue;
      if (object.type === 'commit') {
        if (seenCommits.has(oid)) continue;
        seenCommits.add(oid);
        held.add(oid);
        const commit = parseCommit(object.data);
        const treeObjects = treeClosure(repo, commit.tree, lookup);
        for (const treeOid of treeObjects) held.add(treeOid);
        if (boundarySet.has(oid)) continue; // a shallow boundary: the client holds nothing below it
        for (const parent of commit.parents) queue.push(parent);
      } else if (object.type === 'tag') {
        held.add(oid);
        queue.push(parseTag(object.data).object);
      } else {
        held.add(oid);
      }
    }
  }

  // The commits to include: walk from wants, depth-limited (distance 0..depth-1 when depth is set).
  const depth = request.depth;
  const wantedCommits = new Map<string, number>(); // oid -> distance
  {
    const queue: Array<{ oid: string; distance: number }> = [];
    for (const want of request.wants) {
      if (!isObjectId(want)) continue;
      queue.push({ oid: want, distance: 0 });
    }
    const seen = new Set<string>();
    while (queue.length) {
      const { oid, distance } = queue.shift()!;
      if (seen.has(oid)) continue;
      seen.add(oid);
      const object = lookup(oid);
      if (object === undefined) continue;
      if (object.type === 'tag') {
        // A tag want counts at the same distance; its target follows.
        wantedCommits.set(oid, distance);
        queue.push({ oid: parseTag(object.data).object, distance });
        continue;
      }
      if (object.type !== 'commit') continue;
      if (depth !== null && distance >= depth) continue; // beyond the depth: not included
      wantedCommits.set(oid, distance);
      for (const parent of parseCommit(object.data).parents) queue.push({ oid: parent, distance: distance + 1 });
    }
  }

  // The objects to send: each wanted commit + its tree closure, minus what the client holds.
  const objects = new Map<string, GitObject>();
  for (const [oid] of wantedCommits) {
    const object = lookup(oid);
    if (object === undefined) continue;
    if (object.type === 'tag') {
      if (!held.has(oid)) objects.set(oid, object);
      continue;
    }
    if (object.type !== 'commit') continue;
    if (!held.has(oid)) objects.set(oid, object);
    const treeObjects = treeClosure(repo, parseCommit(object.data).tree, lookup);
    for (const treeOid of treeObjects) {
      if (held.has(treeOid)) continue;
      const treeObject = lookup(treeOid);
      if (treeObject !== undefined) objects.set(treeOid, treeObject);
    }
  }

  // Shallow-info: the new boundaries (distance depth-1, with parents) and the unshallowed old ones.
  const shallow: string[] = [];
  const unshallow: string[] = [];
  if (depth !== null) {
    for (const [oid, distance] of wantedCommits) {
      if (distance !== depth - 1) continue;
      const object = lookup(oid);
      if (object === undefined || object.type !== 'commit') continue;
      if (parseCommit(object.data).parents.length > 0) shallow.push(oid);
    }
    const boundarySet = new Set(request.shallow);
    for (const old of boundarySet) {
      const distance = wantedCommits.get(old);
      if (distance !== undefined && distance < depth - 1) unshallow.push(old);
    }
  } else if (request.shallow.length > 0) {
    // No depth: every prior boundary the walk now reaches in full is unshallowed.
    for (const old of request.shallow) {
      if (wantedCommits.has(old)) unshallow.push(old);
    }
  }

  // ACKs: each have that is an ancestor of (or equal to) a want.
  const acks: string[] = [];
  for (const have of request.haves) {
    if (!isObjectId(have)) continue;
    for (const want of request.wants) {
      if (!isObjectId(want)) continue;
      if (have === want || isAncestor(have, want, lookup)) {
        acks.push(have);
        break;
      }
    }
  }

  return { objects, shallow, unshallow, acks };
}

// ---------------------------------------------------------------------------------------------
// fetch responses
// ---------------------------------------------------------------------------------------------

export function fetchV2Response(repo: Repo, request: FetchRequest): Buffer {
  const plan = computeFetch(repo, request);
  const parts: Buffer[] = [];
  const ackLines: string[] = [];
  if (plan.acks.length > 0) {
    for (const ack of plan.acks) ackLines.push(`ACK ${ack}\n`);
  } else {
    ackLines.push('NAK\n');
  }
  ackLines.push('ready\n');
  parts.push(encodePktLines(['acknowledgments\n', ...ackLines, 'delim']));
  if (plan.shallow.length > 0 || plan.unshallow.length > 0) {
    const lines: string[] = ['shallow-info\n'];
    for (const oid of plan.shallow) lines.push(`shallow ${oid}\n`);
    for (const oid of plan.unshallow) lines.push(`unshallow ${oid}\n`);
    lines.push('delim');
    parts.push(encodePktLines(lines));
  }
  const pack = writePack([...plan.objects.values()]);
  parts.push(encodePktLines(['packfile\n']));
  for (const chunk of sidebandChunks(pack)) parts.push(chunk);
  parts.push(encodePktLines(['flush']));
  return Buffer.concat(parts);
}

export function fetchV0Response(repo: Repo, request: FetchRequest): Buffer {
  const plan = computeFetch(repo, request);
  const parts: Buffer[] = [];
  const lines: string[] = [];
  if (plan.acks.length > 0) {
    for (const ack of plan.acks) lines.push(`ACK ${ack}\n`);
  } else {
    lines.push('NAK\n');
  }
  for (const oid of plan.shallow) lines.push(`shallow ${oid}\n`);
  for (const oid of plan.unshallow) lines.push(`unshallow ${oid}\n`);
  if (lines.length > 0) parts.push(encodePktLines(lines));
  const pack = plan.objects.size === 0 ? writeEmptyPack() : writePack([...plan.objects.values()]);
  for (const chunk of sidebandChunks(pack)) parts.push(chunk);
  parts.push(encodePktLines(['flush']));
  return Buffer.concat(parts);
}

// ---------------------------------------------------------------------------------------------
// request parsing (upload-pack / receive-pack bodies)
// ---------------------------------------------------------------------------------------------

export interface ParsedFetchArgs {
  command: string;
  wants: string[];
  haves: string[];
  shallow: string[];
  depth: number | null;
  prefixes: string[];
  peel: boolean;
  symrefs: boolean;
  unborn: boolean;
}

/** Parse a v2 upload-pack request body (ls-refs or fetch). Throws on malformed framing. */
export function parseV2Request(body: Buffer): ParsedFetchArgs {
  const packets = decodePktLines(body);
  const out: ParsedFetchArgs = {
    command: '',
    wants: [],
    haves: [],
    shallow: [],
    depth: null,
    prefixes: [],
    peel: false,
    symrefs: false,
    unborn: false,
  };
  let inArgs = false;
  for (const packet of packets) {
    if (packet.kind === 'flush') break;
    if (packet.kind === 'delim') {
      inArgs = true;
      continue;
    }
    if (packet.kind !== 'data') continue;
    const line = packet.payload.toString('utf8');
    if (!inArgs) {
      const match = /^command=([a-z-]+)\n?$/.exec(line);
      if (match) out.command = match[1];
      continue;
    }
    const text = line.replace(/\n$/, '');
    if (text === 'peel') out.peel = true;
    else if (text === 'symrefs') out.symrefs = true;
    else if (text === 'unborn') out.unborn = true;
    else if (text.startsWith('want ')) out.wants.push(text.slice(5).trim());
    else if (text.startsWith('have ')) out.haves.push(text.slice(5).trim());
    else if (text.startsWith('shallow ')) out.shallow.push(text.slice(8).trim());
    else if (text.startsWith('deepen ')) {
      const n = Number.parseInt(text.slice(7).trim(), 10);
      if (Number.isFinite(n)) out.depth = n;
    } else if (text.startsWith('ref-prefix ')) out.prefixes.push(text.slice(11).trim());
  }
  if (out.command === '') throw new Error('upload-pack request has no command');
  return out;
}

/** Parse a v0 upload-pack fetch body: wants (+capabilities) / shallow / deepen, flush, haves / done, flush. */
export function parseV0Fetch(body: Buffer): ParsedFetchArgs {
  const packets = decodePktLines(body);
  const out: ParsedFetchArgs = {
    command: 'fetch',
    wants: [],
    haves: [],
    shallow: [],
    depth: null,
    prefixes: [],
    peel: false,
    symrefs: false,
    unborn: false,
  };
  let section = 0;
  for (const packet of packets) {
    if (packet.kind === 'flush') {
      section += 1;
      continue;
    }
    if (packet.kind !== 'data') continue;
    const text = packet.payload.toString('utf8').replace(/\n$/, '');
    if (section === 0) {
      if (text.startsWith('want ')) {
        const rest = text.slice(5);
        const space = rest.indexOf(' ');
        out.wants.push(space < 0 ? rest.trim() : rest.slice(0, space));
      } else if (text.startsWith('shallow ')) out.shallow.push(text.slice(8).trim());
      else if (text.startsWith('deepen ')) {
        const n = Number.parseInt(text.slice(7).trim(), 10);
        if (Number.isFinite(n)) out.depth = n;
      }
    } else {
      if (text.startsWith('have ')) out.haves.push(text.slice(5).trim());
    }
  }
  return out;
}

export interface PushCommand {
  oldOid: string;
  newOid: string;
  ref: string;
}

/**
 * Parse a receive-pack body: command pkt-lines up to the flush, then the raw pack bytes.
 * Returns the commands and the pack (null when the body carried no pack bytes).
 */
export function parseReceivePack(body: Buffer): { commands: PushCommand[]; pack: Buffer | null } {
  let offset = 0;
  const commands: PushCommand[] = [];
  const HEX4 = /^[0-9a-fA-F]{4}$/;
  for (;;) {
    if (offset + 4 > body.length) throw new Error('truncated receive-pack body');
    const header = body.subarray(offset, offset + 4).toString('ascii');
    if (!HEX4.test(header)) throw new Error(`invalid pkt-line length ${JSON.stringify(header)}`);
    const length = Number.parseInt(header, 16);
    if (length === 0) {
      offset += 4;
      break;
    }
    if (length === 1 || length === 2) throw new Error('unexpected control packet in receive-pack commands');
    if (length > 65520) throw new Error('pkt-line too long in receive-pack');
    if (offset + length > body.length) throw new Error('truncated receive-pack command');
    const payload = body.subarray(offset + 4, offset + length).toString('utf8');
    offset += length;
    let line = payload.replace(/\n$/, '');
    const nul = line.indexOf('\u0000');
    if (nul >= 0) line = line.slice(0, nul);
    const parts = line.split(' ');
    if (parts.length < 3) throw new Error(`bad push command ${JSON.stringify(line.slice(0, 60))}`);
    commands.push({ oldOid: parts[0], newOid: parts[1], ref: parts.slice(2).join(' ') });
  }
  const pack = offset < body.length ? body.subarray(offset) : null;
  return { commands, pack };
}

// ---------------------------------------------------------------------------------------------
// receive-pack
// ---------------------------------------------------------------------------------------------

export interface PushOutcome {
  unpackOk: boolean;
  unpackDetail: string;
  results: Array<{ ref: string; ok: boolean; reason: string }>;
}

/** Apply a push: unpack the pack, validate each command, apply the accepted ones. */
export function applyPush(store: Store, repo: Repo, body: Buffer): PushOutcome {
  let commands: PushCommand[];
  let pack: Buffer | null;
  try {
    const parsed = parseReceivePack(body);
    commands = parsed.commands;
    pack = parsed.pack;
  } catch (err) {
    return { unpackOk: false, unpackDetail: err instanceof Error ? err.message : String(err), results: [] };
  }
  if (commands.length === 0) {
    return { unpackOk: false, unpackDetail: 'no commands', results: [] };
  }
  // Unpack first: every object the pack carries joins the store.
  if (pack !== null && pack.length > 0) {
    try {
      const read = readPack(pack, { resolveBase: (oid) => repo.objects.get(oid) });
      for (const [oid, object] of read.objects) repo.objects.set(oid, object);
    } catch (err) {
      return { unpackOk: false, unpackDetail: err instanceof Error ? err.message : String(err), results: [] };
    }
  }
  const results: Array<{ ref: string; ok: boolean; reason: string }> = [];
  for (const command of commands) {
    const current = repo.refs.get(command.ref);
    if (!command.ref.startsWith('refs/') || /[\s\u0000-\u001f]/.test(command.ref)) {
      results.push({ ref: command.ref, ok: false, reason: 'invalid ref' });
      continue;
    }
    if (command.newOid === ZERO_OID) {
      // Delete.
      if (current === undefined) {
        results.push({ ref: command.ref, ok: false, reason: 'remote ref does not exist' });
      } else if (command.oldOid !== ZERO_OID && command.oldOid !== current) {
        results.push({ ref: command.ref, ok: false, reason: 'stale info' });
      } else {
        repo.refs.delete(command.ref);
        results.push({ ref: command.ref, ok: true, reason: '' });
      }
      continue;
    }
    const object = repo.objects.get(command.newOid);
    if (object === undefined) {
      results.push({ ref: command.ref, ok: false, reason: 'object not found' });
      continue;
    }
    if (command.ref.startsWith('refs/heads/') && object.type !== 'commit') {
      results.push({ ref: command.ref, ok: false, reason: 'not a commit' });
      continue;
    }
    if (command.ref.startsWith('refs/tags/') && object.type !== 'commit' && object.type !== 'tag') {
      results.push({ ref: command.ref, ok: false, reason: 'not a commit or tag' });
      continue;
    }
    if (command.oldOid === ZERO_OID) {
      // Create.
      if (current !== undefined) {
        results.push({ ref: command.ref, ok: false, reason: 'already exists' });
        continue;
      }
      repo.refs.set(command.ref, command.newOid);
      results.push({ ref: command.ref, ok: true, reason: '' });
      continue;
    }
    // Update: the old value must match, and the new tip must descend from it.
    if (current === undefined) {
      results.push({ ref: command.ref, ok: false, reason: 'remote ref does not exist' });
      continue;
    }
    if (current !== command.oldOid) {
      results.push({ ref: command.ref, ok: false, reason: 'stale info' });
      continue;
    }
    if (command.newOid === command.oldOid) {
      results.push({ ref: command.ref, ok: true, reason: '' });
      continue;
    }
    const lookup = (oid: string): GitObject | undefined => repo.objects.get(oid);
    if (!isAncestor(command.oldOid, command.newOid, lookup)) {
      results.push({ ref: command.ref, ok: false, reason: 'non-fast-forward' });
      continue;
    }
    repo.refs.set(command.ref, command.newOid);
    results.push({ ref: command.ref, ok: true, reason: '' });
  }
  return { unpackOk: true, unpackDetail: 'ok', results };
}

/** The push report-status stream, framed as sideband band-1 packets. */
export function pushReportResponse(outcome: PushOutcome): Buffer {
  const lines: string[] = [];
  if (outcome.unpackOk) lines.push('unpack ok');
  else lines.push(`unpack ${outcome.unpackDetail}`);
  for (const result of outcome.results) {
    if (result.ok) lines.push(`ok ${result.ref}`);
    else lines.push(`ng ${result.ref} ${result.reason}`);
  }
  const text = `${lines.join('\n')}\n`;
  const parts: Buffer[] = [sidebandPacket(1, Buffer.from(text, 'utf8')), encodePktLines(['flush'])];
  return Buffer.concat(parts);
}

// ---------------------------------------------------------------------------------------------
// HTTP entry point: /{owner}/{repo}.git/info/refs | git-upload-pack | git-receive-pack
// ---------------------------------------------------------------------------------------------

function gitProtocol(headers: Record<string, string | string[] | undefined>): string {
  const value = headers['git-protocol'];
  if (typeof value === 'string') return value;
  if (Array.isArray(value)) return value[0] ?? '';
  return '';
}

/**
 * Serve one git smart-HTTP request. A refusal (unknown repo, non-fast-forward, malformed body)
 * is an HTTP 200 with an ng line / a 400, never a crash: the server must stay up for the next call.
 */
export function handleGitHttp(
  store: Store,
  ctx: ReqCtx,
  owner: string,
  repoName: string,
  tail: string[],
  headers: Record<string, string | string[] | undefined>,
): Resp {
  if (!tokenAccepted(ctx.token)) return errorResp(401, 'Requires authentication');
  const login = loginForRequest(store, ctx, owner);
  if (login === null) return errorResp(404, 'Not Found');
  const repo = store.getRepo(owner, repoName);
  if (repo === undefined) return errorResp(404, 'Not Found');

  const isV2 = gitProtocol(headers) === 'version=2';

  // GET /{owner}/{repo}.git/info/refs?service=git-upload-pack|git-receive-pack
  if (tail.length === 2 && tail[0] === 'info' && tail[1] === 'refs' && ctx.method === 'GET') {
    const service = ctx.query.get('service') ?? '';
    if (service === 'git-upload-pack') {
      if (isV2) return rawResp(200, advertiseV2(), 'application/x-git-upload-pack-advertisement');
      return rawResp(200, advertiseV0(repo, 'git-upload-pack'), 'application/x-git-upload-pack-advertisement');
    }
    if (service === 'git-receive-pack') {
      return rawResp(200, advertiseV0(repo, 'git-receive-pack'), 'application/x-git-receive-pack-advertisement');
    }
    return errorResp(400, 'Unknown git service');
  }

  // POST /{owner}/{repo}.git/git-upload-pack
  if (tail.length === 1 && tail[0] === 'git-upload-pack' && ctx.method === 'POST') {
    try {
      if (isV2) {
        const parsed = parseV2Request(ctx.body);
        if (parsed.command === 'ls-refs') {
          return rawResp(
            200,
            lsRefsResponse(repo, { peel: parsed.peel, symrefs: parsed.symrefs, unborn: parsed.unborn, prefixes: parsed.prefixes }),
            'application/x-git-upload-pack-result',
          );
        }
        if (parsed.command === 'fetch') {
          return rawResp(
            200,
            fetchV2Response(repo, { wants: parsed.wants, haves: parsed.haves, shallow: parsed.shallow, depth: parsed.depth }),
            'application/x-git-upload-pack-result',
          );
        }
        return errorResp(400, `Unknown upload-pack command: ${parsed.command}`);
      }
      const parsed = parseV0Fetch(ctx.body);
      return rawResp(
        200,
        fetchV0Response(repo, { wants: parsed.wants, haves: parsed.haves, shallow: parsed.shallow, depth: parsed.depth }),
        'application/x-git-upload-pack-result',
      );
    } catch (error) {
      return errorResp(400, error instanceof Error ? error.message : 'Malformed upload-pack request');
    }
  }

  // POST /{owner}/{repo}.git/git-receive-pack
  if (tail.length === 1 && tail[0] === 'git-receive-pack' && ctx.method === 'POST') {
    try {
      const outcome = applyPush(store, repo, ctx.body);
      return rawResp(200, pushReportResponse(outcome), 'application/x-git-receive-pack-result');
    } catch (error) {
      return errorResp(400, error instanceof Error ? error.message : 'Malformed receive-pack request');
    }
  }

  return errorResp(404, 'Not Found');
}
