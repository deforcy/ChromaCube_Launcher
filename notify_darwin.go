//go:build darwin

package main

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework Foundation -framework AppKit

#import <Foundation/Foundation.h>

static void showNotification(const char *title, const char *body) {
	@autoreleasepool {
		NSUserNotification *n = [[NSUserNotification alloc] init];
		n.title = [NSString stringWithUTF8String:title];
		n.informativeText = [NSString stringWithUTF8String:body];
		[[NSUserNotificationCenter defaultUserNotificationCenter] deliverNotification:n];
	}
}
*/
import "C"
import "unsafe"

// notify shows a native macOS notification. Delivered in-process via
// NSUserNotificationCenter rather than shelling out to osascript, so
// Notification Center attributes it to this app and shows its own icon
// instead of a generic Script Editor placeholder.
func notify(title, body string) error {
	cTitle := C.CString(title)
	defer C.free(unsafe.Pointer(cTitle))
	cBody := C.CString(body)
	defer C.free(unsafe.Pointer(cBody))
	C.showNotification(cTitle, cBody)
	return nil
}
