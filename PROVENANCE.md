# Provenance

## How the code was written

Written for SigmaM2M, a oneM2M platform, in Go, by the author of the
iec60870 module and from the same standards (read in licensed copies).
No code was ported or copied from another implementation. The simulated
stations were first written next to SigmaM2M's IEC 104 and IEC 101
integrations to test them without devices; the misbehaviour switches
were added from a review of real-device behaviour on 2026-10-06, when the
commands under `cmd/` were written. The simulators, their commands and
the iec60870 tests and examples that drive them moved out of the
iec60870 module into this one on 2026-10-11 (with `git mv`), so the
library does not depend on its simulators, and the lib60870 interop
tests (interop/) were added. For v0.2.0 (also 2026-10-11) the interop
tests were extended and the peer program grew; besides lib60870's
headers and examples, some of its source files were read to write the
peer (see iec60870's PROVENANCE.md). The one simulator fix of v0.2.0 came
from a failing interop run.

## Sources

| Area (files) | Followed |
|---|---|
| IEC 104 station (sim/iec104sim) | IEC 60870-5-104:2006 with Amendment 1:2016 (APCI, controlled-station side of Figures 17 and 36, time-tagged commands and their maximum delay); IEC 60870-5-101 clause 7 (ASDUs, command and station procedures, clause 7.4); IEC 62351-5 and IEC TS 60870-5-7 for the A-Profile, through iec60870's package sec |
| IEC 101 line (sim/iec101sim) | IEC 60870-5-101 clause 6 with IEC 60870-5-1 and -2 (FT1.2, balanced and unbalanced transmission, class 1 and 2 data, DFC, FC14) |
| Commands (cmd/) | none (flags are our own) |
| Interop peer (interop/peer) | lib60870-C v2.4.1's public API headers and example programs, read to write our peer program; lib60870 is built from its own sources in the test container |

No specification text is reproduced; comments cite clause, table and
figure numbers in our own words. Comments that cite `F-` numbers refer
to SigmaM2M's register of readings; the iec60870 module's PROVENANCE.md
explains them.

## Tests

| Test | Source |
|---|---|
| cs104/link_test.go, cs104/seams_test.go, cs104/example_test.go, cs101/example_test.go | the iec60870 links against these simulators, written from the same standards; the window and timer sequences of IEC 60870-5-104 clause 5 |
| sim/iec104sim/sim_test.go, sim/iec101sim/sim_test.go, cmd/*/main_test.go | the simulators through the iec60870 links |
| interop/ | lib60870-C v2.4.1 as controlling station (IEC 104 with and without TLS, TLS refusals, two connections; IEC 101 balanced and unbalanced with every field size); expected values are our own choices checked against what the simulators and lib60870's getters read |

All keys, certificates and addresses are made-up demo values.
