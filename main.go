package main

import (
	"embed"
	"log"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
	"github.com/wailsapp/wails/v2/pkg/options/windows"
)

//go:embed all:frontend/dist
var assets embed.FS

const appVersion = "0.3.0"

func main() {
	app := NewApp()
	err := wails.Run(&options.App{
		Title: "Tenbyte Mail Migrator", Width: 1240, Height: 820, MinWidth: 980, MinHeight: 680,
		AssetServer: &assetserver.Options{Assets: assets}, BackgroundColour: &options.RGBA{R: 244, G: 246, B: 248, A: 1},
		OnStartup: app.startup, OnDomReady: app.domReady, OnShutdown: app.shutdown, Bind: []interface{}{app},
		Mac:     &mac.Options{TitleBar: mac.TitleBarHiddenInset(), WebviewIsTransparent: false, WindowIsTranslucent: false, DisableZoom: true},
		Windows: &windows.Options{WebviewIsTransparent: false, WindowIsTranslucent: false, DisableWindowIcon: true},
	})
	if err != nil {
		log.Fatal(err)
	}
}
