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
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"regexp"
	"strings"
	"time"

	"vmsync/pkg/actionlog"
	"vmsync/pkg/disk"
	"vmsync/pkg/failover"
	"vmsync/pkg/libvirtsync"
	"vmsync/pkg/trace"
	"vmsync/pkg/util"

	"libvirt.org/go/libvirt"
)

// The action journal's engine side: where each verb's records go, and the one
// verb that reads them back.
//
// Everything that decides anything about the format -- the record, its bounds,
// the rotation rule, the shell command, the reader and the report -- lives in
// pkg/actionlog, which has no libvirt and no pkg/util dependency and is
// therefore testable on any machine. What is here is only the wiring: which
// host owns the disks a given verb acts on, how to reach that host, and when
// to say something.
//
// THE RULE THIS FILE MUST NOT BREAK: a journal write never fails an action.
// Every sink error is counted and warned about, and every recorder is safe to
// be nil so no call site needs a conditional. A diagnostic that can stop a
// replication is a diagnostic that causes outages, and pkg/actionlog's
// TestNothingThatDecidesImportsThisPackage is what keeps the other half of
// that rule -- nothing ever branches on what is written here.

// The -journal levels.
const (
	// journalLevelActions is the default and, today, the only level that
	// writes anything: ONE intent when a verb starts and ONE outcome when it
	// ends. Per-step records were deliberately left out -- a line per disk
	// turns a 40-disk reinit into hundreds of records on the very filesystem
	// the replica lives on, to answer a question the disks already answer.
	journalLevelActions = "actions"
	// journalLevelOff writes nothing at all. It never affects the
	// replica_incomplete safety field, which is metadata on the domain and has
	// nothing to do with this.
	journalLevelOff = "off"
)

// The verb each action records itself under. Constants rather than literals at
// the call sites, because these strings are the vocabulary bench and the docs
// describe and a typo would produce a verb nothing can search for.
const (
	journalVerbSync              = "sync"
	journalVerbPromote           = "promote"
	journalVerbInvert            = "invert"
	journalVerbShutdownDomain    = "shutdown-domain"
	journalVerbFenceDomain       = "fence-domain"
	journalVerbUpdateRole        = "update-role"
	journalVerbRestoreRestore    = "restore-restore-point"
	journalVerbCloneRestorePoint = "clone-restore-point"
)

// DELIBERATELY NOT JOURNALLED: -list-restore-points, -read-fence and
// -explain-domain. All three only read, and a listing that writes is a listing
// nobody dares run during an incident -- which is exactly when it is needed.
// -explain-domain would additionally be writing into the file it is printing.

// actionIDRe and actionIDMax bound what -action-id will accept.
//
// The SAME rule vmsync-agent's usableActionID applies and the same one
// libvirtsync.ReplicaIncompleteValue will carry, so an id that gets past this
// flag is an id that survives everywhere it has to go. Three places, one rule:
// an id that passed here and was silently dropped further down would break the
// join it exists to be, and leave nobody able to tell why.
var actionIDRe = regexp.MustCompile(`^[0-9A-Za-z._-]+$`)

const actionIDMax = actionlog.MaxActionIDBytes

// validateJournalLevel refuses a -journal value up front, the way every other
// flag is validated.
//
// Refused rather than defaulted, because the two values differ in whether
// anything is recorded at all: silently reading a typo as "off" would leave an
// operator believing every action is being journalled while nothing is, and
// they would find out at the one moment the journal was the only evidence
// left.
func validateJournalLevel(level string) error {
	switch level {
	case journalLevelActions, journalLevelOff:
		return nil
	default:
		return fmt.Errorf("-journal must be %q or %q, not %q", journalLevelActions, journalLevelOff, level)
	}
}

// validateActionID refuses an id that could not be carried whole.
//
// Refused HERE, before anything is done, because at this point refusing costs
// nothing: no domain has been touched and the operator gets to retype it. That
// is a different decision from libvirtsync.ReplicaIncompleteValue's, which
// DROPS an unusable id rather than refusing, and the difference is deliberate:
// by the time that function runs, refusing would abort the arming write and
// leave a replica about to be overwritten with nothing recording it. An
// unattributed marker is a small loss; an unarmed one is CI-02.
//
// The bound is the same in both places, so an id that reaches the builder from
// here can never be the one it drops.
func validateActionID(id string) error {
	if id == "" {
		return nil
	}
	if len(id) > actionIDMax {
		return fmt.Errorf("-action-id is %d bytes; it must be at most %d, because it is carried in the domain's replica_incomplete field alongside the host name an operator reads during a disaster", len(id), actionIDMax)
	}
	if !actionIDRe.MatchString(id) {
		return fmt.Errorf("-action-id %q may only contain letters, digits, '.', '_' and '-': it is written into a comma-separated metadata value and into a JSON journal, and anything else would make one of them unreadable", id)
	}
	return nil
}

// mintActionID invents a correlation id for a run that arrived without one.
//
// Every run gets one, including one an operator typed by hand, because the id
// is what ties the journal record to the replica_incomplete marker the same
// run may leave on the domain. Without it a refusal on the DR host names a run
// that cannot be found in any journal, which is half a diagnosis.
//
// Eight random bytes, rendered hex: the same shape as the example in the
// design, short enough to read out over a phone during an incident, and wide
// enough that two runs in the same second do not collide.
//
// crypto/rand rather than math/rand: not for secrecy, but because
// math/rand/v2's global source is not something this program seeds, and two
// vmsync processes started in the same instant by a scheduler are exactly the
// case that must not produce the same id.
func mintActionID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("mint an action id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// syncJournalMode names the kind of sync that is ABOUT to run, from the flags
// alone.
//
// Only what is known at intent time, which is the point of an intent: whether
// this run turns out to be incremental or a full copy depends on the source's
// checkpoint chain, and that is not read until after the reinit block has
// already displaced the replica. Recording a guess here would put a claim in
// the record that the run may then contradict -- and the record written before
// the damage is the one that has to be trustworthy.
//
// force-clean and reinit are told apart for the reason reinitVerb tells them
// apart: they leave different wreckage, and an operator reading an unfinished
// intent needs to know which one to go looking for.
func syncJournalMode(cfg syncConfig) string {
	switch {
	case cfg.ForceClean:
		return libvirtsync.ReplicaIncompleteVerbForceClean
	case cfg.Reinit:
		return libvirtsync.ReplicaIncompleteVerbReinit
	default:
		return "sync"
	}
}

// syncOutcomeMode names what the run actually turned out to be, now that the
// checkpoint chain has been read.
//
// parent is the checkpoint this run built on; empty means there was none, so
// this run wrote base images directly -- a full copy, with no overlay to
// commit, which is the shape CI-02 is about. Saying so in the outcome is what
// lets a reader tell a routine incremental from the run that replaced the
// whole replica, without having to infer it from the flags.
func syncOutcomeMode(cfg syncConfig, parent string) string {
	if mode := syncJournalMode(cfg); mode != "sync" {
		return mode
	}
	if parent == "" {
		return libvirtsync.ReplicaIncompleteVerbFullSync
	}
	return "incremental"
}

// journalStdinRunner is a host that can run a command with a payload on its
// STDIN. *remotessh.Client satisfies it.
//
// A seam of its own rather than an addition to remoteRunner, because most
// things that run commands here have no use for stdin and localRunner cannot
// offer it at all -- a local verb writes its journal with os, not with a
// shell. The type switch in journalSink is what tells the two apart, exactly
// as acquireTargetRunLock switches on util.CommandHolder.
type journalStdinRunner interface {
	RunWithInput(ctx context.Context, command string, input []byte) (stdout string, stderr string, err error)
}

// journalWriteTimeout bounds how long ONE record may hold an action up.
//
// Generous for what it is -- appending half a kilobyte over an SSH connection
// that is already open and already carrying this run's commands -- and bounded
// at all for a reason worth stating plainly: a target whose filesystem has
// wedged must cost this sync ten seconds, not the rest of the night. The trade
// is real and it goes this way round. Losing the intent costs the diagnosis of
// one run; blocking on it costs the replication itself, and the refusal that
// actually stops a half-written replica being promoted is the
// replica_incomplete field on the domain, which is written over libvirt and
// does not go through here at all.
const journalWriteTimeout = 10 * time.Second

// remoteJournalSink appends a record over SSH, on the host that owns the disks.
func remoteJournalSink(runner journalStdinRunner, root, file string) actionlog.Sink {
	return func(ctx context.Context, line []byte) error {
		ctx, cancel := context.WithTimeout(ctx, journalWriteTimeout)
		defer cancel()
		_, stderr, err := runner.RunWithInput(ctx, actionlog.AppendCommand(root, file, len(line)), line)
		if err != nil {
			if stderr != "" {
				return fmt.Errorf("%w: %s", err, stderr)
			}
			return err
		}
		return nil
	}
}

// localJournalSink appends a record on THIS host, for the verbs that act on a
// local domain.
//
// os rather than a shell, because there is no reason to spawn one: the same
// rotation rule, expressed directly. O_APPEND with a single Write of at most
// MaxRecordBytes, so two vmsync processes journalling the same domain
// interleave whole records rather than half-lines -- the same property
// AppendCommand relies on at the other end.
func localJournalSink(root, file string) actionlog.Sink {
	return func(_ context.Context, line []byte) error {
		if err := os.MkdirAll(root, 0o755); err != nil {
			return fmt.Errorf("create the journal directory %s: %w", root, err)
		}
		if fi, err := os.Stat(file); err == nil && actionlog.RotateDecision(fi.Size()+int64(len(line))) {
			if err := os.Rename(file, actionlog.RotatedFile(file)); err != nil {
				// Not fatal to the append: a journal that could not be rotated
				// is still a journal, and refusing to record the action
				// because of it would be the wrong trade in both directions.
				trace.Warning("could not rotate the action journal; it will keep growing until this succeeds",
					"file", file, "error", err)
			}
		}
		f, err := os.OpenFile(file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return fmt.Errorf("open the journal %s: %w", file, err)
		}
		defer f.Close()
		if _, err := f.Write(line); err != nil {
			return fmt.Errorf("append to the journal %s: %w", file, err)
		}
		return nil
	}
}

// journalSink picks the way to reach the host that owns the disks.
//
// runner may be nil, which means "this host": a local verb reaches its own
// filesystem with os. A runner that cannot carry stdin (localRunner, which
// shells out here) is treated the same way rather than being made to build a
// here-document, because the payload is arbitrary text and getting it through
// a shell intact is the problem AppendCommand exists to avoid.
func journalSink(runner remoteRunner, root, file string) actionlog.Sink {
	if w, ok := runner.(journalStdinRunner); ok {
		return remoteJournalSink(w, root, file)
	}
	return localJournalSink(root, file)
}

// journalHost names the machine that RAN the action.
//
// Not the machine the journal is written to, and the difference matters for a
// sync: the file lives beside the replica on the TARGET, while the run itself
// happens on the source. This is what tells a reader on the DR host where to
// go looking for the log of a run that never came back.
//
// -local-host-name wins because that is the name the rest of the system knows
// this machine by -- an agent can be told to report under a name that is not
// os.Hostname(), and a journal disagreeing with it would fail to correlate
// with everything else the control plane holds.
func journalHost(cfg syncConfig) string {
	if cfg.LocalHostName != "" {
		return cfg.LocalHostName
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return ""
}

// journalActor is whoever asked for this action, for the verbs that record an
// actor at all.
//
// One invocation is one verb, so at most one of these two flags is ever set,
// and reading both here saves every call site from having to know which of
// them its verb accepts. It is the counterpart of -promoted-by and -restored-by
// being written onto the domain, and it exists for the same reason: an audit
// log in a control plane does not survive losing the control plane, and a file
// beside the disks does.
func journalActor(cfg syncConfig) string {
	if cfg.PromotedBy != "" {
		return cfg.PromotedBy
	}
	return cfg.RestoredBy
}

// journalIdentity is what every record this process writes says about itself.
//
// OpID is deliberately left empty. vmsync-agent passes its OPERATION's id as
// -action-id (see its CommandArgs), so for an agent-driven run the operation
// id is already the correlation id, and filling both with the same string
// would make a record look like it carried two joins when it carries one. The
// key stays in the schema for a caller that one day has a genuinely separate
// operation id to record.
func journalIdentity(cfg syncConfig, domain string) actionlog.Identity {
	return actionlog.Identity{
		ActionID: cfg.ActionID,
		Domain:   domain,
		Host:     journalHost(cfg),
		PID:      os.Getpid(),
		RunID:    cfg.RunID,
		By:       journalActor(cfg),
	}
}

// newRecorder builds the journal for one action, or nil when there is nowhere
// or no reason to write one.
//
// diskPath is any one of the disks the action acts on; the journal goes beside
// it, on the host that owns it. runner is nil for a local verb.
//
// A nil return is a working, silent recorder -- see pkg/actionlog on why every
// method tolerates it. The reason for a nil is always said out loud, because a
// journal that is quietly not being written looks exactly like a journal
// nothing has happened to yet, and the difference matters to whoever comes
// looking for an intent that is not there.
func newRecorder(cfg syncConfig, runner remoteRunner, diskPath, domain string) *actionlog.Recorder {
	if cfg.Journal == journalLevelOff {
		return nil
	}
	if diskPath == "" || domain == "" {
		why := "vmsync could not work out where this domain's disks are, and the journal lives beside them"
		if domain == "" {
			why = "this action names no domain, and the journal is one file per domain"
		}
		trace.Warning("no action journal will be written for this action: "+why+". Nothing else is affected -- the journal is evidence, never an input to a decision",
			"vm", domain)
		return nil
	}
	root := actionlog.Root(diskPath)
	file := actionlog.File(root, domain)
	return actionlog.New(journalSink(runner, root, file), journalIdentity(cfg, domain), func(err error) {
		trace.Warning("could not write to the action journal; this action is unaffected and continues, but it will leave no record of itself beside these disks",
			"vm", domain, "journal", file, "error", err)
	})
}

// newRecorderInDir is newRecorder for a caller that knows the DIRECTORY rather
// than one of the disks -- the restore point verbs, which are handed a path by
// the operator.
func newRecorderInDir(cfg syncConfig, runner remoteRunner, dir, domain string) *actionlog.Recorder {
	if dir == "" {
		return newRecorder(cfg, runner, "", domain)
	}
	// A sentinel basename, never touched: Root takes a disk path and strips
	// the last element, and handing it the directory itself would put the
	// journal one level too high, beside another machine's disks.
	return newRecorder(cfg, runner, path.Join(dir, journalDirSentinel), domain)
}

// journalDirSentinel stands in for a disk file when only the directory is
// known. It names nothing and is never created; actionlog.Root strips it.
const journalDirSentinel = "disk"

// journalDomainName is the name a record files itself under, for the verbs
// that accept the domain under either flag.
//
// The journal is one file per domain, so the name has to be the same one every
// other verb uses for that replica or its history splits in two. main()
// defaults -target-domain to -source-domain, but only AFTER the action modes
// have already run, so the modes have to do it themselves -- exactly as
// -update-role's own roleDomain does.
func journalDomainName(cfg syncConfig) string {
	if cfg.TargetDomain != "" {
		return cfg.TargetDomain
	}
	return cfg.SourceDomain
}

// newLocalDomainRecorder opens the journal for a verb acting on a domain on
// THIS host -- promote, invert, the two shutdowns, update-role.
//
// All of them run where the domain lives (see requireLocalURI: a promotion has
// to work when the other site is unreachable), so the journal is an ordinary
// file write and needs no runner.
func newLocalDomainRecorder(cfg syncConfig, mgr *libvirtsync.Manager, uri, domain string) *actionlog.Recorder {
	if cfg.Journal == journalLevelOff {
		return nil
	}
	// The disk directory below is read out of the DOMAIN's own definition, so
	// it names a path on whichever host that domain lives on -- and this
	// recorder writes with plain os calls, on the machine vmsync is running
	// on. Those are the same machine for every verb that requires a local
	// URI, which is most of them; -update-role deliberately does not require
	// one. Writing anyway would create a .vmsync-journal directory HERE at
	// the far host's disk path: a journal beside nothing, for a domain whose
	// real history is elsewhere, which is worse than no journal at all
	// because somebody reading it would believe it complete.
	if util.UriUsesSSH(uri) {
		trace.Warning("no action journal will be written for this action: it acts on a domain at a remote libvirt URI, and this journal is written beside that domain's own disks on the host that holds them. The action itself is unaffected: the journal is evidence, never an input to a decision",
			"vm", domain, "uri", uri)
		return nil
	}
	dir, why := localDomainDiskDir(mgr, domain)
	if dir == "" {
		trace.Warning("no action journal will be written for this action, because "+why+". The action itself is unaffected: the journal is evidence, never an input to a decision",
			"vm", domain)
		return nil
	}
	return newRecorderInDir(cfg, nil, dir, domain)
}

// journalResult maps a verb's error to what the outcome record says happened.
//
// Standing down on a held lock is NOT a failure, and recording it as one would
// make every scheduled overlap read as a broken replica in the history -- the
// same distinction main() already draws when it exits with util.ExitBusy
// instead of 1.
func journalResult(err error) (result string, exit int) {
	switch {
	case err == nil:
		return actionlog.ResultOK, 0
	case errors.Is(err, util.ErrLockHeld):
		return actionlog.ResultBusy, util.ExitBusy
	default:
		return actionlog.ResultFailed, 1
	}
}

// finishAction writes the one outcome record for a verb, from a defer.
//
// A helper rather than five copies of the same three lines, because the thing
// that must not vary between verbs is that the outcome is written on EVERY
// return path including the early refusals -- a verb that journals its intent
// and only sometimes its outcome would manufacture the exact finding the
// journal exists to report.
func finishAction(ctx context.Context, rec *actionlog.Recorder, err error, detail map[string]string) {
	if rec == nil {
		return
	}
	// DETACHED FROM THE ACTION'S OWN CANCELLATION, and that is the point of
	// this wrapper rather than a bare rec.Outcome. By the time an outcome is
	// written the action is over, and the outcomes most worth having are
	// exactly the ones whose context was cancelled -- an interrupt, a deadline,
	// a caller that gave up. Writing through the dead context would mean the
	// runs that need explaining are precisely the ones with no outcome beside
	// their intent, which is the opposite of what an unmatched intent is meant
	// to tell a reader.
	//
	// Still bounded: the sink applies journalWriteTimeout of its own, so a
	// wedged target cannot turn a finished action into a hung process.
	outcomeCtx := context.WithoutCancel(ctx)
	result, exit := journalResult(err)
	rec.Outcome(outcomeCtx, result, exit, err, detail)
}

// --- the reader verb ---------------------------------------------------------

// runExplainDomain prints what this host knows about one domain: the marker
// that would refuse a promotion, and the actions recorded beside its disks.
//
// LOCAL URI ONLY, for the same reason -promote is (see requireLocalURI): the
// journal is a file beside the disks, and during a disaster the other site is
// gone. It reads and prints, changes nothing anywhere, and is deliberately not
// journalled itself -- a listing that writes is one nobody dares run during an
// incident, and this one would be writing into the file it is printing.
//
// It does NOT decide anything, and says so in its own output. Whether this
// replica may be promoted is -promote's answer, made from the domain's
// metadata; this only explains how the domain came to be in that state.
func runExplainDomain(cfg syncConfig, domain string) error {
	if cfg.TargetURI == "" {
		return fmt.Errorf("-explain-domain needs -target-uri naming the hypervisor holding %s", domain)
	}
	if err := requireLocalURI(cfg.TargetURI, "-target-uri"); err != nil {
		return err
	}

	mgr, err := libvirtsync.Connect(cfg.TargetURI)
	if err != nil {
		return fmt.Errorf("connect to libvirt at %s: %w", cfg.TargetURI, err)
	}
	defer mgr.Close()

	exp := actionlog.Explanation{
		Domain: domain,
		URI:    cfg.TargetURI,
		Limit:  explainActionLimit,
	}

	st, err := libvirtsync.ReadFailoverState(mgr, domain)
	if err != nil {
		return fmt.Errorf("read %s's replication state: %w", domain, err)
	}
	if st.Exists {
		exp.ReplicaIncomplete = st.ReplicaIncomplete
		exp.ReplicaIncompleteNote = explainReplicaIncomplete(st.ReplicaIncomplete)
	} else if cfg.TargetDiskPath == "" {
		// Nothing to read the disks off, so there is nothing to find. The
		// remedy is named rather than left implicit, because the case below is
		// the one somebody in trouble is most likely to be in.
		return fmt.Errorf("no domain named %s on this host, and no -target-disk-path to look beside instead -- an interrupted -force-clean undefines the target and then rewrites its disks, so the disks and their journal can outlive the domain; pass -target-disk-path to read that journal anyway", domain)
	} else {
		// THE WORST CASE, and the one this verb must not refuse: -force-clean
		// undefines the target domain BEFORE it starts replacing the disks, so
		// a run killed in that window leaves disks, an aside set, and this
		// journal, with no domain above them and therefore no replica_incomplete
		// marker anywhere. -promote already refuses this ("there is nothing here
		// to promote"); what nobody has is an explanation, and the journal below
		// is the only artefact that holds one.
		exp.DomainNote = "There is NO DOMAIN by this name on this host, so there is no metadata to carry a marker -- which is not a clean bill of health. A -force-clean undefines the target before it replaces the disks, so a run killed in that window leaves the disks and this journal with nothing above them. An unfinished action below is what that looks like; the disks beside this journal may be the half-written set it left, and the complete replica, if it was renamed rather than deleted, is in the sibling files carrying a " + failover.ReplicaReplacedSuffix + "<unix> suffix."
	}

	dir, why := explainDiskDir(mgr, cfg, domain)
	if dir == "" {
		exp.JournalNote = why
	} else {
		// Through the same sentinel the writers use, so this verb reads the
		// exact file they write. Two ways of turning a directory into a
		// journal path is how a reader ends up reporting "nothing recorded"
		// about a domain with a full history one directory away.
		root := actionlog.Root(path.Join(dir, journalDirSentinel))
		exp.JournalPath = actionlog.File(root, domain)
		rotated, live, note := readLocalJournal(exp.JournalPath)
		exp.JournalNote = note
		exp.Reading = actionlog.Read(rotated, live)
	}

	exp.Render(os.Stdout)
	return nil
}

// explainActionLimit is how many actions the report lists.
//
// Bounded because the newest ones are what an incident is about, and an
// unbounded list of a busy pair's history would push them off the top of a
// terminal. Twenty is roughly a day of hourly syncs plus whatever failover
// verbs were run around them.
const explainActionLimit = 20

// explainDiskDir is localDomainDiskDir with -target-disk-path allowed to win.
//
// Only -explain-domain accepts that override, and deliberately: it is the one
// verb built to work on a domain whose definition is broken or whose disks
// have moved, which is a state somebody reaching for it may well be in. The
// acting verbs read the domain instead, because a flag naming the wrong
// directory would put their record beside another machine's disks.
func explainDiskDir(mgr *libvirtsync.Manager, cfg syncConfig, domain string) (dir, why string) {
	if cfg.TargetDiskPath != "" {
		return cfg.TargetDiskPath, ""
	}
	dir, why = localDomainDiskDir(mgr, domain)
	if dir == "" {
		// The hint belongs here and not in localDomainDiskDir: the override it
		// names is this verb's alone, and telling an operator to pass it to a
		// promotion would send them somewhere it does nothing.
		why += "; name the directory with -target-disk-path"
	}
	return dir, why
}

// localDomainDiskDir finds the directory a domain on THIS host keeps its disks
// in, or says why it could not.
//
// The answer is where that domain's journal belongs: beside the data it
// describes. A domain vmsync cannot read is one with no journal rather than
// one journalled somewhere arbitrary -- see newRecorder on why a nil recorder
// is the right answer and why it is said out loud.
func localDomainDiskDir(mgr *libvirtsync.Manager, domain string) (dir, why string) {
	dom, err := mgr.LookupDomain(domain)
	if err != nil {
		return "", fmt.Sprintf("%s could not be looked up in order to find its disks (%v)", domain, err)
	}
	defer dom.Free()
	// INACTIVE, matching every other metadata read in vmsync: the persistent
	// definition is what a replica is described by, and it is the only one a
	// shut-down domain has.
	xml, err := dom.GetXMLDesc(libvirt.DOMAIN_XML_INACTIVE)
	if err != nil {
		return "", fmt.Sprintf("%s's definition could not be read in order to find its disks (%v)", domain, err)
	}
	disks, err := disk.ParseQcowDisks(xml)
	if err != nil || len(disks) == 0 {
		return "", fmt.Sprintf("%s lists no qcow2 disks, so there is nowhere beside them for a journal to live", domain)
	}
	// Path(), not RootSource: ParseQcowDisks does not resolve backing chains
	// -- that costs a qemu-img run per disk -- so RootSource is empty here and
	// reading it would give path.Dir("") = "." for every domain, which is the
	// silent bug its own doc comment was written about.
	return path.Dir(disks[0].Path()), ""
}

// readLocalJournal reads the live file and the one rotated generation.
//
// A missing file is the ordinary state of a domain nothing has happened to
// yet, and is reported as that rather than as an error: refusing to print the
// replica_incomplete marker because no journal exists would withhold the half
// of this report that actually refuses promotions.
func readLocalJournal(file string) (rotated, live []byte, note string) {
	var notes []string
	live, err := os.ReadFile(file)
	if err != nil {
		if os.IsNotExist(err) {
			notes = append(notes, "no journal has been written beside these disks yet")
		} else {
			notes = append(notes, fmt.Sprintf("the journal could not be read (%v)", err))
		}
		live = nil
	}
	rotated, err = os.ReadFile(actionlog.RotatedFile(file))
	if err != nil {
		if !os.IsNotExist(err) {
			notes = append(notes, fmt.Sprintf("the rotated generation could not be read (%v)", err))
		}
		rotated = nil
	}
	return rotated, live, strings.Join(notes, "; ")
}

// explainReplicaIncomplete words the marker for a reader who is not being
// refused anything right now.
//
// Deliberately SHORTER than the refusal pkg/failover produces, and it points
// at that refusal rather than restating it. Two full copies of the recovery
// prose would eventually disagree, and the one an operator must act on is the
// one -promote prints when it actually stops them.
func explainReplicaIncomplete(raw string) string {
	if raw == "" {
		return ""
	}
	ri := failover.ParseReplicaIncomplete(raw)
	var b strings.Builder
	if ri.Parsed {
		fmt.Fprintf(&b, "A %s started at %s", ri.Verb, time.Unix(ri.AtUnix, 0).UTC().Format("2006-01-02 15:04:05 UTC"))
		if ri.Host != "" {
			fmt.Fprintf(&b, " from %s", ri.Host)
		}
		if ri.ActionID != "" {
			fmt.Fprintf(&b, " under action %s", ri.ActionID)
		}
		b.WriteString(" and never recorded finishing.")
	} else {
		// Fail closed, exactly as the refusal does: the field's PRESENCE is
		// the finding, and a value this build cannot read still means a copy
		// was started here.
		b.WriteString("A full copy was started and never recorded finishing; the record it left could not be read, which changes nothing about what it means.")
	}
	b.WriteString(" These disk files may therefore be a half-written image, while every other field on this domain still describes the replica that copy replaced.")
	if ri.AsideStamp != "" {
		fmt.Fprintf(&b, " The complete replica is in the sibling files carrying the suffix %s%s.",
			failover.ReplicaReplacedSuffix, ri.AsideStamp)
	}
	b.WriteString(" -promote refuses this domain and prints the full recovery; run it to see exactly what it says.")
	if ri.ActionID != "" {
		fmt.Fprintf(&b, " The action list below shows that run under %s.", ri.ActionID)
	}
	return b.String()
}
