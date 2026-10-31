//go:build !headless

package main

import (
	"embed"
	"os"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
)

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	if code, handled := runCLIFromGUI(os.Args[1:]); handled {
		os.Exit(code)
	}
	app := NewApp()

	workW, workH := windowWorkArea()
	width, height := fitWindowSize(workW, workH, windowWidth, windowHeight, windowMinWidth, windowMinHeight)

	err := wails.Run(&options.App{
		Title:     "ssh-gete",
		Width:     width,
		Height:    height,
		MinWidth:  windowMinWidth,
		MinHeight: windowMinHeight,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		BackgroundColour: &options.RGBA{R: 238, G: 243, B: 249, A: 1},
		OnStartup:        app.startup,
		OnShutdown:       app.shutdown,
		Bind: []interface{}{
			app,
		},
	})
	if err != nil {
		println("Error:", err.Error())
	}
}
