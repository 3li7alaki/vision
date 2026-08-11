# vision principles

## One purpose

Agents ship interfaces nobody looks at. vision takes the picture, keeps the film, and puts it in
front of a human.

Every surviving feature must capture a rendered state, preserve it honestly, or reduce the effort
between a change landing and a human seeing it.

## Ownership

The agent owns getting there: navigation, clicks, forms, authentication, seeded data, feature
flags, viewport, and color scheme. It decides what is worth photographing and when the page has
settled.

vision owns the film: content-addressed shots, capture conditions, baselines, diffing, the
append-only record, the review queue, and the verdicts coming back out.

The human owns the verdict. Nothing else does.

## Invariants

1. vision never drives a browser. PinchTab does, and vision shells out to it for the capture.
2. A human pressing ok is the only approval that exists. No model verdict is stored as one.
3. Shots, notes, and baselines are append-only and immutable. No command makes a queue look
   clean.
4. Unchanged shots never reach the human. Changed shots always do.
5. A diff is never rendered across mismatched capture conditions. A fake diff is worse than none,
   because it teaches the reviewer to stop looking.
6. There is no approve-all. A queue that can be cleared without looking manufactures false
   confidence, which is the problem vision exists to solve.
7. State is global, XDG, and project-keyed. Linked worktrees of one repository share a project.
8. The daemon is loopback-only and unauthenticated by design, so it must never bind anything
   else.
9. JSON schemas are versioned. Human output is never the integration protocol.

## Why the camera stays a camera

Every feature that would make vision navigate, script a flow, seed a database, or log in is a
feature the agent can already do better, with full knowledge of the application, and vision
cannot do at all without inventing a second browser automation tool inside a screenshot tool.

The same holds at the other end. A model can caption a screenshot and guess whether it looks
wrong, and that guess is useful for sorting a queue. It is not an approval, and storing it as one
would produce exactly the green checkmark with nothing behind it that made this tool necessary.

## What vision is not

- Not a browser driver. That is [PinchTab](https://pinchtab.com).
- Not a decision about what is worth building. That is
  [blueprint](https://github.com/3li7alaki/blueprint).
- Not a completion verdict. That is [mint](https://github.com/3li7alaki/mint). vision produces
  evidence and certifies nothing; it is an instrument, on the same shelf as a test runner.
- Not proof that an interface is good. A screenshot proves a render happened. A human deciding it
  is fine is a different claim, and only they can make it.
