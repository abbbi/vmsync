/*
	Copyright (C) 2026  Michael Ablassmeier <abi@grinser.de>

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

package nbdbridge

import (
	"fmt"
	"strconv"
	"strings"

	"vmsync/pkg/util"
)

// quoteArgs renders a command as individually shell-quoted tokens, so the
// remote shell's own word-splitting (and its $IFS setting, whatever that
// happens to be) can't corrupt an argument like "127.0.0.1:20200" into the
// wrong number of words.
func quoteArgs(args ...string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = util.ShQuote(a)
	}
	return strings.Join(quoted, " ")
}

// BuildStartCommand returns a shell command that backgrounds cfg.HelperPath
// (vmsync-bridge-helper, deployed to the remote host by the user ahead of
// time -- see pkg/nbdbridge/tools.go's CheckRemote) listening on
// bridgePort and forwarding each accepted connection to the real NBD export
// on 127.0.0.1:realPort, and records its PID in pidFile.
//
// The listen address is 0.0.0.0:bridgePort by default, so the local relay
// can dial it directly over the network, or 127.0.0.1:bridgePort when
// cfg.UseSSH is set (reachable only through the caller's own SSH tunnel) --
// see Config.UseSSH's doc comment for the tradeoff.
//
// No socat (or any other external tool) is involved: the helper does its own
// listening, accepting, compression, and buffering natively in one program.
func BuildStartCommand(cfg Config, bridgePort, realPort int, pidFile, logFile string) string {
	listenHost := "0.0.0.0"
	if cfg.UseSSH {
		listenHost = "127.0.0.1"
	}
	args := []string{
		cfg.HelperPath,
		"-listen", fmt.Sprintf("%s:%d", listenHost, bridgePort),
		"-connect", fmt.Sprintf("127.0.0.1:%d", realPort),
	}
	if cfg.Compress {
		args = append(args, "-compress", "-level", strconv.Itoa(cfg.CompressLevel))
	}
	if cfg.NetBufferEnabled() {
		args = append(args, "-netbuffer", cfg.NetBufferBlock+","+cfg.NetBufferSize)
	}
	helperCmd := quoteArgs(args...)

	return fmt.Sprintf("setsid sh -c %s </dev/null >%s 2>&1 & echo $! > %s",
		util.ShQuote(helperCmd), util.ShQuote(logFile), util.ShQuote(pidFile))
}

// BuildStopCommand returns a shell command that kills the process group
// started by BuildStartCommand and removes its bookkeeping files. setsid
// makes the backgrounded shell its own process group leader (pid == pgid);
// since the shell execs the helper directly (a single simple command with
// only redirections, no pipeline), the helper ends up running as that same
// PID/PGID, so a negative-PID kill reaches it (and, defensively, anything
// else that might ever share its process group) directly.
func BuildStopCommand(pidFile, logFile string) string {
	return fmt.Sprintf("kill -9 -$(cat %s) || true; rm -f %s %s",
		util.ShQuote(pidFile), util.ShQuote(pidFile), util.ShQuote(logFile))
}
