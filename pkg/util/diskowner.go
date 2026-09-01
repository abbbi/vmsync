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

package util

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"strings"
)

// Target disk ownership.
//
// vmsync creates the target's disk files by running qemu-img over SSH, so
// they are owned by whatever user that SSH session is -- root, in every
// realistic deployment. qemu does NOT run as root: RHEL and Fedora run it as
// `qemu`, Debian and Ubuntu as `libvirt-qemu`. A root-owned disk is therefore
// one the promoted domain may be unable to open, and the failure lands at the
// worst possible moment -- during a failover, on the copy that was supposed
// to take over.
//
// libvirt's own dynamic_ownership usually papers over this by chowning disks
// as it starts a domain, which is why it can go unnoticed for a long time.
// It is not something to rely on: it is off in plenty of deployments, and it
// cannot work at all on NFS with root_squash, which is exactly where a DR
// replica often lives.
//
// The sharper half of the problem is -reinit, which renames the existing
// (correctly owned) disk aside and creates a fresh root-owned one in its
// place. That silently converts a replica that WAS bootable into one that is
// not, with nothing in the log about it.

// DiskOwnerAuto is the default mode: preserve whatever owned the file
// before, and otherwise take what the target's libvirt is configured to run
// qemu as. It never guesses.
const DiskOwnerAuto = "auto"

// DiskOwnerOff disables ownership handling entirely -- the behaviour before
// this existed. Kept because a site whose storage layer assigns ownership
// its own way should be able to say so.
const DiskOwnerOff = "off"

// DiskOwner is a resolved user[:group] to apply to target disk files.
type DiskOwner struct {
	User  string
	Group string
	// Source records where this came from, purely so the log can say why a
	// chown happened -- "because you asked for it" and "because that is what
	// owned the file before" are different enough to be worth telling apart
	// when somebody is working out why a disk has the ownership it has.
	Source string
}

// Empty reports whether no owner could be determined at all.
//
// Both halves, not just the user: a qemu.conf that sets only `group` has
// still told us something, and chown accepts ":group" perfectly well. It is
// a partial answer rather than no answer, and treating it as no answer would
// discard a setting the operator explicitly wrote.
func (o DiskOwner) Empty() bool { return o.User == "" && o.Group == "" }

// Spec renders the owner the way chown takes it.
func (o DiskOwner) Spec() string {
	if o.Group == "" {
		return o.User
	}
	return o.User + ":" + o.Group
}

// ownerNameRe is what a user or group name may look like.
//
// Deliberately strict, because this value is interpolated into a chown
// command that runs as root on another machine. ShQuote already makes that
// safe against injection; this refuses the nonsense that would merely
// produce a confusing failure instead -- and a shell metacharacter here is
// far more likely to be a typo than an intention.
var ownerNameRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*\$?$`)

// ParseDiskOwner turns a -target-disk-owner value into a DiskOwner.
//
// Accepts "auto", "off", "user", "user:group" and ":group". An empty string
// means auto, so an operator who never sets the flag gets the safe default
// rather than the old broken behaviour.
func ParseDiskOwner(spec string) (DiskOwner, error) {
	spec = strings.TrimSpace(spec)
	switch spec {
	case "", DiskOwnerAuto:
		return DiskOwner{Source: DiskOwnerAuto}, nil
	case DiskOwnerOff:
		return DiskOwner{Source: DiskOwnerOff}, nil
	}

	user, group, hasGroup := strings.Cut(spec, ":")
	if user != "" && !ownerNameRe.MatchString(user) {
		return DiskOwner{}, fmt.Errorf("-target-disk-owner %q: %q is not a valid user name", spec, user)
	}
	if hasGroup && group != "" && !ownerNameRe.MatchString(group) {
		return DiskOwner{}, fmt.Errorf("-target-disk-owner %q: %q is not a valid group name", spec, group)
	}
	if user == "" && group == "" {
		return DiskOwner{}, fmt.Errorf("-target-disk-owner %q names neither a user nor a group", spec)
	}
	return DiskOwner{User: user, Group: group, Source: "the -target-disk-owner flag"}, nil
}

// IsAuto and IsOff report which mode a parsed value asked for.
func (o DiskOwner) IsAuto() bool { return o.Empty() && o.Source == DiskOwnerAuto }
func (o DiskOwner) IsOff() bool  { return o.Source == DiskOwnerOff }

// statOwnerRe matches what `stat -c %U:%G` prints. A user or group with no
// passwd/group entry prints as its numeric id, which is still a perfectly
// good thing to chown back to.
var statOwnerRe = regexp.MustCompile(`^([A-Za-z0-9_][A-Za-z0-9_.-]*\$?|[0-9]+):([A-Za-z0-9_][A-Za-z0-9_.-]*\$?|[0-9]+)$`)

// ParseStatOwner reads the output of `stat -c %U:%G <file>`.
//
// Returns an empty DiskOwner rather than an error when the output is not a
// plausible owner: the command is run in a "if there is a file here, what
// owns it" spirit, where "there was no file" is an ordinary answer that
// arrives as stat's error text on stdout.
func ParseStatOwner(out string) DiskOwner {
	line := strings.TrimSpace(out)
	if i := strings.LastIndex(line, "\n"); i >= 0 {
		line = strings.TrimSpace(line[i+1:])
	}
	m := statOwnerRe.FindStringSubmatch(line)
	if m == nil {
		return DiskOwner{}
	}
	return DiskOwner{User: m[1], Group: m[2], Source: "the disk file it replaces"}
}

// qemuConfOwnerRe matches an uncommented user/group assignment in libvirt's
// qemu.conf, e.g. `user = "qemu"`.
var qemuConfOwnerRe = regexp.MustCompile(`(?m)^[ \t]*(user|group)[ \t]*=[ \t]*"([^"]*)"`)

// ParseQemuConfOwner reads the user/group libvirt is configured to run qemu
// as, from the text of its qemu.conf.
//
// Only UNCOMMENTED settings count, and that is the whole point of reading the
// file rather than assuming. Every distribution ships this file with the
// setting present but commented out, and the value behind the comment is a
// compile-time default that differs between them -- `qemu` on RHEL and
// Fedora, `libvirt-qemu` on Debian and Ubuntu. Reading a commented line as
// though it were in force would produce a confident chown to a user that may
// not be the right one, which is worse than saying nothing: it would look
// like the problem had been handled.
//
// So an all-commented file returns empty, and the caller warns instead.
func ParseQemuConfOwner(conf string) DiskOwner {
	var o DiskOwner
	for _, m := range qemuConfOwnerRe.FindAllStringSubmatch(conf, -1) {
		// Last assignment wins, matching how libvirt parses it.
		switch m[1] {
		case "user":
			o.User = m[2]
		case "group":
			o.Group = m[2]
		}
	}
	if o.User == "" && o.Group == "" {
		return DiskOwner{}
	}
	o.Source = "the target's libvirt qemu.conf"
	return o
}

// StatOwnerCommand builds the shell command that reports a file's owner.
func StatOwnerCommand(p string) string {
	return "stat -c %U:%G " + ShQuote(p) + " 2>/dev/null || true"
}

// ChownCommand builds the shell command that applies an owner to a file.
func ChownCommand(o DiskOwner, p string) string {
	return "chown " + ShQuote(o.Spec()) + " " + ShQuote(p)
}

// MkdirOwnedCommand creates a directory and gives the SAME owner to every
// component it had to create -- and to nothing else.
//
// The problem it solves: the disk file is chowned to the account qemu runs
// as, but the directories above it were left owned by the SSH user, which is
// root. A root-owned 0755 directory happens to be traversable, so this looks
// fine on most hosts -- until one has a restrictive umask, `mkdir -p` makes
// the chain 0700 root-owned, and qemu cannot open a disk it demonstrably
// owns. The failure surfaces as a permission error on the disk, which is
// exactly the wrong place to go looking.
//
// EXISTING DIRECTORIES ARE NEVER TOUCHED. That is the whole difficulty, and
// why this is not `install -d -o …` or a chown of the parent: the target path
// commonly lives under something the operator already set up -- /var/lib/
// libvirt/images, an NFS mount, a dataset root -- and quietly re-owning that
// because a replica landed inside it would be a far worse bug than the one
// being fixed. Only components this command brings into existence are
// chowned.
//
// The path is decomposed HERE, in Go, and the command is a flat sequence of
// `mkdir <component> && chown …` with no shell loop and no `test`.
//
// Two reasons, and the second is the important one:
//
//   - A shell loop that does `[ ! -d "$p" ]` and then creates is a
//     time-of-check-to-time-of-use race. `mkdir` WITHOUT -p is atomic and
//     already answers the only question being asked: it succeeds exactly
//     when it created the directory, and fails when something is already
//     there. Making success the signal removes the race rather than
//     narrowing it.
//   - Everything decidable from the path string alone is then decided in Go,
//     where it is directly testable. What remains in the shell is one
//     `mkdir` and one `chown` per component, each argument quoted here.
//
// Why shell at all: this runs on the TARGET host over SSH, so os.MkdirAll
// would create the directory on the wrong machine. Every other thing vmsync
// does to a target -- qemu-img, stat, chown, mv, the reflink copies -- goes
// through the same one-method CommandRunner seam, and the alternative is a
// second transport (SFTP) to authenticate and keep in agreement with the
// first, for one mkdir.
//
// A final `mkdir -p` closes the sequence deliberately un-silenced: the
// per-component ones discard their errors, because "it already exists" is the
// expected case and indistinguishable from a real failure by exit status
// alone, so the last one is what reports a genuine problem with its own
// message.
//
// Returns a plain `mkdir -p` when no owner is known (auto that resolved to
// nothing, or explicitly off), which is exactly the previous behaviour.
func MkdirOwnedCommand(o DiskOwner, dir string) string {
	mk := "mkdir -p " + ShQuote(dir)
	if o.Empty() {
		return mk
	}
	spec := ShQuote(o.Spec())
	var b strings.Builder
	for _, c := range ancestorPaths(dir) {
		q := ShQuote(c)
		// 2>/dev/null: an existing directory is the ordinary case here, not a
		// problem worth a line of stderr per component per sync.
		fmt.Fprintf(&b, "mkdir %s 2>/dev/null && chown %s %s; ", q, spec, q)
	}
	b.WriteString(mk)
	return b.String()
}

// ancestorPaths lists every component of an absolute path, shallowest first.
//
// "/data/replicas/web01" -> ["/data", "/data/replicas", "/data/replicas/web01"]
//
// The root itself is never included: it always exists, and a `chown /` that
// somehow slipped through would be catastrophic rather than merely wrong.
func ancestorPaths(dir string) []string {
	clean := path.Clean(dir)
	if clean == "/" || clean == "." || clean == "" {
		return nil
	}
	var parts []string
	for p := clean; p != "/" && p != "." && p != ""; p = path.Dir(p) {
		parts = append(parts, p)
	}
	// Collected leaf-first; create shallowest-first.
	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	return parts
}

// QemuConfPaths are where libvirt's qemu.conf lives, most likely first.
var QemuConfPaths = []string{
	"/etc/libvirt/qemu.conf",
	"/usr/local/etc/libvirt/qemu.conf",
}

// KnownQemuAccounts are the accounts a distribution's libvirt runs qemu as,
// created by the package that ships qemu precisely for that purpose.
//
// Used only as a last resort, when qemu.conf leaves the setting commented
// out -- which is how every distribution ships it, so this is the ordinary
// case on a first-ever sync rather than an exotic one.
//
// Inferring from a system account's existence is weaker evidence than
// reading a configured value, and it is worth being precise about why it is
// nonetheless worth acting on. Being WRONG here is not worse than doing
// nothing: the file is root-owned either way, and root-owned is already
// unusable by a non-root qemu. If libvirt happens to run qemu as root, a
// qemu-owned file is still perfectly openable by it. So the failure mode of
// guessing is "no worse than before", while the failure mode of doing
// nothing is a replica that cannot boot -- discovered during a failover.
var KnownQemuAccounts = []DiskOwner{
	{User: "qemu", Group: "qemu"},        // RHEL, Fedora, CentOS, SUSE
	{User: "libvirt-qemu", Group: "kvm"}, // Debian, Ubuntu
}

// AccountExistsCommand builds a command that prints "yes" or "no".
//
// getent rather than reading /etc/passwd: it consults NSS, so an account
// that lives in LDAP or SSSD -- entirely normal on a managed hypervisor --
// is found too.
// database is one of getent's own fixed names ("passwd", "group") and is
// never operator input, so it is not quoted; name is, because it comes from
// KnownQemuAccounts today and could come from elsewhere tomorrow.
func AccountExistsCommand(database, name string) string {
	return "getent " + database + " " + ShQuote(name) + " >/dev/null 2>&1 && echo yes || echo no"
}

// ParseAccountExists reads what AccountExistsCommand printed.
func ParseAccountExists(out string) bool {
	return strings.TrimSpace(out) == "yes"
}

// DetectQemuAccount finds which well-known qemu account exists on a host.
//
// Returns the owner to use plus every candidate that matched. More than one
// match is deliberately NOT resolved by preference order: a host carrying
// both `qemu` and `libvirt-qemu` is unusual enough that picking one silently
// would be a worse answer than telling somebody to decide.
func DetectQemuAccount(ctx context.Context, r CommandRunner) (DiskOwner, []string) {
	var found []string
	var first DiskOwner
	for _, cand := range KnownQemuAccounts {
		out, err := r.Run(ctx, AccountExistsCommand("passwd", cand.User))
		if err != nil || !ParseAccountExists(out) {
			continue
		}
		found = append(found, cand.User)
		if !first.Empty() {
			continue
		}
		o := DiskOwner{User: cand.User, Source: "a " + cand.User + " account on the target host"}
		// Only name the group if it exists too. RHEL's qemu user is in a
		// qemu group and Debian's libvirt-qemu is in kvm, but a chown naming
		// a group that is absent fails outright -- and failing the run over
		// the group half would throw away a correct answer for the user half.
		if gout, gerr := r.Run(ctx, AccountExistsCommand("group", cand.Group)); gerr == nil && ParseAccountExists(gout) {
			o.Group = cand.Group
		}
		first = o
	}
	if len(found) != 1 {
		return DiskOwner{}, found
	}
	return first, found
}

// ReadQemuConfOwner asks a host what user its libvirt runs qemu as.
//
// Best-effort by design: a host that cannot be read, or whose qemu.conf says
// nothing, yields an empty owner and no error. There is nothing wrong with
// either, and turning "I could not determine this" into a failed sync would
// be a poor trade for a check that only matters on a first-ever copy.
func ReadQemuConfOwner(ctx context.Context, r CommandRunner) DiskOwner {
	for _, p := range QemuConfPaths {
		out, err := r.Run(ctx, "cat "+ShQuote(p)+" 2>/dev/null || true")
		if err != nil {
			continue
		}
		if o := ParseQemuConfOwner(out); !o.Empty() {
			return o
		}
	}
	return DiskOwner{}
}
