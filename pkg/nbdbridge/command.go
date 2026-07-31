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

	"vmsync/pkg/util"
)

// innerPipeline is the per-connection filter chain socat hands each accepted
// TCP connection's stdin/stdout to. It reads compressed bytes coming from the
// bridge client, decompresses, forwards to the real NBD export, and
// compresses the replies flowing back.
func innerPipeline(cfg Config, realPort int) string {
	return fmt.Sprintf("zstd -dq | socat - TCP:127.0.0.1:%d | zstd -q -%d", realPort, cfg.CompressLevel)
}

// BuildStartCommand returns a shell command that backgrounds a socat
// listener on 127.0.0.1:bridgePort, forwarding each accepted connection
// through innerPipeline, and records the listener's PID in pidFile. Using
// socat's SYSTEM: address (rather than EXEC:) is required here since the
// filter chain is itself a shell pipeline, not a single program.
func BuildStartCommand(cfg Config, bridgePort, realPort int, pidFile, logFile string) string {
	listen := fmt.Sprintf("socat TCP-LISTEN:%d,bind=127.0.0.1,reuseaddr,fork SYSTEM:%s",
		bridgePort, util.ShQuote(innerPipeline(cfg, realPort)))
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
