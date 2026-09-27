/*
	Copyright (C) 2026  Orsiris de Jong <ozy@netpower.fr>

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU General Public License for more details.

You should have received a copy of the GNU General Public License
along with this program.  If not, see <http://www.gnu.org/licenses/>.
*/

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vmsync/pkg/actionlog"
	"vmsync/pkg/failover"
	"vmsync/pkg/libvirtsync"
	"vmsync/pkg/restorepoint"
	"vmsync/pkg/util"
)

// recordingRunner is a target host that remembers, in order, every command it
// was asked to run -- over stdin or not. One shared log, so "the journal
// landed before anything was displaced" is a fact about a single sequence
// rather than two that have to be correlated afterwards.
type recordingRunner struct {
	commands []string
	inputs   []string
	// ctxState parallels commands, SNAPSHOTTED at the moment of the call. The
	// context itself would be useless afterwards: the sink cancels its own
	// timeout on the way out, so a test inspecting a stored context always
	// finds it cancelled and would conclude the opposite of the truth.
	ctxState []ctxSnapshot
	// listing is what a restore point listing answers with, so the sweep
	// below gets far enough to actually rename something.
	listing string
	failOn  string
}

// ctxSnapshot is what the context looked like when the command was issued.
type ctxSnapshot struct {
	err         error
	deadline    time.Time
	hasDeadline bool
}

func (r *recordingRunner) note(ctx context.Context) {
	d, ok := ctx.Deadline()
	r.ctxState = append(r.ctxState, ctxSnapshot{err: ctx.Err(), deadline: d, hasDeadline: ok})
}

func (r *recordingRunner) Run(ctx context.Context, cmd string) (string, error) {
	r.commands = append(r.commands, cmd)
	r.inputs = append(r.inputs, "")
	r.note(ctx)
	if r.failOn != "" && strings.Contains(cmd, r.failOn) {
		return "", errors.New("injected failure")
	}
	if strings.Contains(cmd, "__VMSYNC_RP_LIST__") {
		return r.listing, nil
	}
	return "", nil
}

func (r *recordingRunner) RunWithInput(ctx context.Context, cmd string, input []byte) (string, string, error) {
	r.commands = append(r.commands, cmd)
	r.inputs = append(r.inputs, string(input))
	r.note(ctx)
	if r.failOn != "" && strings.Contains(cmd, r.failOn) {
		return "", "injected stderr", errors.New("injected failure")
	}
	return "", "", nil
}

// indexOfCommand finds the first command containing needle, or -1.
func (r *recordingRunner) indexOfCommand(needle string) int {
	for i, c := range r.commands {
		if strings.Contains(c, needle) {
			return i
		}
	}
	return -1
}

func journalTestConfig() syncConfig {
	return syncConfig{
		TargetDomain:       "web01",
		TargetURI:          "qemu+ssh://dr/system",
		SourceDomain:       "web01",
		SourceURI:          "qemu+ssh://hv-a/system",
		LocalHostName:      "hv-a",
		ActionID:           "9f3c1a2b4d5e6f70",
		RunID:              "run-7",
		Journal:            journalLevelActions,
		ReplacedDiskAction: replacedDiskRename,
	}
}

// THE ORDERING THE WHOLE FEATURE RESTS ON, asserted against the real sweep
// function rather than a re-implementation of it: the intent is on the target
// before the first command that displaces anything.
//
// sweepRestorePointsForReinit is the first thing a -reinit does that touches
// the target's filesystem -- everything above it in the reinit block is
// libvirt-side -- so if the journal write is not already past by the time it
// renames the restore point set aside, an interrupted reinit leaves no record
// of itself anywhere.
func TestTheSyncIntentReachesTheTargetBeforeAnythingIsDisplaced(t *testing.T) {
	runner := &recordingRunner{listing: "__VMSYNC_RP_LIST__\n1756041600-vmsync-cpt-000042\n"}
	cfg := journalTestConfig()
	cfg.Reinit = true

	const replicaDisk = "/data/replicas/web01-disk0.qcow2"
	journal := newRecorder(cfg, runner, replicaDisk, cfg.TargetDomain)
	if journal == nil {
		t.Fatal("no recorder was built for a target that can be reached")
	}
	journal.Intent(context.Background(), journalVerbSync, map[string]string{"mode": syncJournalMode(cfg)})

	if err := sweepRestorePointsForReinit(context.Background(), cfg, runner, replicaDisk); err != nil {
		t.Fatalf("sweepRestorePointsForReinit: %v", err)
	}

	appendAt := runner.indexOfCommand(actionlog.DirName)
	if appendAt != 0 {
		t.Fatalf("the journal append is command %d of %v; it must be the first thing this sync sends to the target", appendAt, runner.commands)
	}
	// restorepoint.AsideSuffix, not "mv": the journal's own append command
	// rotates with mv too, so matching on the verb would find the journal and
	// this test would pass while proving nothing.
	displaceAt := runner.indexOfCommand(restorepoint.AsideSuffix)
	if displaceAt < 0 {
		t.Fatalf("the sweep displaced nothing, so this test proves nothing about ordering: %v", runner.commands)
	}
	if displaceAt < appendAt {
		t.Errorf("the restore point set was moved aside (command %d) before the intent was written (command %d): %v", displaceAt, appendAt, runner.commands)
	}
}

// The record crosses on STDIN, never in the command line: it is arbitrary text
// up to two kilobytes, and quoting it for whatever shell the target runs is a
// problem with no end.
func TestTheRecordCrossesOnStdin(t *testing.T) {
	runner := &recordingRunner{}
	journal := newRecorder(journalTestConfig(), runner, "/data/replicas/web01-disk0.qcow2", "web01")
	journal.Intent(context.Background(), journalVerbSync, map[string]string{"mode": "reinit"})

	if len(runner.inputs) != 1 || runner.inputs[0] == "" {
		t.Fatalf("nothing was sent on stdin: %v", runner.inputs)
	}
	rec, err := actionlog.DecodeLine([]byte(runner.inputs[0]))
	if err != nil {
		t.Fatalf("what crossed on stdin is not a record: %v (%q)", err, runner.inputs[0])
	}
	if rec.Kind != actionlog.KindIntent || rec.Verb != journalVerbSync || rec.Domain != "web01" {
		t.Errorf("the record does not describe this action: %+v", rec)
	}
	if rec.Action != "9f3c1a2b4d5e6f70" || rec.RunID != "run-7" || rec.Host != "hv-a" {
		t.Errorf("the record lost part of this run's identity: %+v", rec)
	}
	if strings.Contains(runner.commands[0], "intent") {
		t.Errorf("part of the record is in the command line: %s", runner.commands[0])
	}
}

// The journal goes beside the disks it describes, in the directory the REPLICA
// lives in on the target -- not beside the source's own disks and not in a log
// directory on whichever machine launched the sync.
func TestTheJournalGoesBesideTheReplica(t *testing.T) {
	runner := &recordingRunner{}
	journal := newRecorder(journalTestConfig(), runner, "/data/replicas/web01-disk0.qcow2", "web01")
	journal.Intent(context.Background(), journalVerbSync, nil)

	want := "/data/replicas/" + actionlog.DirName + "/web01.jsonl"
	if !strings.Contains(runner.commands[0], util.ShQuote(want)) {
		t.Errorf("the journal is not at %s: %s", want, runner.commands[0])
	}
}

// A journal write failure must never fail the action. Counted and warned
// about, and the recorder keeps trying for the outcome.
func TestAJournalWriteFailureNeverFailsTheAction(t *testing.T) {
	runner := &recordingRunner{failOn: actionlog.DirName}
	journal := newRecorder(journalTestConfig(), runner, "/data/replicas/web01-disk0.qcow2", "web01")
	journal.Intent(context.Background(), journalVerbSync, nil)
	finishAction(context.Background(), journal, nil, nil)

	if journal.Failures() != 2 {
		t.Errorf("counted %d journal failures, want 2", journal.Failures())
	}
	if len(runner.commands) != 2 {
		t.Errorf("the recorder stopped trying after the first failure: %v", runner.commands)
	}
}

// A wedged target must cost this action ten seconds, not the rest of the
// night. The bound is applied by the sink rather than left to the caller,
// because every call site would otherwise have to remember it.
func TestTheRemoteSinkBoundsHowLongOneRecordMayBlock(t *testing.T) {
	runner := &recordingRunner{}
	rec := newRecorder(journalTestConfig(), runner, "/data/replicas/web01-disk0.qcow2", "web01")
	rec.Intent(context.Background(), journalVerbSync, nil)

	if len(runner.ctxState) != 1 {
		t.Fatalf("no command was sent: %v", runner.commands)
	}
	got := runner.ctxState[0]
	if !got.hasDeadline {
		t.Fatal("the journal write carried no deadline, so a wedged target would hold the action open indefinitely")
	}
	if left := time.Until(got.deadline); left <= 0 || left > journalWriteTimeout {
		t.Errorf("the deadline is %s away, want at most %s", left, journalWriteTimeout)
	}
}

// The outcomes most worth having are exactly the ones whose context was
// cancelled -- an interrupt, a deadline, a caller that gave up. Writing
// through the dead context would mean the runs that need explaining are the
// ones with nothing after their intent, which is what an unmatched intent is
// supposed to mean instead.
func TestTheOutcomeIsWrittenEvenAfterTheActionsContextIsCancelled(t *testing.T) {
	runner := &recordingRunner{}
	rec := newRecorder(journalTestConfig(), runner, "/data/replicas/web01-disk0.qcow2", "web01")

	ctx, cancel := context.WithCancel(context.Background())
	rec.Intent(ctx, journalVerbSync, nil)
	cancel()
	finishAction(ctx, rec, context.Canceled, nil)

	if rec.Failures() != 0 {
		t.Fatalf("%d journal writes failed after the action's context was cancelled", rec.Failures())
	}
	if len(runner.inputs) != 2 {
		t.Fatalf("wrote %d records, want an intent and an outcome", len(runner.inputs))
	}
	if err := runner.ctxState[1].err; err != nil {
		t.Errorf("the outcome was written through a dead context: %v", err)
	}
	got, err := actionlog.DecodeLine([]byte(runner.inputs[1]))
	if err != nil {
		t.Fatalf("DecodeLine: %v", err)
	}
	if got.Kind != actionlog.KindOutcome || got.Result != actionlog.ResultFailed {
		t.Errorf("the outcome does not describe the cancellation: %+v", got)
	}
}

// -journal=off is a recorder that is nil, so every call site stays free of
// conditionals -- a conditional is how a caller ends up journalling in one
// branch and not the other.
func TestJournalOffBuildsNoRecorder(t *testing.T) {
	cfg := journalTestConfig()
	cfg.Journal = journalLevelOff
	runner := &recordingRunner{}
	if rec := newRecorder(cfg, runner, "/data/replicas/web01-disk0.qcow2", "web01"); rec != nil {
		t.Fatal("-journal=off still built a recorder")
	}
	// And the nil it produced is safe to use.
	var rec *actionlog.Recorder
	rec.Intent(context.Background(), journalVerbSync, nil)
	finishAction(context.Background(), rec, errors.New("boom"), nil)
	if len(runner.commands) != 0 {
		t.Errorf("-journal=off wrote something: %v", runner.commands)
	}
}

// Nowhere to write is the same object as -journal=off, and it is said out loud
// rather than left silent: a journal that is quietly not being written looks
// exactly like one nothing has happened to yet.
func TestNoDiskPathBuildsNoRecorder(t *testing.T) {
	if rec := newRecorder(journalTestConfig(), &recordingRunner{}, "", "web01"); rec != nil {
		t.Error("a recorder was built with nowhere to put the file")
	}
	if rec := newRecorder(journalTestConfig(), &recordingRunner{}, "/data/replicas/d.qcow2", ""); rec != nil {
		t.Error("a recorder was built with no domain to name the file after")
	}
}

// newRecorderInDir must not climb a directory. Root takes a DISK path and
// strips the last element, so handing it the directory itself would put the
// journal one level up, beside another machine's disks.
func TestNewRecorderInDirStaysInTheDirectory(t *testing.T) {
	runner := &recordingRunner{}
	rec := newRecorderInDir(journalTestConfig(), runner, "/data/replicas", "web01")
	rec.Intent(context.Background(), journalVerbRestoreRestore, nil)
	want := "/data/replicas/" + actionlog.DirName + "/web01.jsonl"
	if !strings.Contains(runner.commands[0], util.ShQuote(want)) {
		t.Errorf("the journal is not at %s: %s", want, runner.commands[0])
	}
	if strings.Contains(runner.commands[0], util.ShQuote("/data/"+actionlog.DirName)) {
		t.Errorf("the journal climbed out of the replica's directory: %s", runner.commands[0])
	}
}

// A runner that cannot carry stdin (localRunner shells out here) falls back to
// writing with os, rather than being made to build a here-document out of
// arbitrary text.
func TestALocalRunnerWritesWithOsRatherThanAShell(t *testing.T) {
	dir := t.TempDir()
	cfg := journalTestConfig()
	rec := newRecorderInDir(cfg, localRunner{}, dir, "web01")
	if rec == nil {
		t.Fatal("no recorder for a local target")
	}
	rec.Intent(context.Background(), journalVerbPromote, map[string]string{"promote_mode": "forced"})
	finishAction(context.Background(), rec, nil, nil)
	if rec.Failures() != 0 {
		t.Fatalf("%d journal writes failed", rec.Failures())
	}

	file := filepath.Join(dir, actionlog.DirName, "web01.jsonl")
	content, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	reading := actionlog.Read(nil, content)
	if len(reading.Actions) != 1 || !reading.Actions[0].HasIntent || !reading.Actions[0].HasOutcome {
		t.Fatalf("the local journal did not record a complete action: %+v", reading)
	}
	if reading.Actions[0].Verb() != journalVerbPromote {
		t.Errorf("verb = %q, want %q", reading.Actions[0].Verb(), journalVerbPromote)
	}
}

// A nil runner means "this host", which is what the local verbs pass.
func TestANilRunnerWritesLocally(t *testing.T) {
	dir := t.TempDir()
	rec := newRecorderInDir(journalTestConfig(), nil, dir, "web01")
	rec.Intent(context.Background(), journalVerbUpdateRole, map[string]string{"role": libvirtsync.RoleTarget})
	if rec.Failures() != 0 {
		t.Fatalf("%d journal writes failed", rec.Failures())
	}
	if _, err := os.Stat(filepath.Join(dir, actionlog.DirName, "web01.jsonl")); err != nil {
		t.Fatalf("nothing was written locally: %v", err)
	}
}

// Appending must not overwrite: a second action's records join the first's in
// the same file, which is the only reason a history exists at all.
func TestTheLocalSinkAppends(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 3; i++ {
		rec := newRecorderInDir(journalTestConfig(), nil, dir, "web01")
		rec.Intent(context.Background(), journalVerbSync, nil)
		finishAction(context.Background(), rec, nil, nil)
	}
	content, err := os.ReadFile(filepath.Join(dir, actionlog.DirName, "web01.jsonl"))
	if err != nil {
		t.Fatalf("read the journal: %v", err)
	}
	reading := actionlog.Read(nil, content)
	if reading.Records != 6 {
		t.Errorf("the journal holds %d records after three actions, want 6", reading.Records)
	}
}

// --- flags -------------------------------------------------------------------

func TestValidateJournalLevel(t *testing.T) {
	for _, ok := range []string{journalLevelActions, journalLevelOff} {
		if err := validateJournalLevel(ok); err != nil {
			t.Errorf("-journal=%s was refused: %v", ok, err)
		}
	}
	// Refused rather than defaulted: a typo read as "off" would leave an
	// operator believing every action is journalled while nothing is.
	for _, bad := range []string{"", "Actions", "ACTIONS", "on", "steps", "true", "1"} {
		if err := validateJournalLevel(bad); err == nil {
			t.Errorf("-journal=%q was accepted", bad)
		}
	}
}

func TestValidateActionID(t *testing.T) {
	for _, ok := range []string{"", "9f3c1a2b4d5e6f70", "op-123", "op_123", "a.b.c", strings.Repeat("a", actionIDMax)} {
		if err := validateActionID(ok); err != nil {
			t.Errorf("-action-id %q was refused: %v", ok, err)
		}
	}
	for _, bad := range []string{
		strings.Repeat("a", actionIDMax+1),
		"has space",
		"has,comma",
		"has=equals",
		"has\nnewline",
		"has\"quote",
		"has/slash",
	} {
		if err := validateActionID(bad); err == nil {
			t.Errorf("-action-id %q was accepted; it could not be carried whole in replica_incomplete or in a journal record", bad)
		}
	}
}

// The bound has to be the SAME one libvirtsync.ReplicaIncompleteValue applies,
// or an id that passed this flag would be silently dropped further down and
// the join it exists to be would break with nobody able to say why.
func TestTheActionIDBoundMatchesEverywhereItIsCarried(t *testing.T) {
	long := strings.Repeat("a", actionIDMax)
	value, err := libvirtsync.ReplicaIncompleteValue(libvirtsync.ReplicaIncompleteVerbReinit, 1758441600, long, "hv-a", "1758441600")
	if err != nil {
		t.Fatalf("ReplicaIncompleteValue refused an id this flag accepts: %v", err)
	}
	if !strings.Contains(value, "action="+long) {
		t.Errorf("an id of the maximum length this flag accepts was dropped by the marker builder: %s", value)
	}
	if actionIDMax != actionlog.MaxActionIDBytes {
		t.Errorf("the flag's bound (%d) and the journal's (%d) disagree", actionIDMax, actionlog.MaxActionIDBytes)
	}
}

func TestMintActionIDProducesAUsableID(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		id, err := mintActionID()
		if err != nil {
			t.Fatalf("mintActionID: %v", err)
		}
		if err := validateActionID(id); err != nil {
			t.Fatalf("a minted id is not one this program accepts: %q: %v", id, err)
		}
		if len(id) != 16 {
			t.Fatalf("a minted id is %d characters; the design fixes it at 16 hex so it can be read out over a phone", len(id))
		}
		if seen[id] {
			t.Fatalf("mintActionID repeated %q within 200 draws", id)
		}
		seen[id] = true
	}
}

// --- what the records say ----------------------------------------------------

// The intent records only what is known when it is written. Whether this run
// turns out to be incremental or a full copy depends on the source's
// checkpoint chain, which is not read until the replica has already been
// displaced -- so a guess here would put a claim in the one record that has to
// be trustworthy.
func TestSyncJournalMode(t *testing.T) {
	cfg := journalTestConfig()
	if got := syncJournalMode(cfg); got != "sync" {
		t.Errorf("an ordinary sync records mode %q", got)
	}
	cfg.Reinit = true
	if got := syncJournalMode(cfg); got != libvirtsync.ReplicaIncompleteVerbReinit {
		t.Errorf("a reinit records mode %q", got)
	}
	cfg.ForceClean = true
	if got := syncJournalMode(cfg); got != libvirtsync.ReplicaIncompleteVerbForceClean {
		t.Errorf("a force-clean records mode %q", got)
	}
}

// The outcome says what the run actually turned out to be, which is the only
// place a reader learns that an ordinary-looking sync in fact wrote base
// images directly -- the shape CI-02 is about.
func TestSyncOutcomeMode(t *testing.T) {
	cfg := journalTestConfig()
	if got := syncOutcomeMode(cfg, "vmsync-cpt-000007"); got != "incremental" {
		t.Errorf("a sync with a parent checkpoint is %q, want incremental", got)
	}
	if got := syncOutcomeMode(cfg, ""); got != libvirtsync.ReplicaIncompleteVerbFullSync {
		t.Errorf("a sync with no parent checkpoint is %q, want full-sync", got)
	}
	cfg.Reinit = true
	if got := syncOutcomeMode(cfg, ""); got != libvirtsync.ReplicaIncompleteVerbReinit {
		t.Errorf("a reinit is %q whatever the chain said, want reinit", got)
	}
}

// Standing down on a held lock is not a failure. Recording it as one would
// make every scheduled overlap read as a broken replica in the history, which
// is the same distinction main() draws when it exits ExitBusy instead of 1.
func TestJournalResult(t *testing.T) {
	if res, exit := journalResult(nil); res != actionlog.ResultOK || exit != 0 {
		t.Errorf("success recorded as %s/%d", res, exit)
	}
	if res, exit := journalResult(errors.New("boom")); res != actionlog.ResultFailed || exit != 1 {
		t.Errorf("failure recorded as %s/%d", res, exit)
	}
	busy := fmt.Errorf("promote web01: %w", util.ErrLockHeld)
	if res, exit := journalResult(busy); res != actionlog.ResultBusy || exit != util.ExitBusy {
		t.Errorf("a stand-down recorded as %s/%d, want %s/%d", res, exit, actionlog.ResultBusy, util.ExitBusy)
	}
}

// The journal is one file per domain, so a verb that takes the domain under
// either flag has to file its records under the same name every other verb
// uses -- or that replica's history quietly splits in two.
func TestJournalDomainName(t *testing.T) {
	cfg := journalTestConfig()
	if got := journalDomainName(cfg); got != "web01" {
		t.Errorf("journalDomainName = %q", got)
	}
	cfg.TargetDomain = ""
	cfg.SourceDomain = "db02"
	if got := journalDomainName(cfg); got != "db02" {
		t.Errorf("with no -target-domain, journalDomainName = %q, want the source domain", got)
	}
}

// The actor is a first-class field of the record rather than an entry in its
// detail map, because it is the counterpart of -promoted-by / -restored-by
// being written onto the domain: an audit log in a control plane does not
// survive losing the control plane, and a file beside the disks does.
func TestJournalActorComesFromWhicheverFlagTheVerbAccepts(t *testing.T) {
	cfg := journalTestConfig()
	if got := journalActor(cfg); got != "" {
		t.Errorf("journalActor = %q with neither flag set", got)
	}
	cfg.PromotedBy = "alice"
	if got := journalActor(cfg); got != "alice" {
		t.Errorf("journalActor = %q, want alice", got)
	}
	cfg.PromotedBy, cfg.RestoredBy = "", "bob"
	if got := journalActor(cfg); got != "bob" {
		t.Errorf("journalActor = %q, want bob", got)
	}
	// And it reaches the record itself, rather than only the config.
	runner := &recordingRunner{}
	rec := newRecorder(cfg, runner, "/data/replicas/web01-disk0.qcow2", "web01")
	rec.Intent(context.Background(), journalVerbRestoreRestore, nil)
	got, err := actionlog.DecodeLine([]byte(runner.inputs[0]))
	if err != nil {
		t.Fatalf("DecodeLine: %v", err)
	}
	if got.By != "bob" {
		t.Errorf("the record's by = %q, want bob", got.By)
	}
}

// -local-host-name wins, because that is the name the rest of the system knows
// this machine by: an agent can be told to report under a name that is not
// os.Hostname(), and a journal disagreeing with it correlates with nothing.
func TestJournalHostPrefersTheConfiguredName(t *testing.T) {
	cfg := journalTestConfig()
	if got := journalHost(cfg); got != "hv-a" {
		t.Errorf("journalHost = %q, want the configured local host name", got)
	}
	cfg.LocalHostName = ""
	if got := journalHost(cfg); got == "hv-a" {
		t.Error("journalHost ignored the empty configured name")
	}
}

// The three read-only verbs must never journal. A listing that writes is a
// listing nobody dares run during an incident, and -explain-domain would be
// writing into the file it is printing.
func TestTheReadOnlyVerbsAreNotInTheJournalVocabulary(t *testing.T) {
	vocabulary := []string{
		journalVerbSync, journalVerbPromote, journalVerbInvert,
		journalVerbShutdownDomain, journalVerbFenceDomain, journalVerbUpdateRole,
		journalVerbRestoreRestore, journalVerbCloneRestorePoint,
	}
	for _, forbidden := range []string{"list-restore-points", "read-fence", "explain-domain"} {
		for _, v := range vocabulary {
			if v == forbidden {
				t.Errorf("%q is a journalled verb; the read-only verbs must not be", forbidden)
			}
		}
	}
	// And the set is exactly the eight the design names, so adding a verb
	// without deciding whether it is journalled fails here.
	if len(vocabulary) != 8 {
		t.Errorf("the journal vocabulary has %d verbs, and the design names 8", len(vocabulary))
	}
}

// --- the marker, as the reader verb words it ---------------------------------

func TestExplainReplicaIncompleteSaysWhatItMeans(t *testing.T) {
	raw, err := libvirtsync.ReplicaIncompleteValue(libvirtsync.ReplicaIncompleteVerbReinit,
		1758441600, "9f3c1a2b4d5e6f70", "hv-a", "1758441600")
	if err != nil {
		t.Fatalf("ReplicaIncompleteValue: %v", err)
	}
	note := explainReplicaIncomplete(raw)
	for _, want := range []string{
		libvirtsync.ReplicaIncompleteVerbReinit,
		"hv-a",
		"9f3c1a2b4d5e6f70",
		"2025-09-21 08:00:00 UTC",
		// The aside suffix, spelled the way the files on disk are: it is the
		// one-line recovery, and it exists nowhere else on this host.
		failover.ReplicaReplacedSuffix + "1758441600",
		"-promote refuses this domain",
	} {
		if !strings.Contains(note, want) {
			t.Errorf("the note does not mention %q:\n%s", want, note)
		}
	}
}

// Fail closed, exactly as the refusal does: the field's PRESENCE is the
// finding, so a value this build cannot read still means a copy was started
// here and never finished.
func TestExplainReplicaIncompleteStillSpeaksForAnUnreadableValue(t *testing.T) {
	note := explainReplicaIncomplete("this is not the grammar at all")
	if note == "" {
		t.Fatal("an unreadable marker produced no note, so the finding would be invisible")
	}
	if !strings.Contains(note, "could not be read") {
		t.Errorf("the note does not say the value was unreadable:\n%s", note)
	}
	if !strings.Contains(note, "half-written") {
		t.Errorf("the note does not say what it means for these disks:\n%s", note)
	}
}

func TestExplainReplicaIncompleteSaysNothingWhenThereIsNoMarker(t *testing.T) {
	if note := explainReplicaIncomplete(""); note != "" {
		t.Errorf("a domain with no marker produced a note: %q", note)
	}
}

// A full sync renames nothing aside, so the note must not send an operator
// hunting for files that were never created.
func TestExplainReplicaIncompleteNamesNoAsideWhenThereIsNone(t *testing.T) {
	raw, err := libvirtsync.ReplicaIncompleteValue(libvirtsync.ReplicaIncompleteVerbFullSync,
		1758441600, "9f3c1a2b4d5e6f70", "hv-a", "")
	if err != nil {
		t.Fatalf("ReplicaIncompleteValue: %v", err)
	}
	if note := explainReplicaIncomplete(raw); strings.Contains(note, "sibling files") {
		t.Errorf("the note names aside files a full sync never created:\n%s", note)
	}
}
