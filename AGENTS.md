# vision: the camera contract

> You drive the browser. vision takes the picture, keeps the film, and shows the human.

This file is for agents working **on** vision. The contract for agents **using** vision from
another repo is `skills/vision/SKILL.md`, and it is deliberately shorter.

## One purpose

vision exists because agents ship UI nobody looks at. It is a camera, an honest ledger, and a
gallery. Every surviving feature must take a picture, keep it honestly, or put it in front of a
human.

It is not a browser driver, a test runner, a dev server, a seeder, an authentication manager, or
a judge of whether an interface is good. The agent puts the page into the state worth
photographing and presses the shutter. Anything that navigates, scripts, seeds, logs in, or
asserts correctness does not belong in this repository, no matter how convenient it would be.

## Layout

```text
cli/                        Go module, module name `vision`
  cmd/vision/               main, flag parsing, exit codes
  internal/store/           paths, project identity, index and notes records
  internal/diff/            content hashing, pixel compare, conditions guard
  internal/capture/         pinchtab shell-out and response parsing
  internal/server/          HTTP, SSE, embedded gallery
  internal/gallery/         gallery.html and assets, served via go:embed
skills/vision/SKILL.md      how another agent uses vision
docs/principles.md          the boundaries, and why they hold
```

Go 1.26. No third-party dependencies. Standard library only, including the pixel diff on
`image/png`. A dependency in this repository needs a reason that survives the question "what
would twenty lines of `image` package cost instead".

## Build and test

```bash
cd cli
gofmt -l .            # must print nothing
go vet ./...
go build ./...
go test ./... -count=1
```

Tests must cover project identity and key parsing, the content hash and pixel diff, the
conditions guard, and the notes cursor. The gallery is checked by a human looking at it; do not
add a browser-driving test suite to a tool whose entire premise is that browser automation
belongs somewhere else.

## Surface

```bash
vision snap <key> [--as <variant>] [--note <text>] [--json]
vision notes [--unread | --since <duration>] [--json]
vision on | off | status
```

Three commands. A fourth needs to justify why it is not the agent's job or the human's.

There is no `bless`: pressing ok in the gallery is the only thing that moves a baseline. There is
no manifest: the agent already knows what it changed. There is no `--url`: the page is wherever
the agent already put it.

Every command takes `--json`. Human output is never the integration protocol.

## Keys and variants

A key is `<feature>/<slug>`, deliberately the same shape as a blueprint requirement slug, so a
screenshot and the sentence it is supposed to satisfy can find each other.

A `#n` suffix marks a step in a flow: `checkout/happy-path#1`. The gallery sorts on the number
and renders those as a filmstrip. That is the only place in the codebase that knows flows exist;
the store treats `#n` as part of the key and nothing more.

`--as <variant>` is a freeform label, defaulting to `default`. Each key and variant pair has an
independent baseline. When a key sees a variant it has never seen, warn rather than silently
starting a second baseline, because a typo would otherwise fork history where nobody notices.

## The conditions guard

The one piece of intelligence vision keeps, and it exists because only vision can see it.

Every snap records the conditions PinchTab reports: viewport width and height, device pixel
ratio, page URL, and color scheme. Before diffing against a baseline, compare them. If they
differ, **refuse the diff**:

```text
checkout/empty-cart@mobile-dark: baseline 375x812 @2x, this shot 390x844 @2x. Not comparable.
```

A fake diff caused by a resize is worse than no diff, because it teaches the human to click
through changes without looking. Never render one.

## Storage

Global, XDG, never inside the repository. vision creates no directory in the project and never
edits `.gitignore`. Tests use `VISION_STATE_HOME`.

```text
$VISION_STATE_HOME/                       # else $XDG_DATA_HOME/vision, else ~/.local/share/vision
  projects/<project-id>/
    base/<feature>/<slug>@<variant>.png    # current baseline: the last shot marked ok
    shots/<sha256>.png                     # content-addressed, written once, never mutated, pruned
    index.jsonl                            # one record per snap, append-only
    notes.jsonl                            # one record per verdict, append-only
    cursor.json                            # read cursor for `notes --unread`
```

Project identity is the SHA-256 of the canonical Git remote URL plus the repository root path,
taking the same approach mint does. **Linked worktrees of one repository are one project.** A
SlayZone task worktree must not fork a fresh gallery with no baselines; that would fail on
exactly the branch where UI is being changed.

Branch, worktree path, commit sha, dirty-tree state, and session id are fields on each snap
record, never directory levels. Baselines are per key and variant, and branch-agnostic.

**The ledger is permanent, the film is not.** `index.jsonl` and `notes.jsonl` are text, they
are tiny, and they are never deleted or rewritten. Shot PNGs are ~100KB each and are pruned
when a verdict lands: what survives is everything still pending, the current baseline, and
the two shots behind it per key and variant. Nothing else is worth the disk.

This is what makes the append-only rule affordable rather than a slow leak. A pruned shot
still has its index record and its note, so nobody can quietly erase that a shot was taken
or that it was flagged. Only the picture is reclaimed, never the fact that it existed.

```
ponytail: baselines are branch-agnostic, so parallel worktrees touching the same component
diff against each other's approvals. Per-branch baselines with base-branch fallback is the
correct fix; it roughly triples the state logic and is not worth it until it bites.
```

## Records

`index.jsonl`, one line per snap:

```json
{"schemaVersion":1,"ts":"2026-08-10T14:22:03Z","key":"checkout/empty-cart",
 "variant":"mobile-dark","digest":"sha256:...","branch":"feat/checkout","sha":"4f2a1c9",
 "dirty":true,"worktree":"/Users/x/dev/shop","session":"ae81a5f3",
 "conditions":{"width":375,"height":812,"dpr":2,"url":"http://localhost:3000/checkout",
 "scheme":"dark"}}
```

`notes.jsonl`, one line per verdict:

```json
{"schemaVersion":1,"ts":"2026-08-10T14:31:11Z","key":"checkout/empty-cart",
 "variant":"mobile-dark","digest":"sha256:...","verdict":"flag",
 "note":"CTA under the fold on 375"}
```

Both are append-only. A verdict never rewrites a snap record; pending status is derived by
folding notes over index. Nothing mutates or deletes a line of either file, nothing mutates a
shot, and no command exists to make a queue look clean. Old shot files are pruned once judged,
which is a disk decision and never a record decision: see Storage.

## The daemon

One machine-wide daemon serving every project, not one per repository. That is what makes a fixed
port safe and a bookmark stable.

- Binds `127.0.0.1:4747` only. There is no authentication, so binding any other interface would
  publish an unauthenticated write endpoint. Do not add a `--host` flag.
- Reached at `http://vision.test:4747` through a hosts entry. Never `.local`, which is Bonjour on
  macOS and causes multi-second resolution stalls.
- Managed by launchd on macOS. `vision on`, `off`, and `status` mirror the `model` command
  already on the box.
- `vision snap` POSTs to the daemon. If nothing answers it fails loudly and names `vision on`. It
  never drops a shot silently, and it never starts a server as a side effect of a capture.

The gallery is one HTML file with vanilla JS, served from `go:embed`. No npm, no build step. If
it ever needs a bundler, the design went wrong. Live updates are Server-Sent Events, so shots
appear while a capture burst is still running.

## Gallery behavior

The default view is a single cross-project queue of everything pending, one card at a time,
keyboard driven. Project, branch, and key are a breadcrumb on the card, not a path the human had
to navigate to reach it.

Two verdicts. `ok` accepts the shot and makes it the new baseline, with an optional note. `flag`
rejects it and **requires** a note; a flag without text is not a valid state. Identical shots
never enter the queue at all.

There is no approve-all button, and there must never be one. A queue that can be cleared without
looking manufactures false confidence, which is worse than the original problem.

There is no archive browser, and that is deliberate. Browsing a key's history over time is the
feature you build and never open: the queue is what a human actually opens, and compare already
shows this shot against the approved one, which is the only comparison anyone asks for. Nobody
asks what happened in run seven. It also cannot coexist with pruning, because the history it
would browse is exactly what pruning reclaims. Bounded disk beats a screen nobody visits.

## Invariants

1. vision never drives a browser. It shells out to PinchTab for the capture, plus one
   read-only eval of the color scheme, which the capture response does not carry, and
   nothing else. Navigating, clicking, seeding, and logging in stay the agent's job.
2. A human pressing ok is the only thing that approves a shot. No model verdict is ever stored as
   approval.
3. Records are append-only and immutable: nothing rewrites or deletes a line of
   `index.jsonl` or `notes.jsonl`, ever. Shot files are immutable but reclaimable, pruned
   only once a verdict has been recorded for them. No command exists that makes a queue
   look clean, and pruning cannot, because it never takes a pending shot.
4. Unchanged shots never reach the human. Changed shots always do.
5. A diff is never rendered across mismatched capture conditions.
6. State is global, XDG, and project-keyed. Linked worktrees share a project. No repo-local state
   exists.
7. The daemon is loopback-only and unauthenticated by design.
8. JSON schemas are versioned. Human output is never the integration protocol.
9. Standard library only.
