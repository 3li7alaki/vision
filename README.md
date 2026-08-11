# vision

> You drive the browser. vision takes the picture, keeps the film, and shows the human.

[![License: MIT](https://img.shields.io/badge/license-MIT-black)](LICENSE)
[![Site](https://img.shields.io/badge/site-vision.area--51.cloud-4338ca)](https://vision.area-51.cloud)
[![Ko-fi](https://img.shields.io/badge/Ko--fi-support-ff5e5b?logo=kofi&logoColor=white)](https://ko-fi.com/3li7alaki)

Your agent writes a screen, says it looks good, and commits. Nobody opened it. The code compiles,
the tests pass, and the primary button is sitting under the fold on a phone. You find out when a
user tells you, or you never find out.

The fix is not more automation. It is one human glance at the right moment, and the reason that
glance never happens is friction: start the app, find the route, log in, get the data into the
right shape, resize, look. vision removes every step except looking.

## Install

```bash
curl -fsSL https://raw.githubusercontent.com/3li7alaki/vision/main/install.sh | sh
vision on
```

One self-contained binary, no runtime dependencies, no root. Re-run the same line to update.
Capture is done through [PinchTab](https://pinchtab.com), which vision expects on your PATH.

## What a run looks like

The agent already has the browser open, so it puts the page in the state it wants and presses the
shutter:

```bash
pinchtab nav http://localhost:3000/checkout
pinchtab click "Empty the cart"
vision snap checkout/empty-cart --as mobile-dark
```

```text
✓ checkout/empty-cart@mobile-dark  changed  →  http://vision.test:4747
```

Then it keeps working. Nothing blocks.

You open that URL whenever you feel like it and get one card at a time, only for shots that
actually changed since you last approved something:

```text
  shop · feat/checkout · checkout/empty-cart @mobile-dark            3 of 7

        [ the screenshot ]

  [ok]  [flag]                                              j/k to move
```

`ok` accepts it and makes it the new baseline. `flag` needs a note. Later, in the session, the
agent reads what you said:

```bash
$ vision notes --unread
checkout/empty-cart @mobile-dark   flag   "CTA under the fold on 375"
settings/danger-zone @desktop      ok     "fine, but the spacing is tight"
```

It fixes the first, re-snaps, and the card closes.

## Commands

```bash
vision snap <key> [--as <variant>] [--note <text>]   take the picture
vision notes [--unread | --since <duration>]         read the verdicts
vision on | off | status                             the daemon
```

Three commands, and that is the whole tool. A key is `<feature>/<slug>`, the same shape as a
[blueprint](https://github.com/3li7alaki/blueprint) requirement slug. A `#n` suffix marks a step
in a flow and the gallery lays those out as a filmstrip.

## The rules it will not bend

- **Unchanged shots never reach you.** Only what actually moved since your last approval enters
  the queue, so the normal case is three cards, not forty.
- **There is no approve-all button.** A queue you can clear without looking is a queue that
  manufactures false confidence, which is the problem this tool exists to solve.
- **A diff is never faked.** If the baseline was 375x812 and this shot is 390x844, vision says
  they are not comparable instead of showing you a screen of red pixels.
- **Nothing is ever deleted.** Shots, notes, and baselines are append-only. No command exists to
  make a queue look clean.
- **Only you approve.** No model verdict is stored as approval.

## What it is not

- Not a browser driver. That is [PinchTab](https://pinchtab.com), and vision shells out to it.
- Not a decision about what is worth building. That is
  [blueprint](https://github.com/3li7alaki/blueprint).
- Not a completion verdict. That is [mint](https://github.com/3li7alaki/mint). vision produces
  evidence and certifies nothing.
- Not a dev server, a seeder, or a login manager. The agent already knows how to reach the page.

## More

- [AGENTS.md](AGENTS.md) is the contract for agents working on vision.
- [skills/vision/SKILL.md](skills/vision/SKILL.md) is the contract for agents using it.
- [docs/principles.md](docs/principles.md) covers the product boundaries.
- [vision.area-51.cloud](https://vision.area-51.cloud) is the overview, with the rest of the lab
  at [area-51.cloud](https://area-51.cloud).

## Support

vision is free and MIT licensed, and there is no pricing page. If it caught one broken screen
before a user did, that is worth about the price of a coffee.

[![ko-fi](https://ko-fi.com/img/githubbutton_sm.svg)](https://ko-fi.com/3li7alaki)

Need something like this built for your own stack? Commissions are open at
[ko-fi.com/3li7alaki](https://ko-fi.com/3li7alaki).

## License

MIT. See [LICENSE](LICENSE).
