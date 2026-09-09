package main

import (
	"embed"
	"log"
	"runtime/debug"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/logger"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
	"github.com/wailsapp/wails/v2/pkg/options/windows"

	"github.com/tenbyte/mail-migrator/internal/diagnostics"
)

//go:embed all:frontend/dist
var assets embed.FS

const appVersion = "0.7.0"

func main() {
	diagnosticManager, diagnosticErr := diagnostics.New(appVersion)
	if diagnosticErr != nil {
		log.Printf("diagnostics unavailable: %v", diagnosticErr)
	}
	if diagnosticManager != nil {
		defer func() {
			if recovered := recover(); recovered != nil {
				diagnosticManager.RecordPanic("main", 0, "", recovered, debug.Stack())
				_ = diagnosticManager.Close()
				panic(recovered)
			}
			_ = diagnosticManager.Close()
		}()
	}
	app := NewApp(diagnosticManager)
	appOptions := &options.App{
		Title: "Tenbyte Mail Migrator", Width: 1240, Height: 820, MinWidth: 980, MinHeight: 680,
		AssetServer: &assetserver.Options{Assets: assets}, BackgroundColour: &options.RGBA{R: 244, G: 246, B: 248, A: 1},
		OnStartup: app.startup, OnDomReady: app.domReady, OnShutdown: app.shutdown, Bind: []interface{}{app},
		Mac:      &mac.Options{TitleBar: mac.TitleBarHiddenInset(), WebviewIsTransparent: false, WindowIsTranslucent: false, DisableZoom: true},
		Windows:  &windows.Options{WebviewIsTransparent: false, WindowIsTranslucent: false, DisableWindowIcon: true},
		LogLevel: logger.INFO, LogLevelProduction: logger.INFO,
	}
	if diagnosticManager != nil {
		appOptions.Logger = diagnosticManager
	}
	err := wails.Run(appOptions)
	if err != nil {
		if diagnosticManager != nil {
			diagnosticManager.ErrorEvent("application_run_failed", diagnostics.Fields{ErrorCode: "TB-DIAG-WAILS-RUN", Message: err.Error()})
		}
		if diagnosticManager != nil {
			_ = diagnosticManager.Close()
		}
		log.Fatal(err)
	}
}
