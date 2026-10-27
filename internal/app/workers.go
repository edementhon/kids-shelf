// Background workers: each long-running service loop runs in its own
// goroutine with panic recovery, and restarts after a backoff when it
// returns or crashes. The queue-draining workers (ingest, youtube, kiwix)
// return when the queue is empty, so they restart quickly and poll; the
// library scan and the daily yt-dlp check run on their own schedules.
package app

import (
	"context"
	"fmt"
	"runtime/debug"
	"time"
)

// Worker pacing: how long a worker waits between runs while healthy, and
// how far repeated failures back off before the next attempt.
const (
	minRestart    = time.Second
	maxBackoff    = time.Minute
	scanInterval  = 5 * time.Minute
	dailyInterval = 24 * time.Hour
)

// worker is one named background loop: run executes one pass, restart is
// the nominal wait before the next pass on a successful one.
type worker struct {
	name    string
	restart time.Duration
	run     func(context.Context) error
}

// workersOf lists every background loop of the app. The scan and the
// yt-dlp check run their first pass immediately, then on their interval.
// An ImportOnly app runs the ingest worker alone: a one-off import waits on
// the conversions it queued itself, and the scanner would queue the whole
// library ahead of them.
func workersOf(a *App) []worker {
	ingest := worker{name: "ingest", restart: minRestart, run: a.worker.Run}
	if a.cfg.ImportOnly {
		return []worker{ingest}
	}
	return []worker{
		ingest,
		{name: "youtube", restart: minRestart, run: a.youtube.Run},
		{name: "kiwix", restart: minRestart, run: a.kiwix.Work},
		{name: "library scan", restart: scanInterval, run: a.scan},
		{name: "yt-dlp update", restart: dailyInterval, run: a.updateYtDlp},
	}
}

// scan enqueues new files from the registered media sources. It is a single
// pass; the loop gives it its 5-minute cadence.
func (a *App) scan(ctx context.Context) error {
	return a.library.Scan(ctx, a.ingestQ)
}

// updateYtDlp runs the daily release check. EnsureLatest's once-a-day gate
// makes repeated calls cheap; this worker sets the cadence.
func (a *App) updateYtDlp(ctx context.Context) error {
	_, err := a.ytdlp.EnsureLatest(ctx, false)
	return err
}

// runWorker executes one worker until the app context ends. A run that
// returns an error or panics is logged and retried, backing off
// exponentially between attempts; a clean run waits only restart.
func (a *App) runWorker(w worker) {
	defer a.wg.Done()
	backoff := time.Duration(0)
	for {
		err := a.guarded(w)
		if a.ctx.Err() != nil {
			return
		}
		if err != nil {
			logf("worker %s: %v", w.name, err)
			if backoff == 0 {
				backoff = minRestart
			} else {
				backoff *= 2
			}
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		} else {
			backoff = 0
		}
		timer := time.NewTimer(w.restart + backoff)
		select {
		case <-a.ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// guarded runs one worker pass, converting a panic into an error after
// logging it, so a crashing pass restarts instead of taking down the app.
func (a *App) guarded(w worker) (err error) {
	defer func() {
		if r := recover(); r != nil {
			logf("worker %s: panic: %v\n%s", w.name, r, debug.Stack())
			err = fmt.Errorf("worker %s panicked: %v", w.name, r)
		}
	}()
	return w.run(a.ctx)
}
