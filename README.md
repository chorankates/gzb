# gzb

A Go client for Zigbee coordinators, built from scratch against the wire
protocol. No Zigbee libraries — the stack below is ours, from UART framing up.

## The adapter

The target is a Sonoff dongle on `/dev/ttyUSB0`. Its USB descriptor reads
`Itead Sonoff Zigbee 3.0 USB Dongle Plus V2` and it enumerates as
`10c4:ea60` (Silicon Labs CP210x UART bridge).

**That descriptor is misleading.** It looks exactly like a ZBDongle-P (TI
CC2652P, Z-Stack/ZNP over UNPI framing), but the chip behind the bridge is a
**Silicon Labs EFR32 running EmberZNet 7.4.4**, which speaks **EZSP over ASH**.
Itead ships a CP2102N bridge on EFR32 hardware too, so the USB VID/PID does not
tell you which silicon is behind it.

Identify the chip by protocol, never by USB ID. Deassert DTR and RTS, open the
port at 115200 8N1, and look at what arrives:

| Bytes | Meaning |
| --- | --- |
| `1A C1 02 02 9B 7B 7E` | ASH RSTACK — Silicon Labs EFR32, EZSP |
| response to `FE 00 21 01 20` | UNPI SRSP — TI CC2652P, Z-Stack |

An EZSP adapter never emits a `0xFE`-led UNPI frame, and a Z-Stack adapter
never emits a `0x7E`-terminated ASH frame.

## Architecture

Three layers, each independently testable:

```
cmd/gzb            CLI: probe, network form/leave, permit-join
  internal/ezsp    EZSP: version negotiation, commands, callbacks
    internal/ash   ASH: framing, CRC, randomization, ACK/retransmit
      serial       115200 8N1, DTR and RTS deasserted
```

### `internal/ash`

Silicon Labs' reliable-delivery layer. Each frame is
`[control][data...][CRC hi][CRC lo][0x7E]`, with three transformations applied
in a strict order on the way out:

1. DATA payloads are XORed with an LFSR sequence seeded at `0x42`, so repeated
   bytes cannot imitate a flag or XON/XOFF byte.
2. A CRC-16/CCITT is appended over the control byte and the randomized data.
3. Reserved bytes are escaped, so `0x7E` appears only as a terminator.

The CRC must cover the randomized bytes but not the escaping. Getting that
order wrong is the classic ASH bug.

On top of framing sits a sliding-window protocol with sequence numbers,
acknowledgement and retransmission. The window is fixed at one outstanding
frame, which is sufficient for a host client and removes a class of ordering
bug.

### `internal/ezsp`

The command interface. EZSP has two incompatible frame layouts:

```
legacy   (version < 8)    [seq][frameControl][frameID][params...]
extended (version >= 8)   [seq][fcLo][fcHi][idLo][idHi][params...]
```

The `version` command bootstraps this: it is always sent in the legacy layout,
and its reply states which version the NCP actually speaks. This dongle
negotiates **EZSP version 13**, so everything after the handshake uses the
extended layout.

Despite version 13, status fields on this firmware are **one byte**
(`EmberStatus`), not the four-byte `sl_status` used elsewhere in that
generation. This was measured, not assumed: `networkInit` on an unformed
adapter returns a single byte `0x93` (`NOT_JOINED`), and
`getNetworkParameters` returns 22 bytes = `status(1) + nodeType(1) +
params(20)`.

## Network lifecycle

Forming a network is **never** a startup side effect. It is an explicit,
destructive command that requires `--confirm`, because forming rewrites the
adapter's credentials and orphans every device holding the old network key.

Connecting, by contrast, is non-destructive: it resets the ASH link and then
calls `networkInit` to restore whatever network is already saved in the
adapter's tokens. A fresh adapter answers `NOT_JOINED`, which is expected and
not an error.

That restore step is required. Resetting the ASH link resets the NCP, which
comes back with its radio idle even when credentials are stored — so without
`networkInit`, a perfectly good network looks like no network at all.

## Usage

```console
$ gzb probe
Adapter      /dev/ttyUSB0
Protocol     EZSP over ASH (EmberZNet)
EZSP version 13
Stack        EmberZNet 7.4.4 build 0 (type 2)
IEEE         C0:2C:ED:FF:FE:05:83:33
Network      up

Network
  Role         coordinator
  Node ID      0x0000
  PAN ID       0x6F88
  Ext PAN ID   4B:F9:3D:C3:75:45:69:2B
  Channel      15
  TX power     8 dBm
```

```console
$ gzb network form --channel 15          # dry run; prints what it would do
$ gzb network form --channel 15 --confirm
$ gzb network leave --confirm
$ gzb permit-join 60                     # open to new devices
$ gzb permit-join 0                      # close again
```

Every command takes `--json` for machine-readable output, `--trace` to log
decoded EZSP frames, and `--trace-ash` to log the raw ASH frames beneath them.
The port defaults to `/dev/ttyUSB0` and can be set with `--port` or `GZB_PORT`.

## Testing

```console
$ go test ./...
```

Protocol tests are pinned to bytes captured from the real dongle rather than to
the implementation. The ASH vectors (`1A C0 38 BC 7E` for RST, the RSTACK reply
and its CRC) came off the wire, and the EZSP header sizes were measured against
EmberZNet 7.4.4.

## Status

Working and verified against hardware:

- ASH framing, CRC, randomization, escaping, ACK and retransmission
- EZSP version negotiation and both frame layouts
- `probe`, `network show`, `network form`, `network leave`, `permit-join`
- Network formation and persistence across reconnects
- Raw command escape hatch (`ezsp.Conn.Call`) for any unmodelled command

Not built yet:

- Device discovery and the device registry
- ZDO queries: active endpoints, simple and node descriptors
- ZCL: attribute read/write, discovery, cluster commands
- The friendly layer over common clusters
- REPL and continuous monitoring
