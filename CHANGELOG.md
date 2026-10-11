# Changelog

## v0.3.0 (2026-10-11)

Requires iec60870 v0.3.0.

- Both simulators run on iec60870's new `station` package: the point
  database, station interrogation, the general counter request, clock
  synchronisation, the test command with time tag and commands
  (select-before-operate, return information, termination, deactivation)
  are the library's; the misbehaviour switches reach it through its
  hooks, and `Procedures` still answers (and records) the other clause
  7.4 procedures first. The exported API is unchanged; `Station.Library`
  returns the library station of each simulator, and
  `iec101sim.Station.Get` was added.
- Behaviour that follows from the library and differs from v0.2.0: station
  interrogation sends a CP56Time2a point as its type without time tag
  and fills each ASDU as far as it goes instead of 20 objects; a
  regulating step command moves the monitored step position by one
  instead of setting it to the command's value; an execute after a
  select must carry the select's value; a clock synchronisation whose
  time is marked invalid gets a negative confirmation; `iec101sim`
  answers the general counter request with its integrated totals and an
  activation termination.
- The lib60870 interop suite was run again against the simulators on
  the library station; it passed unchanged.

## v0.2.0 (2026-10-11)

Requires iec60870 v0.2.0.

- Fixed: `iec101sim` ignored every frame on a balanced line without a
  link address field (`Line.ASize` 0); such frames now reach the line's
  station, and the broadcast check applies only when there is an address
  field.
- Interop tests against lib60870-C v2.4.1 extended: every command type
  with and without time tag, the system commands and clock
  synchronisation edge cases into `iec104sim`, TLS refusals both ways,
  two controlling stations on one station, and lib60870's IEC 101
  controlling station, balanced and unbalanced with every field size,
  against `iec101sim` (see README.md).

## v0.1.0 (2026-10-11)

First release: `iec104sim` and `iec101sim` with their commands, the
iec60870 tests and examples that drive them, and interop tests with
lib60870-C v2.4.1 as controlling station.
