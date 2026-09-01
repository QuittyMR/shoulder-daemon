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

import { mkdirSync, rmdirSync } from "node:fs";
import { spawn } from "node:child_process";
import { tmpdir } from "node:os";
import { join } from "node:path";

const ADDR = process.env.SHOULDER_ADDR || "127.0.0.1:8787";
const BASE = `http://${ADDR}`;
const TOKEN = process.env.SHOULDER_TOKEN || "";

// Long enough for a loopback call to a relay that answers in microseconds,
// short enough that a wedged daemon is not something the user can feel.
const DEADLINE_MS = Number(process.env.SHOULDER_TIMEOUT_MS || 250);

const headers = () => {
  const h = { "Content-Type": "application/json" };
  if (TOKEN) h["X-Shoulder-Token"] = TOKEN;
  return h;
};

/** post sends one neutral event. Resolves to the advice, or null for anything at all. */
async function post(event) {
  try {
    const res = await fetch(`${BASE}/v1/events`, {
      method: "POST",
      headers: headers(),
      body: JSON.stringify({ protocol: 1, harness: "opencode", ...event }),
      signal: AbortSignal.timeout(DEADLINE_MS),
    });
    if (!res.ok) return null;
    const body = await res.json();
    return body && body.advice ? body.advice : null;
  } catch {
    return null;
  }
}

/** send fires an event we do not need an answer to, without making the turn wait. */
function send(event) {
  post(event).catch(() => {});
}

/** answering resolves true if something is already listening. */
async function answering() {
  try {
    const res = await fetch(`${BASE}/healthz`, { signal: AbortSignal.timeout(1000) });
    return res.ok;
  } catch {
    return false;
  }
}

/**
 * ensureDaemon starts the relay if nothing is serving.
 *
 * This runs once when the plugin loads, which is the only moment OpenCode gives
 * that is equivalent to a session-start hook. Several editors opening together
 * would each find nothing listening and each start one, so the winner is decided
 * by an atomic mkdir; the losers wait for the winner rather than racing to bind.
 *
 * It is never awaited by anything that matters and never throws.
 */
async function ensureDaemon() {
  if (await answering()) return;

  const lock = join(process.env.XDG_RUNTIME_DIR || tmpdir(), "shoulder-daemon.start.lock");
  try {
    mkdirSync(lock);
  } catch {
    // Somebody else is starting it. Give them a moment, then stop caring: a
    // daemon that never arrives costs this session its advice and nothing else.
    for (let i = 0; i < 10; i++) {
      await new Promise((r) => setTimeout(r, 300));
      if (await answering()) return;
    }
    return;
  }
  // A killed launch must not leave the lock behind and wedge every later start.
  setTimeout(() => {
    try {
      rmdirSync(lock);
    } catch {}
  }, 30_000).unref?.();

  try {
    const cmd = process.env.SHOULDER_START_CMD;
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
  }
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

  const turn = (id) => {
    if (!assistant.has(id)) assistant.set(id, { text: "", reasoning: "" });
    return assistant.get(id);
  };

  return {
    "chat.message": async ({ sessionID }, output) => {
      try {
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

        if (event.type === "session.compacted" && p.sessionID) {
          send({ session_id: p.sessionID, event: "compact", cwd });
          return;
        }

        if (event.type === "session.deleted" && p.info && p.info.id) {
          pending.delete(p.info.id);
          assistant.delete(p.info.id);
          send({ session_id: p.info.id, event: "session_end", cwd });
        }
      } catch {
        /* never rethrow */
      }
    },
  };
};

export default ShoulderDaemon;
