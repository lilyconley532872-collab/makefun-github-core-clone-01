// The entry <-> env lifecycle, as env/src/wire.ts settles it: bind an ephemeral port, tell the env
// the port it bound, then poll until the env says the trial is over and exit 0. Keep this file as it is:
// the suite scores the server app.ts builds, and the lifecycle is the harness's, not the contest's.
//
//   1. MAKEFUN_ENV_URL names the env's origin (http://store:8080 in a trial container). Without it
//      there is nothing to talk to, so the program exits 1 at once.
//   2. Create the server and listen(0): the kernel picks a free port. Never bind 8080 — the env owns it.
//   3. POST $MAKEFUN_ENV_URL/register {"port": N}. The env records the address the request came from
//      plus N as your base URL and dials it from then on. The first register wins; a 409 means another
//      entry already registered, and this process keeps waiting like any other.
//   4. GET $MAKEFUN_ENV_URL/done until the body is {"done": true}. The env answers at once ({"done":
//      false} while the suite runs), so this is a poll, not a long-held request. A failed or slow poll
//      is retried with a short exponential backoff; the process never exits before the env says done.
//   5. Close the server and exit 0. The harness then stops the env, which writes the export the scorer
//      reads.
//
// This entrypoint is a thin launcher: the forge itself is the statically linked Go binary committed
// next to it as ./forge (vendored source — RULES.txt: "a statically linked Linux amd64 binary you
// commit runs from your repository as vendored source"). The launcher execs it with the same
// environment and stdio, forwards signals, and exits with the child's code. The Go program owns the
// whole lifecycle (bind, register, done-poll) — this file only starts it.

import { spawn } from 'node:child_process';
import { existsSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const ENV_URL = (process.env.MAKEFUN_ENV_URL ?? '').replace(/\/+$/, '');
if (ENV_URL === '') {
  process.stderr.write('[entry] MAKEFUN_ENV_URL is not set: there is no env to register with\n');
  process.exit(1);
}

const here = dirname(fileURLToPath(import.meta.url));
const binary = join(here, 'forge');
if (!existsSync(binary)) {
  process.stderr.write('[entry] vendored forge binary is missing from the repository\n');
  process.exit(1);
}

const child = spawn(binary, [], { stdio: 'inherit', env: process.env });
const forward = (signal: NodeJS.Signals): void => {
  try {
    child.kill(signal);
  } catch {
    // The child is already gone; nothing left to do.
  }
};
process.on('SIGTERM', () => forward('SIGTERM'));
process.on('SIGINT', () => forward('SIGINT'));

child.on('error', (err: Error) => {
  process.stderr.write(`[entry] ${err instanceof Error ? err.message : String(err)}\n`);
  process.exit(1);
});
child.on('exit', (code: number | null, signal: NodeJS.Signals | null) => {
  if (signal !== null) {
    process.kill(process.pid, signal);
    return;
  }
  process.exit(code ?? 1);
});
