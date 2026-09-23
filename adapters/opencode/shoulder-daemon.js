/**
 * shoulder-daemon adapter for OpenCode.
 *
 * Maps an OpenCode session onto the harness-neutral event shape the relay
 * accepts at POST /v1/events, and injects whatever advice comes back into the
 * system prompt of the next request.
 *
 * Two things about OpenCode's plugin dispatcher shape everything here.
 *
 * It awaits every hook with no timeout, and it awaits them through
 * Effect.promise, which treats a rejection as an unrecoverable defect rather
 * than a typed error - and no call site catches it. A hook that throws takes
 * the user's turn with it. Claude Code's harness fails open on our behalf; here
 * that guarantee is ours to keep, so every hook body below is wrapped and can
 * never rethrow, and every request carries its own deadline.
 *
 * And `output` is passed by reference: push into it, never reassign it, or the
 * caller keeps the array it already had.
 */

import { mkdirSync, readFileSync, rmdirSync, statSync } from "node:fs";
import { spawn, spawnSync } from "node:child_process";
import { homedir, tmpdir } from "node:os";
import { join } from "node:path";

/**
 * setting reads one SHOULDER_ variable, falling back to the daemon's own env
 * file when the process does not have it.
 *
 * An editor started from a desktop launcher inherits the session environment,
 * not the login shell's, so the exports that configure this thing are usually
 * absent from it. Without the token the adapter posts unauthenticated and the
 * daemon rejects every hook, which looks exactly like the daemon being down
 * while it sits there healthy - the one failure the user cannot see from
 * either side. The file is the same one the daemon is configured from, so
 * there is nothing extra to keep in step.
 */
const envFile = (() => {
  const path =
    process.env.SHOULDER_ENV_FILE ||
    join(process.env.XDG_CONFIG_HOME || join(homedir(), ".config"), "shoulder-daemon", "env");
  try {
    const out = {};
    for (const line of readFileSync(path, "utf8").split("\n")) {
      const m = /^\s*(?:export\s+)?(SHOULDER_[A-Z0-9_]+)\s*=\s*(.*)$/.exec(line);
      if (m) out[m[1]] = m[2].trim().replace(/^(['"])(.*)\1$/, "$2");
    }
    return out;
  } catch {
    return {};
  }
})();

const setting = (name, fallback = "") => process.env[name] || envFile[name] || fallback;

const ADDR = setting("SHOULDER_ADDR", "127.0.0.1:8787");
const BASE = `http://${ADDR}`;
const TOKEN = setting("SHOULDER_TOKEN");

// Long enough for a loopback call to a relay that answers in microseconds,
// short enough that a wedged daemon is not something the user can feel.
const DEADLINE_MS = Number(setting("SHOULDER_TIMEOUT_MS", "250"));

const headers = () => {
  const h = { "Content-Type": "application/json" };
  if (TOKEN) h["X-Shoulder-Token"] = TOKEN;
  return h;
};

/** post sends one neutral event. Resolves to the advice, or null for anything at all. */
async function post(event) {
  const body = JSON.stringify({ protocol: 1, harness: "opencode", ...event });
  const once = async () => {
    const res = await fetch(`${BASE}/v1/events`, {
      method: "POST",
      headers: headers(),
      body,
      signal: AbortSignal.timeout(DEADLINE_MS),
    });
    if (!res.ok) return null;
    const out = await res.json();
    return out && out.advice ? out.advice : null;
  };
  try {
    return await once();
  } catch {
    // Nothing answered. The daemon stops when the last session it knows about
    // ends, and a daemon that restarted a moment ago knows about one editor
    // however many are open - so it can and does stop under a session that is
    // still working. Starting one and trying again is what keeps this editor
    // observed for the rest of its life; it is only reached when a post has
    // already failed, so a daemon that is up never pays for it.
    try {
      await reviveDaemon();
      return await once();
    } catch {
      return null;
    }
  }
}

// reviveDaemon runs at most one start attempt at a time, however many hooks
// discover the daemon missing at once.
let reviving = null;
function reviveDaemon() {
  if (!reviving) {
    reviving = ensureDaemon().finally(() => {
      reviving = null;
    });
  }
  return reviving;
}

/**
 * watchReadiness re-asks whether the relay can still do its job, and replaces it
 * if it cannot, without the turn waiting on either.
 *
 * Probing when the plugin loads is not enough on its own. A store that dies
 * under a running editor leaves a relay that goes on answering /v1/events with
 * a 200 and no advice, so nothing on the request path ever fails and nothing
 * ever asks again - which is how an editor left open for days stayed pointed at
 * a relay that had been useless since the first morning. The relay caches its
 * own answer for ten seconds precisely so that a hook may ask this often.
 *
 * Deliberately not awaited and deliberately not returned: a prompt must not
 * wait a second on a probe, and a rejection that escaped here would reach a
 * hook body and take the user's turn with it.
 */
function watchReadiness() {
  answering()
    .then((state) => (state === "ready" || state === "unknown" ? null : reviveDaemon()))
    .catch(() => {});
}

/** send fires an event we do not need an answer to, without making the turn wait. */
function send(event) {
  post(event).catch(() => {});
}

/**
 * answering reports what is at the other end, as one of five verdicts: "ready",
 * "unknown" for a relay whose readiness cannot be established, "unreachable"
 * for one whose store should be there and is not, "none" for one with no store
 * configured at all, and "down" for nothing answering.
 *
 * Asking about readiness rather than liveness is the whole point. A relay whose
 * store is unreachable still answers /healthz with ok and still accepts every
 * request it is given, so a probe that only asked whether something was
 * listening stayed satisfied while every recall and every write failed - real
 * sessions ran for days in that state with nothing on either side to show for
 * it.
 *
 * The status alone is not enough to act on, which is why the body is read. The
 * relay answers 503 for two faults that want opposite handling: a store that
 * died, which a new relay may well fix, and a relay configured with no store,
 * which no relay ever started from here can fix.
 */
async function answering() {
  try {
    const res = await fetch(`${BASE}/readyz`, { signal: AbortSignal.timeout(1000) });
    // A relay from before /readyz existed has no such route and 404s on it.
    // That relay is plainly serving and its readiness is simply not knowable,
    // so it is left alone: reading the missing route as a failure would put
    // everybody who has not upgraded into a start attempt on every probe, for
    // the life of the editor, against a relay that was never unwell.
    if (res.status === 404) return "unknown";
    if (res.ok) return "ready";
    try {
      const body = await res.json();
      // Only the one value is singled out, and every other unready body is
      // taken as a store to be replaced: a verdict this adapter has not been
      // taught is far more likely to be a newer relay describing a fault than
      // a reason to do nothing, and the floor below bounds what that costs.
      return body && body.memory === "none" ? "none" : "unreachable";
    } catch {
      // A 503 whose body will not parse says only that something is wrong, and
      // the one thing worse than not recovering is recovering blindly: this is
      // also the shape a body truncated by the deadline arrives in, from a
      // relay that may be perfectly well.
      return "unknown";
    }
  } catch {
    return "down";
  }
}

// warnedNoMemory keeps the one fault nothing here can repair from being
// reported on every prompt for the rest of the day. Once per process is enough
// to tell somebody their sessions are not being kept; it is a line about
// configuration, and the configuration will not change under them.
let warnedNoMemory = false;

// lastStart is when a start was last handed off, and restartFloorMs is the
// shortest gap allowed between an unreachable store and the start it is
// permitted to cause.
//
// A store that is broken for good answers 503 to every probe it will ever be
// given, and post() reaches for reviveDaemon whenever a request fails - so with
// no floor an unreachable store means spawning the start command once per hook
// for as long as the editor is open, every spawn as useless as the one before
// it and each one a process. The floor is consulted only for a relay that
// answered: one that is down is observing nobody, and holding its replacement
// back for half a minute would cost a live session its advice to prevent a
// storm that cannot happen, because a relay that comes back stops failing.
let lastStart = 0;
const restartFloorMs = 30_000;

/**
 * ensureDaemon starts the relay unless one is already serving and ready.
 *
 * This runs once when the plugin loads, which is the only moment OpenCode gives
 * that is equivalent to a session-start hook. Several editors opening together
 * would each find nothing listening and each start one, so the winner is decided
 * by an atomic mkdir; the losers wait for the winner rather than racing to bind.
 *
 * It is never awaited by anything that matters and never throws.
 */
async function ensureDaemon() {
  const state = await answering();
  if (state === "ready" || state === "unknown") return;

  // A relay with no store configured is the one unready answer that is not a
  // fault of the process: it is doing exactly what it was told to do, and
  // starting another one from the same configuration would produce another
  // relay that keeps nothing. Left in the general case it would mean a start
  // every thirty seconds for as long as the editor is open, and post() asks
  // again after every failed hook, so in practice rather more than that.
  if (state === "none") {
    if (!warnedNoMemory) {
      warnedNoMemory = true;
      console.error(
        `shoulder-daemon: the relay at ${ADDR} has no memory backend configured, so nothing this session does is being kept.\n` +
          "shoulder-daemon: set SHOULDER_MEMORY and restart it yourself - run 'shoulderd doctor' for which backends it can see.",
      );
    }
    return;
  }

  if (state === "unreachable" && Date.now() - lastStart < restartFloorMs) return;
  // Taken before the lock rather than after the spawn, because a hook that
  // lost the race to another process still found a start in flight and must
  // not come back for another one the moment the next request fails.
  lastStart = Date.now();

  const lock = join(process.env.XDG_RUNTIME_DIR || tmpdir(), "shoulder-daemon.start.lock");
  // A lock left behind by a launch that died wedges every start that follows,
  // for good: the daemon never comes back and nothing says why. A timer cannot
  // be relied on to clear it, because the process holding the timer is exactly
  // the one that may not survive. So it is broken on age instead: one older
  // than staleLockMs belongs to a launch that is not coming back.
  const staleLockMs = 60_000;
  const take = () => {
    try {
      mkdirSync(lock);
      return true;
    } catch {}
    try {
      if (Date.now() - statSync(lock).mtimeMs > staleLockMs) {
        rmdirSync(lock);
        mkdirSync(lock);
        return true;
      }
    } catch {}
    return false;
  };

  if (!take()) {
    // Somebody else is starting it. Give them a moment, then stop caring: a
    // daemon that never arrives costs this session its advice and nothing else.
    for (let i = 0; i < 10; i++) {
      await new Promise((r) => setTimeout(r, 300));
      // Anything that answers ends the wait, well or not: this loop is about
      // the winner's relay binding, and a relay that came up with a sick store
      // is the next probe's business rather than something more waiting fixes.
      if ((await answering()) !== "down") return;
    }
    return;
  }

  try {
    const cmd = setting("SHOULDER_START_CMD");
    const child = cmd
      ? spawn(cmd, { shell: true, detached: true, stdio: "ignore" })
      : spawn("shoulderd", { detached: true, stdio: "ignore" });
    child.on("error", () => {
      console.error(
        `shoulder-daemon: nothing answering at ${ADDR} and no 'shoulderd' on PATH.\n` +
          "shoulder-daemon: go install gitlab.com/quittymr/shoulder-daemon/relay/cmd/shoulderd@latest",
      );
    });
    child.unref();
  } catch {
    /* starting the daemon is best effort; it is not this session's problem */
  } finally {
    // Released as soon as the start has been handed off, so a second editor
    // opening moments later is not made to wait out the staleness window.
    try {
      rmdirSync(lock);
    } catch {}
  }
}

/**
 * syncPost makes one HTTP call from a context that can no longer await, by
 * handing the work to a short-lived child. process.execPath is no use as that
 * child: under OpenCode it is the opencode binary, and running it re-enters the
 * editor rather than making a request.
 */
function syncPost(url, hdrs, body) {
  const argv = (bin) =>
    bin === "curl"
      ? [
          "-sS", "-m", "1", "-o", "/dev/null", "-X", "POST", url,
          ...Object.entries(hdrs).flatMap(([k, v]) => ["-H", `${k}: ${v}`]),
          "--data-binary", body,
        ]
      : [
          "-e",
          `fetch(${JSON.stringify(url)},{method:"POST",headers:${JSON.stringify(hdrs)},` +
            `body:${JSON.stringify(body)}}).then(()=>process.exit(0),()=>process.exit(1))`,
        ];

  // The first tool that is present is the only one tried. Falling through on a
  // failed post would mean spending a second timeout per remaining tool on the
  // ordinary case of the daemon having already stopped, and that time is paid
  // watching an editor that will not close.
  for (const bin of ["curl", "bun", "node"]) {
    const r = spawnSync(bin, argv(bin), { timeout: 1500, stdio: "ignore" });
    if (r.error && r.error.code === "ENOENT") continue;
    return !r.error && r.status === 0;
  }
  return false;
}

/**
 * closeOnExit tells the daemon that every session this process opened is over,
 * at the moment the process goes away.
 *
 * OpenCode emits session.deleted only when a session is explicitly discarded,
 * which `opencode run` never does and a user quitting the TUI rarely does. Left
 * to that event alone the daemon is told a session began and never that it
 * ended, and a daemon whose last session never ends never stops.
 *
 * It has to be "exit" rather than "beforeExit": OpenCode terminates by calling
 * exit itself, which skips beforeExit entirely.
 */
function closeOnExit(live, cwd) {
  process.on("exit", () => {
    for (const id of live) {
      syncPost(`${BASE}/v1/events`, headers(), JSON.stringify({ session_id: id, event: "session_end", cwd }));
    }
    live.clear();
  });
}

export const ShoulderDaemon = async ({ directory, worktree }) => {
  const cwd = worktree || directory || process.cwd();

  // Not awaited: plugin load is on OpenCode's startup path, and a daemon that is
  // slow to bind must not be felt as an editor that is slow to open.
  ensureDaemon().catch(() => {});

  // Advice is fetched when the user speaks and injected when the request is
  // built, so the hook on the request path never touches the network.
  const pending = new Map(); // sessionID -> advice text
  const assistant = new Map(); // sessionID -> { text, reasoning }
  const live = new Set(); // sessions opened here and not yet reported as over
  closeOnExit(live, cwd);

  const turn = (id) => {
    if (!assistant.has(id)) assistant.set(id, { text: "", reasoning: "" });
    return assistant.get(id);
  };

  return {
    "chat.message": async ({ sessionID }, output) => {
      try {
        watchReadiness();
        const parts = (output && output.parts) || [];
        const prompt = parts
          .filter((p) => p && p.type === "text" && typeof p.text === "string")
          .map((p) => p.text)
          .join("\n");
        const advice = await post({
          session_id: sessionID,
          event: "user_prompt",
          cwd,
          prompt,
        });
        if (advice && advice.text) pending.set(sessionID, advice.text);
      } catch {
        /* never rethrow: this hook runs inside the user's turn */
      }
    },

    "experimental.chat.system.transform": async ({ sessionID }, output) => {
      try {
        const text = sessionID && pending.get(sessionID);
        if (!text) return;
        pending.delete(sessionID);
        // Pushed rather than assigned, and re-evaluated on every request, which
        // is what makes this the one injection point that survives compaction.
        output.system.push(text);
      } catch {
        /* never rethrow */
      }
    },

    "tool.execute.before": async ({ tool, sessionID, callID }, output) => {
      try {
        send({
          session_id: sessionID,
          event: "tool_call",
          cwd,
          tool_name: tool,
          tool_use_id: callID,
          tool_input: (output && output.args) ?? undefined,
        });
      } catch {
        /* never rethrow */
      }
    },

    "tool.execute.after": async ({ tool, sessionID, callID }, output) => {
      try {
        send({
          session_id: sessionID,
          event: "tool_result",
          cwd,
          tool_name: tool,
          tool_use_id: callID,
          tool_result: (output && output.output) || "",
        });
      } catch {
        /* never rethrow */
      }
    },

    event: async ({ event }) => {
      try {
        if (!event || !event.type) return;
        const p = event.properties || {};

        // Reasoning is accumulated as it streams because OpenCode is the only
        // harness that gives us any: Claude Code persists thinking blocks with
        // an empty body, so this field is dead on that path and live on this one.
        if (event.type === "message.part.updated" && p.part) {
          const part = p.part;
          const id = part.sessionID;
          if (!id) return;
          if (part.type === "text" && part.text) turn(id).text = part.text;
          if (part.type === "reasoning" && part.text) turn(id).reasoning = part.text;
          return;
        }

        if (event.type === "session.idle" && p.sessionID) {
          live.add(p.sessionID);
          const t = turn(p.sessionID);
          assistant.delete(p.sessionID);
          send({
            session_id: p.sessionID,
            event: "turn_end",
            cwd,
            assistant: t.text,
            thinking: t.reasoning,
          });
          return;
        }

        if (event.type === "session.created" && p.info && p.info.id) {
          live.add(p.info.id);
          send({ session_id: p.info.id, event: "session_start", cwd });
          return;
        }

        if (event.type === "session.compacted" && p.sessionID) {
          send({ session_id: p.sessionID, event: "compact", cwd });
          return;
        }

        if (event.type === "session.deleted" && p.info && p.info.id) {
          pending.delete(p.info.id);
          assistant.delete(p.info.id);
          live.delete(p.info.id);
          send({ session_id: p.info.id, event: "session_end", cwd });
        }
      } catch {
        /* never rethrow */
      }
    },
  };
};

export default ShoulderDaemon;
