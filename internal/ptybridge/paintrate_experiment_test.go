package ptybridge_test

// An experiment, not a test: run with TMUX_WEB_PAINT_EXPERIMENT=1 and read the
// numbers it prints. It exists to answer one question with measurements rather
// than reasoning -- where the latency in a watched agent pane comes from -- and
// it runs against a real tmux server on a socket of its own, through the real
// bridge.
//
// What the owner's own daemon log says the real thing looks like, for scale:
// one connection watching an agent for 22m48s sent 200,845,064 bytes in 47,928
// frames (147 KB/s, 35 frames/s, 4.19 KB/frame) with no typing at all, over a
// WireGuard link whose minimum RTT is 113ms.

import (
	"fmt"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/diegok/tmux-web/internal/ptybridge"
	"github.com/diegok/tmux-web/internal/tmux/testutil"
)

// paintScript repaints a full screen of coloured cells at a fixed rate, which
// is what an agent's TUI does between tokens. Written through tmux's own pane
// so that what the bridge reads is tmux's rendering, not the painter's output:
// the whole question is what tmux emits to a client, and that is a different
// byte stream from what the program wrote.
const paintScript = `python3 -c '
import sys, time
cols, rows, hz, secs = 200, 50, 15, 20
cell = "".join("\x1b[3%dm%s" % (c % 8, "ab") for c in range(cols // 2))
end = time.time() + secs
while time.time() < end:
    out = ["\x1b[H"]
    for r in range(rows):
        out.append("\x1b[%d;1H%s" % (r + 1, cell))
    sys.stdout.write("".join(out))
    sys.stdout.flush()
    time.sleep(1.0 / hz)
'`

type paintFrame struct {
	at time.Time
	n  int
}

func TestExperimentPaintRate(t *testing.T) {
	if os.Getenv("TMUX_WEB_PAINT_EXPERIMENT") == "" {
		t.Skip("set TMUX_WEB_PAINT_EXPERIMENT=1 to run the measurement")
	}

	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "work", "-x", "200", "-y", "50")

	s := open(t, srv, ptybridge.Config{TmuxArgs: srv.Args(), Base: "work", Cols: 200, Rows: 50})

	// Start the painter after the attach, so the initial repaint is not counted
	// as part of the steady state.
	time.Sleep(500 * time.Millisecond)
	srv.Run(t, "send-keys", "-t", "=work:", paintScript, "Enter")

	var frames []paintFrame
	deadline := time.After(21 * time.Second)
	started := time.Now()
collect:
	for {
		select {
		case b, ok := <-s.Output():
			if !ok {
				break collect
			}
			frames = append(frames, paintFrame{at: time.Now(), n: len(b)})
		case <-deadline:
			break collect
		}
	}
	elapsed := time.Since(started)

	if len(frames) < 10 {
		t.Fatalf("only %d frames in %v: the painter never ran", len(frames), elapsed)
	}

	var bytes int
	sizes := make([]int, 0, len(frames))
	gaps := make([]time.Duration, 0, len(frames))
	for i, f := range frames {
		bytes += f.n
		sizes = append(sizes, f.n)
		if i > 0 {
			gaps = append(gaps, f.at.Sub(frames[i-1].at))
		}
	}
	sort.Ints(sizes)
	sort.Slice(gaps, func(i, j int) bool { return gaps[i] < gaps[j] })

	fmt.Printf("\n=== what one watched agent pane costs ===\n")
	fmt.Printf("window        200x50, painter 15 full repaints/s, %v of stream\n", elapsed.Round(time.Millisecond))
	fmt.Printf("frames        %d (%.1f/s)\n", len(frames), float64(len(frames))/elapsed.Seconds())
	fmt.Printf("bytes         %d (%.1f KB/s)\n", bytes, float64(bytes)/1024/elapsed.Seconds())
	fmt.Printf("frame size    p50 %d  p90 %d  max %d bytes\n",
		sizes[len(sizes)/2], sizes[len(sizes)*9/10], sizes[len(sizes)-1])
	fmt.Printf("gap between   p50 %v  p90 %v\n",
		gaps[len(gaps)/2].Round(time.Microsecond), gaps[len(gaps)*9/10].Round(time.Microsecond))

	// What coalescing would have produced. One WebSocket frame per PTY read is
	// what the write loop does today; this is the same bytes written as fewer,
	// larger frames, with the cost stated as the delay a byte waits for the
	// window it landed in to close.
	fmt.Printf("\n=== coalescing the same bytes ===\n")
	fmt.Printf("%-8s %8s %10s %12s %12s\n", "window", "frames", "per frame", "mean delay", "max delay")
	for _, w := range []time.Duration{4 * time.Millisecond, 8 * time.Millisecond, 16 * time.Millisecond, 33 * time.Millisecond} {
		n, totalDelay, maxDelay := coalesce(frames, w)
		fmt.Printf("%-8v %8d %10.0f %12v %12v\n", w, n, float64(bytes)/float64(n),
			(totalDelay / time.Duration(len(frames))).Round(time.Microsecond),
			maxDelay.Round(time.Microsecond))
	}

	// The link is the other half. These are the owner's measured numbers, not
	// this machine's: 8.2 Mbps and 113ms of one-way-and-back, which makes the
	// bandwidth-delay product the floor on how much is always in flight.
	const linkBps, rttMs = 8238503.0 / 8, 113.0
	bdp := linkBps * rttMs / 1000
	fmt.Printf("\n=== the same stream on his link (8.2 Mbps, 113ms RTT) ===\n")
	fmt.Printf("bandwidth-delay product   %.0f KB -- always in flight, cannot be reduced\n", bdp/1024)
	fmt.Printf("stream needs              %.1f KB/s of %.0f KB/s available (%.0f%%)\n",
		float64(bytes)/1024/elapsed.Seconds(), linkBps/1024, 100*float64(bytes)/elapsed.Seconds()/linkBps)
	for _, sndbuf := range []float64{493056, 262144, 131072, 65536} {
		fmt.Printf("send buffer %6.0f KB -> queue drains in %5.0f ms at link rate\n",
			sndbuf/1024, 1000*sndbuf/linkBps)
	}
}

// coalesce reports how many frames the same stream would have been written as
// if every read arriving within w of the window's first read went out together,
// and what that cost the bytes that waited.
func coalesce(frames []paintFrame, w time.Duration) (n int, totalDelay, maxDelay time.Duration) {
	if len(frames) == 0 {
		return 0, 0, 0
	}
	windowStart := frames[0].at
	n = 1
	for _, f := range frames {
		if f.at.Sub(windowStart) > w {
			windowStart = f.at
			n++
		}
		d := w - f.at.Sub(windowStart)
		if d < 0 {
			d = 0
		}
		totalDelay += d
		if d > maxDelay {
			maxDelay = d
		}
	}
	return n, totalDelay, maxDelay
}
