# How Kea actually behaves

Why this exporter is shaped the way it is. Each claim here was read from the
Kea source or measured against a running server, and several contradict the
Kea ARM — the source wins.

This is the reference the code points at. Code comments say what a line does
and why it is local to that line; the derivations live here, so they are in
one place and can be checked as a whole.

Symbols are named rather than cited by line number, because line numbers rot
silently and a stale-but-precise-looking citation is worse than none. They
hold across the [supported Kea releases](../README.md#supported-kea-versions).

## Allocation failures are two partitions, not one

`AllocEngine::allocateUnreservedLease4` counts a single failure in **two
independent branches**:

- `shared-network` **xor** `subnet` — where the lookup was scoped
- `no-pools` **xor** `v4-allocation-fail` — why it failed

So `v4-allocation-fail` is **not** the total. It is the `else` of "no attempts
were made", a peer of `no-pools`, which is why it is exported under the label
`attempts-exhausted` rather than as a sum. The ARM calls it the "total address
allocation failures"; the source does not support that.

Each branch sums to the real total on its own. Collapsing both into one
`reason` label would therefore double every `sum()`, which is why there are two
metrics rather than one.

`v4-allocation-fail-classes` is not exported at all:
`Dhcpv4Exchange::classifyPacket` adds the `ALL` class to every packet, so the
class set is never empty by the time allocation runs, and the counter tracks
the by-cause total rather than isolating a classification-driven subset.

### Why the global keys, not the per-subnet ones

Unlike the lease gauges, allocation failures are read from the **global**
statistics. Kea initialises the names in its `dhcp4_statistics` set to zero at
startup, and the per-subnet `subnet[N].v4-allocation-fail*` observations are
not among them — they spring into existence on the first failure.

Exporting only the per-subnet form therefore left a healthy server with **no
series at all**, so an alert could not distinguish "no failures" from "exporter
broken". Losing the subnet dimension is a real cost, recorded as debt rather
than pretended away.

## Drop reasons are an exact partition

Kea records a specific reason and bumps the total alongside it —
`Dhcpv4Srv::processPacket` carries the comment *"Specific drop cause stat was
increased by accept\* methods"*. Several sites bump the total, and each records
a reason at the same time, which is what keeps the partition exact:

```promql
sum(kea_dhcp4_packets_dropped_by_reason_total) == kea_dhcp4_packets_dropped_total
```

The total is kept as its own metric anyway, because it stays correct when a Kea
release adds a reason this exporter does not know yet.

### No relation holds between `pkt4-received` and the typed counters

The gap between them is *not* the set of drops occurring before typing, and no
fixed "pre-typing" set exists. `HAImpl::buffer4Receive` drops `not-for-us` in
the `buffer4_receive` callout, which `Dhcpv4Srv::processPacket` runs **before**
it types the packet, while `Dhcpv4Srv::acceptServerId` bumps the same statistic
**after** typing.

A live hot-standby standby shows the consequence: 98,836 `pkt4-not-for-us` with
every typed counter at zero. Two earlier versions of this exporter asserted such
a relation — first as an equality, then as a band — and the capture disproved
both.

## Declined addresses are a subset of assigned

`Dhcpv4Srv::declineLease` deliberately keeps a declined lease counted in
`assigned-addresses`, so pool utilisation stays meaningful. `assigned /
capacity` is therefore already correct, and `assigned + declined` over-counts.

## Server-wide lease counters duplicate the per-subnet ones

Kea increments `assigned-addresses` and its `subnet[N]` equivalent for the same
event, so the global is the cross-subnet sum. Exporting both would double any
`sum()`. The per-subnet form is exported, being strictly more informative.

The same applies to `pkt4-received` and `pkt4-sent`: they are grand totals, not
packet types — `pkt4-sent` equals `pkt4-offer-sent + pkt4-ack-sent` exactly.
Exporting them beside the per-type series made
`sum(kea_dhcp4_packets_sent_total)` return double.

## HA relationships have no identifier

`HAImpl::commandProcessed` builds each entry of `high-availability` from
exactly two keys, `ha-servers` and `ha-mode`. There is no name, no index, and
no identifier of any kind — in the response or in the configuration. Stork
synthesises one by naming a relationship after a single peer
(`DetectHAServices`).

The two labels this exporter exports are therefore derived from the server
names Kea does report. See the
[README](../README.md#ha) for what they mean operationally.

### Why `relationship` is the lower of the two names

Kea requires server names to be **unique across every relationship on a
daemon**, and says so when rejecting a configuration:
`HAConfigParser::parseOne` reports *"server names must be unique for different
relationships"*, backed by `HARelationshipMapper::map` throwing on the repeated
key underneath it.

`min()` over two names drawn from a daemon-globally-unique set is therefore
itself unique, and — because both nodes of a pair compute it over the same two
strings — identical from both sides. That is what lets the two exporters' views
of one relationship be joined, which a peer-derived label cannot do.

Stork reaches the same "name it after one real member" conclusion but can
afford an asymmetric name, because it reconciles the two sides by scanning both
(`HAStatusPuller.pullDataForDaemon`). A Prometheus label cannot express a
set-membership match, hence `min()` rather than the local name.

### The partner state is empty until first contact

`CommunicationState::getReport` writes `last-state` as
`stateToString(getPartnerState())` inside a `try`/`catch`, and the constructor
starts `partner_state_` at **-1**, which `stateToString` rejects. A partner
never contacted therefore yields `""` rather than a state name, and a
`state=""` series would be the same class of falsehood as reporting `age: 0` as
"contacted 0 seconds ago".

Gating on `last-state` is **equivalent** to gating on `in-touch` for every input
Kea can produce: `getReport` derives `in-touch` from `getPartnerState() > 0`,
and `setPartnerStateInternal` only ever writes valid states. The code gates on
`last-state` because that is the field the metric reads.

`unavailable` is a real state, written by
`CommunicationState::setPartnerUnavailable` once Kea concludes the partner is
gone, and `in-touch` is **true** then. It is the most alertable partner state
there is and must not be suppressed.

### `communication-interrupted` implies an age floor

`CommunicationState::isCommunicationInterrupted` is exactly
`getDurationInMillisecs() > getMaxResponseDelay()`, and `max-response-delay`
defaults to 60000 ms. So `communication-interrupted: true` implies `age >= 60`
at default configuration — a fixture claiming otherwise is describing a server
that cannot exist.

### `ha-mode` reports the first relationship's mode

`HAImpl::commandProcessed` stamps every entry with `config_->get()`, and
`HARelationshipMapper::get` returns the first element rather than the one being
iterated. With several relationships, every entry reports relationship 0's
mode.

This is **latent, not live**: `HAConfigParser::validateRelationships` requires
every relationship to be hot-standby once there is more than one, so all the
modes are equal and the first is always the right answer. The label becomes
wrong only if Kea lifts that restriction. Unchanged on Kea's `master` at the
time of writing.

## Design consequences

**A duplicate label set fails the whole scrape.** Prometheus fails the entire
`Gather` on a duplicate series, not the one metric — so `/metrics` answers 500
and every metric is lost, including the subnet gauges that have nothing to do
with HA. This is why identities are de-duplicated even though Kea's own
uniqueness rule makes collisions impossible: the exporter parses JSON, and the
JSON might not have come from a healthy Kea.

The rule applied throughout: **defend where a malformed field costs more than
itself, document where it does not.** The `last-state` gate is deliberately not
hardened against a Kea reporting a state it should not, because getting that
wrong costs one series rather than the scrape.

**A label-arity mismatch kills the process.** Passing the wrong number of label
values panics inside `MustNewConstMetric`, and `Registry.Gather` runs
collectors *without* recovering — so the panic is not a failed scrape, it is a
dead exporter. This is why `classifyStat` returns label values as a slice
rather than a string plus a "has a label" flag: the arity then has exactly one
source, checked in one place.

**Table entries are shared, not copied.** The mapping tables are package-level,
so their label slices are handed out by reference. One `append` at a call site
would write through into the table and corrupt it for the rest of the process,
which is why the classifier clones before returning.
