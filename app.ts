// app.ts — the entry's HTTP server: one process, one in-memory store, four surfaces.
// Routing: /healthz (a probe), /mcp, /api/graphql, /api/v3/*, /{owner}/{repo}.git/* (git smart HTTP).
// Every reply carries a non-empty X-GitHub-Request-Id; a handler that throws still answers JSON 500
// and the server stays up. Bodies are capped (32 MiB) so a bad client cannot grow the process.

import { createServer, type IncomingMessage, type ServerResponse } from 'node:http';
import type { ReqCtx, Resp } from './context.ts';
import { errorResp } from './context.ts';
import { handleGitHttp } from './git-http.ts';
import { routeRest } from './rest.ts';
import { routeGraphql } from './graphql.ts';
import { routeMcp } from './mcp.ts';
import { Store } from './store.ts';

const BODY_LIMIT_BYTES = 32 * 1024 * 1024;

export function createApp(): ReturnType<typeof createServer> {
  const store = new Store();
  let requestCounter = 0;

  const server = createServer(async (req: IncomingMessage, res: ServerResponse) => {
    const requestId = `req_${++requestCounter}`;
    try {
      const resp = await dispatch(store, req, requestId);
      sendResponse(res, resp, requestId);
    } catch (error) {
      process.stderr.write(`[entry] request failed: ${error instanceof Error ? error.message : String(error)}\n`);
      try {
        sendResponse(res, errorResp(500, 'Internal Server Error'), requestId);
      } catch {
        // The client is gone; nothing left to do.
      }
    }
  });
  return server;
}

function decodeSegments(pathname: string): string[] {
  const segments: string[] = [];
  for (const raw of pathname.split('/')) {
    if (raw === '') continue;
    try {
      segments.push(decodeURIComponent(raw));
    } catch {
      segments.push(raw);
    }
  }
  return segments;
}

async function readBody(req: IncomingMessage): Promise<Buffer> {
  const chunks: Buffer[] = [];
  let total = 0;
  for await (const chunk of req) {
    const buffer = chunk as Buffer;
    total += buffer.length;
    if (total > BODY_LIMIT_BYTES) throw new Error('Request body too large');
    chunks.push(buffer);
  }
  return Buffer.concat(chunks);
}

function sendResponse(res: ServerResponse, resp: Resp, requestId: string): void {
  const headers: Record<string, string> = { 'x-github-request-id': requestId, ...(resp.headers ?? {}) };
  if (resp.raw !== undefined) {
    res.writeHead(resp.status, {
      ...headers,
      'content-type': resp.contentType ?? 'application/octet-stream',
      'content-length': resp.raw.length,
    });
    res.end(resp.raw);
    return;
  }
  if (resp.status === 204 || resp.body === undefined) {
    res.writeHead(resp.status, headers);
    res.end();
    return;
  }
  const body = Buffer.from(JSON.stringify(resp.body), 'utf8');
  res.writeHead(resp.status, { ...headers, 'content-type': 'application/json', 'content-length': body.length });
  res.end(body);
}

async function dispatch(store: Store, req: IncomingMessage, requestId: string): Promise<Resp> {
  const url = new URL(req.url ?? '/', 'http://localhost');
  const segments = decodeSegments(url.pathname);
  const authorization = req.headers.authorization ?? '';
  const token = authorization.startsWith('Bearer ') ? authorization.slice('Bearer '.length) : null;
  const body = await readBody(req);
  const ctx: ReqCtx = {
    store,
    method: req.method ?? 'GET',
    segments,
    query: url.searchParams,
    host: req.headers.host ?? 'localhost',
    token,
    body,
    requestId,
  };

  if (segments.length === 1 && segments[0] === 'healthz' && ctx.method === 'GET') {
    return { status: 200, body: { ok: true } };
  }

  if (segments.length === 1 && segments[0] === 'mcp') return routeMcp(store, ctx) ?? errorResp(404, 'Not Found');
  if (segments.length === 2 && segments[0] === 'api' && segments[1] === 'graphql') return routeGraphql(store, ctx) ?? errorResp(404, 'Not Found');
  if (segments.length >= 2 && segments[0] === 'api' && segments[1] === 'v3') return routeRest(store, ctx) ?? errorResp(404, 'Not Found');
  if (segments.length >= 3 && segments[1].endsWith('.git')) {
    return handleGitHttp(store, ctx, segments[0], segments[1].slice(0, -'.git'.length), segments.slice(2), req.headers);
  }

  return errorResp(404, 'Not Found');
}
