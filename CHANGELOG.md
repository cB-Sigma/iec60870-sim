# Changelog

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
