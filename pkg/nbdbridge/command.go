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

// mbufferStage renders one mbuffer invocation from cfg's block/buffer size.
func mbufferStage(cfg Config) string {
	return quoteArgs("mbuffer", "-q", "-s", cfg.MbufferBlock, "-m", cfg.MbufferSize)
}

// innerPipeline is the per-connection filter chain socat hands each accepted
// TCP connection's stdin/stdout to. It reads (compressed/buffered) bytes
// coming from the bridge client, restores them, forwards to the real NBD
// export, and re-applies the same treatment to the replies flowing back.
//
// socat's "-" <-> "TCP:..." bridging is itself bidirectional -- it reads its
// own stdin and forwards to the TCP peer, while independently reading the
// TCP peer and writing its own stdout -- so a single shell pipe built around
// it naturally splits into two simplex halves: everything before socat only
// ever sees the client-to-server direction, everything after it only ever
// sees the server-to-client direction. mbuffer, unlike socat, is a simplex
// filter, so it needs one instance on each side of socat to smooth both
// directions; zstd needs the same per-direction split to compress/decompress
// independently.
func innerPipeline(cfg Config, realPort int) string {
	var stages []string
	if cfg.Compress {
		stages = append(stages, quoteArgs("zstd", "-dq"))
	}
	if cfg.MbufferEnabled() {
		stages = append(stages, mbufferStage(cfg))
	}
	stages = append(stages, quoteArgs("socat", "-", fmt.Sprintf("TCP:127.0.0.1:%d", realPort)))
	if cfg.MbufferEnabled() {
		stages = append(stages, mbufferStage(cfg))
	}
	if cfg.Compress {
		stages = append(stages, quoteArgs("zstd", "-q", fmt.Sprintf("-%d", cfg.CompressLevel)))
	}
	return strings.Join(stages, " | ")
}

// BuildStartCommand returns a shell command that backgrounds a socat
// listener on 127.0.0.1:bridgePort, forwarding each accepted connection
// through innerPipeline, and records the listener's PID in pidFile. Using
// socat's SYSTEM: address (rather than EXEC:) is required here since the
// filter chain is itself a shell pipeline, not a single program.
func BuildStartCommand(cfg Config, bridgePort, realPort int, pidFile, logFile string) string {
	listen := quoteArgs("socat", fmt.Sprintf("TCP-LISTEN:%d,bind=127.0.0.1,reuseaddr,fork", bridgePort)) +
		" SYSTEM:" + util.ShQuote(innerPipeline(cfg, realPort))
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
