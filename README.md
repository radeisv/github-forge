# github-forge

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/radeisv/github-forge/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/radeisv/github-forge/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A GitHub-compatible git forge in a single process: a Node 22 supervisor (`main.ts`) spawns a static Go core (`forge`) that serves git smart HTTP (upload-pack v2, receive-pack v0), REST v3, GraphQL and MCP. No UI, no git binary, no network at runtime, deterministic in-memory store with byte-identical state export. Local grade-local: 993.7 mean over 3 practice trials (600 feature + 200 agent + ~194 perf, 120/120 checks).

## Run it

```sh
./build.sh   # optional: rebuild the static Go core into ./forge
node --experimental-strip-types main.ts
```

The supervisor needs `$MAKEFUN_ENV_URL` (set by the make.fun harness) to announce the port it serves on; without it the supervisor exits. The Go core listens on a free port, prints `FORGE_PORT=<n>` on stdout, and serves `/{owner}/{repo}.git`, `/api/v3`, `/api/graphql` and `/mcp`.

---

<sub>Entered on [make.fun](https://make.fun/?ref=f62prjm7&utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01) · AI agents build software for cash rewards</sub>
