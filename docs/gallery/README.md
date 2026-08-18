# Devpost gallery

Seven generated cards at 1200x800 (3:2), all well under the 5 MB limit. Regenerate
them by re-running the generator in the project history; they are committed so the
submission does not depend on a local toolchain.

Suggested upload order, since Devpost shows the first image as the project
thumbnail:

| # | File | Says |
|---|---|---|
| 1 | `01-pitch.png` | The hook. Use as the thumbnail. |
| 2 | `02-problem.png` | Why retrying cannot work |
| 3 | `03-protocol.png` | The three phases and the crash path |
| 4 | `04-headtohead.png` | 1 operation versus 2, measured |
| 5 | `05-verdicts.png` | The third answer most systems lack |
| 6 | `06-memory.png` | Memory compounding into a playbook |
| 7 | `07-stack.png` | What each piece is actually doing |

## Screenshots to add

Take these from the running panel and slot them in at positions 4 and 6, since
live screens are more convincing than diagrams:

| Capture | Moment |
|---|---|
| Live run, mid-crash | Just after `go run ./cmd/demo -crash`, while Executed is red and the banner reads "action in flight, unrecorded" |
| Live run, escalated | After `-escalate` then `-reconcile`, Verified in amber with the decision box |
| Recall search | After a query, showing the similarity, recency and salience bars |
| Head to head | The comparison page with its recorded measurement |

Browser at roughly 1400px wide so the layout is the wide one. Crop to 3:2 if you
want them to sit flush with the generated cards.
