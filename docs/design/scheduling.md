# Scheduling: cadence, verification windows, and templates

Working notes for how `vmsync-agent` decides when to sync and when to verify.
Written while adding a verification cadence, because the design took two
corrections along the way and both are worth more than the code.

**Status.** The verify *interval*, schedule templates, and calendar days with
a wall-clock window are built and tested. The agent reporting its effective
schedule back, and the UI's template editor, are designed here and **not
built**.

---

## Where it stands

One `ScheduleEntry` per VM, held by the agent that owns it:

```go
type ScheduleEntry struct {
	VM                    string
	IntervalSeconds       int
	Enabled               bool
	Profile               SyncProfile   // includes Verify
	TargetHost            string
	ShutdownTimeoutSec    int           // 0 inherits the estate default
	VerifyIntervalSeconds int           // how often a sync also verifies
	VerifyDays            string        // or, the same cadence as a calendar:
	VerifyWindow          string        //   "Sun *-*-01..07" + "02:00-12:00"
	Template              string        // "" means the "default" template
}
```

The scheduler ticks every 10s, keeps `nextRun[vm]`, and launches what is due
subject to `max_concurrent_syncs` and per-target slots.

Two existing mechanisms matter to everything below.

**`stagger(vm, interval)`** offsets a VM's first due time by
`FNV-1a(vm) mod interval`, so an agent start does not bunch every VM into the
same minute, and a given VM keeps its slot across restarts.

**`markRunning` sets `now+interval`, not `previous+interval`.** That is what
implements "skip missed slots, at most one catch-up": an agent down for three
hours runs each entry once when it returns rather than firing every interval
it missed.

**`Preset` is the only template-shaped thing that exists**, and it holds a
`Profile` — transport settings only. It templates *how* bytes cross, never
*when*. `ScheduleEntry.Preset` is `json:"-"`: the agent never receives it and
acts on resolved fields.

There is a precedent for the inheritance pattern, too:
`UIConfig.ShutdownTimeoutSec` is an estate default with a per-VM override.

---

## The problem: verification was all-or-nothing per pair

`Verify` is a field on `SyncProfile`, which lives on the entry. So either
*every* sync of a VM verified or none did — and a verify is a full-image read
on both sides.

An operator wanting verification at all had to choose between paying it every
fifteen minutes, stretching the sync interval and losing RPO, or not
verifying. None of those is an answer, which made the feature effectively
unusable on any short cadence.

---

## Built: a verify interval

`VerifyIntervalSeconds` on the entry, `nextVerify[vm]` in the scheduler,
`verifyDue()` deciding per launch. `0` means "every sync": a profile naming a
verify mode is asking to be verified, and *"how often: unspecified"* reads as
*"every time"* — the safe direction, since the other one is a check somebody
believes is running and is not.

### It selects which syncs verify. It is not a second schedule.

That is forced, not chosen. `-verify` compares the target against the backup
job's export **that the copy read from**, so it is a phase of a sync and a
verify-only run does not exist. The cadence therefore answers "does *this*
run also carry `-verify`", which needs no new run type, no second run lock,
and lands the verify on a sync that was going to happen anyway.

### Three properties, each with a failing test behind it

| property | why |
|---|---|
| the first sighting does **not** verify | an agent restart would otherwise verify every VM it manages at once — the exact burst the stagger exists to prevent. Skipping one cycle costs at most one interval |
| `now+interval`, never `previous+interval` | the same rule `markRunning` follows. An agent down a week must come back and verify once, not owe seven |
| staggered per VM, seeded `"verify-"+vm` | a 24h cadence across an estate would otherwise fall due within the same minute of the same night and queue against `max_concurrent_syncs`. Seeded separately from the sync stagger so a VM's verify slot is independent of its copy slot |

### One bug worth recording

The first implementation cleared `entry.Profile.Verify`. That is a **silent
no-op**: `buildRequest` runs earlier in `launchDue` and has already copied
`entry.Profile` into the plan, and the plan is what becomes the command line.
Every run would have verified regardless of cadence.

It now sets `req.Profile.Verify`, and does so **after `s.admit`** — a run that
never starts must not consume a verify slot, because a claimed-but-skipped
slot pushes the next one a full interval away having verified nothing.

---

## Built: calendar days and a wall-clock window

An interval cannot express a maintenance window, and it **drifts**: a VM
verified at 02:00 today is verified mid-morning two months later, during
exactly the hours that were being avoided. The wanted shape is *"first Sunday
of the month, 02:00–12:00"* — day-of-week and day-of-month together, plus a
window.

### Borrow systemd's day syntax; do not invent one

The agent already ships as systemd units
(`contrib/systemd/vmsync-agent-{control-plane,standalone}.service`), so anyone
writing this config is already writing systemd. The wanted rule is a
documented systemd idiom:

```
Sun *-*-01..07      # day-of-week Sunday AND day-of-month 1–7 = the first Sunday
```

The decisive reason is not familiarity, it is testability:
**`systemd-analyze calendar "Sun *-*-01..07"` lets an operator check an
expression before pasting it in.** A bespoke mini-language with no way to test
it is how a monthly verify silently never fires and nobody notices for a year.

Only the **day** part of the expression is used. The window carries the time,
which is a cleaner split than one expression doing both.

### The window makes this simpler than cron, not harder

There is no "when is the next occurrence" to compute — which is where cron
implementations get hairy. The only question is *does now match*:

- day-of-week ∈ set **and** day-of-month ∈ set
- window start ≤ local `now` < window end
- `lastVerified[vm]` is before this window's start — otherwise the 10s tick
  would verify dozens of times across a ten-hour window

A handful of comparisons on `now`, plus one timestamp per VM. No next-fire
arithmetic, no leap handling, no "this expression matches nothing" case.

### Timezone: the agent's, from the system

The agent's host local time, because that is the machine in the datacentre
whose quiet hours these are. The agent takes it from the system, and the
console is to display it rather than assume one.

**Not yet reported, though.** The zone reaches only a local log line on the
verifying run; no report field carries it, so today the console has nothing to
display. That is on the Open list, together with the effective schedule it
belongs beside — both are the same missing piece, which is the agent telling
the console what it actually resolved.

**DST collides with this by construction.** In Europe the transition *is* the
last Sunday of March and October, so any Sunday rule lands on a transition day
twice a year. A 02:00–12:00 window is 9 or 11 hours that day, and in the
spring-forward hour 02:00–03:00 does not exist at all — a window *starting* at
02:00 may not open in some zones.

The mitigation is to treat the window as **wall clock** and never do
arithmetic on its length: ask "is `now`'s local clock inside 02:00–12:00", not
"is `now` within ten hours of the start".

### A missed window is not made up later

If the agent is down for the whole window, that occurrence does not happen.
There is deliberately **no interval backstop**: the operator asked for that
window, and a verify fired outside it would be the thing they were avoiding.
A window missed twice running is a human problem, and the report is where it
should become visible.

### The interval stays for the simple case

Most VMs want "daily-ish"; a few want "first Sunday". An empty calendar falls
back to `VerifyIntervalSeconds`, so the common case stays a single number.

### What shipped: `pkg/schedcal`

Two fields on both `ScheduleEntry` and `ScheduleTemplate`:

```json
"verify_days":   "Sun *-*-01..07",
"verify_window": "02:00-12:00"
```

`verify_days` is the day half of a systemd `OnCalendar` expression:
`[weekdays] [year-month-day]`, either part optional, both matched with **AND**
— which is what makes `Sun` plus `*-*-01..07` mean *the first Sunday* rather
than *Sundays or the first week*. Accepted forms:

| Form | Example | Means |
|---|---|---|
| weekday | `Sun` | every Sunday |
| weekday list | `Sat,Sun` | weekends |
| weekday range | `Mon..Fri` (or `Mon-Fri`) | working days; wraps, so `Fri..Mon` is Fri–Mon |
| day-of-month | `*-*-01` | the 1st |
| day-of-month range | `*-*-01..07` | the first week |
| month | `*-01,07-*` | January and July |
| combined | `Sun *-*-01..07` | the first Sunday of the month |
| empty | `""` | every day |

`verify_window` is `HH:MM-HH:MM`, local, **start inclusive and end exclusive**
so two abutting windows cannot overlap. `22:00-04:00` crosses midnight and
belongs to the day it *opened* on: `Sun 22:00-04:00` runs into Monday morning.
An empty window means the whole day.

**The parser refuses, by name, every real systemd form it does not implement**
— time components in the day expression, `~` for last-of-month, `/` for
repetition, timezone suffixes, and the `daily`/`weekly` shorthands. That is
the point of the strictness: half-understanding `Sun *-*-01..07 02:00:00` by
ignoring the time would verify at the right frequency on the wrong hours and
look like it worked. `systemd-analyze calendar` remains the way to check the
day half before pasting it in.

### What a review of the first cut caught

Six defects, each a variant of the same thing: a schedule that **looks
configured and is not**, failing in the direction nobody checks.

| defect | what it did |
|---|---|
| an empty date component read as "any" | `*-*-` matched 365 days a year instead of 12, silently — measured, not reasoned |
| an impossible date accepted | `*-02-30` parses and then never fires |
| the timezone refusal matched only `UTC` | every other spelling fell through to an unrelated error |
| the time-component suggestion dropped the date part | answered a monthly expression with a weekly one, in the voice of a correction |
| `VerifyFailureReinit` left set when a run does not verify | argv the engine **refuses**: such a VM replicated only inside its window and failed every interval in between |
| the occurrence claimed at launch, never given back | one transient failure at 02:05 cost the VM its whole month |

### What a SECOND review caught, after the first had been fixed

The first review never reached two of its dimensions — the run hit a session
limit — so they were run again later. They found one live bug and five tests
that did not test what their names said.

| defect | what it did |
|---|---|
| `AddDate(0, 0, -1)` in the crossing-midnight branch | half an occurrence lost, once a year, in midnight-gap zones — see above |
| the `years` field was never exercised at all | deleting the year check entirely left the suite green |
| `parseNumSet`'s upper bound was pinned by nothing | `*-13-*` and `*-*-32` are refused by `feasible()` instead, so `*-*-01,32` would have been accepted |
| "a window with no days" asserted only *inside* the window | equally satisfied by "verify on every sync", so a window-only entry having no cadence at all would have passed |
| no days-only entry was ever resolved | one clause of the inherit-as-a-unit guard could be deleted freely |
| `releaseVerifyOccurrence` only ever met days+window | its guard could be flipped to `||` and only the untested shapes broke |

The method is what made the difference, and it is worth stating: for each
property a test claims to pin, **apply a one-line mutation to the source and
run it**. Only mutations that survive get reported. Four of the six above were
found that way; reading the tests had not found them, and neither had writing
them.

Two more, outside this document's scope but from the same pass: bench stage 15
treated any non-zero exit as proof its fault had fired, so deleting the fault
would have made the stage pass; and `cmd/vmsync` was missing from CI's `-race`
step while `pkg/schedcal`, which has no concurrency at all, was in it.

Two more were about validation reaching only half the deployments. A
**template carrying only the estate-wide window** was refused, because
`Validate` judged the calendar against the template's own verify mode — which
forbade the exact shape templates are most worth having. And the
**control-plane path ran no cadence check at all**: only the standalone loader
did, so a published entry with both forms, or an unparsable calendar, was
adopted without a word.

The last one is the most instructive. `Complaints()` judged **raw** entries, so
after templates landed every templated host logged *"has interval_seconds 0 and
will never run"* about entries that ran perfectly well. Warnings that are false
about a working system are worse than none: they send an operator hunting a
fault that is not there, and they teach them to ignore the channel that exists
to catch the real one.

### Two things the design above got wrong

**"A handful of comparisons on `now`" understated the DST work.** Two places
need calendar arithmetic rather than duration arithmetic, and both are silent
when wrong:

- The window start is built with `time.Date`, not by subtracting from `now`.
  A start time that does not exist gets resolved by the offset change, and
  **the direction is not fixed**: Europe/Paris moves 02:00 forward to 03:00,
  while a zone whose gap begins at midnight moves it *backward* onto the
  previous evening. An earlier version of this document claimed it always
  normalises forward. That was wrong, and believing it is what made the next
  mistake look safe.
- **The crossing-midnight lookback took three attempts**, and the middle one
  is the instructive part.

  `-24h` was the first, and it is wrong whenever the day it crosses is not 24
  hours long: after a 23-hour Sunday, 24 hours before Monday 00:30 is
  Saturday 23:30, so a `Sun 22:00-04:00` window reports closed on precisely
  the night it should be open.

  `AddDate(0, 0, -1)` was the fix for that, and it was **also wrong** — just
  in different zones, which is why it survived. It carries *this* instant's
  wall clock onto the previous date, and this branch only runs in the small
  hours. Where the spring-forward gap begins at **midnight**, that wall clock
  does not exist on the previous day and Go resolves it backward across
  midnight: from Monday 00:30 it returns **Saturday** 23:30, skipping Sunday
  entirely. Measured: a Sunday-night window reported closed for all sixty
  minutes of Monday's small hours in America/Santiago, America/Havana and
  Atlantic/Azores. Half the occurrence, gone silently, once a year.

  The answer is `time.Date(y, m, d-1, 12, ...)` — yesterday's **date**, taken
  at midday. Only the date is being asked about, since `Days.Matches` reads
  nothing finer than the day, and no zone has ever moved its clock across
  noon. It is the one time of day that cannot be normalised onto a different
  date.

  The lesson is not "use `AddDate`" or "use `time.Date`". It is that
  *preserving the wall clock* is the wrong invariant when the question is
  which calendar day it was, and that a DST bug which spares your own
  timezone will sit there for as long as you only test in it.

**The interval's stagger and first-sighting skip are wrong for a window.**
Both exist to break up a restart burst, and cost one cycle. Under a monthly
window a skipped cycle costs a *month*, and a stagger can push a VM past the
window's end. The window is the burst control — the operator already said
where the load may land — so the calendar path does neither.

### The two forms are one setting, so both at once is refused

`verify_interval_seconds` and the calendar are alternatives. There is no
reading of *"every 86400 seconds, and also the first Sunday"* an operator
could predict, and whichever half silently lost would be the one they thought
they were configuring — so an object carrying both is refused, naming both
spellings.

**Refused where a person typed it, complained about where a program sent it.**
A standalone schedule file is rejected outright; a control-plane document is
adopted and named in `Complaints()`, because a separately-versioned program's
output is input to survive. That split is the agent's existing doctrine, not a
new rule — the same one `shutdownTimeoutFor` follows by clamping what a UI
sends while the file loader refuses it.

That forces a resolution rule: **the verify cadence inherits as one unit.**
An entry stating either form states its whole cadence and inherits neither
half of the other. Field-by-field inheritance would manufacture exactly the
combination the validator refuses — an entry saying *"the first Sunday"* under
a template saying *"every 86400 seconds"* would come out holding both, a
contradiction neither author wrote. The verify *mode* (`profile.verify`) still
inherits normally, because it answers a different question: the calendar says
how often, the mode says what.

---

## Built: schedule templates

Today a newly discovered VM is **not replicated until somebody creates its
entry**. A default template inverts that, so the cost of forgetting becomes
"protected with generic settings" rather than "not protected" — which is the
better failure mode, and the actual argument for templates. Saving typing
across an estate is a side benefit.

A template holds **cadence** — sync interval, verify cadence — plus the
profile the VMs using it should run with.

An earlier draft of this section said the template should *reference a preset
by name* rather than carry a profile, on the grounds that two mechanisms for
one job will disagree. That is not implementable: presets are a UI concept and
**the agent has no preset table** — `ScheduleEntry.Preset` is `json:"-"`
precisely because the agent never receives it.

The resolution is the one entries already use. A template carries a concrete
`SyncProfile`; the UI fills it *from* a preset at authoring time and records
which one for the "re-apply" offer, and does not send that. So presets still
resolve at edit time and templates resolve at run time, which is the split the
next section argues for anyway.

### What a VM without a template gets

`ScheduleEntry.Template` names one; empty means the default. An entry's own
fields are **overrides** — 0 or "" inherits from the template — with one
exception:

`Enabled` is authoritative on an explicit entry and never inherited, because
`false` cannot be told from "unset" in a bool and the field's whole job is to
keep an entry visible while stopping it from running. That also supplies the
opt-out: a VM covered by the default that should *not* be replicated on a
schedule gets an explicit entry with `Enabled: false`.

### The default only applies where a pair already exists

`buildSyncRequest` refuses a VM whose domain records no `replica_targets`,
and that metadata is written by a **successful sync**. So auto-applying the
default cannot start replicating a VM nobody set up — it reaches only pairs
somebody established by hand at least once, and a never-synced VM is skipped
with a message saying exactly that.

The feature is also **off until a default template exists**. That is
deliberately an absence rather than a flag: auto-applying a schedule to VMs
nobody wrote an entry for is a decision somebody has to make, and creating a
template called `default` is where they make it -- once, visibly, in the same
file as everything else.

### Presets and templates are not the same kind of thing

| | preset | template |
|---|---|---|
| applied | once, at edit time — the UI "offers to re-apply" | live |
| editing it | changes nothing already scheduled | changes every VM using it |

A preset is a starting point you stamp out; a template is a standing
relationship. Different lifetimes justify resolving them at different times,
so leaving presets resolved in the UI is not drift.

---

## The correction: resolution belongs in the agent

This was proposed the other way round — resolve template + override in the UI,
publish concrete values, keep the agent policy-free — and that was **wrong**.

```go
ScheduleFile string `json:"schedule_file,omitempty"` // STANDALONE ONLY
```

with, at the resolver: *"validation guarantees schedule_file is set if and only
if there is no control plane"*. **The agent runs with no UI at all.** A
UI-side resolver would make templates unavailable in exactly the deployment
that benefits most — someone hand-writing a schedule file per host, who would
otherwise have to pre-resolve every entry and defeat the point.

A shared package is not the escape hatch either: `vmsync_ui`'s go.mod says
*"vmsync-ui deliberately depends on nothing outside the standard library."*

### What the original argument got right, attached to the wrong conclusion

There must be exactly **one** resolver, and what the operator sees must be
what runs. Two implementations of an inheritance rule is the same class of bug
`pkg/libvirtsync/duplicated_names_test.go` exists to prevent for constants —
except logic is far worse to duplicate than a string, and being separate
modules means not even a pinning test could span them.

### The fix is the inverse

Resolve in the agent, and have the agent **report its effective schedule
back** in the report it already sends alongside domains, syncs and fence
state. Then:

- there is one resolver, in the component present in every deployment;
- the UI *displays observed truth* instead of predicting it;
- "why is this VM syncing every 4h?" is answered by the agent's own report;
- the timezone question answers itself, because the agent reports the zone it
  is actually using.

UI-side resolution would have had the console showing what it *would* publish,
which is only the same thing as what runs while the two agree.

A UI-side syntactic check was part of this plan and **is not built, and cannot
be as things stand**: `vmsync-ui` depends on nothing outside the standard
library and so cannot import `pkg/schedcal`, and a second implementation of the
grammar is the divergence this whole section argues against. The console can
enforce the structural rules — not both cadence forms, a cadence needs a mode —
but not the expression itself.

So a malformed calendar published from the console is caught by the agent:
`Complaints()` names it when the document is adopted, and `verifyDueByCalendar`
warns on every run and does not verify. Loud, but later than authoring time.
See the Open list.

---

## Open

- The report gaining the effective schedule, the resolved timezone, and the
  next window per VM. Until this lands the console cannot show observed truth,
  only what it published — which is precisely what the "resolution belongs in
  the agent" correction above says it must not predict.
- The UI's template editor, and a per-VM template selector on the schedule
  form. The wire types carry `verify_days`/`verify_window` and round-trip
  them, but nothing in the console sets them yet: today the calendar form is
  reachable only from a standalone schedule file.
- UI-side validation of the calendar. `vmsync-ui` depends on nothing outside
  the standard library and cannot import `pkg/schedcal`, so it can enforce the
  structural rules (not both forms; a cadence needs a verify mode) but not the
  grammar — and it does not enforce even those yet. A malformed expression
  published from the console is caught by the agent, which names it in
  `Complaints()` on adoption and warns per run without verifying: loud, but
  later than authoring time.
- `lastVerified` is in memory only, so an agent restarting **inside** a window
  re-verifies each of its VMs once more that window. Bounded and cheap — each
  verify rides a sync that was already due, inside hours chosen for exactly
  that — but it is a restart amplifier that persistence would remove. It also
  means the window a restart lands in is re-armed for VMs that already
  verified in it.
- Whether the weekday should be checked for feasibility too. `*-02-30` is
  refused because no such date exists, but a pinned year plus a weekday plus a
  date (`2026-*-01 Sun`) can also be unsatisfiable, and that case is not
  detected — checking it exactly means walking every day of every listed year
  on a path that runs per sync launch.
- Whether a **missed** window should be visible as more than a gap in the run
  history — a metric, or a degradation on the next report. Nothing currently
  says "this VM has not been verified in two months", which is the state the
  no-backstop decision makes possible.
