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

The public `zigbee` package is the application boundary. It owns the report
loop, registry enrichment and coordinator services so applications do not each
grow their own EZSP/ZCL loop:

```
cmd/gzb            CLI: probe, network, join, monitor, devices, name, config
  zigbee           public coordinator API: readings, permit-join, Time service
  internal/store   device registry: identities, names and last known readings
  internal/zcl     ZCL: attribute reports, readings, response encoding
  internal/ezsp    EZSP: negotiation, commands, callbacks, endpoints
    internal/ash   ASH: framing, CRC, randomization, ACK/retransmit
      serial       115200 8N1, DTR and RTS deasserted
```

Applications can consume readings without depending on the protocol internals:

```go
coordinator, err := zigbee.Open(ctx, zigbee.Options{Path: "/dev/ttyUSB0"})
if err != nil {
	log.Fatal(err)
}
defer coordinator.Close()

readings, errs := coordinator.Readings(ctx)
for readings != nil || errs != nil {
	select {
	case reading, ok := <-readings:
		if !ok {
			readings = nil
			continue
		}
		fmt.Printf("%s: %.2f %s\n", reading.Capability, reading.Value, reading.Unit)
	case err, ok := <-errs:
		if !ok {
			errs = nil
			continue
		}
		log.Print(err)
	}
}
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
$ gzb permit-join 60                     # open to new devices, then exit
$ gzb permit-join 0                      # close again
$ gzb join 90                            # open, and watch devices arrive
$ gzb join --verbose 90                  # ...and show every APS frame
$ gzb devices                            # list what has been seen
$ gzb name 0x90CB living room thermo     # call a device something human
$ gzb name                               # list the names
$ gzb name --clear "living room thermo"  # forget one
$ gzb monitor                            # print readings until Ctrl-C
$ gzb monitor --for 60s --raw            # ...bounded, including undecoded frames
$ gzb config                             # dump NCP configuration (diagnostic)
```

`join` is the one to use when pairing. `permit-join` opens the window and
exits, so a device that arrives afterwards is joined to the network but
recorded nowhere; `join` holds the session open for the whole window, decodes
the arrival and writes it to the local registry.

## NCP state is host state

The single most important thing this codebase learned the hard way:

> **EZSP configuration, endpoints and policies live in NCP RAM and reset to
> firmware defaults on every reboot — and opening the ASH link reboots it.**

They are not adapter settings you write once. They are host state that must be
re-applied on every connection, in a specific order, because most of it is
rejected once the network is up:

```
reset ASH  →  version handshake  →  configuration  →  endpoints  →  networkInit
                                    └──── only valid while the network is down ────┘
```

Three separate failures traced back to skipping a step here, and every one of
them presented as total silence rather than an error.

### Stack profile

`EZSP_CONFIG_STACK_PROFILE` defaults to **0** on this firmware. ZigBee PRO is
**2**, and the value ends up in the beacon. A Zigbee 3.0 device that reads a
beacon advertising profile 0 rejects it and never attempts to associate, so the
coordinator sees nothing at all — not a denied join, no join.

Changing it invalidates a network formed under the old value, so a network
formed before this was fixed has to be re-formed.

### Endpoints

Until an endpoint exists, the coordinator has no APS address a device can talk
to. ZDO discovery finds nothing, bindings have nothing to point at, and
attribute reports are rejected. The device joins, finds a network it cannot
use, and leaves a few seconds later.

### Trust-centre policy

Two separate things must both be true before a device can join:

- **`permitJoining`** opens a *time window*.
- **The trust-centre policy** decides what happens to a device that arrives
  during it. Without `EZSP_DECISION_ALLOW_JOINS` the NCP denies every join,
  however wide the window.

The key-request policy matters just as much. A Zigbee 3.0 device asks the trust
centre for a link key of its own once it is on the network, and the answer
decides whether it stays:

| Decision | Result |
| --- | --- |
| `0x50` allow, send current key | device joins, then **leaves after ~21s** |
| `0x51` allow, generate new key | device joins and stays |

`gzb join` applies these every session and then **reads the policy back**,
because a silently ineffective policy and a device that was never in pairing
mode both look like "nothing joined".

The stack reports the window opening and closing through `stackStatusHandler`,
and `join` surfaces those directly:

```console
$ gzb join 20
[   0.0s] stack         network opened for joining
[  19.9s] stack         network closed to joining
```

Those two statuses are `0x9C` and `0x9D` on EmberZNet 7.4.4, measured on the
wire. Seeing them is what separates "the coordinator was listening and nothing
called" from "the window never opened".

A joining device produces up to three callbacks, which are complementary
rather than redundant:

| Callback | Fires for | Uniquely carries |
| --- | --- | --- |
| `trustCenterJoinHandler` | any device joining anywhere in the mesh | both addresses, the join decision |
| `childJoinHandler` | devices parented directly to the coordinator | node type |
| ZDO Device Announce | broadcast by the device itself | MAC capability flags |

Only the announce says whether a device is mains powered or a sleepy battery
node, so the registry merges all three rather than letting the last one win.

## Reading data

`monitor` decodes ZCL attribute reports into named quantities with units:

```console
$ gzb monitor
15:55:11  A4:C1:38:18:56:07:FF:FF  temperature     28.20 °C   lqi 255  rssi -25
15:55:11  A4:C1:38:18:56:07:FF:FF  humidity        34.60 %    lqi 255  rssi -25
15:55:15  A4:C1:38:18:56:07:FF:FF  battery        100.00 %    lqi 255  rssi -25
```

Listening is not a debugging convenience — it is the primary way to get data
from a battery device. Sleepy nodes are unreachable most of the time and report
when a value changes, so readings are written to the registry as they arrive:

```console
$ gzb devices
A4:C1:38:18:56:07:FF:FF  0x90CB
  sleepy end device, last seen 2026-08-15T16:00:10-06:00
  battery        100.00 %     (2026-08-15T15:55:15-06:00)
  humidity        31.20 %     (2026-08-15T16:00:10-06:00)
  temperature     28.20 °C    (2026-08-15T15:55:12-06:00)
```

`--json` emits one object per reading, and `--raw` additionally shows frames
that carry no interpretable attributes.

## Naming devices

An IEEE address identifies a device but does not say what it is, and a network
address is not even stable across a rejoin. `gzb name` attaches the missing
half:

```console
$ gzb name 0x90CB living room thermo
A4:C1:38:18:56:07:FF:FF is now "living room thermo".

$ gzb monitor
15:55:11  living room thermo       temperature     28.20 °C   lqi 255  rssi -25
15:55:11  living room thermo       humidity        34.60 %    lqi 255  rssi -25
```

A name is not decoration: it is also an address. Anywhere a device can be
given — `name`, `interview` — an IEEE address, a `0xNNNN` network address and a
name are interchangeable, and names match loosely, so any unambiguous part will
do:

```console
$ gzb interview thermo
Interviewing living room thermo (0x90CB)
```

That is why names must be unique and cannot look like an address. Loose
matching never guesses: a query matching several devices is an error that names
them.

```console
$ gzb name bedroom
gzb: "bedroom" matches 2 devices ("bedroom lamp", "bedroom sensor"); use the full name or an address
```

Naming touches no hardware, which matters more than it sounds: the moment you
want to name a device is right after seeing it in a monitor log, and by then a
battery sensor is asleep and unreachable. Listings keep both — the name for
reading, the addresses for matching against other tools:

```console
$ gzb devices
living room thermo  0x90CB  A4:C1:38:18:56:07:FF:FF
  sleepy end device, last seen 2026-08-15T16:00:10-06:00
  battery        100.00 %     (2026-08-15T15:55:15-06:00)
```

An unnamed device falls back to its interviewed model, and then to its address,
so output is readable at every stage of knowing what a device is.

### Answering the Time cluster

The coordinator serves cluster `0x000A` rather than only consuming clusters.
Devices read the time to stamp their own data, and one that gets no answer
keeps asking — this sensor retried every two seconds, which on a battery is not
free. Advertising the cluster and then ignoring the reads is the worst of both
worlds, so `monitor` answers them.

DST is folded into the reported timezone offset rather than modelled with
transition times: `LocalTime` is correct, which is all devices use, without
pretending to know future transitions.

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

Working and verified against hardware, with a real device paired:

- ASH framing, CRC, randomization, escaping, ACK and retransmission
- EZSP version negotiation and both frame layouts
- NCP configuration and endpoint registration, re-applied every session
- Network formation and persistence across reconnects
- Trust-centre join policy, applied and verified by read-back
- Pairing: all three join callbacks decoded, merged and recorded
- ZCL attribute reports decoded into readings, and the device registry
- Device names, and addressing a device by name wherever one is taken
- Outbound unicast, exercised by the Time cluster responder
- `probe`, `network`, `permit-join`, `join`, `devices`, `name`, `monitor`,
  `config`
- Raw command escape hatch (`ezsp.Conn.Call`) for any unmodelled command

Verified end to end with a SONOFF temperature/humidity sensor
(`A4:C1:38:18:56:07:FF:FF`): paired, survived reconnects, and reported
temperature, humidity and battery. The ZCL tests are pinned to bytes that
sensor actually sent.

Not built yet:

- ZDO queries: active endpoints, simple and node descriptors
- Reading and writing attributes on demand, and configuring reporting
- Cluster commands for lights, plugs and switches
- Binding management
- REPL mode


## usage

```
conor@pride:~/git/gzb[chorankates/gzb|main|3964526|U]
 4:07.25 [44730] $ ./gzb devices
A4:C1:38:18:56:07:FF:FF  0x90CB
  sleepy end device, last seen 2026-08-15T16:00:10-06:00
  battery        100.00 %     (2026-08-15T15:55:15-06:00)
  humidity       31.20 %      (2026-08-15T16:00:10-06:00)
  temperature    28.20 °C     (2026-08-15T15:55:12-06:00)

1 device(s) in /home/conor/.config/gzb/devices.json
conor@pride:~/git/gzb[chorankates/gzb|main|3964526|U]
 4:08.33 [44732] $ ./gzb monitor
Listening on /dev/ttyUSB0. Ctrl-C to stop.

^C
Stopped.
conor@pride:~/git/gzb[chorankates/gzb|main|3964526|U]
 4:08.45 [44733] $ ./gzb monitor -raw
Listening on /dev/ttyUSB0. Ctrl-C to stop.

16:09:00  A4:C1:38:18:56:07:FF:FF  temperature     27.40 °C   lqi 255  rssi -28
^C
Stopped.
conor@pride:~/git/gzb[chorankates/gzb|main|3964526|U]
 4:09.08 [44734] $ ./gzb devices
A4:C1:38:18:56:07:FF:FF  0x90CB
  sleepy end device, last seen 2026-08-15T16:09:00-06:00
  battery        100.00 %     (2026-08-15T15:55:15-06:00)
  humidity       31.20 %      (2026-08-15T16:00:10-06:00)
  temperature    27.40 °C     (2026-08-15T16:09:00-06:00)

1 device(s) in /home/conor/.config/gzb/devices.json
```

