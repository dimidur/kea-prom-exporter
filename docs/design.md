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

## Compatibility is checked against a real Kea

The unit tests run on captures from one Kea release, so what a new release
changes is exactly what they cannot see. The compatibility check
(`.github/workflows/compat.yml`) runs the exporter against a real Kea instead,
and holds it to the properties the exporter depends on rather than to
particular numbers.

- **One version.** The supported Kea is pinned by digest as well as version,
  so the check is reproducible. Renovate moves the pin within 3.x only:
  supporting Kea 4 is a decision to make, not an update to take.
- **When.** On the Renovate pull request that moves the pin, which tests the
  new Kea against current `main`, and on a pull request that changes the check
  itself. Not on other pull requests: supporting a new Kea is a task of its
  own, and must not block unrelated work. Before every release, against the Kea
  the tagged tree pins, so a regression in the exporter cannot reach a release.
- **What a failure means.** On a Renovate pull request, a break the new Kea
  brings: the check opens an issue, and the fix comes with a unit test, so the
  corrected behaviour stays tested after the pin moves on.
- **What it leaves out.** The Kea under test has one subnet and no HA hook, so
  the values of the allocation-failure and HA metrics stay covered by the unit
  tests alone.

## Coverage is held to a ratchet

Coverage that is only reported drifts down one untested branch at a time. The
ratchet (`scripts/coverage-ratchet.sh`) holds every file, and the total, to a
floor that only rises.

- **Floors with slack.** A floor sits 2 points below the coverage that set it
  and never above 90. It catches a file losing its tests without demanding
  full coverage of every change; on a small file, 2 points is less than one
  statement.
- **Only `main` raises them.** After a merge, `main` raises the floors once its
  test job is green, so a raised floor only ever reflects merged code. A pull
  request is checked against the floors in its tree and normally leaves them
  alone; editing them needs a strong reason, stated for review.
- **A bot commits the raised floors.** `main` accepts only commits that passed
  the required checks, which a commit made by the run cannot have yet. A
  dedicated GitHub App, with no permission beyond the repository contents,
  commits the new `coverage-baseline.json` with `[skip ci]`. It may bypass
  only the required checks: deletion, force-pushes and unsigned commits are
  refused by a ruleset with no bypass at all. It commits through the GraphQL
  `createCommitOnBranch` mutation rather than `git push`: GitHub signs such
  commits, and the mutation takes no author, so a request authenticated as
  the app gets a verified bot signature ([mutation][create-commit],
  [bot signatures][bot-signatures]). Its key is a secret of an environment
  only `main` may use, and its token is minted in a job of its own, so no
  other branch's workflow and no code the tests run can reach it.
- **Only a ratchet is committed.** The raised floors come from the test job,
  which runs the dependencies' code, so before committing them the bot's job
  checks them against the baseline at the measured commit: every floor at
  least its old value and at most 100, the cap and buffer unchanged, and a
  file's floors dropped only once the file is gone.
- **A rejected commit fails the run.** The one rejection tolerated is `main`
  having moved on during the run, since the run for that merge raises the
  floors. Any other would leave the floors stale with nothing to show for it.
- **The badge shows measured coverage,** since a floor only says how far
  coverage may fall. How it is published: "The badges are committed by CI".

[create-commit]: https://docs.github.com/en/graphql/reference/commits#mutation-createcommitonbranch
[bot-signatures]: https://docs.github.com/en/authentication/managing-commit-signature-verification/about-commit-signature-verification#signature-verification-for-bots

## The badges are committed by CI

GitHub shows a README image from another site through its image proxy, which
gives up after about four seconds ([shields#1568][shields-1568]). A badge
service such as shields.io renders a dynamic badge on request and caches it
for minutes, so the first view after each expiry waits on the service's own
fetch and often misses that limit; the badge then shows as broken until a
reload. An image GitHub serves itself, from `raw.githubusercontent.com`, does
not pass through the proxy at all.

So CI commits the version and coverage badges as SVG files to the `badges`
branch, which holds only them and a README saying what the branch is for,
and from which the README links them (`scripts/badge.sh`). Each SVG is
shields.io's static badge, fetched in CI, where no proxy is waiting on it.
Fetching rather than drawing with a third-party action means no third-party
action runs in the job that holds the write token, so fetching and
committing share one step. The SVG is the one input CI does not pin, so it
is committed only while it is badge-sized, has an SVG root and holds no
script, embedded HTML, link or event handler.

Whether a badge may be published, and for the version what it shows, is
read from GitHub in that step, never handed down by an earlier job, so a
re-run of a failed job, however late, commits nothing stale:

- **Coverage** is the figure the run measured for its own commit, committed
  only while that commit is still `main`'s newest, so of two merges finishing
  out of order the older does not replace the newer one's figure.
- **Version** is the repository's Latest release, read once the release run
  has created its release, after the images are on Docker Hub. It follows
  the choice of Latest rather than making one of its own, and a failure there
  holds back nothing of the release.

A branch rather than GitHub Pages, which replaces the whole site with each
deploy, so that whichever run deployed would have to regenerate the other's
badge too. GitHub signs the commits, made through the same GraphQL mutation
as the coverage floors. Each is made against the branch head read before the
source, and GitHub refuses it once anything else has been committed; the
retry reads both again. Both sources only move forward, so the badge left on
the branch never shows the older of two runs racing. Made with the
workflow's own token, the commits start no workflow.

The README of v0.3.1 reads a shields.io endpoint file on GitHub Pages
instead. That site is still served but no longer updated, so that README
shows the coverage it last held, and Pages stays enabled for it.

Docker pulls is a count only Docker Hub has, so it stays a shields.io badge;
its six-hour cache makes a miss rare.

[shields-1568]: https://github.com/badges/shields/issues/1568

## Only the highest release names the latest

A release that is not the highest stable one, such as a patch to an older
line, is published like any other: its own image tag, release notes. It
moves nothing that names the latest release: the image's `latest` tag, the
Docker Hub overview, GitHub's Latest release, and so the version badge,
which shows it. These would otherwise follow whichever release came last, so
the newest line would appear to step back. In the same way, a release moves
its minor line's tag only if it is the highest in that line.

Each step that moves the image tags, the overview or the Latest release
decides immediately before it does, from the stable tags pushed by then, so
re-running a failed job decides afresh rather than reusing an answer from
before a higher release; the version badge follows the Latest release. It
compares against the tags rather than the releases: a tag exists from the
moment it is pushed, while its release appears only once its run has
published, so two releases pushed close together still see each other. The
same holds for a stable tag whose run never publishes: it still holds back
every lower release.

For the image, that means building and pushing the version tag alone, then
deciding, then moving `latest` and the minor line's tag onto the pushed
version without rebuilding (`scripts/tag-image.sh`). Deciding before the
build would leave the build's minutes between the decision and the move,
long enough for a higher release to move `latest` first and be overwritten.
