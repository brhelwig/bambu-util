package web

import "github.com/brhelwig/bambu-util/internal/settings"

// current answers what the settings are right now. Everything that consults
// them calls it at the point of use, so an edit on the page takes effect
// without a restart.
type current func() settings.Values

// A countdown already running keeps the window it was armed with — shortening
// the window is not meant to reach back and shut a heater off early.
