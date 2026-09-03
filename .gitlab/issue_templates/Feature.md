/label ~enhancement

## What is the problem

The situation you are in, not the solution you have in mind. What did the agent fail to
know, or know wrongly?

## What kind of change is this

<!-- A model connector, a memory backend, a harness adapter, a change to what gets
     stored/superseded/injected, or something else. -->

## What you would like it to do

If this is a connector or a backend, name the endpoint or the product and say what it
costs to call. Latency decides most of these: the decision pass runs while the user's turn
is open, so a flash-tier model beats a better one that thinks for twenty seconds.

## Dependency budget

Neither Go module has a dependency outside the standard library. If this needs one, say
which and why it cannot sit behind an existing interface.
