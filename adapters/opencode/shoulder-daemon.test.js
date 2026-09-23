/**
 * Tests for the adapter's recovery decision: given what /readyz says, does it
 * start a relay, and how often.
 *
 * These are here rather than in relay/integration because that suite proves a
 * different thing. It drives a real editor against a real daemon and costs a
 * model call per test, which is what makes it the right place for "does a real
 * OpenCode session reach the daemon at all" and the wrong place for this: a
 * real relay cannot be made to answer 404 without building an older one, a
 * broken store is not something it can stage, and "no second start within
 * thirty seconds" is a statement about one process's memory that an editor
 * invocation - one process, one plugin load - cannot even express.
 *
 * So the relay is a stub that answers however the case needs, and the thing
 * under test is the adapter module itself.
 *
 *  node --test adapters/opencode/shoulder-daemon.test.js
 */

import { strict as assert } from "node:assert";
import { createServer } from "node:http";
import { mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { after, test } from "node:test";
import { fileURLToPath } from "node:url";

const adapter = fileURLToPath(new URL("./shoulder-daemon.js", import.meta.url));

let count = 0;

// The address the adapter uses when it is told nothing, which on a developer's
// machine is their own daemon, holding the memory of everything they have
// worked on.
const LIVE_DAEMON = "127.0.0.1:8787";

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

/** waitFor polls until want is true, or gives up and returns false. */
async function waitFor(want, within = 3000) {
  const deadline = Date.now() + within;
  while (Date.now() < deadline) {
    if (want()) return true;
    await sleep(10);
  }
  return want();
}

/**
 * stub is a relay that answers /readyz with one fixed status and counts how
 * often it was asked, which is how a test knows the probe has happened and can
 * then assert on what did not follow it.
 *
 * status and body are writable, because a store does not usually die at the
 * moment an editor opens: the case that matters is a relay that was ready when
 * the plugin loaded and is not ready by the time somebody types.
 *
 * dropEvents kills the connection instead of answering an event, which is the
 * only way to reach post()'s recovery path: a refused event is a reply, and the
 * adapter only reaches for a relay when a request fails outright.
 *
 * stop() takes it away entirely, leaving the address the adapter is pointed at
 * with nothing behind it. Open connections are destroyed rather than allowed to
 * drain, because the client keeps one alive between probes and a probe that
 * reused it would be answered by a relay the test believes it has killed.
 */
async function stub(t, status, body = { ok: false, memory: "unreachable" }) {
  const s = { probes: 0, events: 0, status, body, raw: null, dropEvents: false };
  const server = createServer((req, res) => {
    // Everything but the readiness route answers 404, which for /v1/events is
    // a reply rather than a failure: the adapter reads a refused event as no
    // advice and does not reach for a start, so a start seen in a test that
    // posts an event is one the readiness check asked for and nothing else.
    if (req.url !== "/readyz") {
      s.events++;
      if (s.dropEvents) req.socket.destroy();
      else res.writeHead(404).end();
      return;
    }
    s.probes++;
    res.writeHead(s.status, { "Content-Type": "application/json" });
    if (s.raw !== null) {
      res.end(s.raw);
      return;
    }
    res.end(s.status === 404 ? "" : JSON.stringify(s.status === 200 ? { ok: true, memory: "ok" } : s.body));
  });
  await new Promise((r) => server.listen(0, "127.0.0.1", r));
  s.addr = `127.0.0.1:${server.address().port}`;
  s.stop = async () => {
    server.closeAllConnections();
    await new Promise((r) => server.close(r));
  };
  t.after(() => s.stop().catch(() => {}));
  return s;
}

/**
 * isolated refuses to let a test run against anything real.
 *
 * Every one of these settings has a fallback that leads somewhere live. The
 * address defaults to the port a developer's own daemon is listening on. The
 * token and the address are both read from that daemon's env file when the
 * process does not carry them, which is the whole point of that file and is
 * exactly what makes it dangerous here. The start command falls back to running
 * whatever `shoulderd` is on PATH. A test that forgot any of them would not
 * fail - it would pass, having posted invented sessions into somebody's real
 * memory, and this suite exists for a user who has already lost days of work to
 * a fault nobody was watching.
 *
 * So it is asserted on every load rather than set once and trusted.
 */
function isolated(addr, dir) {
  assert.notEqual(addr, LIVE_DAEMON, "a test is pointed at the live daemon's address");
  assert.equal(process.env.SHOULDER_ADDR, addr, "the adapter would resolve an address other than the stub's");
  assert.equal(process.env.SHOULDER_ENV_FILE, "/dev/null", "the real daemon's env file is readable from a test");
  assert.equal(process.env.SHOULDER_TOKEN, undefined, "a test carries a token that could authenticate to a real daemon");
  assert.ok(
    (process.env.SHOULDER_START_CMD || "").includes(dir),
    "the start command could reach something outside this test's directory",
  );
}

/**
 * plugin loads a fresh copy of the adapter pointed at addr. The address, the
 * token and the env file are read once when the module is evaluated, so a test
 * that wants its own relay needs its own module - hence the cache-busting
 * query, which is the only way to re-evaluate an ES module in one process.
 *
 * The start command appends a byte to a file instead of starting anything, so
 * the file's length is the number of start attempts. It is read through
 * SHOULDER_START_CMD, which the adapter consults per call rather than at load,
 * and it takes precedence over looking for a `shoulderd` on PATH - which is the
 * other reason to set it: a developer who has one installed would otherwise
 * have these tests start their real daemon.
 */
async function plugin(t, addr) {
  const dir = mkdtempSync(join(tmpdir(), "shoulder-adapter-test-"));
  const starts = join(dir, "starts");
  writeFileSync(starts, "");

  process.env.SHOULDER_ADDR = addr;
  process.env.SHOULDER_ENV_FILE = "/dev/null";
  process.env.SHOULDER_START_CMD = `printf x >> ${starts}`;
  // Whoever is running this suite almost certainly has a token exported for
  // their own daemon, and a token that matched is the difference between a post
  // that was rejected and a post that was kept.
  delete process.env.SHOULDER_TOKEN;
  // The start lock lives here, so each test gets one of its own and never
  // inherits a lock, or a stale-lock window, from the test before it.
  process.env.XDG_RUNTIME_DIR = dir;

  isolated(addr, dir);
  const mod = await import(`${adapter}?case=${count++}`);
  return {
    // Loading the plugin is the moment the adapter probes and decides, and it
    // deliberately does not wait for that decision, so every assertion below
    // is made after polling rather than after an await.
    load: () => mod.ShoulderDaemon({ directory: dir, worktree: dir }),
    starts: () => readFileSync(starts, "utf8").length,
  };
}

// Nothing here starts a real daemon, but a test that regressed into starting
// one would leave it behind, so the environment it would need is not left set.
after(() => {
  for (const k of ["SHOULDER_ADDR", "SHOULDER_ENV_FILE", "SHOULDER_START_CMD"]) delete process.env[k];
});

// The guard itself, because a guard that stopped guarding would be discovered
// by the thing it was guarding against.
test("a test pointed at the live daemon is refused", () => {
  assert.throws(() => isolated(LIVE_DAEMON, "/tmp"), /live daemon/);
});

// The failure this whole change exists for: a relay that is serving happily
// while the store behind it is unreachable answers every request and fixes
// nothing. It used to be indistinguishable from a healthy one, and sessions ran
// for days against it.
test("a relay that is serving but not ready is replaced", async (t) => {
  const relay = await stub(t, 503);
  const p = await plugin(t, relay.addr);

  await p.load();

  assert.ok(await waitFor(() => p.starts() === 1), "no start was attempted against an unready relay");
});

// The floor. A store that is broken for good answers 503 to every probe, and
// the adapter probes again whenever a request fails, so the only thing standing
// between a user and one spawned process per hook is this.
test("a relay whose store stays unreachable is not restarted again within the floor", async (t) => {
  const relay = await stub(t, 503);
  const p = await plugin(t, relay.addr);

  await p.load();
  assert.ok(await waitFor(() => p.starts() === 1), "no start was attempted against an unready relay");

  const probes = relay.probes;
  await p.load();
  assert.ok(await waitFor(() => relay.probes > probes), "the second load never probed the relay");

  // The probe has been answered, so whatever start it was going to cause has
  // been handed off by now; what is left is the spawn's own latency, which is
  // what this waits out.
  await sleep(300);
  assert.equal(p.starts(), 1, "a second start was spawned within the backoff floor");
});

// The one unready answer that starting a relay cannot mend. A daemon told to
// use no store is not broken, it is configured, and every relay this adapter
// could start would be configured the same way - so a start here is a process
// spawned for nothing, and post() asks again after every failed hook.
test("a relay with no memory backend configured is never restarted", async (t) => {
  const relay = await stub(t, 503, {
    ok: false,
    memory: "none",
    error: "no memory backend is configured; nothing observed in a session will be kept",
  });
  // Events fail at the transport, so every prompt below goes down post()'s
  // recovery path as well as the prompt hook's own readiness check. This is the
  // worst case the floor was written for, and the one case the floor is not
  // allowed to be the answer to.
  relay.dropEvents = true;

  const said = [];
  const wasError = console.error;
  console.error = (...args) => said.push(args.join(" "));
  t.after(() => {
    console.error = wasError;
  });

  const p = await plugin(t, relay.addr);
  const hooks = await p.load();

  for (let i = 0; i < 3; i++) {
    await hooks["chat.message"]({ sessionID: "session" }, { parts: [{ type: "text", text: "hello" }] });
  }
  await sleep(300);

  assert.ok(relay.probes >= 3, `the adapter stopped asking after ${relay.probes} probes`);
  assert.ok(relay.events >= 3, `the adapter stopped posting after ${relay.events} events`);
  assert.equal(p.starts(), 0, "a relay was started for a fault that starting a relay cannot fix");
  assert.equal(said.length, 1, `the missing store was reported ${said.length} times rather than once`);
  assert.match(said[0], /no memory backend/, "the warning does not name the fault");
});

// A 503 nobody can read is not evidence of anything. It is also what a body cut
// short by the probe's own deadline looks like, from a relay that is well.
test("a relay whose readiness body will not parse is left alone", async (t) => {
  const relay = await stub(t, 503);
  relay.raw = "{ this is not json";
  const p = await plugin(t, relay.addr);

  await p.load();
  assert.ok(await waitFor(() => relay.probes > 0), "the adapter never probed the relay");

  await sleep(300);
  assert.equal(p.starts(), 0, "a relay was restarted on a readiness answer that could not be read");
});

// Backward compatibility. A relay built before /readyz has no such route, and
// reading its 404 as unreadiness would restart a perfectly good relay on every
// probe for everybody who has not upgraded.
test("a relay with no readiness route is left alone", async (t) => {
  const relay = await stub(t, 404);
  const p = await plugin(t, relay.addr);

  await p.load();
  assert.ok(await waitFor(() => relay.probes > 0), "the adapter never probed the relay");

  await sleep(300);
  assert.equal(p.starts(), 0, "an older relay was restarted for answering 404 to /readyz");
});

// A ready relay is the ordinary case and must cost nothing.
test("a ready relay is left alone", async (t) => {
  const relay = await stub(t, 200);
  const p = await plugin(t, relay.addr);

  await p.load();
  assert.ok(await waitFor(() => relay.probes > 0), "the adapter never probed the relay");

  await sleep(300);
  assert.equal(p.starts(), 0, "a ready relay was restarted");
});

// The state this change is really aimed at is not a relay that is broken when
// the editor opens - it is one that breaks under an editor that stays open for
// days. Nothing on the request path notices, because a relay whose store is
// gone still answers, so the prompt hook has to ask.
test("a store that dies under a live session is noticed at the next prompt", async (t) => {
  const relay = await stub(t, 200);
  const p = await plugin(t, relay.addr);

  const hooks = await p.load();
  assert.ok(await waitFor(() => relay.probes > 0), "the adapter never probed the relay");
  assert.equal(p.starts(), 0, "a ready relay was restarted");

  relay.status = 503;
  await hooks["chat.message"]({ sessionID: "session" }, { parts: [{ type: "text", text: "hello" }] });

  assert.ok(await waitFor(() => p.starts() === 1), "the session went on talking to a relay that could not answer");
});

// The floor must not reach the case it was never meant for. A relay that is
// down is observing nobody, and a session that finds one has to get it back
// now rather than in half a minute.
test("an absent relay is started immediately, floor or no floor", async (t) => {
  const relay = await stub(t, 503);
  const p = await plugin(t, relay.addr);

  // First the unready path, which arms the floor.
  await p.load();
  assert.ok(await waitFor(() => p.starts() === 1), "no start was attempted against an unready relay");

  // Then the relay goes away, and the same loaded adapter - the one holding the
  // floor that was armed a moment ago - finds nothing listening at all.
  await relay.stop();
  await p.load();
  assert.ok(await waitFor(() => p.starts() === 2), "an absent relay was made to wait out the backoff floor");
});
