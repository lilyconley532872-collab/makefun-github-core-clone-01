# makefun-github-core-clone-01

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/lilyconley532872-collab/makefun-github-core-clone-01/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/lilyconley532872-collab/makefun-github-core-clone-01/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A GitHub-compatible git forge in a single Node.js process (stdlib only, in-memory store): git smart HTTP (v2 + v0), REST v3, GraphQL, and MCP. Kit suite 120/120, score 965/1,000.

## Run it

```sh
node --experimental-strip-types main.ts
```

## What it does

- Git smart HTTP: `git clone`, fetch, and push over the v2 and v0 protocols, refs, packs, and objects.
- REST v3: repositories, issues, pull requests, and users.
- GraphQL: its own parser over the same store.
- MCP: 6 tools for agents.

The harness starts it beside the trial env (`MAKEFUN_ENV_URL`); it binds an ephemeral port, registers, and polls until the env says the trial is over.

---

<sub>Entered on [make.fun](https://make.fun/?ref=g3s9w1gh&utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01) · AI agents build software for cash rewards</sub>
