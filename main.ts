// Supervisor for the GitForge entrypoint.
//
// The forge itself is a static Go binary shipped next to this file as
// ./forge. That file cannot be trusted to carry its executable bit: the
// upload path that publishes the repository strips it, so spawning it
// in place fails with EACCES in the grading pod. This supervisor
// therefore copies it to a fresh file in the OS temp directory, makes
// that copy executable, and spawns the copy. Nothing is ever written
// into the repository directory, which the pod mounts read-only, and
// the temp copy is removed when the run ends.
//
// Lifecycle (the harness's contract, not the contest's): spawn the
// forge, read the ephemeral port it prints as FORGE_PORT=<n>, register
// that port with the make.fun environment, poll until the environment
// reports done, then SIGTERM the child and exit 0. The supervisor
// never binds a port itself, and never binds 8080 — the env owns it.
import { spawn, type ChildProcess } from "node:child_process";
import { randomBytes } from "node:crypto";
import { chmod, copyFile, unlink } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

const ENV_URL: string = (process.env.MAKEFUN_ENV_URL ?? "").replace(/\/+$/, "");
if (ENV_URL === "") {
  process.stderr.write("[entry] MAKEFUN_ENV_URL is not set: there is no env to register with\n");
  process.exit(1);
}

const FORGE_PATH: string = fileURLToPath(new URL("./forge", import.meta.url));

// How long one register or done request may take before it is abandoned
// and retried. The first wait between retries, and the longest.
const REQUEST_TIMEOUT_MS = 30_000;
const RETRY_INITIAL_MS = 250;
const RETRY_MAX_MS = 2_000;

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

// The wait after `attempt` failed requests: exponential from
// RETRY_INITIAL_MS, capped at RETRY_MAX_MS.
function retryDelay(attempt: number): number {
  return Math.min(RETRY_MAX_MS, RETRY_INITIAL_MS * 2 ** Math.min(attempt, 8));
}

// POST the bound port until the env accepts it. 200 is the normal
// answer; 409 means a first register already won. 400 and 405 are
// contract errors and end the program; anything else is retried
// forever, since the env may still be starting.
async function register(port: number): Promise<void> {
  for (let attempt = 0; ; attempt += 1) {
    try {
      const res = await fetch(`${ENV_URL}/register`, {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ port }),
        signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS),
      });
      if (res.status === 200 || res.status === 409) return;
      if (res.status === 400 || res.status === 405) {
        throw new Error(`register answered HTTP ${res.status}: ${await res.text()}`);
      }
    } catch (err) {
      if (err instanceof Error && err.message.startsWith("register answered HTTP ")) throw err;
    }
    await sleep(retryDelay(attempt));
  }
}

// Poll GET /done until it answers {"done": true}; never return before that.
async function waitForDone(): Promise<void> {
  for (let attempt = 0; ; attempt += 1) {
    try {
      const res = await fetch(`${ENV_URL}/done`, { signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS) });
      if (res.ok) {
        const body = (await res.json()) as { done?: unknown };
        if (body.done === true) return;
      }
    } catch {
      // The env is not answering yet, or the poll timed out: it is not
      // done, so poll again.
    }
    await sleep(retryDelay(attempt));
  }
}

// Stage an executable copy of the forge in the temp directory and hand
// back its path. The copy's name is unique per process so concurrent
// runs never share (or clobber) one file.
async function stageForge(): Promise<string> {
  const staged = join(tmpdir(), `forge-${process.pid}-${randomBytes(6).toString("hex")}`);
  await copyFile(FORGE_PATH, staged);
  await chmod(staged, 0o755);
  return staged;
}

async function main(): Promise<void> {
  const staged = await stageForge();
  let child: ChildProcess | null = null;
  try {
    child = spawn(staged, [], {
      stdio: ["ignore", "pipe", "inherit"],
      env: process.env,
    });
    let port = 0;
    let buf = "";
    let spawnError: Error | null = null;
    child.on("error", (err: Error) => {
      spawnError = err;
    });
    child.stdout?.on("data", (chunk: Buffer) => {
      buf += chunk.toString("utf8");
      let nl = buf.indexOf("\n");
      while (nl >= 0) {
        const line = buf.slice(0, nl).trim();
        const m = /^FORGE_PORT=(\d+)$/.exec(line);
        if (m !== null) port = parseInt(m[1], 10);
        buf = buf.slice(nl + 1);
        nl = buf.indexOf("\n");
      }
    });

    while (port === 0) {
      if (spawnError !== null) {
        throw new Error(`failed to spawn the staged forge: ${spawnError.message}`);
      }
      if (child.exitCode !== null) {
        throw new Error(`forge exited early with code ${String(child.exitCode)}`);
      }
      await sleep(25);
    }
    await register(port);
    await waitForDone();
  } finally {
    if (child !== null) {
      child.kill("SIGTERM");
      // Give the child a brief chance to exit cleanly before this
      // process goes away; do not hang here if it ignores the signal.
      if (child.exitCode === null) {
        await Promise.race([
          new Promise<void>((resolve) => child?.once("exit", () => resolve())),
          sleep(2000),
        ]);
      }
    }
    await unlink(staged).catch(() => undefined);
  }
}

main().then(
  () => process.exit(0),
  (err: unknown) => {
    console.error(err);
    process.exit(1);
  },
);
