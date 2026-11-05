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

// What vmsync says when the bridge it was asked to use is not there to use --
// one file for the two refusals that an operator meets as the same question,
// "why does this need a binary on the other host", and that must therefore
// answer it the same way.
package main

import (
	"errors"
	"fmt"
)

// useSSHWithoutBridgeRefusal refuses -use-ssh passed without -compress or
// -netbuffer, and returns nil for every other combination.
//
// Refused rather than ignored, which it was until 2026-11-05. -use-ssh tunnels
// the BRIDGED connection through the existing SSH connection, and the bridge is
// what -compress/-netbuffer create; every reader of cfg.UseSSH sits inside a
// bridgeCfg.Enabled() branch, so with neither flag the value was carried the
// length of the run and never consulted.
//
// The cost of that no-op is not the usual one. A flag that quietly does
// nothing is normally discovered late; this one left an operator believing
// their NBD traffic was inside an SSH tunnel while it crossed the network in
// the clear. And two other places already said it was refused -- the agent's
// own profile validation and docs/HOWTO.md -- so the engine was the only one
// of the three that permitted what its documentation called impossible.
//
// A function rather than an inline check because main() cannot be entered by a
// test: everything around it exits the process.
func useSSHWithoutBridgeRefusal(cfg syncConfig) error {
	if !cfg.UseSSH || cfg.Compress != "" || cfg.NetBuffer != "" {
		return nil
	}
	return errors.New("-use-ssh needs -compress or -netbuffer: it tunnels the bridged connection through the existing SSH connection, and those are what create the bridge. Without one of them there is nothing to tunnel and the NBD traffic goes over a direct TCP connection between the hosts, unencrypted -- the opposite of what passing this flag asks for")
}

// helperMissingRefusal is what a run says when the target has no usable
// vmsync-bridge-helper and the run did not opt out of the integrity check.
//
// It is reached ONLY on a run that asked for no bridging: `case
// bridgeCfg.Enabled()` is matched first, so -compress and -netbuffer are
// refused by nbdbridge.CheckRemote and never by this. Which is why the
// wording says the integrity check is what THIS run needs the helper for,
// rather than describing the helper -- and then names the rest, because the
// operator reading this is deciding whether to deploy a binary, and that
// decision is about everything on the target that uses it, not about one
// check.
//
// WHAT THE TARGET HELPER IS FOR, measured against the code 2026-11-05, since
// this message is the only place an operator meets most of it:
//
//	refuses without it   the pre-commit integrity check (here)
//	                     -compress / -netbuffer, the NBD relay
//	                       (nbdbridge.CheckRemote, and no -no-checksum past it)
//	                     -test=corrupt-before-checksum (main.go)
//	                     taking the target run lock AT ALL, on a target whose
//	                       shell has no `test -ef` or no /proc/<pid>/fd
//	                       (pkg/util/remotelock.go, which names this binary as
//	                       the remedy)
//	                     -break-target-lock later, since only the leased hold
//	                       stamps the lock with who took it
//	degrades, loudly     supervision of every target command
//	                     the lease on the target run lock -- and with it the
//	                       only way a run notices its lock being taken away
//	degrades, quietly    the digest fast path of -verify=fast|full
//
// The first wording of this refusal named the binary to deploy and then said
// "avoid using features that need the helper" without naming -no-checksum at
// all, and was read, correctly, as "vmsync cannot be used without the
// helper". The second named the flag but explained itself purely as a
// checksum message, which understates by five entries what the operator is
// actually choosing about. Both failures are the same one: a refusal that
// states part of the picture has made the decision it claims to be handing
// over.
func helperMissingRefusal(targetDomain, reason, helperPath, targetHost string) error {
	return fmt.Errorf("refusing to sync %s: vmsync-bridge-helper %s. "+
		"This run needs it on the target for the pre-commit integrity check, which is on by default. "+
		"Deploy a matching one at %s on %s -- matching exactly, since an older helper under a newer vmsync is refused just like an absent one -- or pass -no-checksum to sync without the check. "+
		"A sync without the helper also runs its target commands unsupervised and takes the target's run lock unleased, each warned about on its own, and an unleased hold cannot notice that lock being removed under a live run. "+
		"-no-checksum does not cover -compress or -netbuffer: they need the same helper on the same host and are refused on their own",
		targetDomain, reason, helperPath, targetHost)
}
