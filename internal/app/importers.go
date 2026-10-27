package app

import "github.com/edementhon/kids-shelf/internal/core"

// Importers are the live services a one-off data import writes through (the
// legacy cutover driver uses them for internal/migrate). They are the app's
// own instances, so imported rows land exactly where the running app reads
// them, and the ingest queue is the one the app's workers drain.
type Importers struct {
	Kids     core.Kids
	Budget   core.BudgetImporter
	Lookup   core.EpisodeLookup
	Playback core.PlaybackImporter
	Practice core.PracticeImporter
	Queue    core.IngestQueue
	Sources  core.SourceRegistry
}

// Importers returns the app's import targets.
func (a *App) Importers() Importers {
	return Importers{
		Kids:     a.budget,
		Budget:   a.budget,
		Lookup:   a.library,
		Playback: a.library,
		Practice: a.prac,
		Queue:    a.ingestQ,
		Sources:  a.library,
	}
}
