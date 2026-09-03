## What this changes

<!-- The intent, not the mechanism. One coherent change per pull request. -->

## Why

<!-- The problem it solves. Link the issue if there is one. -->

## Checks

- [ ] `make lint` clean
- [ ] `make test` passing
- [ ] `make bench` run, and the hook round trip did not move (or the change explains why it did)
- [ ] Tests cover the new behaviour
- [ ] No new dependency outside the standard library, or the reason for one is argued above
- [ ] Nothing new on the hook path that does network or synchronous disk I/O

## Anything not covered by CI

<!-- A real harness, a live provider, a live memory backend - say what you ran and what
     you did not. The integration suite and the live tests do not run in CI. -->
