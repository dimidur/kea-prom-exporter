# Design notes

Why the exporter is built the way it is. Each section states a decision and the
fact about Kea's output behind it. How to read the metrics is in the
[README](../README.md#metrics); how Kea produces its output is out of scope.

## Allocation failures are two metrics

Kea counts each failed dynamic address allocation twice: once by where the
client was looked up, and once by why it failed. Each count sums to the same
number of failures, so they are two metrics, by scope and by cause. One metric
carrying both as labels would double every `sum()`.

`v4-allocation-fail-classes` is not exported: Kea puts every packet in at least
one class, so it counts the same failures as the by-cause metric.

The server-wide failure statistics are exported, not the per-subnet ones. Kea
reports the server-wide ones from startup and the per-subnet ones only after a
first failure (the live capture `testdata/statistic-get-all.json` has the
server-wide ones and none of the per-subnet ones), so a healthy server would
otherwise have no series at all, and an alert could not tell "no failures" from
"exporter broken". The cost is the subnet dimension.

## Drop reasons and the drop total

The drop reasons sum to the drop total in every capture in `testdata/`. Both are
exported, as two metrics rather than the total plus a label: the reasons say
why packets were dropped, and the total stays complete when a Kea release adds
a reason the exporter does not know.

Between packets received and the per-type counters, the only relation assumed
is that the per-type counters never exceed packets received. A packet can be
counted as received and dropped without being counted by type: in the live
capture `testdata/statistic-get-all-standby.json`, from a hot-standby standby,
all 98,836 packets received were dropped as not-for-us and every per-type
counter is 0.

## Totals of other series are not exported

A statistic that counts what exported series already count would double any
`sum()`, so it is ignored:

- `pkt4-received` and `pkt4-sent`, grand totals rather than packet types;
- the server-wide `assigned-addresses` and `declined-addresses`, the totals of
  the per-subnet ones;
- the per-pool statistics (`subnet[N].pool[M].<name>`): the address gauges are
  exported per subnet, and the per-pool ones count only the addresses inside a
  pool. Every per-pool key is ignored, including ones the exporter does not
  otherwise know.

## Statistics the exporter cannot export

A statistic the exporter has no mapping for is counted, not exported; per-pool
keys are the exception, ignored as above. A mapped statistic whose value is not
a number is counted the same way, since it means Kea changed what the statistic
holds.

A mapped statistic with no value, an empty sample list or a null, is left out of
that scrape and not counted: its mapping is complete. Kea itself does not send
either: a statistic with no samples makes Kea fail the whole
`statistic-get-all` command instead, which shows as `kea_up 0`.

## HA relationship labels

Kea reports each HA relationship without a name, so the exporter derives both
labels from the server names it does report. `relationship` is the lower of the
two names rather than either node's own name: Kea refuses a configuration that
repeats a server name across relationships, so the lower name is unique, and
both nodes of a pair compute the same value. That is what lets the two
exporters' views of one relationship be joined. A label named after the local
server would differ between the two views, and a Prometheus label cannot be
matched against a set of names.

`mode` is exported as Kea reports it, and Kea reports the first relationship's
mode for every relationship. That is harmless while Kea allows several
relationships only when all are hot-standby, and wrong only if Kea lifts that
restriction.

## Partner metrics before Kea has a partner state

Until Kea has a state for the partner, it reports the partner's age as 0 and its
state as an empty string. Exported as they are, those would read as "contacted
0 seconds ago" and `state=""`, so the age is withheld until the partner is in
touch and the state until it is not empty. Kea derives the age, the state and
in-touch from the same partner state, so the two gates are the same condition;
each checks the field its metric reads.

## Defensive choices

The exporter parses JSON that might not have come from a healthy Kea, and
defends where a bad field would cost more than itself:

- **Duplicate HA label sets are made distinct.** A duplicate series fails the
  whole scrape with HTTP 500, losing every metric, so two relationships that
  would share labels get a suffix instead.
- **A subnet key without the `subnet[N].<name>` shape is unmapped.** Read as
  the statistic it resembles, it would export a duplicate of that statistic's
  series.
- **Label values come back from one classifier, as a slice.** A wrong number of
  label values panics, and the panic fails every scrape that reaches the
  statistic.

Where a bad field costs only itself, it is documented rather than defended: a
partner state Kea should not report costs one series.
