// Supervisor for the forge entrypoint.
//
// Spawns the ./forge Go binary, reads the ephemeral port it prints as
// FORGE_PORT=<n>, registers that port with the make.fun environment,
// then waits until the environment reports done and shuts the child
// down. The supervisor never binds a port itself.
import { spawn, type ChildProcess } from "node:child_process";
import { fileURLToPath } from "node:url";

const envUrl: string = process.env.MAKEFUN_ENV_URL ?? "";
const forgePath: string = fileURLToPath(new URL("./forge", import.meta.url));

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

async function register(port: number): Promise<void> {
  let delay = 100;
  for (let attempt = 0; attempt < 60; attempt++) {
    try {
      const res = await fetch(`${envUrl}/register`, {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ port }),
      });
      if (res.status === 200 || res.status === 409) return;
    } catch {
      // Environment not reachable yet; retry with backoff.
    }
    await sleep(delay);
    delay = Math.min(delay * 2, 2000);
  }
  throw new Error("register failed: environment did not accept the port");
}

async function waitDone(): Promise<void> {
  let delay = 250;
  for (;;) {
    try {
      const res = await fetch(`${envUrl}/done`);
      if (res.ok) {
        const body = (await res.json()) as { done?: boolean };
        if (body.done === true) return;
      }
    } catch {
      // Environment not reachable yet; keep polling.
    }
    await sleep(delay);
    delay = Math.min(delay * 2, 2000);
  }
}

async function main(): Promise<void> {
  const child: ChildProcess = spawn(forgePath, [], {
    stdio: ["ignore", "pipe", "inherit"],
    env: process.env,
  });
  let port = 0;
  let buf = "";
  child.stdout?.on("data", (chunk: Buffer) => {
    buf += chunk.toString("utf8");
    const nl = buf.indexOf("\n");
    if (nl < 0) return;
    const line = buf.slice(0, nl).trim();
    const m = /^FORGE_PORT=(\d+)$/.exec(line);
    if (m !== null) port = parseInt(m[1], 10);
    buf = buf.slice(nl + 1);
  });

  const deadline = Date.now() + 10000;
  while (port === 0 && Date.now() < deadline) {
    if (child.exitCode !== null) {
      throw new Error(`forge exited early with code ${String(child.exitCode)}`);
    }
    await sleep(25);
  }
  if (port === 0) throw new Error("forge did not print FORGE_PORT");
  await register(port);
  await waitDone();
  child.kill("SIGTERM");
}

main().then(
  () => process.exit(0),
  (err: unknown) => {
    console.error(err);
    process.exit(1);
  },
);
