---
name: vision
description: Photograph UI you built so a human can actually look at it. Use whenever you add or change a screen, component, widget, modal, empty state, error state or flow and nobody has seen it rendered; when asked to screenshot, capture, show, or get eyes on a UI change; and before calling any visual work done. Also use to read back the human's approvals and complaints with `vision notes --unread`.
---

# vision

You ship UI that nobody looks at. The human trusts the code and never walks the page, so a
clipped button or a modal that renders behind the overlay reaches production unseen.

vision fixes the looking, not the building. You drive the browser into the state worth seeing,
press the shutter, and keep working. The human reviews a queue on their own clock, and their
verdicts come back to you as text.

## Take the picture

Get the page into the exact state first, with PinchTab or whatever else you use. vision never
navigates, never clicks, never seeds, never logs in. Then:

```bash
vision snap checkout/empty-cart --as mobile-dark
```

That is the whole surface for capture. The shot is filed, compared against what the human last
approved, and pushed live to the gallery. If it is byte-identical to the approved baseline it is
archived silently and never bothers anyone.

Snap after the state is settled, not while something is still loading. `pinchtab capture` waits
for stability; do not race it.

## Keys and variants

A key is `<feature>/<slug>`, the same shape as a blueprint requirement slug, so a screenshot and
the sentence it is meant to satisfy can find each other. Use the real slug when the repo has one.

A `#n` suffix marks a step in a flow, and the gallery lays those out as a filmstrip:

```bash
vision snap checkout/happy-path#1 --as desktop-light   # cart
vision snap checkout/happy-path#2 --as desktop-light   # payment
vision snap checkout/happy-path#3 --as desktop-light   # confirmation
```

`--as` labels the condition you chose: `mobile-dark`, `desktop-light`, `loading`, `error`,
`long-content`. Each key and variant pair carries its own baseline.

**Keep a variant label meaning the same thing every time.** If `mobile-dark` is 375x812 today and
390x844 tomorrow, every diff under that label is noise, the human stops trusting the queue, and
the tool dies. vision refuses to diff across mismatched conditions and will tell you when you
have drifted.

**One capture size per `vp` bucket, and these are the sizes.** `vp=mobile` is every width under
600, so a 360 shot and a 430 shot encode to the same variant, claim the same baseline, and then
refuse to diff against each other. Pick the size from this table and do not improvise a new one:

| `vp` | capture | why this width |
|---|---|---|
| `mobile` | 360x700 | narrowest width still in real use, and the common Android one. Every current iPhone is 375 to 440, so a layout that survives 360 survives all of them. Breaks come from too narrow, never too wide. |
| `tablet` | 820x700 | iPad Air and iPad Pro 11 portrait, the middle of the 744 to 834 tablet cluster. |
| `desktop` | 1440x700 | common laptop. Avoid 1024: it is an iPad Pro 12.9 portrait, an iPad landscape, and Tailwind's `lg` edge all at once, so it sits exactly where layouts change. |

Two ceilings on that table, both worth knowing before you blame the page:

The height is 700 because the emulated viewport must stay under the real browser window height,
around 721 CSS px on a laptop. Go taller and the bottom of the capture comes back unpainted
white. So a phone shot shows the top 700 px of the page, not a real phone's 844. Anything below
that fold is unreviewed, and shooting it means a second key, not a taller viewport.

Shooting two phone widths needs a second axis, not a second width. `--dim vp=mobile` cannot tell
them apart, so use `--dim case=narrow` alongside it or the two shots will fight over one baseline.

Shoot the states that actually break, not just the happy one: empty, loading, error, and content
long enough to overflow. Those are where generated UI fails, and a single desktop screenshot of
the ideal case hides all of them.

## Read the verdicts

```bash
vision notes --unread
```

Returns what the human said since you last looked, and advances the cursor. `ok` means it stands
and is now the baseline. `flag` always carries a note explaining what is wrong. Fix flagged items
before moving on, and re-snap the same key so the queue closes.

## Rules

1. Never block a turn waiting for the human to review. Snap and keep going.
2. Never mark your own work visually approved. The only thing that approves a shot is a human
   pressing ok in the gallery. Do not infer approval from silence.
3. Never delete or rewrite shots, notes, or baselines to make a queue look clean.
4. If `vision snap` reports no daemon, say so and name `vision on`. Do not silently skip the
   capture and claim the UI was checked.
5. A screenshot proves a render happened, not that the interface is good. Do not cite one as
   evidence that a requirement is satisfied.
