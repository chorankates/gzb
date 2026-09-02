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
                        interview, read, write, reporting, light, and
                        repl, which takes the same commands at a prompt
  zigbee           public coordinator API: readings, discovery, attributes,
                        reporting, permit-join, join watching, the device
                        registry and naming, Time
  internal/store   device registry: identities, names and last known readings
  internal/zdo     ZDO: descriptors, endpoint discovery, transaction matching
  internal/zcl     ZCL: attribute reports and readings, request and response
                        encoding, the attribute table
  internal/ezsp    EZSP: negotiation, commands, callbacks, endpoints
    internal/ash   ASH: framing, CRC, randomization, ACK/retransmit
      serial       115200 8N1, DTR and RTS deasserted
```

The process holding the serial port holds it exclusively, so anything an
application wants to offer — pairing from a UI, naming a device from an HTTP
handler — has to go through the process that is already listening. The public
package is built for that: `Joins` streams pairing activity, `Devices`,
`Device`, `SetName` and `ClearName` expose the registry, and all of it is safe
to call from other goroutines while the readings loop runs. `gzb join` is
itself a consumer of this API rather than a private implementation of it.

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
$ gzb repl                               # the device commands at a prompt, with Tab
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
interview one remains while it is awake, immediately after it joins. The way
around that is to need fewer answers from it: a second unit of a model already
interviewed [inherits the
first one's](#not-asking-a-question-already-answered), and has to be caught
awake once rather than five times.

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

### One measurement is not a multiple of what was sent

Most ZCL measurements are the value in fixed-point: a temperature arrives as
`2820` and means 28.20 °C, so a scale factor is all a reading needs. Illuminance
is the exception, and gzb got it wrong for long enough to fill a registry with
the raw number. The Illuminance Measurement cluster carries

```
MeasuredValue = 10000 · log₁₀(lux) + 1
```

a logarithmic encoding that fits five decades of daylight into sixteen bits.
Read as though it were already lux it is wrong by orders of magnitude, and
wrong in the direction that looks plausible — a lamp reporting `12788` was
sitting in a room at 19 lx, not in a floodlit one:

```console
$ gzb read light1 0x0400 0x0000
  illuminance              19.00 lx   (raw 12788, uint16)
```

An `attributeSpec` therefore carries an optional `convert` alongside its scale,
for the quantities that are not a plain multiple of the raw value. The sentinel
gets the same treatment: `0xFFFF` means the device has nothing to report, and
through the formula it would claim three million lux with a straight face, so
it is not a reading at all rather than a very bright one.

Registries written before the fix hold the raw numbers under `lx`. A stored
value cannot be converted in place — a corrected entry is indistinguishable
from a raw one and would be converted again on the next start — so `store.Open`
drops the entry and lets the next report replace it with a true one.

## Asking a device what it is

A device that has joined is only an address. ZDO — Zigbee Device Objects,
profile `0x0000`, endpoint 0 at both ends — is the protocol that turns it into
a description, and `gzb interview` asks the whole set of questions:

```console
$ gzb interview 0x0000
Interviewing 0x0000 (0x0000)
  manufacturer and model...
  node descriptor...
  power descriptor...
  active endpoints...
  endpoint 1 descriptor...

  coordinator, mains, always listening
  Manufacturer code 0xABCD
  Powered by mains

  Endpoint 1  profile 0x0104  device 0x0005
    in   basic, identify, time, ota
    out  basic, power, identify, groups, scenes, on/off, level, ...
```

**Manufacturer and model** come first, from the Basic cluster. It is the one
answer that cannot be worked out from any other, the only one that produces a
name a person would recognise, and — as the next section explains — the one
that can make the rest unnecessary. Reading it has to guess an endpoint, since
the endpoint list is exactly what has not been asked for yet; endpoint 1 is
where a device with a single endpoint puts everything.

The remaining questions build on each other. The **node descriptor** says what
kind of node this is and whether it is reachable on demand. The **active
endpoint list** says where to address anything else, because an endpoint is
where clusters live. A **simple descriptor** per endpoint says what that
endpoint implements. If the opening guess missed, the endpoint list has now
settled where the Basic cluster really is, and the model is asked for again —
a guess that missed is not a fault of the device, so it is not reported as one.

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
  manufacturer and model...
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

### Not asking a question already answered

Identical devices are identical. A network is usually built from a handful of
models bought a few at a time, and every unit of a model came off the same line
with the same endpoints carrying the same clusters. Asking each of them
separately is asking the same question over and over and waiting a long time
for the same answer.

So once one unit has been interviewed, the next only has to say what it is:

```console
$ gzb interview "outside #1 thermo"
Interviewing outside #1 thermo (0x584D)
  manufacturer and model...

  eWeLink TH01
  Same model as living room thermo, so everything below is that device's answers
  end device, battery, sleepy
  Powered by disposable battery

  Endpoint 1  profile 0x0104  device 0x0302
    in   basic, power configuration, identify, temperature, humidity, ...
    out  ota upgrade, time
```

One round trip instead of five. On a sleepy sensor that answers in its own
time, that is the difference between minutes and most of an hour — and with
`--all` it means one device answers properly and the rest fall in behind it.

Inherited structure is marked as inherited, in the registry as well as in that
output. The claim being made is a deduction — *these two report the same model,
so they are built the same* — and a deduction that turns out to be wrong is
only findable if it does not look exactly like an observation. For the same
reason a record only inherits from a device that answered for itself, never
from another inherited record: one bad match would otherwise copy itself across
the registry with nothing left pointing at a device that was actually asked.

A device that has answered for itself is never given a sibling's answers
instead, and `--full` asks everything regardless — which is how an inherited
record is promoted to a firsthand one:

```console
$ gzb interview --full "outside #1 thermo"
```

### Picking up where the last run stopped

Interviewing a network of sleepy devices is long enough that something will go
wrong partway through — a device that never wakes, an NCP that stops answering
for a moment, a Ctrl-C. None of that should cost the answers already collected,
so each one is written to the registry as it arrives rather than at the end,
one device failing does not end the run, and `--all` means *every device
without an answer* rather than every device:

```console
$ gzb interview --all
7 device(s) already interviewed, skipping; --full asks them again.

Interviewing bathroom thermo (0x2C1B)
...
gzb: door1: zigbee: reading network state: ezsp: timed out waiting for NCP: no response to networkState

3 device(s) interviewed, 2 of them from an identical device; 1 could not be reached.
Re-run to try those again; what succeeded is already recorded.
```

Re-running is then cheap: the seven that answered are not asked again, and
neither are the three that just did. That is the whole difference between
`--all` and `--full` — `--all` finishes the job, `--full` starts it over.

A device named on the command line is always asked, whatever the registry
already holds. Naming it is the request.

## Reading and writing attributes

An interview says what a device *can* do. An attribute is what it is doing:
`gzb read` asks now, rather than waiting to be told.

Most of the time asking is unnecessary — a sensor reports on its own and
`monitor` catches it. It earns its place when the report has not arrived yet,
when the attribute is one the device never reports, or when the question is
whether a write took effect.

```console
$ gzb read "outside #2 thermo" temperature
outside #2 thermo (0x4BCD) endpoint 1, temperature
  waiting up to 5m0s; a battery device only listens while polling its parent
  temperature              26.40 °C   (raw 2640, int16)
  minimum measurable       -4000 (int16)
  maximum measurable       11500 (int16)
  tolerance                !  unsupported attribute
```

Both halves of a measurement are printed. `23.90 °C` is what it means; `2390`
is what the device actually said, and the scale between them is gzb's claim
rather than the sensor's. The last two lines are the same attribute read
succeeding and failing: the sensor knows how cold it can measure, and has no
opinion on its own tolerance.

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
that cluster on — or the endpoint an identical device's interview found it on,
which is most of the value of [inheriting a
sibling's answers](#not-asking-a-question-already-answered). A device the
registry knows nothing about falls back to endpoint 1, where a device with only
one endpoint puts everything, and `--endpoint` overrides both.

Values that gzb recognises as measurements are written to the registry exactly
as a report would be. A reading is a reading whether the device volunteered it
or was asked.

### Writing

`gzb write` is the same addressing in the other direction. Several attributes
can be set at once, which for a battery device is the difference between one
wake-up and two:

```console
$ gzb write "outside #2 thermo" temperature temperature 2500
outside #2 thermo (0x4BCD) endpoint 1, temperature
  temperature              = 2500 (int16)
  waiting up to 5m0s; a battery device only listens while polling its parent
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

```console
$ gzb reporting -min 60s -max 1h -change 50 "outside #2 thermo" temperature temperature
outside #2 thermo (0x4BCD) endpoint 1, temperature
  temperature              every 1m0s to 1h0m0s, on a change of 50 (0.50 °C)
  waiting up to 5m0s; a battery device only listens while polling its parent
  ok  temperature
```

What is about to be asked for is printed before it is asked, including what the
raw threshold works out to.

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

`--show` asks a device what it currently holds and changes nothing:

```console
$ gzb reporting -show "outside #2 thermo" temperature temperature
outside #2 thermo (0x4BCD) endpoint 1, temperature
  waiting up to 5m0s; a battery device only listens while polling its parent
  temperature              every 5s to 1h0m0s, on a change of 20 (int16)
```

It is worth knowing about before you need it, because the failure it diagnoses
is a silent one. A configure command answers with a status, which tells you the
device accepted an instruction — not what it now holds. The two come apart
precisely when it matters, because an attribute that has been switched off
looks exactly like one that has nothing new to say:

```console
  temperature              not reported
```

Reading it back is the only thing that tells those apart, and the only way to
confirm that an undo undid anything.

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

### Asking until something answers

A sleepy device is the hard case, and the reason one send is never enough is
worth being precise about. A message for a sleepy child waits in the NCP's
indirect queue until the device polls its parent, and the NCP discards it after
[indirect transmission timeout](#indirect-transmission-timeout) seconds —
thirty, as gzb configures it. Wait longer than that and you are waiting on a
message that no longer exists. The patience is spent in the wrong place: the
host is being patient about something the adapter has already given up on.

So every request repeats itself. `ezsp.Conn.Request` re-sends every ten
seconds, which is shorter than the queue's own patience on purpose, so that
copies overlap and one is always live whenever the device finally wakes. The
reply subscription spans all of them, so an answer cannot land in the gap
between two attempts. A timeout says how many attempts went out and over how
long, because "no reply after one attempt" and "no reply after thirty" are
different problems.

This is why the default timeouts are long — five minutes for an attribute
command, ninety seconds for each step of an interview. Measured against these
sensors, reaching one took anywhere from seconds to nine minutes depending on
where in its sleep cycle the request arrived. A default that gives up first
turns a working command into one that looks broken. `--timeout` is there for
when you would rather be told quickly that a device is asleep.

Repeating a message means it may be delivered more than once, so only requests
that are safe to repeat go through this path. Reading an attribute, writing
one, and configuring reporting all are: the second copy asks for exactly what
the first did.

## Operating a light

Reading and writing attributes is enough to watch a device and not enough to
work one. A light's `CurrentHue` is read-only and a device says so:

```console
$ gzb write light1 color hue 0
light1 (0xA489) endpoint 1, color
  hue                      = 0 (uint8)
  waiting up to 5m0s; a battery device only listens while polling its parent
  !   hue: read only
```

Colour and brightness are changed by cluster-specific commands, which are the
other half of the ZCL: not "set this attribute" but "do this thing", with the
device updating its own attributes as a result. The difference on the wire is
one bit in the frame control field, and it is not a forgiving one — command
`0x06` is Move to Hue and Saturation on the colour cluster, Step (with On/Off)
on the level cluster, and Configure Reporting profile-wide. The same byte, three
unrelated meanings. `zcl.ClusterCommandName` therefore refuses to render one
without its cluster, so a refusal cannot be reported against the wrong command.

A command has no response of its own — the light simply acts — so the Default
Response is the only evidence it arrived and was accepted. gzb leaves it
enabled and waits for it, on the same reasoning as [being refused rather than
ignored](#being-refused-rather-than-ignored).

### A vocabulary rather than a flag

`gzb light` takes words, not options:

```console
$ gzb light light1 red dim
light1 (0xA489) endpoint 1
  hue 0°, saturation 100%
  level 63 (25%)
  ok
```

The grammar is one rule: **a plain word is a place to go to, and its comparative
is a distance to move.** `dim` puts a light at a quarter brightness; `dimmer`
takes a quarter off wherever it happens to be now. The second is a ZCL Step
command, so the device does the arithmetic against its own state — no read
first, and saying it twice compounds.

```
on, off, toggle
full, bright, half, dim, low, min, faint    a brightness to go to
brighter, up, dimmer, darker, down          a step from where it is
40%                                         a brightness to go to
blue, cyan, green, magenta, orange,
pink, purple, red, yellow                   a colour
candle, cool, day, soft, warm, white        a white point
#ff8800                                     a colour as sRGB
2700k                                       a white point in kelvin
hue:169/254                                 an exact hue and saturation
level:127                                   an exact level
```

The last two are the escape hatch. `blue` is a colour someone chose; `hue:169`
is the colour a lamp was actually found at, and the two are not the same thing
when the job is to put a device back exactly as it was. They are what
`make recapture-light` restores with.

The parsing lives in `zigbee.ParseActions`, not in the flag parser, because the
CLI is not the interesting caller. `light1 brighter` typed at a REPL prompt is
the same phrase, and should not need a second implementation of the grammar to
reach the same commands. [The prompt](#a-prompt-for-the-tab-key) is that
caller, and it does not.

### Asking the lamp rather than keeping a table of lamps

Lights disagree about how to be told a colour. Some take hue and saturation,
some take CIE 1931 xy, some can only move a white point along the Planckian
locus. The generic answer is to ask: `ColorCapabilities` (`0x400A`) says what a
lamp accepts, and the colour is converted at the last moment to suit.

So a colour is held in a device-independent form and rendered on the way out —
hue and saturation where the lamp has them, xy where it does not, a colour
temperature where that is all there is. The conversions are the sRGB matrix and
Kim et al.'s fit to the Planckian locus, and they are checked against the
published chromaticities rather than against gzb's own output, because a wrong
matrix is perfectly self-consistent:

```
red    (0.6401, 0.3300)      sRGB primary (0.640, 0.330)
green  (0.3000, 0.6000)      sRGB primary (0.300, 0.600)
blue   (0.1500, 0.0600)      sRGB primary (0.150, 0.060)
warm   (0.4591, 0.4106)      2700 K on the Planckian locus
```

A lamp that can only do colour temperature is told that `red` is not something
it can be, rather than being sent an approximation that arrives as beige.

### Setting a light that is off

`gzb light` sends Move to Level, not Move to Level (with On/Off). The
distinction is the whole reason a motion light can be configured in daylight:
the plain command sets how bright the lamp is without deciding whether it is
lit, so a light that is off stays off.

```console
$ gzb light light1 red dim
light1 (0xA489) endpoint 1
  hue 0°, saturation 100%
  level 63 (25%)
  ok
$ gzb read light1 on/off 0x0000
light1 (0xA489) endpoint 1, on/off
  waiting up to 5m0s; a battery device only listens while polling its parent
  on/off                   false (bool)
```

That covers what the lamp is doing. What it will do next is a different
attribute. When something other than gzb turns a light on — a motion sensor, a
wall switch — the level it comes up at is `OnLevel` (`0x0011`), and `--persist`
writes it:

```console
$ gzb light -persist light1 red dim
light1 (0xA489) endpoint 1
  hue 0°, saturation 100%
  level 63 (25%)
  on level 63 — what it returns to when something else switches it on
  ok
```

Without it, a lamp switched on by its own sensor is free to come back at
whatever level it last decided on. There is no equivalent standard attribute
for a startup hue — colour is held in device NVM and restored, in practice, but
the specification does not promise it, so the honest thing is to verify a
colour survives a real off/on cycle rather than to assume it.

## A prompt, for the Tab key

Every command above opens the adapter, resets it, waits for the network to come
back and closes it again: a second or two each, so a light told three things is
told them three seconds apart. `gzb repl` pays that once and takes the same
commands at a prompt. The reason it exists, though, is completion:

```console
$ gzb repl
gzb on /dev/ttyUSB0: 11 device(s) in the registry, 8 interviewed.
Tab completes; `help` lists commands; Ctrl-D quits.

gzb> light <Tab><Tab>
  light1  light2
gzb> light 1 <Tab>
  blue      cool      dim       full      magenta   orange    soft      white
  bright    cyan      dimmer    green     min       pink      toggle    yellow
  brighter  darker    down      half      off       purple    up
  candle    day       faint     low       on        red       warm
gzb> light 1 toggle
light1 (0xA489) endpoint 1
  toggle
  ok
gzb> read "living room thermo" <Tab>
  0x0020       0xFC57       humidity     power
  0xFC11       basic        identify     temperature
```

What Tab offers is drawn from the interviews, not from a list. `light` offers
the devices whose interview found the On/Off cluster; `light 1` offers the
words that light understands — the colours because it has Colour Control,
`brighter` because it has Level Control, where a plain plug would get `on`,
`off` and `toggle` and nothing about red. `read <device>` offers the clusters
that device's interview found, by name where gzb has one and as hex where it
does not, and after one of those the attributes gzb knows on it, minus any
already typed. A device nothing has interviewed is not offered as a light,
because nothing is known about it; `interview` it and it is.

A shell completion script could not do this without re-deriving the registry
and the attribute table in another language, which is exactly the context that
is easy to put in the wrong place. Here the completer is the code that runs the
command: the grammar is written down once, as data — this argument is a device,
that one a cluster the device carries, the rest attributes on it — and
completing a command and running it cannot disagree about what it takes.

`light 1` means light1. A device argument is resolved first among the devices
that carry the command's cluster and only then across the whole registry, so
`1` — ambiguous among light1, emylo1 and outside #1 thermo — is unambiguous
among the lights. The command line resolves the same way, so `gzb light 1 off`
works there too. Nothing that resolved before stops resolving: the whole
registry is still tried when the scope has no match, and an ambiguity is still
an error naming the candidates rather than a guess.

`join` is at the prompt too, and it is where a session earns its keep. The
best moment to interview a battery device is [immediately after it
joins](#interviewing-something-that-is-asleep), while it is still awake, and
from the shell that moment is spent starting another process and resetting
the adapter. Here the join window closes, the new device is already in the
registry the prompt completes from, and `interview` it is the next line.
`join -verbose` shows every frame the device sends after joining, through the
session's own readings loop rather than a second one; `monitor -raw` shows
the same frames outside a join.

Names with spaces complete with their quotes — `read liv<Tab>` becomes
`read "living room thermo" ` — and double quotes are the prompt's only quoting,
because it is not a shell. History persists in a file beside the registry.
Reports keep being recorded to the registry for as long as the session is open,
and `monitor` prints them until Enter. Ctrl-C interrupts a command that is
waiting on a sleepy device without ending the session — in raw mode no signal
arrives, so the session reads the key itself — and quits at an empty prompt.

Without a terminal the same commands are read one per line from standard
input, which is how the state of a light is recorded and checked after a test:

```console
$ printf 'read 1 on/off on/off\nread 1 level level\n' | gzb repl
```

The line editor is `golang.org/x/term`, the Go project's own, which does raw
mode, history and a Tab hook. Two things it does not do are done here. It holds
its lock while asking about a key, so a list of candidates cannot go through
it; the list is written straight to the terminal and the prompt and line are
redrawn exactly as the editor drew them, cursor included, which is the only
thing the editor keeps track of. And raw mode stops the terminal turning `\n`
into `\r\n`, which every printer in gzb relies on, so that one translation is
put back with a `termios` call after `MakeRaw`.

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

### Keeping the transcripts honest

The console blocks in this file are output, not illustration, and they are the
part most able to quietly stop being true — a renamed attribute or a changed
column is enough. Two things hold them down.

The golden tests in `cmd/gzb` pin the rendering: they run the real printers
over values two sensors actually returned, and fail if a line changes shape. So
a block cannot drift from the code without a test noticing.

What those tests cannot show is that a device ever said it, so the commands are
runnable in one go:

```console
$ make recapture DEVICE="living room thermo"
```

That runs the read, the refused write and the reporting question, and writes
each as a fenced block — to the terminal so a run taking minutes can be
watched, and to `recapture.md` so the result can be read back from a file
rather than copied out of a scrollback. `RECAPTURE_OUT` chooses the path. All
three commands are read-only: the write targets a measured value, which a
device refuses.

The `$ gzb ...` line in each block is quoted so it can actually be pasted. That
sounds like a detail until a device is called `outside #2 thermo`, where an
unquoted `#` starts a shell comment and silently truncates the command.

The reporting configuration is the one capture that changes the device, so it
is opt-in:

```console
$ make recapture DEVICE="living room thermo" CONFIGURE=--configure
```

Before changing anything it reads the configuration the device currently holds,
and it refuses to continue if that attribute is not being reported — because
"off" is not the same state as "never configured", and a restore to a state you
cannot describe is a guess. Afterwards it puts back exactly what it recorded,
retrying until the device is awake to accept it, and then reads it back, on the
grounds that a write reporting success is not evidence of what a device holds.

That order — record, change, restore, verify — is not fussiness. Getting it
wrong once here silenced a working sensor, and the silence was indistinguishable
from a room whose temperature had not changed.

A light is not a sensor, so it has its own capture with the same shape:

```console
$ make recapture-light LIGHT=light1
$ make recapture-light LIGHT=light1 CONFIGURE=--configure
```

Nothing it runs turns a light on — every command it sends sets a colour or a
level, both of which leave the on/off state alone — so it is safe against a
light that is meant to stay dark. The state it records and puts back is a
colour, a brightness and an `OnLevel` rather than a reporting configuration,
and it restores them with the exact forms (`hue:0/254`, `level:63`) for the
reason those exist: a restore that rounds a hue to the nearest named colour has
not restored anything.

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
- Requests that repeat until answered, which is what makes any of the above
  reach a battery device at all
- Attribute reads on demand, including per-attribute refusals, recorded to the
  registry the same way a report is
- Attribute writes, including the encoding a device rejects and the read-only
  attribute it will not change
- Reading a device's reporting configuration back, which is what tells a
  configuration that took from one that did not
- Reporting configuration, accepted by a real sensor and restored to its own
  default afterwards
- Device names, and addressing a device by name wherever one is taken
- Pairing as a library concern: `Coordinator.Joins` streams device arrivals,
  departures and the join window opening and closing, each arrival recorded
  to the registry as it happens; `gzb join` consumes it like any application
- Registry access and naming through the public API (`Devices`, `Device`,
  `SetName`, `ClearName`), safe from any goroutine alongside the readings loop
- Outbound unicast, exercised by the Time cluster responder
- Cluster-specific commands, which is what operating a device takes as opposed
  to watching one: on/off, level and colour, with the colour command chosen
  from what a lamp says it can be told rather than from a table of models
- A light vocabulary — `red`, `dim`, `brighter`, `40%`, `2700k` — parsed once
  in the `zigbee` package rather than in a flag parser, so a REPL and the CLI
  can share it
- `probe`, `network`, `permit-join`, `join`, `devices`, `name`, `monitor`,
  `interview`, `read`, `write`, `light`, `reporting`, `config`, `repl`
- A prompt with Tab completion drawn from the interviews: the lights, the
  words each light understands, a device's clusters and their attributes —
  driven through a pseudo-terminal against the real network, lights toggled
  and put back
- Device resolution scoped to the command's cluster, so `light 1` is light1
- Pairing from the prompt: `join` shares its watch with the command line and
  leaves the new device one `interview` away while it is still awake
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

- Binding management


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
