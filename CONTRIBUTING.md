# Contributing to vision

Read [AGENTS.md](AGENTS.md) first. Changes must serve vision's single purpose: taking a picture
of what was built, keeping it honestly, and putting it in front of a human.

The Go module is in `cli/`, standard library only. Keep the camera a camera: no navigation, no
click scripting, no data seeding, no authentication, no dev-server lifecycle, and no model
verdict stored as approval. Those belong to the agent driving the browser, and every one of them
that lands here makes vision worse at the one thing it does.

Before completion run:

```bash
cd cli
gofmt -l .
go vet ./...
go test ./... -count=1
cd ..
git diff --check
```

Add focused tests for new behavior. Do not add repo-local state or `vision` entries to
`.gitignore`; tests use `VISION_STATE_HOME`.

Two rules that are not negotiable, because they are the point: nothing mutates or deletes a shot,
a note, or a baseline, and no approve-all shortcut is ever added to the gallery.
