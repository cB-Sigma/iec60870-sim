# iec60870-sim

Simulated IEC 60870-5-104 and IEC 60870-5-101 controlled stations in Go, for testing SCADA masters and gateways without a device.

[![Go Reference](https://pkg.go.dev/badge/github.com/cB-Sigma/iec60870-sim.svg)](https://pkg.go.dev/github.com/cB-Sigma/iec60870-sim) [![test](https://github.com/cB-Sigma/iec60870-sim/actions/workflows/test.yml/badge.svg)](https://github.com/cB-Sigma/iec60870-sim/actions/workflows/test.yml) [![Licence: Apache-2.0](https://img.shields.io/badge/licence-Apache--2.0-blue.svg)](LICENSE) [![Go](https://img.shields.io/badge/go-1.26.0-00ADD8.svg)](go.mod)

The IEC 104 station answers interrogations, takes commands with
select-before-operate, runs the clause 7.4 station procedures and file
transfer, speaks TLS and the IEC 62351-5 A-Profile, and has switches
that make it misbehave the way field devices do. The IEC 101 line holds
one or more stations behind a TCP "converter", balanced or unbalanced.
Both run in-process for Go tests or from the command line. They are
built on the codec and links of
[iec60870](https://github.com/cB-Sigma/iec60870). Written for SigmaM2M, a
oneM2M platform; it has no dependency on it.

## What is simulated

| Package | What it does |
|---|---|
| `sim/iec104sim` | An IEC 104 controlled station: `New(ca)`, `Listen` (TCP, or TLS with a `tls.Config` or an `iotsec.TLSServer` in `TLS`), a point database (`Set`, `Get`) answered to station and group interrogation, spontaneous data (`Send`), commands with select-before-operate and return information (`Controls`), clock synchronisation (`ClockSyncs`), the clause 7.4 procedures and files (`Proc`), the A-Profile (`Security`), and switches: no acknowledgement, no STARTDT confirmation, refused or silent commands, no or negative termination, late confirmations (`ConfirmDelay`), a maximum command delay (`MaxDelay`); what the controlling station sent (`Commands`, `Requests`, `Counts`, `Links`) |
| `sim/iec101sim` | An IEC 101 line of one or more stations (`NewStation`, `NewLine`) behind a TCP listener standing in for a serial-to-IP converter: unbalanced or balanced, class 1 and 2 data, busy (DFC), FC14 and lost answers, spontaneous data (`Spontaneous`), commands and clock synchronisation, the A-Profile |
| `cmd/iec104sim`, `cmd/iec101sim` | Run the simulators with a small demo database |

Not simulated: real serial lines (use a pty bridge such as socat),
redundancy groups across several connections, interoperability
statement files.

## Install

```
go get github.com/cB-Sigma/iec60870-sim@latest
```

Requires Go 1.26.0 or later. The commands:

```
go install github.com/cB-Sigma/iec60870-sim/cmd/iec104sim@latest
go install github.com/cB-Sigma/iec60870-sim/cmd/iec101sim@latest
```

## Command line

```
iec104sim -listen 127.0.0.1:2404
iec104sim -listen 127.0.0.1:19998 -cert rtu.pem -key rtu.key
iec104sim -listen 127.0.0.1:2404 -secure -trust <controlling station public key, hex>
iec101sim -listen 127.0.0.1:2410
iec101sim -listen 127.0.0.1:2410 -balanced
```

| iec104sim flag | Default | Meaning |
|---|---|---|
| `-listen` | `127.0.0.1:2404` | address to listen on |
| `-ca` | `1` | common address (1 to 65534) |
| `-cert`, `-key` | | serve under TLS with this certificate and key (PEM files) |
| `-secure` | off | run the IEC 62351-5 A-Profile with a fresh self-signed identity, whose public key is printed |
| `-trust` | | with `-secure`: the controlling station's public key (hex) |
| `-report` | `5s` | how often the demo values change (0: never) |

The IEC 104 demo database at the common address: 1001 M_DP_NA_1 breaker
position, 2001 C_DC_NA_1 breaker command (select-before-operate,
changes 1001), 3001 and 3002 M_ME_NC_1 feeder voltage and current (3001
changes spontaneously), 4001 M_IT_NA_1 energy counter.

| iec101sim flag | Default | Meaning |
|---|---|---|
| `-listen` | `127.0.0.1:2410` | address to listen on (one controlling station at a time) |
| `-balanced` | off | balanced transmission instead of unbalanced polling |
| `-link` | `3` | link address of the station (1 to 254) |
| `-ca` | `7` | common address (1 to 254) |
| `-report` | `5s` | how often the tank level changes (0: never) |

The IEC 101 line uses one-octet link address, cause and common address,
and two-octet object addresses: 100 M_SP_NA_1, 300 M_ME_NC_1 tank level
(spontaneous), 200 a command that changes 100.

## In Go tests

From the cs104 example:

```go
rtu := iec104sim.New(1)
rtu.Set(1001, iec104sim.Point{Type: iec60870.MDpNa1, Value: 2})
rtu.Controls[2001] = 1001
if err := rtu.Listen("", nil); err != nil {
	panic(err)
}
defer rtu.Stop()
link, err := cs104.Dial(ctx, rtu.Addr(), cs104.LinkConfig{})
c := cs104.NewClient(link, cs104.ClientConfig{CA: 1, OnData: func(a *iec60870.ASDU) { /* ... */ }})
err = c.Interrogate(ctx, iec60870.QOIStation, 5*time.Second)
_, err = c.Operate(ctx, iec60870.Operation{Type: iec60870.CDcNa1, Object: iec60870.Object{IOA: 2001, Value: 1},
	SelectBeforeOperate: true, Termination: 5 * time.Second})
```

## Demo values

Every key, certificate, address and point value in this module is a
made-up demo value; identities are generated when the simulators start.
Do not use them for real devices.

## Documentation

Reference: https://pkg.go.dev/github.com/cB-Sigma/iec60870-sim

## Testing

```
go vet ./...
go test -race ./...
```

`cs104` and `cs101` hold the iec60870 tests and examples that drive the
simulators (window and timer behaviour, the iotsec seams, the examples);
each simulator package tests itself through the library's links. Tests
carry `// Covers: SIG-...` comments: requirement IDs of the SigmaM2M
project's compliance report, with no meaning outside it.

`interop/` is a nested module with its own go.mod. It builds lib60870-C
v2.4.1 (GPLv3, used only as a test peer in a Debian container; none of
its code is in this repository) with a peer program of ours
(`interop/peer`) and runs lib60870 controlling stations against the
simulators. It is skipped unless `SIGMA_IEC60870_INTEROP=1` is set and
Docker is running:

```
cd interop && SIGMA_IEC60870_INTEROP=1 go test -v ./...
```

### Interop tested against

lib60870-C v2.4.1 (mbedtls 3.6.6), built from source in Docker, on
2026-10-11:

- A lib60870 controlling station (our peer program) against
  `iec104sim`: STARTDT, station interrogation (activation confirmation,
  M_SP_NA_1 and M_ME_NC_1, termination), spontaneous M_ME_NC_1, clock
  synchronisation (received to the millisecond), C_SC_NA_1
  select-before-operate with return information (cause 11) and
  termination, C_DC_NA_1 direct execute, C_SE_NC_1 select-before-operate
  changing the measured value, and a negative confirmation (cause 47)
  for an unknown object. Passed.
- The same over TLS 1.2 and TLS 1.3 with mutual certificate
  authentication (mbedtls client, crypto/tls station). Passed.
- lib60870's own cs104_client example against `iec104sim`: STARTDT,
  station interrogation, test command with time tag (C_TS_TA_1). Passed.
- A lib60870 unbalanced primary on a pty, bridged by socat to
  `iec101sim`: link start-up and polling, station interrogation, clock
  synchronisation, C_SC_NA_1 select-before-operate with termination, a
  spontaneous value. Passed.
- Not tested against a reference: the A-Profile (lib60870's secure
  authentication is not in its open source release), the IEC 101
  balanced mode, file transfer, the misbehaviour switches.

## Related repositories

- [iec60870](https://github.com/cB-Sigma/iec60870): the IEC 104 and IEC
  101 codec, links and A-Profile.

## Status

v0.x. The API may change before v1.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).

## Security

See [SECURITY.md](SECURITY.md).

## Licence

Apache-2.0 (see [LICENSE](LICENSE) and [NOTICE](NOTICE)). Copyright 2026
Abhinav Gautam.
