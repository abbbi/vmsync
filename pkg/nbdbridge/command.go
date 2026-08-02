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

// quoteArgs renders a command as individually shell-quoted tokens rather than
// relying on the remote shell's own whitespace word-splitting. This chain
// runs inside a shell spawned by socat's SYSTEM: (via libc system(), not a
// shell we invoke ourselves), which inherits whatever environment socat
// itself started with -- if $IFS there isn't the default, an unquoted
// argument like "TCP:127.0.0.1:20200" can silently be split into pieces
// socat then sees as the wrong number of address parameters. Quoting each
// token is immune to $IFS entirely, regardless of what it's set to.
func quoteArgs(args ...string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = util.ShQuote(a)
	}
	return strings.Join(quoted, " ")
}

// BuildStartCommand returns a shell command that backgrounds a socat
// listener on 127.0.0.1:bridgePort, forwarding each accepted connection to
// cfg.HelperPath (vmsync-bridge-helper, deployed to the remote host by the
// user ahead of time -- see pkg/nbdbridge/tools.go's CheckRemote), and
// records the listener's PID in pidFile.
//
// socat's EXEC: address (not SYSTEM:) is used deliberately: the helper does
// compression *and* buffering natively in one program now, so there's never
// a shell pipeline of multiple CLI tools to build. EXEC: execs the helper
// directly with no shell involved at all, which also fully retires the
// class of shell-quoting/$IFS bugs the old SYSTEM:-based shell pipe was
// once vulnerable to.
func BuildStartCommand(cfg Config, bridgePort, realPort int, pidFile, logFile string) string {
	args := []string{cfg.HelperPath, "-connect", fmt.Sprintf("127.0.0.1:%d", realPort)}
	if cfg.Compress {
		args = append(args, "-compress", "-level", strconv.Itoa(cfg.CompressLevel))
	}
	if cfg.NetBufferEnabled() {
		args = append(args, "-netbuffer", cfg.NetBufferBlock+","+cfg.NetBufferSize)
	}
	helperCmd := quoteArgs(args...)

	listen := quoteArgs("socat", fmt.Sprintf("TCP-LISTEN:%d,bind=127.0.0.1,reuseaddr,fork", bridgePort)) +
		" EXEC:" + util.ShQuote(helperCmd)
	return fmt.Sprintf("setsid sh -c %s </dev/null >%s 2>&1 & echo $! > %s",
		util.ShQuote(listen), util.ShQuote(logFile), util.ShQuote(pidFile))
}

// BuildStopCommand returns a shell command that kills the process group
// started by BuildStartCommand and removes its bookkeeping files. setsid
// makes the backgrounded shell its own process group leader (pid == pgid),
// so a negative-PID kill reaches socat and any in-flight EXEC/SYSTEM filter
// children, not just the listener itself.
func BuildStopCommand(pidFile, logFile string) string {
	return fmt.Sprintf("kill -9 -$(cat %s) || true; rm -f %s %s",
		util.ShQuote(pidFile), util.ShQuote(pidFile), util.ShQuote(logFile))
}
