// context.ts — one request's context, shared by the REST, GraphQL, MCP and git surfaces.

import type { Store } from './store.ts';

export interface ReqCtx {
  store: Store;
  method: string;
  /** Decoded path segments (no leading empty segment, no query). */
  segments: string[];
  query: URLSearchParams;
  host: string;
  /** The bearer token, or null when no Authorization header was sent. */
  token: string | null;
  /** The raw request body (already read). */
  body: Buffer;
  requestId: string;
}

export interface Resp {
  status: number;
  /** A JSON-serialisable body (ignored when `raw` is set). */
  body?: unknown;
  /** A raw body (binary responses: git smart HTTP). */
  raw?: Buffer;
  /** Content type for a raw body; JSON replies use application/json. */
  contentType?: string;
  headers?: Record<string, string>;
}

export function jsonResp(status: number, body: unknown, headers?: Record<string, string>): Resp {
  return { status, body, headers };
}

export function errorResp(status: number, message: string): Resp {
  return { status, body: { message } };
}

export function rawResp(status: number, raw: Buffer, contentType: string, headers?: Record<string, string>): Resp {
  return { status, raw, contentType, headers };
}

/** Parse the request's JSON body; throws an Error with a message on malformed JSON. */
export function parseJsonBody(ctx: ReqCtx): unknown {
  if (ctx.body.length === 0) return {};
  const text = ctx.body.toString('utf8');
  try {
    return JSON.parse(text) as unknown;
  } catch {
    throw new Error('Invalid JSON in request body');
  }
}

export function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}
