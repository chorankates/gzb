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
cmd/gzb            CLI: probe, network, join, monitor, devices, name,
                        interview, read, write, reporting
  zigbee           public coordinator API: readings, discovery, attributes,
                        reporting, permit-join, Time
  internal/store   device registry: identities, names and last known readings
  internal/zdo     ZDO: descriptors, endpoint discovery, transaction matching
  internal/zcl     ZCL: attribute reports and readings, request and response
                        encoding, the attribute table
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

### Indirect transmission timeout

`EZSP_CONFIG_INDIRECT_TRANSMISSION_TIMEOUT` defaults to **3000 ms**: the MAC
holds a message for a sleepy child for three seconds, then discards it.

A battery sensor polls its parent far less often than that. The request is
gone before the device ever asks for it, and no amount of patience on the host
helps — the wait that matters is the NCP's, not yours. On-demand traffic to a
sleepy device therefore fails every time, and fails looking exactly like a dead
device: no error, no reply, just the host's own timeout.

gzb raises it to **30000 ms**, the stack maximum. That is necessary but not
always sufficient, and the ceiling is the point: a device that polls less often
than every thirty seconds still cannot be reached on demand, whatever the host
does. Measured here, the SONOFF sensors are in that category — they sleep
through a 30-second hold and a 45-second wait — so the only reliable moment to
interview one remains while it is awake, immediately after it joins.

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

## Asking a device what it is

A device that has joined is only an address. ZDO — Zigbee Device Objects,
profile `0x0000`, endpoint 0 at both ends — is the protocol that turns it into
a description, and `gzb interview` asks the whole set of questions:

```console
$ gzb interview 0x0000
Interviewing 0x0000 (0x0000)
  node descriptor...
  power descriptor...
  active endpoints...
  endpoint 1 descriptor...
  manufacturer and model...

  coordinator, mains, always listening
  Manufacturer code 0xABCD
  Powered by mains

  Endpoint 1  profile 0x0104  device 0x0005
    in   basic, identify, time, ota
    out  basic, power, identify, groups, scenes, on/off, level, ...
```

The questions build on each other. The **node descriptor** says what kind of
node this is and whether it is reachable on demand. The **active endpoint
list** says where to address anything else, because an endpoint is where
clusters live. A **simple descriptor** per endpoint says what that endpoint
implements. Only then is there an endpoint to read the Basic cluster from,
which is the one part of an interview that produces a name a person would
recognise.

Every request carries a transaction sequence number, and its response echoes
it; the reply arrives on the request's cluster with bit 15 set. Both halves are
checked, along with the answering node, because a sequence number is only
unique per device — another node's reply can carry the same one.

Results go to the device registry, so an unnamed device can be listed by what
it is rather than by its address.

### Interviewing something that is asleep

This is the hard case, and it is the normal one. A sleepy device only receives
while polling its parent, so a request sits in the NCP's indirect queue until
it wakes — see [indirect transmission
timeout](#indirect-transmission-timeout) for the ceiling on how long that queue
will hold it. The best moment to interview a battery device is immediately
after it joins, while it is still awake.

A device that answers some questions and sleeps through the rest has still told
you something, so a failed step is recorded and the interview continues:

```console
$ gzb interview "bedroom thermo"
Interviewing bedroom thermo (0x90CB)
  node descriptor...
  power descriptor...
  active endpoints...

  ! ezsp: timed out waiting for NCP: no reply from 0x90CB on cluster 0x0002
  ! ezsp: timed out waiting for NCP: no reply from 0x90CB on cluster 0x0003
  ! ezsp: timed out waiting for NCP: no reply from 0x90CB on cluster 0x0005
```

Discovery does not need the readings loop stopped, which matters for exactly
this reason: the moment a sleepy device is worth interviewing is while it is
awake and reporting.

## Reading and writing attributes

An interview says what a device *can* do. An attribute is what it is doing:
`gzb read` asks now, rather than waiting to be told.

Most of the time asking is unnecessary — a sensor reports on its own and
`monitor` catches it. It earns its place when the report has not arrived yet,
when the attribute is one the device never reports, or when the question is
whether a write took effect.

<!--CAPTURE:read-success-->

Clusters and attributes may be named or given in hex, and the two forms are
interchangeable — `temperature` and `0x0402` address the same cluster. With no
attribute named, gzb asks for every attribute it knows on that cluster, which
is a serviceable way to find out what a device actually implements: it answers
for the ones it has and says `unsupported attribute` for the rest.

Naming is checked before anything goes out on the wire, because the failure is
otherwise silent and thirty seconds long:

```console
$ gzb read "living room thermo" temperature humidity
gzb: unknown attribute "humidity" on cluster temperature (name one gzb knows, or give a hex ID like 0x0000)
```

`humidity` is a cluster, not an attribute of `temperature`. Both are named in
the same vocabulary, and the position on the command line is what distinguishes
them.

The endpoint comes from the registry: whichever endpoint the interview found
that cluster on. A device that has never been interviewed falls back to
endpoint 1, where a device with only one endpoint puts everything, and
`--endpoint` overrides both.

Values that gzb recognises as measurements are written to the registry exactly
as a report would be. A reading is a reading whether the device volunteered it
or was asked.

### Writing

`gzb write` is the same addressing in the other direction. Several attributes
can be set at once, which for a battery device is the difference between one
wake-up and two:

```console
$ gzb write "living room thermo" temperature temperature 2500
living room thermo (0xCF83) endpoint 1, temperature
  temperature              = 2500 (int16)
  !   temperature: read only
```

A measured value is the device's to report, not yours to set, and it says so
rather than failing silently. What is about to be written is printed first,
with the type it will be encoded as, because that is the part a person gets
wrong.

The wire format carries the type of every value, so a write has to declare one
before the device has said anything about it. gzb knows the encoding of the
attributes it can name; for anything else `--type` must say. A device does not
coerce a wrong guess, it rejects it with `invalid data type`, and answers a
write to an attribute it will not change with `read only`. Both are reported as
the device gave them, per attribute, rather than as a single failure.

### Configuring reporting

Reporting is the setting that makes polling unnecessary. `gzb reporting` asks a
device to send an attribute on its own initiative:

<!--CAPTURE:reporting-->

`--min` throttles a fast-changing value so a noisy last digit cannot flood the
network. `--max` is the opposite: a heartbeat that proves the device is alive
when nothing has changed. `--change` is the threshold between them.

The threshold is in the attribute's own units, because that is what travels on
the wire — a temperature reported in hundredths of a degree takes `50` to mean
half a degree. Scaling it here would be friendlier for the attributes gzb
happens to understand and impossible for the rest, so gzb prints back what it
is about to ask for instead, where a wrong guess by a factor of a hundred is
visible before the device applies it.

Two things about this are worth saying plainly. The configuration lives in the
device, not in gzb: it survives gzb restarting, and stays set until something
changes it. And it is not free — a battery sensor asked for a report every
second will send one, and will do so until the battery is flat.

`--show` asks a device what it currently holds and changes nothing. It is worth
knowing about before you need it, because the failure it diagnoses is a silent
one: an attribute that has been switched off looks exactly like an attribute
that has nothing new to say. A configure command answers with a status, which
tells you the device accepted an instruction — not what it now holds, and the
two come apart precisely when it matters.

`--off` and `--default` are both undo, and they are not the same undo. The
difference is one field on the wire and it is easy to get backwards:

| | minimum | maximum | effect |
|---|---|---|---|
| `--off` | as given | `0xFFFF` | never report this attribute |
| `--default` | `0xFFFF` | `0xFFFF` | revert to what the device shipped with |

Undoing a configuration you set is `--default`. `--off` silences an attribute,
which on a sensor that was reporting on its own initiative is a change in its
own right rather than a restoration — the sort of thing that looks like a fixed
bug until someone notices the readings stopped.

### Being refused rather than ignored

Every one of these commands waits for a specific answer — a read response, a
write response, a configure-reporting response. A device that does not
implement the command at all sends none of them: it sends a ZCL Default
Response carrying the reason instead. gzb accepts either, so a refusal arrives
as a refusal rather than as a thirty-second timeout that says nothing.

That distinction matters most on exactly the devices where a timeout is also
the normal outcome of being asleep, and where the two would otherwise be
indistinguishable.

A sleepy device is still the hard case, and no amount of framing fixes it:

```console
$ gzb read "living room thermo" temperature
gzb: ezsp: timed out waiting for NCP: no reply from 0xCF83 on cluster 0x0402
```

The request went out and the NCP held it for a sleepy child, but the device did
not poll its parent before the queue gave up on it — see [indirect transmission
timeout](#indirect-transmission-timeout). Retrying is what gets through: each
attempt re-arms the hold, so a request is queued whenever the device next
wakes.

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

Working and verified against hardware, with a real device paired:

- ASH framing, CRC, randomization, escaping, ACK and retransmission
- EZSP version negotiation and both frame layouts
- NCP configuration and endpoint registration, re-applied every session
- Network formation and persistence across reconnects
- Trust-centre join policy, applied and verified by read-back
- Pairing: all three join callbacks decoded, merged and recorded
- ZCL attribute reports decoded into readings, and the device registry
- ZDO discovery: node and power descriptors, active endpoints, simple
  descriptors, matched by transaction sequence and answering node
- Attribute reads on demand, including per-attribute refusals, recorded to the
  registry the same way a report is
- Attribute writes, including the encoding a device rejects and the read-only
  attribute it will not change
- Reading a device's reporting configuration back, which is what tells a
  configuration that took from one that did not
- Reporting configuration, accepted by a real sensor and restored to its own
  default afterwards
- Device names, and addressing a device by name wherever one is taken
- Outbound unicast, exercised by the Time cluster responder
- `probe`, `network`, `permit-join`, `join`, `devices`, `name`, `monitor`,
  `interview`, `read`, `write`, `reporting`, `config`
- Raw command escape hatch (`ezsp.Conn.Call`) for any unmodelled command

Verified end to end with a SONOFF temperature/humidity sensor
(`A4:C1:38:18:56:07:FF:FF`): paired, survived reconnects, and reported
temperature, humidity and battery. The ZCL tests are pinned to bytes that
sensor actually sent.

A second sensor (`A4:C1:38:18:5B:A1:FF:FF`, a SNZB-02B) answered an interview
and then, caught awake, an attribute read — `23.90 °C` as a raw `2390`, with
`tolerance` refused as an unsupported attribute — and a write, which it
declined with `read only`. Both answers went through the same request path a
caller uses, and the read was recorded in the registry as a reading. It also
accepted a reporting configuration for its measured temperature, and then a
revert to its own default — both answered `ok`.

Not built yet:

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

```
$ ./gzb interview "living room thermo"
Interviewing living room thermo (0xCF83)
  node descriptor...
  power descriptor...
  active endpoints...
  endpoint 1 descriptor...
  manufacturer and model...

  SONOFF SNZB-02B
  end device, battery, sleepy
  Manufacturer code 0x1286
  Powered by rechargeable battery

  Endpoint 1  profile 0x0104  device 0x0302
    in   basic, power, identify, temperature, humidity, cluster 0x0020, manufacturer 0xFC57, manufacturer 0xFC11
    out  time, ota
```
