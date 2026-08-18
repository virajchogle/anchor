# Images

`protocol.svg` and `comparison.svg` are generated and committed; they render on
GitHub in both light and dark themes.

Two screenshots are referenced by the top-level README and need to be taken from
the running panel, because they show live data rather than a diagram:

| File | What to capture |
|---|---|
| `screenshot-live.png` | The **Live run** page mid-incident. The best moment is just after `go run ./cmd/demo -crash`, while the Executed stage is red and the banner reads "action in flight, unrecorded". |
| `screenshot-recall.png` | The **Recall search** page after searching a symptom, showing the similarity, recency and salience bars. |

Capture at a browser width of roughly 1400px so the layout is the wide one, and
save as PNG into this directory with exactly those names. The README already
points at them.
