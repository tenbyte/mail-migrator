//go:build darwin

package main

/*
#cgo CFLAGS: -x objective-c -fblocks
#cgo LDFLAGS: -framework Cocoa

#import <Cocoa/Cocoa.h>

static void TenbyteHideZoomButton(void) {
    dispatch_async(dispatch_get_main_queue(), ^{
        NSWindow *window = [NSApp keyWindow];
        if (window == nil) {
            window = [NSApp mainWindow];
        }
        NSButton *zoomButton = [window standardWindowButton:NSWindowZoomButton];
        [zoomButton setHidden:YES];
    });
}
*/
import "C"

func hideZoomButton() { C.TenbyteHideZoomButton() }
