# Security policy

## Reporting a vulnerability

Email **thomas@lumea-technologies.com** with `shoulder-daemon` in the subject. Please do
not open a public issue, and do not post a working exploit to either tracker.

Include the version or commit, the platform, and the smallest thing that reproduces the
problem. You will get an acknowledgement within 72 hours and an assessment within a week.
Fixes land on `main` on both remotes at the same time, and the advisory credits you unless
you ask otherwise.

## Supported versions

`main` is the only supported branch. There are no maintenance branches; fixes are released
forward.

## What is in scope

shoulder-daemon runs as a local daemon that reads the contents of your coding sessions and
holds them in a memory backend. The parts that matter most:

- **The `127.0.0.1:8787` listener.** It is authenticated by `SHOULDER_TOKEN` and nothing
  else. Any way to reach it without the token, or to make it act on a request that failed
  authentication, is in scope. Running without a token configured is a documented
  misconfiguration, not a vulnerability - see section 1 of [docs/INSTALL.md](docs/INSTALL.md).
- **The hook path.** A harness hook that can be made to hang, crash the daemon, or block
  the user's turn is in scope. The relay is designed to fail open.
- **Redaction.** `relay/internal/sanitize` exists to keep secrets out of what is stored
  and out of what is sent to a model provider. Anything it lets through - a credential,
  a token, a key - is in scope.
- **Stored facts crossing a boundary.** A local fact reaching a different project, or a
  project fact reaching the global scope, is in scope.
- **The adapters.** `adapters/` and `scripts/install-plugins.sh` write into your editor's
  configuration. Anything there that escalates beyond what the installer is meant to touch
  is in scope.

## What is not

- Sending your session content to whichever model provider you configured. That is the
  purpose of the tool; choose a local connector if you do not want it.
- Vulnerabilities in a model provider, a memory backend, or a coding harness. Report those
  upstream.
- Anything that requires an attacker who already runs code as your user.
