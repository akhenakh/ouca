// Command visual is an interactive terminal UI for the ouca map matcher.
//
// Click on the map to drop trace points (a blue polyline is drawn as you
// click), press m to run HMM map matching over the clicked trace and display
// the matched road path in orange, and press p to cycle the matching mode
// (car, bike, pedestrian), re-running the match automatically.
//
// Requires a terminal with Kitty graphics protocol support (kitty, WezTerm,
// Ghostty, foot, iTerm2).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"image/color"
	"log/slog"
	"os"

	tea "charm.land/bubbletea/v2"
	"github.com/akhenakh/maprender"
	"github.com/akhenakh/ouca"
	"github.com/akhenakh/tiletea"
	"github.com/peterstace/simplefeatures/geom"
)

var modes = []struct {
	label string
	ouca  string
}{
	{"car", "car"},
	{"bike", "bike"},
	{"pedestrian", "walk"},
}

var (
	traceColor   = color.NRGBA{R: 0x00, G: 0x88, B: 0xff, A: 0xff}
	matchedColor = color.NRGBA{R: 0xff, G: 0x55, B: 0x00, A: 0xff}
)

type matchDoneMsg struct {
	match *ouca.PathMatch
	err   error
}

type app struct {
	m       *tiletea.Map
	index   *ouca.Index
	trace   []ouca.LatLng
	modeIdx int
	clicked bool

	log     *slog.Logger
	logFile *os.File

	status string
}

func main() {
	var (
		lat   = flag.Float64("lat", 40.7128, "initial center latitude")
		lng   = flag.Float64("lng", -74.0060, "initial center longitude")
		zoom  = flag.Int("zoom", 15, "initial zoom level")
		debug = flag.String("debug", "debug.log", "debug log file (empty disables)")
	)
	flag.Parse()

	logger, logFile, err := openDebugLog(*debug)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: debug logging disabled: %v\n", err)
		logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	slog.SetDefault(logger)
	logger.Info("visual starting", "lat", *lat, "lng", *lng, "zoom", *zoom)

	a := &app{
		index:   ouca.NewIndex(),
		m:       tiletea.New(*lat, *lng, *zoom, tiletea.WithLogger(logger)),
		log:     logger,
		logFile: logFile,
	}
	a.m.SetClickCallback(func(clat, clng float64) {
		a.clicked = true
		a.trace = append(a.trace, ouca.LatLng{Lat: clat, Lng: clng})
		logger.Info("click", "n", len(a.trace), "lat", clat, "lng", clng)
	})
	a.setStatus()

	p := tea.NewProgram(a)
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if a.logFile != nil {
		a.logFile.Close()
	}
}

// openDebugLog opens the debug log file for appending. An empty path disables
// file logging and returns a nil logger/file.
func openDebugLog(path string) (*slog.Logger, *os.File, error) {
	if path == "" {
		return nil, nil, nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, nil, err
	}
	logger := slog.New(slog.NewTextHandler(f, nil))
	return logger, f, nil
}

func (a *app) Init() tea.Cmd { return a.m.Init() }

func (a *app) View() tea.View { return a.m.View() }

func (a *app) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c":
			return a, tea.Quit
		case "m":
			if len(a.trace) < 2 {
				a.status = "click at least 2 points before matching"
				a.m.SetStatusExtra(a.status)
				return a, nil
			}
			a.status = "matching (" + modes[a.modeIdx].label + ")..."
			a.m.SetStatusExtra(a.status)
			return a, a.matchCmd()
		case "p":
			a.modeIdx = (a.modeIdx + 1) % len(modes)
			if len(a.trace) >= 2 {
				a.status = "matching (" + modes[a.modeIdx].label + ")..."
				a.m.SetStatusExtra(a.status)
				return a, a.matchCmd()
			}
			a.setStatus()
			return a, nil
		}

	case matchDoneMsg:
		if msg.err != nil {
			a.status = "match failed: " + msg.err.Error()
			a.m.SetStatusExtra(a.status)
			return a, nil
		}
		a.log.Info("match displayed", "mode", modes[a.modeIdx].label)
		a.applyMatch(msg.match)
		a.setStatus()
		return a, a.m.Refresh()
	}

	m, cmd := a.m.Update(msg)
	a.m = m.(*tiletea.Map)

	if a.clicked {
		a.clicked = false
		a.redrawOverlays()
		a.setStatus()
		cmd = a.m.Refresh()
	}
	return a, cmd
}

func (a *app) matchCmd() tea.Cmd {
	trace := append([]ouca.LatLng(nil), a.trace...)
	mode := modes[a.modeIdx].ouca
	opts := ouca.DefaultPathOptions(mode)
	index := a.index
	log := a.log
	log.Info("match request", "mode", mode, "opts", opts, "trace", trace)
	return func() tea.Msg {
		match, err := index.MatchPath(context.Background(), trace, &opts)
		if err != nil {
			log.Error("match failed", "err", err)
			return matchDoneMsg{err: err}
		}
		logMatchResult(log, match)
		return matchDoneMsg{match: match}
	}
}

// logMatchResult dumps the full match plus a per-point summary so the clicked
// trace can be compared against what the matcher snapped to.
func logMatchResult(log *slog.Logger, m *ouca.PathMatch) {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		log.Error("marshal match", "err", err)
		return
	}
	log.Info("match result", "json", string(data))
	for i, p := range m.Points {
		log.Info("match point",
			"i", i,
			"input", fmt.Sprintf("%.6f,%.6f", p.Input.Lat, p.Input.Lng),
			"snapped", fmt.Sprintf("%.6f,%.6f", p.Lat, p.Lng),
			"street", p.Street,
			"class", p.Class,
			"dist_m", p.Distance,
			"matched", p.Matched,
		)
	}
	log.Info("match path",
		"points", len(m.Path),
		"length_m", m.Length,
		"path", fmt.Sprintf("%v", m.Path),
	)
}

// applyMatch stores the result and rebuilds the overlays: the clicked trace in
// blue plus the matched road path in orange.
func (a *app) applyMatch(match *ouca.PathMatch) {
	var overlays []maprender.Overlay
	if o, err := lineStringOverlay(a.trace, traceColor, 3); err == nil {
		overlays = append(overlays, o)
	}
	if o, err := lineStringOverlay(match.Path, matchedColor, 4); err == nil {
		overlays = append(overlays, o)
	}
	a.m.SetOverlays(overlays...)
}

// redrawOverlays draws the current trace (and any previous match).
func (a *app) redrawOverlays() {
	var overlays []maprender.Overlay
	if o, err := lineStringOverlay(a.trace, traceColor, 3); err == nil {
		overlays = append(overlays, o)
	}
	a.m.SetOverlays(overlays...)
}

func (a *app) setStatus() {
	a.status = fmt.Sprintf(
		"[p] mode: %s | [m] match | clicks: %d",
		modes[a.modeIdx].label, len(a.trace),
	)
	a.m.SetStatusExtra(a.status)
}

func lineStringOverlay(pts []ouca.LatLng, c color.Color, width float64) (maprender.Overlay, error) {
	if len(pts) < 2 {
		return maprender.Overlay{}, fmt.Errorf("need at least 2 points")
	}
	vals := make([]float64, 0, len(pts)*2)
	for _, p := range pts {
		vals = append(vals, p.Lng, p.Lat)
	}
	ls := geom.NewLineString(geom.NewSequence(vals, geom.DimXY))
	return maprender.Overlay{
		Geometry:    ls.AsGeometry(),
		StrokeColor: c,
		StrokeWidth: width,
	}, nil
}
