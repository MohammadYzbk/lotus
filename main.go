// Command lotus is an offline-first desktop LaTeX editor. It orchestrates an
// external TeX engine rather than implementing one, and wraps it in a modern
// editing experience.
package main

import (
	"embed"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
)

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	// Create an instance of the app structure
	app := NewApp()

	// Create application with options
	err := wails.Run(&options.App{
		Title:  "lotus",
		Width:  1024,
		Height: 768,
		AssetServer: &assetserver.Options{
			Assets: assets,
			// Claims our own paths ahead of asset resolution, so they work the
			// same under `wails dev` (where assets are proxied to Vite) as in a
			// build.
			Middleware: app.assetMiddleware(),
		},
		BackgroundColour: &options.RGBA{R: 27, G: 38, B: 54, A: 1},
		// Wails only reads its zoomable flag when Mac options are present, and
		// defaults it to false otherwise — which makes it disable the green
		// button outright, taking fullscreen with it. An empty struct is enough
		// to get the standard window behaviour back.
		Mac:       &mac.Options{},
		OnStartup: app.startup,
		Bind: []any{
			app,
		},
	})

	if err != nil {
		println("Error:", err.Error())
	}
}
