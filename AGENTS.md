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
  internal/server/          HTTP, SSE, embedded gallery, element lookup for annotations
  internal/icon/            project logos for the gallery sidebar
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
vision snap <key> [--as <variant> | --dim k=v...] [--selector <css>] [--meta k=v...] [--note <text>] [--json]
vision add <key> <file> [--as <variant>] [--meta k=v...] [--note <text>] [--json]
vision discard <key> [--as <variant>] [--json]
vision notes [--unread | --since <duration> | --open | --wait <duration>] [--flagged] [--session <id>] [--json]
vision session join <thread> | vision session thread [--json]
vision on | off | status [--session <id>] [--json]
```

Capture, read back, and run the daemon are the core. `session` exists because a handoff mints
a new session id and the verdicts have to follow the work, not the process. `discard` exists
because agents abandon keys: it withdraws only the caller's own never-judged shots, into its
own ledger, and never approves or moves a baseline. `add` exists because a recording is not
a capture vision can take: PinchTab writes the file (`pinchtab record`) and vision only queues
it. It takes mp4 or webm, recognised by their bytes, not their extension, and refuses a PNG,
because a still has to come from `snap` so its capture conditions exist for the guard. Any
further command needs to justify why
it is not the agent's job or the human's.

`notes --wait <duration>` is how a verdict reaches a running agent: the agent backgrounds it,
and its exit wakes the session. It shares the unread cursor with `--unread --session` and the
notes hook, so whichever reads first delivers and nothing arrives twice. vision never injects
keystrokes into a session.

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
ratio, page URL, color scheme, and the selector of an element shot. Before diffing against a
baseline, compare the ones that change what a pixel means, and if any differ, **refuse the
diff**: width, device pixel ratio, scheme (an unknown scheme never matches), and selector.

```text
baseline and this shot differ in viewport width 375 vs 390. Not comparable.
```

A recording added with `vision add` carries no conditions and is never pixel diffed, because
two recordings never line up frame for frame. A byte-identical recording still never reaches
the human; any other one is `changed`, and the human compares it by holding `b` to play the
approved recording at the same moment.

Height and URL are recorded but do not refuse. A taller page or a different query string is
a change the human should see as a diff, padded top-left and labelled with what drifted.

A fake diff caused by a resize is worse than no diff, because it teaches the human to click
through changes without looking. Never render one.

## Storage

Global, XDG, never inside the repository. vision creates no directory in the project and never
edits `.gitignore`. Tests use `VISION_STATE_HOME`.

```text
$VISION_STATE_HOME/                       # else $XDG_DATA_HOME/vision, else ~/.local/share/vision
  projects/<project-id>/
    base/<feature>/<slug>@<variant>.png    # current baseline: the last shot marked ok (.mp4, .webm for recordings)
    shots/<sha256>.png                     # content-addressed, written once, never mutated, pruned (.mp4, .webm too)
    shots/<sha256>.json                    # element boxes captured with that shot, pruned with it
    index.jsonl                            # one record per snap, append-only
    notes.jsonl                            # one record per human verdict, append-only
    withdrawn.jsonl                        # one record per agent withdrawal, append-only
    cursor.json                            # project-wide read cursor for `notes --unread`
    cursors/, sessions/                    # per-thread cursors and session-to-thread joins
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

A verdict with annotations also carries `regions`, one per mark the human drew, in image
pixels, with the element the daemon found under it; `note` then holds one rendered line per
mark, so readers that only know `note` still get the whole review:

```json
{"...":"...","verdict":"flag","note":"[1] issue on button \"Withdraw\": too close to the edge",
 "regions":[{"x":210,"y":280,"w":0,"h":0,"tone":"issue","text":"too close to the edge",
 "element":"button \"Withdraw\""}]}
```

On a recording, each region also carries `t`, the second of the video it was placed at, and
the rendered line shows it: `[1] issue 0:01.3 at 40,300: box should be red`. The snap record
carries `media` (`video/mp4` or `video/webm`); absent means a PNG.

`withdrawn.jsonl`, one line per shot an agent withdrew with `vision discard`. It is a separate
file so `notes.jsonl` stays human verdicts only and nothing that reads notes has to filter.

All three are append-only. A review is one key and variant at one digest: identical pixels
under two keys are two reviews. A review is pending until it is decided by a note, withdrawn,
or superseded by a newer snap of the same key and variant on the same thread; all three are
derived by folding the ledgers over the index, and nothing rewrites a line of any of them.
Old shot files are pruned once no review needs them, which is a disk decision and never a
record decision: see Storage.

## The daemon

One machine-wide daemon serving every project, not one per repository. That is what makes a fixed
port safe and a bookmark stable.

- Binds `127.0.0.1:4747` only. There is no authentication, so binding any other interface would
  publish an unauthenticated write endpoint. Do not add a `--host` flag.
- Reached at `http://vision.test:4747` through a hosts entry. Never `.local`, which is Bonjour on
  macOS and causes multi-second resolution stalls.
- Managed by launchd on macOS and by a systemd user unit on Linux. `vision on`, `off`, and
  `status` mirror the `model` command already on the box. The supervisor is the only
  platform-specific code, split across `supervisor_darwin.go`, `supervisor_linux.go` and a
  `supervisor_other.go` that errors with the platform name.
- A systemd `--user` unit dies with the last session unless lingering is on, so `vision on`
  checks `loginctl` and, when it is off, starts anyway and prints the `enable-linger` line
  rather than reporting a daemon that is about to vanish. It never enables lingering itself:
  that is a system-wide setting, and a command that reads as "start my thing" must not change
  one silently.
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

Annotations ride on those two verdicts, the way a GitHub review batches its comments into one
submit. A click drops a pin, a drag draws a box, and each mark carries a tone (issue, praise,
question) and text. With marks on the card, `o` reads "approve with notes" and `f` "request
changes", and any issue suggests `f`. A note on one step of a flow can be carried to every step
as text, never re-pinned, because the same element sits somewhere else on each step. Marks live
in the page until the verdict is sent; there is no draft store.

Compare is one key per view, never a cycle: hold `b` to see the baseline where the shot is,
`s` and `d` toggle swipe and diff, and pressing either again returns to the new shot. A
recording plays in the same frame with a control bar under the picture, never over it, so
every pixel stays pinnable. Space plays and pauses, `,` and `.` step a tenth of a second, a
pin is stamped with the moment it was placed and only shows near that moment, and clicking
a note seeks there.

When a scoped view runs out, the gallery stays on it and says where shots still wait (the
rest of the same project first, else the busiest project), with `n` to go there. It never
redirects: an agent's link landing on an empty view is the proof that its work was seen.

The queue can be scoped by route: `/p/<project>`, `/p/<project>/b/<branch>`, and
`/p/<project>/t/<thread>`, with a sidebar listing projects and their pending counts. That is a
filter over the pending queue, not a history: it shows nothing that has been judged. Project
logos come from `$XDG_CONFIG_HOME/vision/projects.json` (default `~/.config`) (`{"projects":{"<name>":{"logo":"<path>"}}}`)
when set, else from icon files the repository already has, else a letter.

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
   `index.jsonl`, `notes.jsonl` or `withdrawn.jsonl`, ever. Shot files are immutable but
   reclaimable, pruned only once no pending review needs them. No command approves on the
   human's behalf or hides a judged shot. `discard` withdraws only the caller's own
   never-judged shots, and the withdrawal is itself a permanent record, so the queue gets
   shorter without anything being erased.
4. Unchanged shots never reach the human. Changed shots always do.
5. A diff is never rendered across mismatched width, device pixel ratio, scheme, or
   selector, and never between two recordings.
6. State is global, XDG, and project-keyed. Linked worktrees share a project. No repo-local state
   exists.
7. The daemon is loopback-only and unauthenticated by design.
8. JSON schemas are versioned. Human output is never the integration protocol.
9. Standard library only.
